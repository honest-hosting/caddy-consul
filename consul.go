package caddyconsul

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(ConsulRouter{})
}

// ConsulRouter is a Caddy app that dynamically configures HTTP and TCP/TLS
// routing from Consul service registrations.
type ConsulRouter struct {
	logger *zap.Logger

	// Consul connection
	ConsulAddr   string `json:"address,omitempty"`
	ConsulToken  string `json:"token,omitempty"`
	ConsulScheme string `json:"scheme,omitempty"`
	ConsulDC     string `json:"datacenter,omitempty"`

	// TLS to Consul
	ConsulTLSCA         string `json:"tls_ca,omitempty"`
	ConsulTLSCert       string `json:"tls_cert,omitempty"`
	ConsulTLSKey        string `json:"tls_key,omitempty"`
	ConsulTLSSkipVerify bool   `json:"insecure_skip_verify,omitempty"`

	// Behavior
	ServiceProxyEnable *bool  `json:"service_proxy_enable,omitempty"`
	HealthPolicy       string `json:"health_policy,omitempty"`
	ConflictPolicy     string `json:"conflict_policy,omitempty"`
	ConnectProxyEnable *bool  `json:"connect_proxy_enable,omitempty"`
	DebounceDuration   string `json:"debounce_duration,omitempty"`
	PollInterval       string `json:"poll_interval,omitempty"`
	FullSyncInterval   string `json:"full_sync_interval,omitempty"`

	// Grace period before force-closing in-flight TCP connections when a
	// listener port is removed. Default 30s.
	TCPDrain string `json:"tcp_drain,omitempty"`

	// Connect
	ConnectServiceName  string `json:"connect_service_name,omitempty"`
	ConnectAutoRegister *bool  `json:"connect_auto_register,omitempty"`

	// Caddy admin API
	CaddyAdminAPI string `json:"caddy_admin_api,omitempty"`

	// Service discovery tags
	ServiceTag string `json:"service_tag,omitempty"`
	ConnectTag string `json:"connect_tag,omitempty"`

	// Connect port range for dynamic sidecar upstreams
	ConnectPortStart int `json:"connect_port_range_start,omitempty"`
	ConnectPortEnd   int `json:"connect_port_range_end,omitempty"`

	// Data directory for runtime state (persisted across reloads)
	DataDir string `json:"data_dir,omitempty"`

	// Metrics
	Metrics string `json:"metrics,omitempty"`

	// Layer 4 mode: "global" (default) or "node"
	// In "node" mode, TCP/L4 routes are only materialized for services that
	// have at least one healthy instance on the local Consul node.
	L4Mode         string `json:"l4_mode,omitempty"`
	L4NodeHostname string `json:"l4_node_hostname,omitempty"` // explicit override for node identity

	// No-cache response header policy: comma-separated status codes/classes.
	// Default: empty (no modification). Example: "3xx,4xx,5xx".
	NoCacheStatus string `json:"no_cache_status,omitempty"`

	// Internal (not serialized)
	watcher         *ConsulWatcher    `json:"-"`
	compiler        *RouteCompiler    `json:"-"`
	routeTable      *RouteTable       `json:"-"`
	stateMgr        *stateManager     `json:"-"`
	sidecarResolver *SidecarResolver  `json:"-"`
	upstreamMgr     *UpstreamManager  `json:"-"`
	registrar       *ServiceRegistrar `json:"-"`
	sidecarWarnOnce *sync.Once        `json:"-"`
	nodeName        string            `json:"-"` // resolved local node name for l4_mode=node
	noCacheMatcher  *StatusMatcher    `json:"-"` // parsed global no-cache matcher (nil = no modification)

	// In-memory TCP data plane (self-managed listeners — no admin API, no reloads)
	ctx          caddy.Context       `json:"-"` // stored for pooled TCP Listen (plan §6)
	tcpTable     *TCPTable           `json:"-"`
	tcpProxy     *tcpProxy           `json:"-"`
	tcpListeners *TCPListenerManager `json:"-"`
	metrics      *MetricsCollector   `json:"-"` // nil when `metrics` not configured
}

// RouteTable returns the shared route table for the consul_proxy handler.
func (cr *ConsulRouter) RouteTable() *RouteTable {
	return cr.routeTable
}

