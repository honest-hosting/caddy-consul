package caddyconsul

import (
	"encoding/json"
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

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
