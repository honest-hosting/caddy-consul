package caddyconsul

import (
	"fmt"

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
	ConsulAddr   string `json:"consul_addr,omitempty"`
	ConsulToken  string `json:"consul_token,omitempty"`
	ConsulScheme string `json:"consul_scheme,omitempty"`
	ConsulDC     string `json:"consul_dc,omitempty"`

	// TLS to Consul
	ConsulTLSCA   string `json:"consul_tls_ca,omitempty"`
	ConsulTLSCert string `json:"consul_tls_cert,omitempty"`
	ConsulTLSKey  string `json:"consul_tls_key,omitempty"`

	// Behavior
	HealthPolicy     string `json:"health_policy,omitempty"`
	ConflictPolicy   string `json:"conflict_policy,omitempty"`
	ConnectMode      string `json:"connect_mode,omitempty"`
	DebounceDuration   string `json:"debounce_duration,omitempty"`
	MaxConcurrentChecks int   `json:"max_concurrent_checks,omitempty"`

	// Connect
	ConnectServiceName  string `json:"connect_service_name,omitempty"`
	ConnectAutoRegister bool   `json:"connect_auto_register"`

	// Metrics
	Metrics string `json:"metrics,omitempty"`

	// Internal (not serialized)
	watcher             *ConsulWatcher       `json:"-"`
	compiler            *RouteCompiler       `json:"-"`
	reconciler          *Reconciler          `json:"-"`
	sidecarResolver     *SidecarResolver     `json:"-"`
	directResolver      *DirectResolver      `json:"-"`
	certManager         *CertManager         `json:"-"`
	registrar           *ServiceRegistrar    `json:"-"`
	connectAutoRegisterSet bool              `json:"-"` // tracks if explicitly set in Caddyfile
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

	adminAddr := caddy.DefaultAdminListen

	cr.logger.Info("caddy-consul provisioning",
		zap.String("consul_addr", cr.ConsulAddr),
		zap.String("consul_scheme", cr.ConsulScheme),
		zap.String("admin_addr", adminAddr),
		zap.String("health_policy", cr.HealthPolicy),
		zap.String("conflict_policy", cr.ConflictPolicy),
		zap.String("connect_mode", cr.ConnectMode),
		zap.String("connect_service_name", cr.ConnectServiceName),
		zap.Bool("connect_auto_register", cr.ConnectAutoRegister),
		zap.String("debounce", cr.DebounceDuration),
	)

	// Create Consul client
	consulClient, err := cr.newConsulClient()
	if err != nil {
		return fmt.Errorf("caddy-consul: failed to create consul client: %w", err)
	}

	// Initialize components
	cr.compiler = NewRouteCompiler(cr.logger)
	cr.reconciler = NewReconciler(cr.logger, adminAddr)

	// Connect resolvers
	cr.sidecarResolver = NewSidecarResolver(consulClient, cr.logger, cr.ConnectServiceName)
	cr.directResolver = NewDirectResolver(consulClient, cr.logger, cr.ConnectServiceName)
	cr.certManager = NewCertManager(consulClient, cr.logger, cr.ConnectServiceName, ctx)

	// Auto-registration
	if cr.ConnectAutoRegister {
		cr.registrar = NewServiceRegistrar(consulClient, cr.logger, cr.ConnectServiceName)
	}

	cr.watcher = NewConsulWatcher(
		consulClient,
		cr.logger,
		cr.parsedHealthPolicy(),
		cr.parsedDebounceDuration(),
		cr.MaxConcurrentChecks,
		cr.onServicesChanged,
	)

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

	// Start cert manager for direct mode
	if cr.certManager != nil {
		cr.certManager.Start()
	}

	cr.watcher.Start()
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
	if cr.certManager != nil {
		cr.certManager.Stop()
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
	for _, svc := range allServices {
		routes := ParseServiceRoutes(svc, cr.ConnectMode, cr.logger)

		// Resolve connect upstreams based on mode
		for i := range routes {
			switch routes[i].UpstreamMode {
			case UpstreamConnectSidecar:
				if err := cr.sidecarResolver.ResolveUpstreams(&routes[i]); err != nil {
					cr.logger.Warn("failed to resolve connect sidecar upstream",
						zap.String("service", routes[i].ServiceName),
						zap.Error(err),
					)
					routes[i].Upstreams = nil // skip this route
				}
			case UpstreamConnectDirect:
				if err := cr.directResolver.ResolveUpstreams(&routes[i]); err != nil {
					cr.logger.Warn("failed to resolve connect direct upstream",
						zap.String("service", routes[i].ServiceName),
						zap.Error(err),
					)
				}
			}
		}

		allRoutes = append(allRoutes, routes...)
	}

	// Compile routes
	compiled := cr.compiler.Compile(allRoutes)

	// Inject TLS credentials for connect-direct HTTP routes
	cr.injectDirectModeTLS(compiled, allRoutes)

	// Log conflicts
	for _, c := range compiled.Conflicts {
		cr.logger.Warn("route conflict detected",
			zap.String("type", string(c.Type)),
			zap.String("winner", c.Winner.ServiceName),
			zap.String("loser", c.Loser.ServiceName),
			zap.String("reason", c.Reason),
		)
	}

	// Apply to Caddy via admin API
	if err := cr.reconciler.Apply(compiled); err != nil {
		cr.logger.Error("failed to reconcile routes",
			zap.Error(err),
		)
	}
}

// injectDirectModeTLS populates TLS cert fields on compiled HTTP routes whose
// source RouteDefinition used connect-direct mode.
func (cr *ConsulRouter) injectDirectModeTLS(compiled *CompiledConfig, allRoutes []RouteDefinition) {
	if cr.certManager == nil {
		return
	}

	// Build lookup: service name → upstream mode
	modeByService := make(map[string]UpstreamMode)
	for _, r := range allRoutes {
		modeByService[r.ServiceName] = r.UpstreamMode
	}

	leaf := cr.certManager.GetLeafCert()
	caRoots := cr.certManager.GetCARoots()

	if leaf == nil {
		cr.logger.Warn("connect-direct routes exist but no leaf cert available yet")
		return
	}

	for i := range compiled.HTTPRoutes {
		if modeByService[compiled.HTTPRoutes[i].ServiceName] == UpstreamConnectDirect {
			compiled.HTTPRoutes[i].TLSCertPEM = leaf.CertPEM
			compiled.HTTPRoutes[i].TLSKeyPEM = leaf.KeyPEM
			compiled.HTTPRoutes[i].TLSCACertPEM = caRoots
		}
	}
}

// Interface guards
var (
	_ caddy.Module       = (*ConsulRouter)(nil)
	_ caddy.Provisioner  = (*ConsulRouter)(nil)
	_ caddy.CleanerUpper = (*ConsulRouter)(nil)
	_ caddy.App          = (*ConsulRouter)(nil)
)
