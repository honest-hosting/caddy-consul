package caddyconsul

import (
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(ConsulProxyHandler{})
	httpcaddyfile.RegisterHandlerDirective("consul_proxy", parseConsulProxyDirective)

	// Register directive ordering — consul_proxy should run late, after most
	// other handlers but before reverse_proxy (so static reverse_proxy routes
	// defined in Caddyfile take precedence when placed before consul_proxy).
	httpcaddyfile.RegisterDirectiveOrder("consul_proxy", httpcaddyfile.Before, "reverse_proxy")
}

// ConsulProxyHandler is a Caddy HTTP handler that dynamically routes requests
// based on Consul service discovery. It reads from an in-memory route table
// shared with the consul app module — no admin API calls, no config reloads.
type ConsulProxyHandler struct {
	logger               *zap.Logger
	routeTable           *RouteTable
	transport            *http.Transport
	globalNoCacheMatcher *StatusMatcher // from ConsulRouter global config; nil = no modification
}

// CaddyModule returns the Caddy module information.
func (ConsulProxyHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.consul_proxy",
		New: func() caddy.Module { return new(ConsulProxyHandler) },
	}
}

// Provision sets up the handler by getting a reference to the consul app's route table.
func (h *ConsulProxyHandler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger()

	app, err := ctx.App("consul")
	if err != nil {
		return fmt.Errorf("consul_proxy: consul app not configured: %w", err)
	}

	consulApp, ok := app.(*ConsulRouter)
	if !ok {
		return fmt.Errorf("consul_proxy: unexpected consul app type")
	}

	h.routeTable = consulApp.RouteTable()
	if h.routeTable == nil {
		return fmt.Errorf("consul_proxy: consul app route table not initialized")
	}

	h.globalNoCacheMatcher = consulApp.NoCacheMatcher()

	h.transport = &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}

	h.logger.Info("consul_proxy handler provisioned",
		zap.Int("initial_routes", h.routeTable.Len()),
	)

	return nil
}

// ServeHTTP handles an HTTP request by matching it against the dynamic route table.
func (h *ConsulProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	route := h.routeTable.Match(r.Host, r.URL.Path)
	if route == nil {
		return next.ServeHTTP(w, r)
	}

	// Handle redirect routes — no Via header since redirects don't enter Consul/mesh
	if route.RedirectCode > 0 && route.RedirectURL != "" {
		location := expandRedirectURL(route.RedirectURL, r)
		w.Header().Set("Location", location)
		if route.RedirectNoCache {
			setNoCacheHeaders(w.Header())
		}
		w.WriteHeader(route.RedirectCode)
		return nil
	}

	// Resolve effective no-cache matcher for this route
	noCacheMatcher := h.effectiveNoCacheMatcher(route)

	// Handle proxy routes
	upstreams := healthyUpstreams(route.Upstreams)
	if len(upstreams) == 0 {
		h.logger.Warn("no healthy upstreams for route",
			zap.String("host", route.Host),
			zap.String("path", route.Path),
			zap.String("service", route.ServiceName),
		)
		if route.Via != "" {
			w.Header().Set("X-Caddy-Consul-Via", route.Via)
		}
		setNoCacheIfMatched(noCacheMatcher, http.StatusBadGateway, w.Header())
		w.WriteHeader(http.StatusBadGateway)
		return nil
	}

	// Select upstream (simple round-robin via random selection)
	upstream := upstreams[rand.Intn(len(upstreams))]

	// Build target URL
	targetURL := &url.URL{
		Scheme: "http",
		Host:   upstream.Address,
	}

	proxy := &httputil.ReverseProxy{
		// Rewrite rather than Director: with Rewrite, ReverseProxy strips inbound Forwarded and
		// X-Forwarded-* before calling it and appends nothing itself, so the headers below are
		// exactly what the upstream gets (a Director's X-Forwarded-For got the client IP
		// appended a second time).
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(targetURL)

			// Strip prefix if configured
			if route.StripPrefix && route.Path != "" && route.Path != "/" {
				pr.Out.URL.Path = strings.TrimPrefix(pr.Out.URL.Path, route.Path)
				if pr.Out.URL.Path == "" {
					pr.Out.URL.Path = "/"
				}
				pr.Out.URL.RawPath = strings.TrimPrefix(pr.Out.URL.RawPath, route.Path)
			}

			// Preserve original Host header for virtual hosting (SetURL set it to the upstream)
			pr.Out.Host = r.Host

			setForwardedHeaders(pr.Out.Header, r)
		},
	}
	proxy.Transport = h.transport
	proxy.ModifyResponse = func(resp *http.Response) error {
		if route.Via != "" {
			resp.Header.Set("X-Caddy-Consul-Via", route.Via)
		}
		setNoCacheIfMatched(noCacheMatcher, resp.StatusCode, resp.Header)
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		h.logger.Error("proxy error",
			zap.String("service", route.ServiceName),
			zap.String("upstream", upstream.Address),
			zap.Error(err),
		)
		if route.Via != "" {
			w.Header().Set("X-Caddy-Consul-Via", route.Via)
		}
		setNoCacheIfMatched(noCacheMatcher, http.StatusBadGateway, w.Header())
		w.WriteHeader(http.StatusBadGateway)
	}

	proxy.ServeHTTP(w, r)
	return nil
}

