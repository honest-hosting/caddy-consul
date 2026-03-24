package integration_test

import (
	"testing"
	"time"

	consul "github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Port-aware HTTP route matching (non-connect) ---

func TestIntegration_PortMatching_FabioTag_WithPort(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register a service with a port-qualified Fabio tag (like the real asynqmon case)
	err = registerService(client, "port-fabio", "echo-http", 8080,
		[]string{"urlprefix-port-fabio.localdev:8443/"},
		nil,
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "port-fabio")
		_ = waitForHTTPRouteGone("port-fabio.localdev:8443", 10*time.Second)
	}()

	// The route should be stored with the port in the Host field
	route, err := waitForHTTPRoute("port-fabio.localdev:8443", 15*time.Second)
	require.NoError(t, err, "route for port-fabio.localdev:8443 should be injected")

	handler, ok := getReverseProxyHandler(route)
	require.True(t, ok, "route should have a reverse_proxy handler")

	upstreams := getReverseProxyUpstreams(handler)
	require.NotEmpty(t, upstreams, "should have at least one upstream")
	assert.Contains(t, upstreams[0], "8080", "upstream should point at echo service port")
}

func TestIntegration_PortMatching_FabioTag_DifferentPorts(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register a service with two Fabio tags on different ports (like the real
	// pattern: :8000 redirect + :8443 proxy)
	err = registerService(client, "port-dual", "echo-http", 8080,
		[]string{
			"urlprefix-port-dual.localdev:8000/ redirect=301,https://port-dual.localdev$path",
			"urlprefix-port-dual.localdev:8443/",
		},
		nil,
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "port-dual")
		_ = waitForHTTPRouteGone("port-dual.localdev:8443", 10*time.Second)
	}()

	// The :8443 route should be a proxy
	proxyRoute, err := waitForHTTPRoute("port-dual.localdev:8443", 15*time.Second)
	require.NoError(t, err, "proxy route on :8443 should be injected")

	_, ok := getReverseProxyHandler(proxyRoute)
	assert.True(t, ok, ":8443 route should be a proxy route")
	assert.True(t, isProxyRoute(proxyRoute), ":8443 route should not be a redirect")
}

func TestIntegration_PortMatching_Metadata_WithPort(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register using native metadata with a port-qualified host
	err = registerService(client, "port-meta", "echo-http", 8080,
		[]string{"caddy-consul"},
		map[string]string{
			"caddy-host":     "port-meta.localdev:8443",
			"caddy-protocol": "http",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "port-meta")
		_ = waitForHTTPRouteGone("port-meta.localdev:8443", 10*time.Second)
	}()

	route, err := waitForHTTPRoute("port-meta.localdev:8443", 15*time.Second)
	require.NoError(t, err, "route with port-qualified metadata host should be injected")

	handler, ok := getReverseProxyHandler(route)
	require.True(t, ok, "route should have a reverse_proxy handler")

	upstreams := getReverseProxyUpstreams(handler)
	require.NotEmpty(t, upstreams, "should have upstreams")
}

func TestIntegration_PortMatching_NoPort_StillWorks(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register a service with no port in the host (standard case)
	err = registerService(client, "port-none", "echo-http", 8080,
		[]string{"urlprefix-port-none.localdev/"},
		nil,
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "port-none")
		_ = waitForHTTPRouteGone("port-none.localdev", 10*time.Second)
	}()

	// Route should be stored without a port
	route, err := waitForHTTPRoute("port-none.localdev", 15*time.Second)
	require.NoError(t, err, "route without port should still work")

	handler, ok := getReverseProxyHandler(route)
	require.True(t, ok, "route should have a reverse_proxy handler")

	upstreams := getReverseProxyUpstreams(handler)
	require.NotEmpty(t, upstreams, "should have upstreams")
}

// --- Port-aware HTTP route matching (connect) ---

