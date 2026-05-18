package integration_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Fabio redirect tag ---

func TestIntegration_Redirect_FabioTag(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerService(client, "fabio-redir", "echo-connect", 8080,
		[]string{"urlprefix-old-fabio.localdev/ redirect=301,https://new-fabio.localdev$path"},
		nil,
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "fabio-redir")
		_ = waitForHTTPRouteGone("old-fabio.localdev", 10*time.Second)
	}()

	route, err := waitForHTTPRoute("old-fabio.localdev", 15*time.Second)
	require.NoError(t, err, "redirect route should be injected")

	handler, ok := getStaticResponseHandler(route)
	require.True(t, ok, "route should have a static_response handler")
	assert.Equal(t, "301", handler["status_code"])
	assert.Equal(t, "https://new-fabio.localdev{http.request.uri}", getStaticResponseLocation(handler))

	// Should NOT have a reverse_proxy handler
	_, hasProxy := getReverseProxyHandler(route)
	assert.False(t, hasProxy, "redirect route should not have reverse_proxy handler")
}

// --- Native metadata redirect ---

func TestIntegration_Redirect_NativeMetadata(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerService(client, "meta-redir", "echo-connect", 8080,
		[]string{"caddy-consul"},
		map[string]string{
			"caddy-host":          "old-meta.localdev",
			"caddy-redirect-code": "301",
			"caddy-redirect-url":  "https://new-meta.localdev{http.request.uri}",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "meta-redir")
		_ = waitForHTTPRouteGone("old-meta.localdev", 10*time.Second)
	}()

	route, err := waitForHTTPRoute("old-meta.localdev", 15*time.Second)
	require.NoError(t, err, "native metadata redirect route should be injected")

	handler, ok := getStaticResponseHandler(route)
	require.True(t, ok, "route should have a static_response handler")
	assert.Equal(t, "301", handler["status_code"])
	assert.Equal(t, "https://new-meta.localdev{http.request.uri}", getStaticResponseLocation(handler))
}

// --- Redirect coexists with proxy on different hosts ---

func TestIntegration_Redirect_CoexistsWithProxy(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register a redirect service
	err = registerService(client, "redir-coexist", "echo-connect", 8080,
		[]string{"caddy-consul"},
		map[string]string{
			"caddy-host":          "redir-coexist.localdev",
			"caddy-redirect-code": "302",
			"caddy-redirect-url":  "https://target.localdev{http.request.uri}",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "redir-coexist")
		_ = waitForHTTPRouteGone("redir-coexist.localdev", 10*time.Second)
	}()

	// Register a proxy service on a different host
	err = registerService(client, "proxy-coexist", "echo-connect", 8080,
		[]string{"caddy-consul"},
		map[string]string{
			"caddy-host": "proxy-coexist.localdev",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "proxy-coexist")
		_ = waitForHTTPRouteGone("proxy-coexist.localdev", 10*time.Second)
	}()

	// Verify redirect route
	redirRoute, err := waitForHTTPRoute("redir-coexist.localdev", 15*time.Second)
	require.NoError(t, err, "redirect route should be injected")
	_, ok := getStaticResponseHandler(redirRoute)
	assert.True(t, ok, "redirect route should have static_response handler")

	// Verify proxy route
	proxyRoute, err := waitForHTTPRoute("proxy-coexist.localdev", 15*time.Second)
	require.NoError(t, err, "proxy route should be injected")
	_, ok = getReverseProxyHandler(proxyRoute)
	assert.True(t, ok, "proxy route should have reverse_proxy handler")
}

// --- Redirect with no-cache headers ---

func TestIntegration_Redirect_NoCacheHeader(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerService(client, "redir-nocache", "echo-connect", 8080,
		[]string{"caddy-consul"},
		map[string]string{
			"caddy-host":              "redir-nocache.localdev",
			"caddy-redirect-code":     "301",
			"caddy-redirect-url":      "https://target.localdev{http.request.uri}",
			"caddy-redirect-no-cache": "true",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "redir-nocache")
		_ = waitForHTTPRouteGone("redir-nocache.localdev", 10*time.Second)
	}()

	_, err = waitForHTTPRoute("redir-nocache.localdev", 15*time.Second)
	require.NoError(t, err, "redirect route should be injected")

	// Verify the route table exposes RedirectNoCache=true
	routes, routeErr := getConsulRoutes()
	require.NoError(t, routeErr)
	var found bool
	for _, r := range routes {
		if r.Host == "redir-nocache.localdev" {
			assert.True(t, r.RedirectNoCache, "route table should expose RedirectNoCache=true")
			assert.Equal(t, 301, r.RedirectCode)
			found = true
			break
		}
	}
	assert.True(t, found, "redirect route should appear in consul route table")
}

func TestIntegration_Redirect_NoCacheDefaultFalse(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerService(client, "redir-cacheable", "echo-connect", 8080,
		[]string{"caddy-consul"},
		map[string]string{
			"caddy-host":          "redir-cacheable.localdev",
			"caddy-redirect-code": "301",
			"caddy-redirect-url":  "https://target.localdev{http.request.uri}",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "redir-cacheable")
		_ = waitForHTTPRouteGone("redir-cacheable.localdev", 10*time.Second)
	}()

	_, err = waitForHTTPRoute("redir-cacheable.localdev", 15*time.Second)
	require.NoError(t, err)

	routes, routeErr := getConsulRoutes()
	require.NoError(t, routeErr)
	for _, r := range routes {
		if r.Host == "redir-cacheable.localdev" {
			assert.False(t, r.RedirectNoCache, "default should be false when caddy-redirect-no-cache is unset")
			return
		}
	}
	t.Fatal("redirect route not found in consul route table")
}

// --- Redirect with connect proxy enabled (no sidecar resolution needed) ---

func TestIntegration_Redirect_WithConnectProxy(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register a connect service with redirect metadata
	// The redirect should work without needing sidecar resolution
	err = registerConnectService(client, "connect-redir", "echo-connect", 8080,
		map[string]string{
			"caddy-host":          "connect-redir.localdev",
			"caddy-redirect-code": "301",
			"caddy-redirect-url":  "https://target.localdev{http.request.uri}",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "connect-redir")
		_ = waitForHTTPRouteGone("connect-redir.localdev", 10*time.Second)
	}()

	route, err := waitForHTTPRoute("connect-redir.localdev", 15*time.Second)
	require.NoError(t, err, "connect redirect route should be injected even without sidecar")

	handler, ok := getStaticResponseHandler(route)
	require.True(t, ok, "route should have a static_response handler")
	assert.Equal(t, "301", handler["status_code"])
	assert.Equal(t, "https://target.localdev{http.request.uri}", getStaticResponseLocation(handler))
}