// setForwardedHeaders sets X-Forwarded-For and X-Forwarded-Proto on the outbound request the
// way Caddy's reverse_proxy does. If the peer is a trusted proxy (the server's
// trusted_proxies), the inbound values are kept and the peer is appended to X-Forwarded-For.
// Otherwise the inbound values are discarded: X-Forwarded-For is just the peer and
// X-Forwarded-Proto is this connection's scheme, so clients can't forge them.
func setForwardedHeaders(out http.Header, in *http.Request) {
	trusted, _ := caddyhttp.GetVar(in.Context(), caddyhttp.TrustedProxyVarKey).(bool)

	if peer, _, err := net.SplitHostPort(in.RemoteAddr); err == nil {
		prior := strings.Join(in.Header.Values("X-Forwarded-For"), ", ")
		if trusted && prior != "" {
			out.Set("X-Forwarded-For", prior+", "+peer)
		} else {
			out.Set("X-Forwarded-For", peer)
		}
	}

	if proto := in.Header.Get("X-Forwarded-Proto"); trusted && proto != "" {
		out.Set("X-Forwarded-Proto", proto)
	} else if in.TLS != nil {
		out.Set("X-Forwarded-Proto", "https")
	} else {
		out.Set("X-Forwarded-Proto", "http")
	}
}

// UnmarshalCaddyfile parses the consul_proxy directive. It takes no arguments.
//
//	consul_proxy
func (h *ConsulProxyHandler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name
	if d.NextArg() {
		return d.ArgErr()
	}
	return nil
}

// parseConsulProxyDirective parses the Caddyfile consul_proxy directive.
func parseConsulProxyDirective(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var handler ConsulProxyHandler
	err := handler.UnmarshalCaddyfile(h.Dispenser)
	return &handler, err
}

// expandRedirectURL replaces Caddy placeholders in the redirect URL.
func expandRedirectURL(template string, r *http.Request) string {
	result := template
	result = strings.ReplaceAll(result, "{http.request.uri}", r.RequestURI)
	result = strings.ReplaceAll(result, "{http.request.host}", r.Host)
	return result
}

// effectiveNoCacheMatcher returns the matcher to use for a given route.
// Returns nil if no-cache headers should not be applied.
// Precedence: per-service opt-out → per-service matcher → global matcher → nil (no-op).
func (h *ConsulProxyHandler) effectiveNoCacheMatcher(route *CompiledHTTPRoute) *StatusMatcher {
	if route.NoCacheOptOut {
		return nil // service explicitly opted out
	}
	if route.NoCacheMatcher != nil {
		return route.NoCacheMatcher // per-service override
	}
	return h.globalNoCacheMatcher // may be nil if global is unset (default)
}

// setNoCacheIfMatched sets no-cache headers on the response
// if the status code matches the configured set. No-op if matcher is nil.
func setNoCacheIfMatched(matcher *StatusMatcher, statusCode int, header http.Header) {
	if matcher != nil && matcher.Matches(statusCode) {
		setNoCacheHeaders(header)
	}
}

// setNoCacheHeaders sets the full set of no-cache headers used by both the
// status-matched proxy path and the redirect-no-cache path.
func setNoCacheHeaders(header http.Header) {
	header.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	header.Set("Pragma", "no-cache")
	header.Set("Expires", "0")
}

// healthyUpstreams filters to only healthy upstreams.
func healthyUpstreams(upstreams []Upstream) []Upstream {
	var healthy []Upstream
	for _, u := range upstreams {
		if u.Healthy {
			healthy = append(healthy, u)
		}
	}
	return healthy
}

// Interface guards
var (
	_ caddy.Module                = (*ConsulProxyHandler)(nil)
	_ caddy.Provisioner           = (*ConsulProxyHandler)(nil)
	_ caddyhttp.MiddlewareHandler = (*ConsulProxyHandler)(nil)
	_ caddyfile.Unmarshaler       = (*ConsulProxyHandler)(nil)
)
