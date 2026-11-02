package caddyconsul

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// withVars returns r with Caddy's request-vars map in its context, as the HTTP server sets it.
func withVars(r *http.Request, vars map[string]any) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), caddyhttp.VarsCtxKey, vars))
}

func TestParseServiceRoutes_TagValues(t *testing.T) {
	svc := &ServiceState{
		Name: "site-1",
		Instances: []ServiceInstance{{
			Address: "10.0.0.1", Port: 8080, Healthy: true,
			Tags: []string{"caddy-consul", "X-Tenant=acme", "X-Site=blog", "X-Tenant=dup", "=noKey", "plain"},
			Meta: map[string]string{"caddy-host": "blog.example.com"},
		}},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, map[string]string{"X-Tenant": "acme", "X-Site": "blog"}, routes[0].TagValues)

	compiled, _ := NewRouteCompiler(testLogger()).compileHTTPRoutes(routes)
	require.Len(t, compiled, 1)
	assert.Equal(t, routes[0].TagValues, compiled[0].TagValues)
}

func TestParseTagValues_None(t *testing.T) {
	assert.Nil(t, parseTagValues([]string{"caddy-consul", "urlprefix-/"}))
	assert.Nil(t, parseTagValues(nil))
}

func TestConsulRouteInfo_ServeHTTP(t *testing.T) {
	rt := NewRouteTable()
	rt.Update([]CompiledHTTPRoute{{
		Host: "blog.example.com", Path: "/", ServiceName: "site-1",
		Upstreams: []Upstream{{Address: "10.0.0.1:8080", Healthy: true}},
		TagValues: map[string]string{"X-Tenant": "acme", "X-Site": "blog"},
	}})
	h := &ConsulRouteInfoHandler{
		Vars:       map[string]string{"app.tenant": "X-Tenant", "app.site": "X-Site", "app.missing": "X-Nope"},
		routeTable: rt,
	}

	for _, tc := range []struct {
		host string
		want map[string]any
	}{
		{"blog.example.com", map[string]any{ServiceVarKey: "site-1", "app.tenant": "acme", "app.site": "blog"}},
		{"other.example.com", map[string]any{}},
	} {
		vars := map[string]any{}
		called := false
		next := caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
			called = true
			return nil
		})
		r := withVars(httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/post", nil), vars)
		require.NoError(t, h.ServeHTTP(httptest.NewRecorder(), r, next))
		assert.True(t, called, tc.host)
		assert.Equal(t, tc.want, vars, tc.host)
	}
}

func TestConsulRouteInfo_UnmarshalCaddyfile(t *testing.T) {
	var h ConsulRouteInfoHandler
	require.NoError(t, h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`consul_route_info {
		var app.tenant X-Tenant
		var app.site X-Site
	}`)))
	assert.Equal(t, map[string]string{"app.tenant": "X-Tenant", "app.site": "X-Site"}, h.Vars)

	for _, bad := range []string{
		`consul_route_info extra`,
		"consul_route_info {\n\tvar onlyname\n}",
		"consul_route_info {\n\tvar a b c\n}",
		"consul_route_info {\n\tbogus a b\n}",
	} {
		var h ConsulRouteInfoHandler
		assert.Error(t, h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(bad)), bad)
	}
}

func TestSetForwardedHeaders(t *testing.T) {
	for _, tc := range []struct {
		name      string
		trusted   bool
		tls       bool
		xff       []string
		proto     string
		wantXFF   string
		wantProto string
	}{
		{"untrusted discards forged values", false, false, []string{"6.6.6.6"}, "https", "192.0.2.10", "http"},
		{"untrusted over TLS", false, true, nil, "", "192.0.2.10", "https"},
		{"trusted keeps and appends", true, false, []string{"203.0.113.9"}, "https", "203.0.113.9, 192.0.2.10", "https"},
		{"trusted joins multiple headers", true, true, []string{"203.0.113.9", "198.51.100.7"}, "", "203.0.113.9, 198.51.100.7, 192.0.2.10", "https"},
		{"trusted without prior", true, false, nil, "", "192.0.2.10", "http"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := withVars(httptest.NewRequest(http.MethodGet, "http://blog.example.com/", nil),
				map[string]any{caddyhttp.TrustedProxyVarKey: tc.trusted})
			r.RemoteAddr = "192.0.2.10:54321"
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if tc.proto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.proto)
			}

			out := http.Header{}
			setForwardedHeaders(out, r)
			assert.Equal(t, []string{tc.wantXFF}, out.Values("X-Forwarded-For"))
			assert.Equal(t, tc.wantProto, out.Get("X-Forwarded-Proto"))
		})
	}
}

// TestConsulProxy_ForwardedHeadersReachUpstream checks what the upstream actually receives:
// one X-Forwarded-For value (ReverseProxy adds nothing), the original Host, X-Real-IP passed
// through untouched, and the prefix stripped.
func TestConsulProxy_ForwardedHeadersReachUpstream(t *testing.T) {
	var got *http.Request
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	rt := NewRouteTable()
	rt.Update([]CompiledHTTPRoute{{
		Host: "blog.example.com", Path: "/api", StripPrefix: true, ServiceName: "site-1",
		Upstreams: []Upstream{{Address: strings.TrimPrefix(upstream.URL, "http://"), Healthy: true}},
	}})
	h := &ConsulProxyHandler{logger: zap.NewNop(), routeTable: rt, transport: &http.Transport{}}

	r := withVars(httptest.NewRequest(http.MethodGet, "http://blog.example.com/api/posts?x=1", nil),
		map[string]any{caddyhttp.TrustedProxyVarKey: false})
	r.RemoteAddr = "192.0.2.10:54321"
	r.Header.Set("X-Forwarded-For", "6.6.6.6")
	r.Header.Set("X-Forwarded-Host", "evil.example.com")
	r.Header.Set("X-Real-IP", "203.0.113.9")

	w := httptest.NewRecorder()
	require.NoError(t, h.ServeHTTP(w, r, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		t.Fatal("route matched; next must not be called")
		return nil
	})))
	require.Equal(t, http.StatusNoContent, w.Code)
	require.NotNil(t, got)

	assert.Equal(t, []string{"192.0.2.10"}, got.Header.Values("X-Forwarded-For"))
	assert.Equal(t, "http", got.Header.Get("X-Forwarded-Proto"))
	assert.Empty(t, got.Header.Get("X-Forwarded-Host"))
	assert.Equal(t, "203.0.113.9", got.Header.Get("X-Real-IP"))
	assert.Equal(t, "blog.example.com", got.Host)
	assert.Equal(t, "/posts", got.URL.Path)
	assert.Equal(t, "x=1", got.URL.RawQuery)
}