// NoCacheMatcher returns the parsed global no-cache status matcher.
// Returns nil if no global no_cache_status is configured (default).
func (cr *ConsulRouter) NoCacheMatcher() *StatusMatcher {
	return cr.noCacheMatcher
}

// CaddyModule returns the Caddy module information.
func (ConsulRouter) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "consul",
		New: func() caddy.Module { return new(ConsulRouter) },
	}
}

// Provision sets up the ConsulRouter.
func (cr *ConsulRouter) Provision(ctx caddy.Context) error {
	cr.logger = ctx.Logger()
	cr.ctx = ctx // stored for pooled TCP Listen during Start/reconcile (plan §6)

	cr.applyDefaults()

	if err := cr.validate(); err != nil {
		return fmt.Errorf("caddy-consul: invalid configuration: %w", err)
	}

	// Parse global no-cache status matcher (nil if unset — default is no modification)
	if cr.NoCacheStatus != "" {
		matcher, err := ParseStatusMatcher(cr.NoCacheStatus)
		if err != nil {
			return fmt.Errorf("caddy-consul: invalid no_cache_status: %w", err)
		}
		cr.noCacheMatcher = matcher
	}

	// Enable Prometheus metrics when configured. The collector is global and
	// survives reloads (created once); leaving cr.metrics nil disables all
	// recording via nil-safe recorders. Served at the admin /consul/metrics route.
	if cr.Metrics != "" {
		cr.metrics = GetOrCreateGlobalMetrics(cr.logger)
		cr.logger.Info("caddy-consul metrics enabled", zap.String("path", cr.Metrics))
	}

	adminAddr := cr.CaddyAdminAPI

	cr.logger.Info("caddy-consul provisioning",
		zap.String("address", cr.ConsulAddr),
		zap.String("scheme", cr.ConsulScheme),
		zap.String("admin_addr", adminAddr),
		zap.Bool("service_proxy_enable", boolVal(cr.ServiceProxyEnable)),
		zap.String("health_policy", cr.HealthPolicy),
		zap.String("conflict_policy", cr.ConflictPolicy),
		zap.Bool("connect_proxy_enable", boolVal(cr.ConnectProxyEnable)),
		zap.String("connect_service_name", cr.ConnectServiceName),
		zap.Bool("connect_auto_register", boolVal(cr.ConnectAutoRegister)),
		zap.String("debounce", cr.DebounceDuration),
		zap.String("l4_mode", cr.L4Mode),
	)

	// Create Consul client
	consulClient, err := cr.newConsulClient()
	if err != nil {
		return fmt.Errorf("caddy-consul: failed to create consul client: %w", err)
	}

	// Resolve local node name for l4_mode=node
	if cr.L4Mode == "node" {
		if cr.L4NodeHostname != "" {
			cr.nodeName = cr.L4NodeHostname
			cr.logger.Info("l4_mode=node: using configured hostname override",
				zap.String("node_name", cr.nodeName),
			)
		} else {
			self, err := consulClient.Agent().Self()
			if err != nil {
				return fmt.Errorf("caddy-consul: l4_mode is 'node' but failed to query Consul agent self: %w (hint: set l4_node_hostname to override)", err)
			}
			if cfg, ok := self["Config"]; ok {
				if nodeName, ok := cfg["NodeName"].(string); ok && nodeName != "" {
					cr.nodeName = nodeName
				}
			}
			if cr.nodeName == "" {
				return fmt.Errorf("caddy-consul: l4_mode is 'node' but Consul agent returned empty node name (hint: set l4_node_hostname to override)")
			}
			cr.logger.Info("l4_mode=node: resolved node name from Consul agent",
				zap.String("node_name", cr.nodeName),
			)
		}
	}

	// Service discovery components
	if boolVal(cr.ServiceProxyEnable) || boolVal(cr.ConnectProxyEnable) {
		cr.compiler = NewRouteCompiler(cr.logger)
		cr.routeTable = NewRouteTable()
		globalRouteTable.Store(cr.routeTable)

		// Load persisted state from disk (survives config reloads)
		cr.stateMgr = newStateManager(cr.DataDir, cr.logger)
		cr.stateMgr.Load()

		// Restore HTTP routes immediately from persisted state
		// so the consul_proxy handler can serve traffic right away.
		if persistedRoutes := cr.stateMgr.HTTPRoutes(); len(persistedRoutes) > 0 {
			cr.routeTable.Update(persistedRoutes)
			cr.logger.Info("restored HTTP routes from persisted state",
				zap.Int("routes", len(persistedRoutes)),
			)
		}

		// Initialize the in-memory TCP data plane (self-managed listeners — no
		// Caddy admin API, no config reloads). Warm the route table from
		// persisted state so Start() can re-open listeners before the outgoing
		// app instance stops (pooled-listener handoff; see plan §6).
		cr.tcpTable = NewTCPTable()
		globalTCPTable.Store(cr.tcpTable) // expose to the admin /consul/tcp endpoint
		if persistedTCP := cr.stateMgr.TCPRoutes(); len(persistedTCP) > 0 {
			cr.tcpTable.Update(persistedTCP)
			cr.logger.Info("restored TCP routes from persisted state",
				zap.Int("routes", len(persistedTCP)),
			)
		}
		cr.tcpProxy = newTCPProxy(cr.logger, cr.tcpTable, cr.metrics, defaultTCPPeekTimeout, defaultTCPDialTimeout)
		cr.tcpListeners = NewTCPListenerManager(ctx, cr.logger, cr.tcpTable, cr.tcpProxy, cr.metrics,
			cr.parsedTCPDrain(), cr.reservedTCPPorts())

		cr.watcher = NewConsulWatcher(
			consulClient,
			cr.logger,
			cr.parsedHealthPolicy(),
			cr.parsedDebounceDuration(),
			cr.parsedPollInterval(),
			cr.parsedFullSyncInterval(),
			cr.ServiceTag,
			cr.ConnectTag,
			cr.onServicesChanged,
		)
	}

	// Connect components
	if boolVal(cr.ConnectProxyEnable) {
		cr.sidecarResolver = NewSidecarResolver(consulClient, cr.logger, cr.ConnectServiceName)
		cr.upstreamMgr = NewUpstreamManager(
			consulClient, cr.logger, cr.ConnectServiceName,
			cr.ConnectPortStart, cr.ConnectPortEnd,
		)
		cr.sidecarWarnOnce = &sync.Once{}

		// Restore port allocations from persisted state
		if cr.stateMgr != nil {
			if allocs := cr.stateMgr.UpstreamAllocations(); len(allocs) > 0 {
				cr.upstreamMgr.RestoreAllocations(allocs)
			}
		}

		if boolVal(cr.ConnectAutoRegister) {
			cr.registrar = NewServiceRegistrar(consulClient, cr.logger, cr.ConnectServiceName)

			// Deregister from Consul only on actual process exit, not config reloads.
			// ctx.OnExit callbacks fire exclusively during process shutdown.
			registrar := cr.registrar
			ctx.OnExit(func(_ context.Context) {
				registrar.Deregister()
			})
		}

		// Warn if the sidecar proxy is not registered — connect routing won't work without it
		proxyID := cr.ConnectServiceName + "-sidecar-proxy"
		if _, _, err := consulClient.Agent().Service(proxyID, nil); err != nil {
			cr.logger.Warn("connect_proxy_enable is true but sidecar proxy not found — connect routing will not work until a sidecar proxy is running",
				zap.String("expected_sidecar", proxyID),
				zap.String("hint", "run: consul connect envoy -sidecar-for "+cr.ConnectServiceName),
				zap.Error(err),
			)
		}
	}

	return nil
}

