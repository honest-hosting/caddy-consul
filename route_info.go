package caddyconsul

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/prometheus/client_golang/prometheus"
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
//
// With Metrics, it also counts every request that matched a route in
// caddy_consul_route_requests_total{service, <metric_labels...>, class, error}: class is the
// final status class ("2xx".."5xx"), error is "true" when a later handler ended the request with
// an error instead of a response (e.g. a WAF block, which Caddy's error routes then render).
// MetricLabels adds one label per entry, from the instance's key=value tags (e.g. tenant), and
// HostLabel names a label carrying the matched route's host (from the route table, not the
// request's Host header, so it is bounded by the configured routes). It needs the consul app's
// `metrics` option and is served with caddy-consul's other metrics (consul_metrics handler, or
// the admin /consul/metrics route).
type ConsulRouteInfoHandler struct {
	// Vars maps request variable names to the tag keys to read them from.
	Vars map[string]string `json:"vars,omitempty"`

	// Metrics enables the per-route request counter.
	Metrics bool `json:"metrics,omitempty"`

	// MetricLabels maps extra counter label names to the tag keys to read them from. Keep it to
	// low-cardinality, trusted values (they come from Consul tags, never from the request).
	MetricLabels map[string]string `json:"metric_labels,omitempty"`

	// HostLabel, if set, adds a counter label with this name holding the matched route's host
	// (e.g. "host" -> host="blog.example.com"). Redirect routes (aliases) count under their own host.
	HostLabel string `json:"host_label,omitempty"`

	routeTable   *RouteTable
	counter      *prometheus.CounterVec
	labelNames   []string // extra counter labels in order: MetricLabels keys + HostLabel, sorted
	labelTagKeys []string // tag key for each of labelNames ("" = the route host, for HostLabel)
}

var metricLabelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

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

	if !h.Metrics {
		if len(h.MetricLabels) > 0 || h.HostLabel != "" {
			return fmt.Errorf("consul_route_info: metric_labels / host_label set without metrics")
		}
		return nil
	}
	if consulApp.metrics == nil {
		return fmt.Errorf("consul_route_info: metrics needs the consul app's `metrics` option")
	}
	if h.labelNames, h.labelTagKeys, err = h.metricLabelLayout(); err != nil {
		return err
	}
	if h.counter, err = consulApp.metrics.RouteRequestsCounter(h.labelNames); err != nil {
		return fmt.Errorf("consul_route_info: %w", err)
	}
	return nil
}

// metricLabelLayout returns the counter's extra label names (sorted) and, for each, the tag key
// to read it from ("" = the matched route's host, for HostLabel).
func (h *ConsulRouteInfoHandler) metricLabelLayout() (names, tagKeys []string, err error) {
	sources := make(map[string]string, len(h.MetricLabels)+1)
	for name, tagKey := range h.MetricLabels {
		if tagKey == "" {
			return nil, nil, fmt.Errorf("consul_route_info: metric label %q has an empty tag key", name)
		}
		sources[name] = tagKey
	}
	if h.HostLabel != "" {
		if _, dup := sources[h.HostLabel]; dup {
			return nil, nil, fmt.Errorf("consul_route_info: host_label %q is also a metric_labels name", h.HostLabel)
		}
		sources[h.HostLabel] = ""
	}
	for name := range sources {
		if !metricLabelName.MatchString(name) || name == "service" || name == "class" || name == "error" {
			return nil, nil, fmt.Errorf("consul_route_info: invalid metric label name %q", name)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		tagKeys = append(tagKeys, sources[name])
	}
	return names, tagKeys, nil
}

// ServeHTTP sets the route variables for the request, if any, calls next, and counts the
// outcome when metrics are enabled.
func (h *ConsulRouteInfoHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	route := h.routeTable.Match(r.Host, r.URL.Path)
	if route == nil {
		return next.ServeHTTP(w, r)
	}
	setRouteVars(r, route, h.Vars)
	if h.counter == nil {
		return next.ServeHTTP(w, r)
	}

	rec := &statusRecorder{ResponseWriterWrapper: &caddyhttp.ResponseWriterWrapper{ResponseWriter: w}}
	err := next.ServeHTTP(rec, r)
	h.counter.WithLabelValues(h.labelValues(route, rec.status, err)...).Inc()
	return err
}

// labelValues builds the counter's label values: service, the configured tag labels, class,
// error. A handler error's status (e.g. the WAF's 403) wins over anything written so far.
func (h *ConsulRouteInfoHandler) labelValues(route *CompiledHTTPRoute, status int, err error) []string {
	errored := "false"
	if err != nil {
		errored, status = "true", http.StatusInternalServerError
		var he caddyhttp.HandlerError
		if errors.As(err, &he) && he.StatusCode != 0 {
			status = he.StatusCode
		}
	} else if status == 0 {
		status = http.StatusOK // body written without WriteHeader, or nothing written at all
	}

	values := make([]string, 0, len(h.labelTagKeys)+3)
	values = append(values, route.ServiceName)
	for _, key := range h.labelTagKeys {
		if key == "" {
			values = append(values, route.Host)
		} else {
			values = append(values, route.TagValues[key])
		}
	}
	return append(values, fmt.Sprintf("%dxx", status/100), errored)
}

// statusRecorder records the first final status written. The embedded ResponseWriterWrapper
// keeps Flush/Hijack/Push reachable (via Unwrap / http.ResponseController), so streaming
// responses and WebSocket upgrades behave as before.
type statusRecorder struct {
	*caddyhttp.ResponseWriterWrapper
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	if sr.status == 0 && code >= 200 {
		sr.status = code
	}
	sr.ResponseWriterWrapper.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if sr.status == 0 {
		sr.status = http.StatusOK
	}
	return sr.ResponseWriterWrapper.Write(b)
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
//		metrics
//		metric_label <name> <tag-key>
//		metric_host_label <name>
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
		case "metrics":
			if d.NextArg() {
				return d.ArgErr()
			}
			h.Metrics = true
		case "metric_label":
			var name, tagKey string
			if !d.Args(&name, &tagKey) {
				return d.ArgErr()
			}
			if d.NextArg() {
				return d.ArgErr()
			}
			if h.MetricLabels == nil {
				h.MetricLabels = make(map[string]string)
			}
			h.MetricLabels[name] = tagKey
		case "metric_host_label":
			if !d.Args(&h.HostLabel) {
				return d.ArgErr()
			}
			if d.NextArg() {
				return d.ArgErr()
			}
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
