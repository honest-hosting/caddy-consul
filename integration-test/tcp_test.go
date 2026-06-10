package integration_test

import (
	"strings"
	"testing"
	"time"

	consul "github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- TCP routing tests ---
//
// TCP/L4 routing is handled by self-managed in-process listeners (no caddy-l4,
// no admin API, no config reload). These tests assert end-to-end behavior by
// connecting to the published port and checking the echo backend responds
// ("TCP-OK"), rather than introspecting Caddy's config for an L4 server.

func TestIntegration_TCP_FabioTag_PortRouting(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register a TCP service using a Fabio urlprefix- tag.
	err = registerTCPService(client, "tcp-pg", "echo-tcp", 9000, 15432)
	require.NoError(t, err)
	defer func() { _ = deregisterService(client, "tcp-pg") }()

	// In-memory convergence: catalog discovery → health fetch → debounce →
	// compile → TCP table swap → listener open. No reload.
	require.NoError(t, waitForTCPRoute(caddyTCPPostgres, 20*time.Second),
		"TCP port %s should proxy to the echo backend", caddyTCPPostgres)
}

func TestIntegration_TCP_MetadataRouting(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register a TCP service using caddy-* metadata.
	err = registerTCPServiceMeta(client, "tcp-mysql", "echo-tcp", 9000, 13306)
	require.NoError(t, err)
	defer func() { _ = deregisterService(client, "tcp-mysql") }()

	require.NoError(t, waitForTCPRoute(caddyTCPMySQL, 20*time.Second),
		"TCP port %s should proxy to the echo backend", caddyTCPMySQL)
}

func TestIntegration_TCP_ServiceDeregister(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Unique port to avoid clashing with other tests that use 15432.
	const deregAddr = "127.0.0.1:16432"

	err = registerTCPService(client, "tcp-temp", "echo-tcp", 9000, 16432)
	require.NoError(t, err)

	// Listener opens and proxies.
	require.NoError(t, waitForTCPRoute(deregAddr, 20*time.Second),
		"TCP route should be serving after registration")

	// Deregister → the self-managed listener should be closed (route stops
	// proxying). No admin API delete, no reload.
	require.NoError(t, deregisterService(client, "tcp-temp"))
	require.NoError(t, waitForTCPRouteGone(deregAddr, 20*time.Second),
		"TCP route should stop serving after deregistration")
}

// --- TCP + Connect sidecar ---

func TestIntegration_TCP_Connect_Sidecar(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register Caddy's sidecar with an upstream for the TCP service FIRST, so the
	// upstream bind exists when the watcher processes the backend.
	err = registerCaddySidecarWithUpstreams(client, []consul.Upstream{
		{
			DestinationName: "tcp-connect-sidecar",
			LocalBindPort:   9193,
		},
	})
	require.NoError(t, err)

	// Register the TCP backend with connect proxy (sidecar).
	err = registerConnectService(client, "tcp-connect-sidecar", "echo-tcp", 9000,
		map[string]string{
			"caddy-protocol": "tcp",
			"caddy-port":     "15432",
		},
	)
	require.NoError(t, err)
	defer func() { _ = deregisterService(client, "tcp-connect-sidecar") }()

	require.NoError(t, waitForConsulService(client, "tcp-connect-sidecar", 10*time.Second))

	// The integration harness has no running Envoy sidecar, so we verify the
	// control-plane result (matching the original test's intent): caddy-consul
	// resolves the Connect upstream to the local sidecar bind address and creates
	// the TCP route. We assert the in-memory TCP table (via /consul/tcp) has a
	// route on port 15432 whose upstream is a 127.0.0.1 sidecar bind — not the
	// direct service address. End-to-end traffic would additionally require Envoy.
	require.Eventually(t, func() bool {
		routes, err := getConsulTCPRoutes()
		if err != nil {
			return false
		}
		for _, rt := range routes {
			if rt.Port != 15432 {
				continue
			}
			for _, u := range rt.Upstreams {
				if strings.HasPrefix(u.Address, "127.0.0.1:") {
					return true
				}
			}
		}
		return false
	}, 20*time.Second, 500*time.Millisecond,
		"TCP connect-sidecar route should resolve to a 127.0.0.1 sidecar upstream")
}

// --- Post-TCP health check ---

func TestIntegration_TCP_CaddyStaysHealthy(t *testing.T) {
	// After TCP tests, verify Caddy's HTTP endpoints still work.
	client := plainHTTPClient()
	resp, err := waitForEndpoint(client, "http://caddy.localdev/", 5*time.Second)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, 200, resp.StatusCode)
}