// Start begins the Consul watcher. This is non-blocking.
// The admin API connectivity is verified lazily on the first reconciliation
// attempt, with retries, to avoid a chicken-and-egg problem during Caddy startup.
func (cr *ConsulRouter) Start() error {
	cr.logger.Info("caddy-consul starting")

	// Auto-register Caddy as a service in Consul
	if cr.registrar != nil {
		if err := cr.registrar.Register(); err != nil {
			cr.logger.Error("failed to auto-register in consul (continuing without connect)",
				zap.Error(err),
			)
		}
	}

	// Re-open TCP listeners SYNCHRONOUSLY from persisted state (warmed into the
	// table during Provision) before returning from Start. Caddy runs the new
	// instance's Start() to completion before the old instance's Stop(), so
	// opening the same ports here (pooled Listen, refcount 1->2) before the old
	// instance closes them (2->1) hands the socket off with zero downtime — see
	// plan §6 (spike-verified). Uses only in-memory data: zero Consul queries.
	if cr.tcpListeners != nil {
		if ports := cr.tcpTable.Ports(); len(ports) > 0 {
			cr.logger.Info("re-opening TCP listeners from persisted state",
				zap.Int("ports", len(ports)),
			)
			if err := cr.tcpListeners.Reconcile(ports); err != nil {
				cr.logger.Error("some TCP listeners failed to re-open from persisted state",
					zap.Error(err),
				)
			}
		}
	}

	if cr.watcher != nil {
		// Restore watcher state so it resumes blocking queries
		// instead of re-fetching all services from scratch.
		if cr.stateMgr != nil {
			catalogIdx := cr.stateMgr.CatalogIndex()
			healthIdx := cr.stateMgr.HealthStateIndex()
			svcStates := cr.stateMgr.ServiceStates()
			passingChecks := cr.stateMgr.PassingChecks()
			if catalogIdx > 0 && len(svcStates) > 0 {
				cr.watcher.RestoreState(catalogIdx, healthIdx, svcStates, passingChecks)
			}
		}
		cr.watcher.Start()
	}
	return nil
}

