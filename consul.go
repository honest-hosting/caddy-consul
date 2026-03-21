package caddyconsul

import (
	"fmt"
	"sync"

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
	ConsulTLSCA             string `json:"tls_ca,omitempty"`
	ConsulTLSCert           string `json:"tls_cert,omitempty"`
	ConsulTLSKey            string `json:"tls_key,omitempty"`
	ConsulTLSSkipVerify     bool   `json:"insecure_skip_verify,omitempty"`

	// Behavior
	ServiceProxyEnable *bool  `json:"service_proxy_enable,omitempty"`
	HealthPolicy       string `json:"health_policy,omitempty"`
	ConflictPolicy     string `json:"conflict_policy,omitempty"`
	ConnectProxyEnable *bool  `json:"connect_proxy_enable,omitempty"`
	DebounceDuration   string `json:"debounce_duration,omitempty"`
	MaxConcurrentChecks int   `json:"max_concurrent_checks,omitempty"`

	// Connect
	ConnectServiceName  string `json:"connect_service_name,omitempty"`
	ConnectAutoRegister *bool  `json:"connect_auto_register,omitempty"`

	// Caddy admin API
	CaddyAdminAPI string `json:"caddy_admin_api,omitempty"`

	// Data directory for runtime state (persisted across reloads)
	DataDir string `json:"data_dir,omitempty"`

	// Metrics
	Metrics string `json:"metrics,omitempty"`

	// Internal (not serialized)
	watcher             *ConsulWatcher       `json:"-"`
	compiler            *RouteCompiler       `json:"-"`
	reconciler          *Reconciler          `json:"-"`
	routeTable          *RouteTable          `json:"-"`
	stateMgr            *stateManager        `json:"-"`
	sidecarResolver     *SidecarResolver     `json:"-"`
	registrar           *ServiceRegistrar    `json:"-"`
	sidecarWarnOnce     *sync.Once           `json:"-"`
}

// RouteTable returns the shared route table for the consul_proxy handler.
func (cr *ConsulRouter) RouteTable() *RouteTable {
	return cr.routeTable
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

	cr.applyDefaults()

	if err := cr.validate(); err != nil {
		return fmt.Errorf("caddy-consul: invalid configuration: %w", err)
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
	)

	// Create Consul client
	consulClient, err := cr.newConsulClient()
	if err != nil {
		return fmt.Errorf("caddy-consul: failed to create consul client: %w", err)
	}

	// Service discovery components
	if boolVal(cr.ServiceProxyEnable) || boolVal(cr.ConnectProxyEnable) {
		cr.compiler = NewRouteCompiler(cr.logger)
		cr.routeTable = NewRouteTable()
		globalRouteTable.Store(cr.routeTable)

		// Load persisted state from disk (survives config reloads)
		cr.stateMgr = newStateManager(cr.DataDir, cr.logger)
		cr.stateMgr.Load()

		// Initialize reconciler for TCP routes (with persisted state from prior reload)
		cr.reconciler = NewReconciler(cr.logger, adminAddr)
		tcpHashes, tcpNames := cr.stateMgr.TCPState()
		cr.reconciler.RestoreTCPState(tcpHashes, tcpNames)

		cr.watcher = NewConsulWatcher(
			consulClient,
			cr.logger,
			cr.parsedHealthPolicy(),
			cr.parsedDebounceDuration(),
			cr.MaxConcurrentChecks,
			cr.onServicesChanged,
		)
	}

	// Connect components
	if boolVal(cr.ConnectProxyEnable) {
		cr.sidecarResolver = NewSidecarResolver(consulClient, cr.logger, cr.ConnectServiceName)
		cr.sidecarWarnOnce = &sync.Once{}

		if boolVal(cr.ConnectAutoRegister) {
			cr.registrar = NewServiceRegistrar(consulClient, cr.logger, cr.ConnectServiceName)
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

	if cr.watcher != nil {
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

	// Parse route definitions from all services
	var allRoutes []RouteDefinition
	serviceProxyEnabled := boolVal(cr.ServiceProxyEnable)
	connectProxyEnabled := boolVal(cr.ConnectProxyEnable)

	for _, svc := range allServices {
		routes := ParseServiceRoutes(svc, cr.logger)

		// Determine upstream mode for each route
		for i := range routes {
			// Redirect routes don't need upstream resolution
			if routes[i].IsRedirect() {
				continue
			}

			if connectProxyEnabled && cr.sidecarResolver != nil {
				// Try sidecar resolution; if it succeeds, use connect-sidecar mode
				if err := cr.sidecarResolver.ResolveUpstreams(&routes[i]); err == nil {
					routes[i].UpstreamMode = UpstreamConnectSidecar
					continue
				} else {
					// Warn once that connect proxy isn't working (avoid log spam)
					cr.sidecarWarnOnce.Do(func() {
						cr.logger.Warn("connect_proxy_enable is true but sidecar resolution is failing — falling back to direct routing. Ensure a sidecar proxy is running.",
							zap.String("hint", "run: consul connect envoy -sidecar-for "+cr.ConnectServiceName),
							zap.Error(err),
						)
					})
				}
				// Sidecar resolution failed — fall through to direct if enabled
			}

			if serviceProxyEnabled {
				routes[i].UpstreamMode = UpstreamDirect
			} else {
				// Neither mode can route this service
				cr.logger.Debug("no routing mode available for service; skipping",
					zap.String("service", routes[i].ServiceName),
				)
				routes[i].Upstreams = nil
			}
		}

		allRoutes = append(allRoutes, routes...)
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

	// Reconcile TCP routes via admin API (infrequent, acceptable reload)
	_, existingTCPNames := cr.stateMgr.TCPState()
	if len(compiled.TCPRoutes) > 0 || len(existingTCPNames) > 0 {
		tcpConfig := &CompiledConfig{
			TCPRoutes: compiled.TCPRoutes,
		}
		if err := cr.reconciler.ApplyTCP(tcpConfig); err != nil {
			cr.logger.Error("failed to reconcile TCP routes",
				zap.Error(err),
			)
		}
		// Persist TCP state for reload survival
		hashes, names := cr.reconciler.TCPState()
		cr.stateMgr.SetTCPState(hashes, names)
	}

	// Save state to disk (survives config reloads)
	cr.stateMgr.Save()
}

// Interface guards
var (
	_ caddy.Module       = (*ConsulRouter)(nil)
	_ caddy.Provisioner  = (*ConsulRouter)(nil)
	_ caddy.CleanerUpper = (*ConsulRouter)(nil)
	_ caddy.App          = (*ConsulRouter)(nil)
)
