package integration_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	consul "github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- X-Caddy-Consul-Via header tests ---

func TestIntegration_ViaHeader_DirectService(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerService(client, "via-direct", "echo-http", 8080,
		[]string{"caddy-consul"},
		map[string]string{
			"caddy-host":     "via-direct.localdev",
			"caddy-protocol": "http",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "via-direct")
		_ = waitForHTTPRouteGone("via-direct.localdev", 10*time.Second)
	}()

	// Verify route table has Via=caddy-consul
	route, err := waitForHTTPRouteWithVia("via-direct.localdev", "caddy-consul", 15*time.Second)
	require.NoError(t, err, "route should have Via=caddy-consul")
	assert.Equal(t, "caddy-consul", getRouteVia(route))

	// Verify response header — use HTTPS client since Caddy redirects HTTP→HTTPS
	httpClient := http2Client("via-direct.localdev")
	resp, err := waitForViaHeader(httpClient, fmt.Sprintf("https://%s/", "via-direct.localdev"), "caddy-consul", 15*time.Second)
	require.NoError(t, err, "response should have X-Caddy-Consul-Via: caddy-consul")
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, "caddy-consul", resp.Header.Get("X-Caddy-Consul-Via"))
}

func TestIntegration_ViaHeader_ConnectService(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register Caddy's sidecar with upstream for the backend
	err = registerCaddySidecarWithUpstreams(client, []consul.Upstream{
		{
			DestinationName: "via-connect-svc",
			LocalBindPort:   9195,
		},
	})
	require.NoError(t, err)

	// Register backend with connect tag
	err = registerConnectService(client, "via-connect-svc", "echo-connect", 8080,
		map[string]string{
			"caddy-host":     "via-connect.localdev",
			"caddy-protocol": "http",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "via-connect-svc")
		_ = waitForHTTPRouteGone("via-connect.localdev", 10*time.Second)
	}()

	err = waitForConsulService(client, "via-connect-svc", 10*time.Second)
	require.NoError(t, err)

	// Verify route table has Via=caddy-consul-connect
	route, err := waitForHTTPRouteWithVia("via-connect.localdev", "caddy-consul-connect", 15*time.Second)
	require.NoError(t, err, "route should have Via=caddy-consul-connect")
	assert.Equal(t, "caddy-consul-connect", getRouteVia(route))
}

func TestIntegration_ViaHeader_FabioTag(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerService(client, "via-fabio", "echo-http", 8080,
		[]string{"urlprefix-via-fabio.localdev/"},
		nil,
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "via-fabio")
		_ = waitForHTTPRouteGone("via-fabio.localdev", 10*time.Second)
	}()

	// Fabio routes use direct routing, so Via should be the service_tag default
	route, err := waitForHTTPRouteWithVia("via-fabio.localdev", "caddy-consul", 15*time.Second)
	require.NoError(t, err, "fabio route should have Via=caddy-consul")
	assert.Equal(t, "caddy-consul", getRouteVia(route))
}

func TestIntegration_ViaHeader_RedirectNoVia(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerService(client, "via-redir", "echo-http", 8080,
		[]string{"caddy-consul"},
		map[string]string{
			"caddy-host":          "via-redir.localdev",
			"caddy-redirect-code": "301",
			"caddy-redirect-url":  "https://target.localdev{http.request.uri}",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "via-redir")
		_ = waitForHTTPRouteGone("via-redir.localdev", 10*time.Second)
	}()

	// Redirect routes should NOT carry Via — redirects don't enter Consul/mesh
	route, err := waitForHTTPRoute("via-redir.localdev", 15*time.Second)
	require.NoError(t, err, "redirect route should be injected")
	assert.Empty(t, getRouteVia(route), "redirect route should not have Via set")
}

func TestIntegration_ViaHeader_NotSetForUnmatchedRoutes(t *testing.T) {
	// Request a host that has no consul route — should fall through to
	// Caddy's default handler with no X-Caddy-Consul-Via header.
	httpClient := http2Client("no-such-route.localdev")

	req, err := http.NewRequest("GET", fmt.Sprintf("https://%s/", "no-such-route.localdev"), nil)
	require.NoError(t, err)

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Skip("caddy not reachable, skipping")
	}
	defer func() { _ = resp.Body.Close() }()

	assert.Empty(t, resp.Header.Get("X-Caddy-Consul-Via"),
		"unmatched route should not have X-Caddy-Consul-Via header")
}