// Stop gracefully shuts down the Consul watcher and cert manager.
// Note: we intentionally do NOT deregister from Consul here. Stop() is called
// on every config reload (via admin API PATCH), not just on final shutdown.
// Deregistering would cause a registration gap between the old and new app
// instances. The registration persists and the TTL will expire naturally.
func (cr *ConsulRouter) Stop() error {
	cr.logger.Info("caddy-consul stopping")
	if cr.watcher != nil {
		cr.watcher.Stop()
	}
	if cr.registrar != nil {
		cr.registrar.Stop()
	}
	// Close TCP listeners (decrements the pooled-listener refcount). On a reload
	// the incoming instance has already re-opened these in its Start(), so the
	// sockets survive; in-flight connections drain in the background.
	if cr.tcpListeners != nil {
		cr.tcpListeners.Close()
	}
	return nil
}

// Cleanup releases resources.
func (cr *ConsulRouter) Cleanup() error {
	cr.logger.Debug("caddy-consul cleanup")
	return cr.Stop()
}

// onServicesChanged is the callback from the watcher when Consul services change.
func (cr *ConsulRouter) onServicesChanged(changes []ServiceChange, allServices map[string]*ServiceState) {
	cr.logger.Info("consul services changed",
		zap.Int("changes", len(changes)),
		zap.Int("total_services", len(allServices)),
	)

	// Each callback is one debounced converge; time the whole pass.
	convergeStart := time.Now()
	cr.metrics.IncDebounce()
	defer func() { cr.metrics.ObserveReconcile(time.Since(convergeStart)) }()

	// Parse route definitions from all services
	var allRoutes []RouteDefinition
	serviceProxyEnabled := boolVal(cr.ServiceProxyEnable)
	connectProxyEnabled := boolVal(cr.ConnectProxyEnable)

	// First pass: parse routes and identify Connect services that need sidecar upstreams
	type parsedService struct {
		routes []RouteDefinition
	}
	var parsedServices []parsedService
	connectServices := make(map[string]bool)

	for _, svc := range allServices {
		routes := ParseServiceRoutes(svc, cr.ServiceTag, cr.ConnectTag, cr.logger)
		parsedServices = append(parsedServices, parsedService{routes: routes})

		// Identify services that should use Connect (tagged with connect_tag)
		if connectProxyEnabled {
			for _, tag := range svc.Tags {
				if tag == cr.ConnectTag {
					for _, r := range routes {
						if !r.IsRedirect() {
							connectServices[r.ServiceName] = true
						}
					}
					break
				}
			}
		}
	}

	// When l4_mode=node, filter Connect upstreams to only include services
	// with at least one healthy instance on the local node. This prevents
	// sidecar upstream entries from being registered on nodes where the
	// workload doesn't run.
	if cr.L4Mode == "node" && cr.nodeName != "" && connectProxyEnabled {
		filtered := make(map[string]bool)
		for svcName := range connectServices {
			for _, ps := range parsedServices {
				for _, r := range ps.routes {
					if r.ServiceName != svcName {
						continue
					}
					for _, u := range r.Upstreams {
						if u.NodeName == cr.nodeName {
							filtered[svcName] = true
						}
					}
				}
			}
		}
		if len(filtered) != len(connectServices) {
			cr.logger.Info("l4_mode=node: filtered Connect upstreams by local node",
				zap.String("node", cr.nodeName),
				zap.Int("before", len(connectServices)),
				zap.Int("after", len(filtered)),
			)
		}
		connectServices = filtered
	}

	// Sync sidecar upstreams for Connect services (single Consul API call).
	// We must call SyncUpstreams even when connectServices is empty so that
	// stale upstreams get removed (e.g., workload moved off this node).
	if connectProxyEnabled && cr.upstreamMgr != nil {
		if changed, err := cr.upstreamMgr.SyncUpstreams(connectServices); err != nil {
			cr.logger.Error("failed to sync sidecar upstreams",
				zap.Error(err),
			)
		} else if changed {
			// Brief wait for xDS propagation so Envoy opens listeners
			// before SidecarResolver queries them
			select {
			case <-time.After(200 * time.Millisecond):
			default:
			}
		}
	}

	// Second pass: determine upstream mode and Via tag for each route
	for _, ps := range parsedServices {
		for i := range ps.routes {
			if ps.routes[i].IsRedirect() {
				continue
			}

			// Determine the Via tag based on whether this is a connect or direct service.
			// Redirects are excluded — they never enter Consul or the mesh.
			isConnect := connectProxyEnabled && connectServices[ps.routes[i].ServiceName]
			if isConnect {
				ps.routes[i].Via = cr.ConnectTag
			} else if serviceProxyEnabled {
				ps.routes[i].Via = cr.ServiceTag
			}

			// Connect services use sidecar routing
			if isConnect && cr.sidecarResolver != nil {
				if err := cr.sidecarResolver.ResolveUpstreams(&ps.routes[i]); err == nil {
					ps.routes[i].UpstreamMode = UpstreamConnectSidecar
				} else {
					cr.sidecarWarnOnce.Do(func() {
						cr.logger.Warn("connect_proxy_enable is true but sidecar resolution is failing — ensure Envoy sidecar is running",
							zap.String("hint", "run: consul connect envoy -sidecar-for "+cr.ConnectServiceName),
							zap.Error(err),
						)
					})
					// Fall through to direct if enabled
					if serviceProxyEnabled {
						ps.routes[i].UpstreamMode = UpstreamDirect
						ps.routes[i].Via = cr.ServiceTag
					} else {
						ps.routes[i].Upstreams = nil
					}
				}
				continue
			}

			// Non-Connect services use direct routing
			if serviceProxyEnabled {
				ps.routes[i].UpstreamMode = UpstreamDirect
			} else {
				cr.logger.Debug("no routing mode available for service; skipping",
					zap.String("service", ps.routes[i].ServiceName),
				)
				ps.routes[i].Upstreams = nil
			}
		}

		allRoutes = append(allRoutes, ps.routes...)
	}

	// Log route summary before compilation
	var directCount, sidecarCount, redirectCount, totalHealthy int
	for i := range allRoutes {
		switch {
		case allRoutes[i].IsRedirect():
			redirectCount++
		case allRoutes[i].UpstreamMode == UpstreamConnectSidecar:
			sidecarCount++
		default:
			directCount++
		}
		for _, u := range allRoutes[i].Upstreams {
			if u.Healthy {
				totalHealthy++
			}
		}
	}
	cr.logger.Info("applying route changes",
		zap.Int("total_services", len(allServices)),
		zap.Int("routable_services", len(allRoutes)),
		zap.Int("direct_routes", directCount),
		zap.Int("connect_routes", sidecarCount),
		zap.Int("redirect_routes", redirectCount),
		zap.Int("healthy_upstreams", totalHealthy),
	)

	// Count routes by protocol type
	var l4Count, httpCount int
	for _, r := range allRoutes {
		switch r.Protocol {
		case ProtocolTCP, ProtocolTLSPassthrough:
			l4Count++
		case ProtocolHTTP, ProtocolHTTPS:
			httpCount++
		}
	}

	// Filter TCP routes by node locality if l4_mode=node
	if cr.L4Mode == "node" && cr.nodeName != "" {
		l4Before := l4Count
		allRoutes = FilterTCPRoutesByNode(allRoutes, cr.nodeName)
		l4Count = 0
		for _, r := range allRoutes {
			if r.Protocol == ProtocolTCP || r.Protocol == ProtocolTLSPassthrough {
				l4Count++
			}
		}
		cr.logger.Info("l4_mode=node: filtered L4 routes by local node",
			zap.String("node", cr.nodeName),
			zap.Int("l4_before", l4Before),
			zap.Int("l4_after", l4Count),
			zap.Int("l4_filtered_out", l4Before-l4Count),
			zap.Int("http_routes", httpCount),
		)
	} else {
		cr.logger.Info("l4_mode=global: routing all L4 services",
			zap.Int("l4_routes", l4Count),
			zap.Int("http_routes", httpCount),
		)
	}

	// Record converge route metrics (counts are final after l4_mode filtering).
	cr.metrics.SetServicesTotal(len(allServices))
	cr.metrics.SetRoutesTotal("http", httpCount)
	cr.metrics.SetRoutesTotal("tcp", l4Count)

	// Compile routes
	compiled := cr.compiler.Compile(allRoutes)

	// Log conflicts
	for _, c := range compiled.Conflicts {
		cr.logger.Warn("route conflict detected",
			zap.String("type", string(c.Type)),
			zap.String("winner", c.Winner.ServiceName),
			zap.String("loser", c.Loser.ServiceName),
			zap.String("reason", c.Reason),
		)
		cr.metrics.IncConflict(string(c.Type))
	}

	// Update HTTP routes in-memory (no admin API, no config reload)
	newHTTPHash := hashRoutes(compiled.HTTPRoutes)
	if newHTTPHash != cr.stateMgr.HTTPRouteHash() {
		cr.routeTable.Update(compiled.HTTPRoutes)
		cr.stateMgr.SetHTTPRouteHash(newHTTPHash)
		cr.logger.Info("HTTP routes updated in-memory",
			zap.Int("routes", len(compiled.HTTPRoutes)),
		)
	} else {
		cr.logger.Debug("HTTP routes unchanged, skipping update")
	}

	// Persist full state to disk. TCP routes are persisted (mirroring HTTPRoutes)
	// so Start() can re-open listeners from disk after a reload with zero Consul
	// queries. There is no admin API / reload to race anymore, but we still save
	// before applying so a crash can't lose convergence.
	cr.stateMgr.SetHTTPRoutes(compiled.HTTPRoutes)
	cr.stateMgr.SetTCPRoutes(compiled.TCPRoutes)
	cr.stateMgr.SetServiceStates(allServices)
	if cr.watcher != nil {
		cr.stateMgr.SetCatalogIndex(cr.watcher.CatalogIndex())
		cr.stateMgr.SetHealthStateIndex(cr.watcher.HealthStateIndex())
		cr.stateMgr.SetPassingChecks(cr.watcher.PassingChecks())
	}
	if cr.upstreamMgr != nil {
		cr.stateMgr.SetUpstreamAllocations(cr.upstreamMgr.Allocations())
	}
	cr.stateMgr.Save()

	// Apply TCP routes in-memory: swap the route table atomically, then reconcile
	// listeners (open new ports / close removed). No Caddy admin API, no config
	// reload — instance/health churn is a pure table swap; only a new or removed
	// port touches a listener.
	if cr.tcpTable != nil {
		cr.tcpTable.Update(compiled.TCPRoutes)
		if err := cr.tcpListeners.Reconcile(cr.tcpTable.Ports()); err != nil {
			cr.logger.Error("failed to reconcile TCP listeners",
				zap.Error(err),
			)
			cr.metrics.IncReconcileError()
		}
	}
}

// reservedTCPPorts returns ports the TCP listener manager must never bind:
// the HTTP/HTTPS ports plus the Caddy admin API port.
func (cr *ConsulRouter) reservedTCPPorts() map[int]bool {
	reserved := map[int]bool{80: true, 443: true}
	if _, portStr, err := net.SplitHostPort(cr.CaddyAdminAPI); err == nil {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
			reserved[p] = true
		}
	}
	return reserved
}

// Interface guards
var (
	_ caddy.Module       = (*ConsulRouter)(nil)
	_ caddy.Provisioner  = (*ConsulRouter)(nil)
	_ caddy.CleanerUpper = (*ConsulRouter)(nil)
	_ caddy.App          = (*ConsulRouter)(nil)
)
