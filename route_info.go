package caddyconsul

import (
	"fmt"
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	caddy.RegisterModule(ConsulRouteInfoHandler{})
	httpcaddyfile.RegisterHandlerDirective("consul_route_info", parseConsulRouteInfoDirective)
	// It only sets variables, so it sits with "vars" (the anchor must be a standard directive;
	// RegisterDirectiveOrder panics on a plugin's, such as consul_proxy).
	httpcaddyfile.RegisterDirectiveOrder("consul_route_info", httpcaddyfile.After, "vars")
}

// ServiceVarKey is the request variable consul_route_info sets to the matched route's Consul
// service name ({http.vars.consul.service}).
const ServiceVarKey = "consul.service"

// ConsulRouteInfoHandler looks the request up in the same route table consul_proxy uses and
// exposes what it finds as request variables, then always calls the next handler. Place it
// early (before a WAF, logging or rate limiting) so those can identify which Consul service
// a request is for, before consul_proxy routes it.
//
// It sets ServiceVarKey to the service name, plus one variable per entry in Vars, taken
// from the service instance's key=value tags. With `"vars": {"tenant": "X-Tenant"}` and an
// instance tagged `X-Tenant=acme`, {http.vars.tenant} is "acme". Nothing is set when no route
// matches or a tag is absent, so placeholders resolve to empty.
type ConsulRouteInfoHandler struct {
	// Vars maps request variable names to the tag keys to read them from.
	Vars map[string]string `json:"vars,omitempty"`

	routeTable *RouteTable
}

// CaddyModule returns the Caddy module information.
func (ConsulRouteInfoHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.consul_route_info",
		New: func() caddy.Module { return new(ConsulRouteInfoHandler) },
	}
}

// Provision gets a reference to the consul app's route table.
func (h *ConsulRouteInfoHandler) Provision(ctx caddy.Context) error {
	app, err := ctx.App("consul")
	if err != nil {
		return fmt.Errorf("consul_route_info: consul app not configured: %w", err)
	}

	consulApp, ok := app.(*ConsulRouter)
	if !ok {
		return fmt.Errorf("consul_route_info: unexpected consul app type")
	}

	h.routeTable = consulApp.RouteTable()
	if h.routeTable == nil {
		return fmt.Errorf("consul_route_info: consul app route table not initialized")
	}

	return nil
}

// ServeHTTP sets the route variables for the request, if any, and calls next.
func (h *ConsulRouteInfoHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if route := h.routeTable.Match(r.Host, r.URL.Path); route != nil {
		setRouteVars(r, route, h.Vars)
	}
	return next.ServeHTTP(w, r)
}

// setRouteVars sets ServiceVarKey and each configured variable present in the route's tags.
func setRouteVars(r *http.Request, route *CompiledHTTPRoute, vars map[string]string) {
	caddyhttp.SetVar(r.Context(), ServiceVarKey, route.ServiceName)
	for name, tagKey := range vars {
		if v, ok := route.TagValues[tagKey]; ok {
			caddyhttp.SetVar(r.Context(), name, v)
		}
	}
}

// UnmarshalCaddyfile parses the consul_route_info directive:
//
//	consul_route_info {
//		var <name> <tag-key>
//	}
func (h *ConsulRouteInfoHandler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name
	if d.NextArg() {
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		switch d.Val() {
		case "var":
			var name, tagKey string
			if !d.Args(&name, &tagKey) {
				return d.ArgErr()
			}
			if d.NextArg() {
				return d.ArgErr()
			}
			if h.Vars == nil {
				h.Vars = make(map[string]string)
			}
			h.Vars[name] = tagKey
		default:
			return d.Errf("unrecognized subdirective %q", d.Val())
		}
	}
	return nil
}

func parseConsulRouteInfoDirective(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var handler ConsulRouteInfoHandler
	err := handler.UnmarshalCaddyfile(h.Dispenser)
	return &handler, err
}

// Interface guards
var (
	_ caddy.Module                = (*ConsulRouteInfoHandler)(nil)
	_ caddy.Provisioner           = (*ConsulRouteInfoHandler)(nil)
	_ caddyhttp.MiddlewareHandler = (*ConsulRouteInfoHandler)(nil)
	_ caddyfile.Unmarshaler       = (*ConsulRouteInfoHandler)(nil)
)