func TestIntegration_PortMatching_Connect_WithPort(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register Caddy's sidecar with upstream for the backend
	err = registerCaddySidecarWithUpstreams(client, []consul.Upstream{
		{
			DestinationName: "port-connect-svc",
			LocalBindPort:   9196,
		},
	})
	require.NoError(t, err)

	// Register a connect service with a port-qualified host
	err = registerConnectService(client, "port-connect-svc", "echo-connect", 8080,
		map[string]string{
			"caddy-host":     "port-connect.localdev:8443",
			"caddy-protocol": "http",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "port-connect-svc")
		_ = waitForHTTPRouteGone("port-connect.localdev:8443", 10*time.Second)
	}()

	err = waitForConsulService(client, "port-connect-svc", 10*time.Second)
	require.NoError(t, err)

	// Verify the route was injected with the port-qualified host
	route, err := waitForHTTPRoute("port-connect.localdev:8443", 15*time.Second)
	require.NoError(t, err, "connect route with port-qualified host should be injected")

	handler, ok := getReverseProxyHandler(route)
	require.True(t, ok, "route should have a reverse_proxy handler")

	// In sidecar mode, upstream should be localhost
	upstreams := getReverseProxyUpstreams(handler)
	require.NotEmpty(t, upstreams, "should have upstreams")
	assert.Contains(t, upstreams[0], "127.0.0.1:",
		"connect route upstream should point to localhost sidecar bind address")
}

func TestIntegration_PortMatching_Connect_NoPort(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register Caddy's sidecar with upstream for the backend
	err = registerCaddySidecarWithUpstreams(client, []consul.Upstream{
		{
			DestinationName: "port-connect-noport-svc",
			LocalBindPort:   9197,
		},
	})
	require.NoError(t, err)

	// Register a connect service without a port (standard case)
	err = registerConnectService(client, "port-connect-noport-svc", "echo-connect", 8080,
		map[string]string{
			"caddy-host":     "port-connect-noport.localdev",
			"caddy-protocol": "http",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "port-connect-noport-svc")
		_ = waitForHTTPRouteGone("port-connect-noport.localdev", 10*time.Second)
	}()

	err = waitForConsulService(client, "port-connect-noport-svc", 10*time.Second)
	require.NoError(t, err)

	// Verify the route was injected without a port
	route, err := waitForHTTPRoute("port-connect-noport.localdev", 15*time.Second)
	require.NoError(t, err, "connect route without port should still work")

	handler, ok := getReverseProxyHandler(route)
	require.True(t, ok, "route should have a reverse_proxy handler")

	upstreams := getReverseProxyUpstreams(handler)
	require.NotEmpty(t, upstreams, "should have upstreams")
	assert.Contains(t, upstreams[0], "127.0.0.1:",
		"connect route upstream should point to localhost sidecar bind address")
}

// --- HTTP request-level port matching via consul_proxy handler ---

func TestIntegration_PortMatching_HTTPRequest_PortSpecific(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register a service on :8443 (the HTTPS listen port)
	err = registerService(client, "port-http-req", "echo-http", 8080,
		[]string{"urlprefix-port-http-req.localdev:8443/"},
		nil,
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "port-http-req")
		_ = waitForHTTPRouteGone("port-http-req.localdev:8443", 10*time.Second)
	}()

	// Wait for route to appear
	_, err = waitForHTTPRoute("port-http-req.localdev:8443", 15*time.Second)
	require.NoError(t, err)

	// Make an HTTPS request with the port in the URL so the Host header
	// is "port-http-req.localdev:8443" (matching the route).
	// dialDirect routes the connection to caddyHTTPS regardless.
	httpClient := http2Client("port-http-req.localdev")
	resp, err := waitForViaHeader(httpClient,
		"https://port-http-req.localdev:8443/",
		"caddy-consul", 15*time.Second)
	require.NoError(t, err, "request to port-qualified route should be proxied with Via header")
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, "caddy-consul", resp.Header.Get("X-Caddy-Consul-Via"),
		"port-specific route should match and return Via header")
}

func TestIntegration_PortMatching_HTTPRequest_ConnectWithPort(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register Caddy's sidecar with upstream
	err = registerCaddySidecarWithUpstreams(client, []consul.Upstream{
		{
			DestinationName: "port-http-connect-svc",
			LocalBindPort:   9198,
		},
	})
	require.NoError(t, err)

	// Register connect service with port-qualified host
	err = registerConnectService(client, "port-http-connect-svc", "echo-connect", 8080,
		map[string]string{
			"caddy-host":     "port-http-connect.localdev:8443",
			"caddy-protocol": "http",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "port-http-connect-svc")
		_ = waitForHTTPRouteGone("port-http-connect.localdev:8443", 10*time.Second)
	}()

	err = waitForConsulService(client, "port-http-connect-svc", 10*time.Second)
	require.NoError(t, err)

	// Verify route appears with connect Via
	route, err := waitForHTTPRouteWithVia("port-http-connect.localdev:8443", "caddy-consul-connect", 15*time.Second)
	require.NoError(t, err, "connect route with port should have Via=caddy-consul-connect")
	assert.Equal(t, "caddy-consul-connect", getRouteVia(route))
}
