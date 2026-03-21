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
	logger     *zap.Logger
	routeTable *RouteTable
	transport  *http.Transport
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

	// Handle redirect routes
	if route.RedirectCode > 0 && route.RedirectURL != "" {
		location := expandRedirectURL(route.RedirectURL, r)
		w.Header().Set("Location", location)
		w.WriteHeader(route.RedirectCode)
		return nil
	}

	// Handle proxy routes
	upstreams := healthyUpstreams(route.Upstreams)
	if len(upstreams) == 0 {
		h.logger.Warn("no healthy upstreams for route",
			zap.String("host", route.Host),
			zap.String("path", route.Path),
			zap.String("service", route.ServiceName),
		)
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

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.Transport = h.transport
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		h.logger.Error("proxy error",
			zap.String("service", route.ServiceName),
			zap.String("upstream", upstream.Address),
			zap.Error(err),
		)
		w.WriteHeader(http.StatusBadGateway)
	}

	// Custom director for path rewriting and header injection
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)

		// Strip prefix if configured
		if route.StripPrefix && route.Path != "" && route.Path != "/" {
			req.URL.Path = strings.TrimPrefix(req.URL.Path, route.Path)
			if req.URL.Path == "" {
				req.URL.Path = "/"
			}
			req.URL.RawPath = strings.TrimPrefix(req.URL.RawPath, route.Path)
		}

		// Preserve original Host header for virtual hosting
		req.Host = r.Host

		// Set standard proxy headers
		if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
				req.Header.Set("X-Forwarded-For", prior+", "+clientIP)
			} else {
				req.Header.Set("X-Forwarded-For", clientIP)
			}
		}
		if r.TLS != nil {
			req.Header.Set("X-Forwarded-Proto", "https")
		} else {
			req.Header.Set("X-Forwarded-Proto", "http")
		}
	}

	proxy.ServeHTTP(w, r)
	return nil
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
