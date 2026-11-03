package caddyconsul

import (
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	caddy.RegisterModule(ConsulMetricsHandler{})
	httpcaddyfile.RegisterHandlerDirective("consul_metrics", parseConsulMetricsDirective)
	httpcaddyfile.RegisterDirectiveOrder("consul_metrics", httpcaddyfile.Before, "respond")
}

// ConsulMetricsHandler serves caddy-consul's Prometheus metrics (the same registry as the admin
// /consul/metrics route) from an ordinary HTTP route, so they can be scraped with the admin API
// disabled or bound to localhost:
//
//	http://:2019 {
//		route {
//			metrics /metrics
//			consul_metrics /metrics/consul
//			respond 404
//		}
//	}
//
// Requires the consul app's `metrics` option, as the admin route does. It answers every request
// it gets and never calls the next handler.
type ConsulMetricsHandler struct{}

// CaddyModule returns the Caddy module information.
func (ConsulMetricsHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.consul_metrics",
		New: func() caddy.Module { return new(ConsulMetricsHandler) },
	}
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (ConsulMetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	if r.Method != http.MethodGet {
		return caddyhttp.Error(http.StatusMethodNotAllowed, nil)
	}
	metrics := GetMetrics()
	if metrics == nil {
		http.Error(w, "metrics not enabled - set the consul app's 'metrics' option", http.StatusNotFound)
		return nil
	}
	metrics.ServeHTTP(w, r)
	return nil
}

// UnmarshalCaddyfile parses the directive. Syntax:
//
//	consul_metrics [<matcher>]
func (h *ConsulMetricsHandler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name
	if d.NextArg() {
		return d.ArgErr()
	}
	if d.NextBlock(0) {
		return d.Errf("consul_metrics takes no options")
	}
	return nil
}

func parseConsulMetricsDirective(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var handler ConsulMetricsHandler
	err := handler.UnmarshalCaddyfile(h.Dispenser)
	return &handler, err
}

// Interface guards
var (
	_ caddyhttp.MiddlewareHandler = (*ConsulMetricsHandler)(nil)
	_ caddyfile.Unmarshaler       = (*ConsulMetricsHandler)(nil)
)
