package caddyconsul

import (
	"encoding/json"
	"net/http"
	"sync/atomic"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

// globalRouteTable holds a reference to the active RouteTable for the admin API.
// Set by ConsulRouter on Provision.
var globalRouteTable atomic.Value

// globalTCPTable holds a reference to the active TCPTable for the admin API.
// Set by ConsulRouter on Provision.
var globalTCPTable atomic.Value

func init() {
	caddy.RegisterModule(AdminConsul{})
}

// AdminConsul is a Caddy admin module that exposes consul metrics and debug endpoints.
type AdminConsul struct {
	logger *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (AdminConsul) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "admin.api.consul",
		New: func() caddy.Module { return new(AdminConsul) },
	}
}

// Provision sets up the admin consul handler.
func (ac *AdminConsul) Provision(ctx caddy.Context) error {
	ac.logger = ctx.Logger()
	ac.logger.Info("consul admin endpoints registered",
		zap.String("metrics", "/consul/metrics"),
		zap.String("state", "/consul/state"),
		zap.String("routes", "/consul/routes"),
		zap.String("tcp", "/consul/tcp"),
	)
	return nil
}

// Routes returns the routes for the admin API.
func (ac *AdminConsul) Routes() []caddy.AdminRoute {
	return []caddy.AdminRoute{
		{
			Pattern: "/consul/metrics",
			Handler: caddy.AdminHandlerFunc(ac.serveMetrics),
		},
		{
			Pattern: "/consul/state",
			Handler: caddy.AdminHandlerFunc(ac.serveState),
		},
		{
			Pattern: "/consul/routes",
			Handler: caddy.AdminHandlerFunc(ac.serveRoutes),
		},
		{
			Pattern: "/consul/tcp",
			Handler: caddy.AdminHandlerFunc(ac.serveTCP),
		},
	}
}

// serveMetrics serves the Prometheus metrics endpoint.
func (ac *AdminConsul) serveMetrics(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet {
		return caddy.APIError{
			HTTPStatus: http.StatusMethodNotAllowed,
			Message:    "method not allowed",
		}
	}

	metrics := GetMetrics()
	if metrics == nil {
		return caddy.APIError{
			HTTPStatus: http.StatusNotFound,
			Message:    "metrics not enabled - add 'metrics /metrics/consul' to your consul configuration",
		}
	}

	metrics.ServeHTTP(w, r)
	return nil
}

// serveState serves a JSON dump of the current routing state.
func (ac *AdminConsul) serveState(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet {
		return caddy.APIError{
			HTTPStatus: http.StatusMethodNotAllowed,
			Message:    "method not allowed",
		}
	}

	// Return a summary of the current state
	state := map[string]interface{}{
		"status": "ok",
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(state)
}

// serveRoutes serves a JSON dump of the current in-memory HTTP route table.
func (ac *AdminConsul) serveRoutes(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet {
		return caddy.APIError{
			HTTPStatus: http.StatusMethodNotAllowed,
			Message:    "method not allowed",
		}
	}

	rt := globalRouteTable.Load()
	if rt == nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
		return nil
	}

	routes := rt.(*RouteTable).Routes()
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(routes)
}

// serveTCP serves a JSON dump of the current in-memory TCP route table
// (port, SNI, upstreams, passthrough) — useful for verifying that TCP/Connect
// routes resolved correctly without inspecting Caddy's config.
func (ac *AdminConsul) serveTCP(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet {
		return caddy.APIError{
			HTTPStatus: http.StatusMethodNotAllowed,
			Message:    "method not allowed",
		}
	}

	tt := globalTCPTable.Load()
	if tt == nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
		return nil
	}

	routes := tt.(*TCPTable).Snapshot()
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(routes)
}

// Cleanup cleans up the admin handler.
func (ac *AdminConsul) Cleanup() error {
	if ac.logger != nil {
		ac.logger.Debug("consul admin cleanup called")
	}
	return nil
}

// Interface guards
var (
	_ caddy.Module       = (*AdminConsul)(nil)
	_ caddy.Provisioner  = (*AdminConsul)(nil)
	_ caddy.AdminRouter  = (*AdminConsul)(nil)
	_ caddy.CleanerUpper = (*AdminConsul)(nil)
)
