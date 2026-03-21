package integration_test

import (
	"fmt"
	"testing"
	"time"

	consul "github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- TCP routing tests ---

func TestIntegration_TCP_FabioTag_PortRouting(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register a TCP service using Fabio urlprefix- tag
	err = registerTCPService(client, "tcp-pg", "echo-tcp", 9000, 15432)
	require.NoError(t, err)
	defer func() { _ = deregisterService(client, "tcp-pg") }()

	// Allow time for: catalog discovery → health fetch → debounce → compile →
	// state save → admin API PUT → Caddy reload → L4 server ready
	time.Sleep(10 * time.Second)

	// Try to connect to the TCP port through Caddy
	err = waitForTCP(caddyTCPPostgres, 10*time.Second)
	require.NoError(t, err, "TCP port %s should be reachable through Caddy", caddyTCPPostgres)

	// Read the response from the echo service
	resp, err := dialTCP(caddyTCPPostgres, "", 5*time.Second)
	require.NoError(t, err)
	assert.Contains(t, resp, "TCP-OK")
}

func TestIntegration_TCP_MetadataRouting(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register a TCP service using caddy-* metadata
	err = registerTCPServiceMeta(client, "tcp-mysql", "echo-tcp", 9000, 13306)
	require.NoError(t, err)
	defer func() { _ = deregisterService(client, "tcp-mysql") }()

	// Allow extra time: the previous TCP test (FabioTag) may have triggered
	// a Caddy reload via L4 admin API, which restarts caddy-consul. This test's
	// service needs to be re-discovered after the reload completes.
	time.Sleep(15 * time.Second)

	err = waitForTCP(caddyTCPMySQL, 10*time.Second)
	require.NoError(t, err, "TCP port %s should be reachable through Caddy", caddyTCPMySQL)

	resp, err := dialTCP(caddyTCPMySQL, "", 5*time.Second)
	require.NoError(t, err)
	assert.Contains(t, resp, "TCP-OK")
}

func TestIntegration_TCP_ServiceDeregister(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Use a unique port (16432) to avoid conflicts with other tests that
	// register services on port 15432 with deferred cleanup.
	const deregPort = 16432

	err = registerTCPService(client, "tcp-temp", "echo-tcp", 9000, deregPort)
	require.NoError(t, err)

	// Allow time for: catalog discovery → health fetch → debounce → compile →
	// state save → admin API PUT → Caddy reload → L4 server ready
	time.Sleep(10 * time.Second)

	serverName := fmt.Sprintf("consul_tcp_%d", deregPort)
	server, err := getCaddyTCPServer(serverName)
	require.NoError(t, err, "L4 server %s should exist in Caddy config", serverName)
	assert.NotNil(t, server)

	// Deregister the service
	err = deregisterService(client, "tcp-temp")
	require.NoError(t, err)

	// Wait and verify the L4 server was removed
	err = waitForCaddyTCPServerGone(serverName, 15*time.Second)
	assert.NoError(t, err, "L4 server %s should be removed after deregistration", serverName)
}

// --- TCP + Connect sidecar ---

func TestIntegration_TCP_Connect_Sidecar(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register Caddy's sidecar with upstream for the TCP service FIRST,
	// so the upstream entry exists when the watcher processes the backend.
	err = registerCaddySidecarWithUpstreams(client, []consul.Upstream{
		{
			DestinationName: "tcp-connect-sidecar",
			LocalBindPort:   9193,
		},
	})
	require.NoError(t, err)

	// Register the TCP backend with connect proxy (sidecar)
	err = registerConnectService(client, "tcp-connect-sidecar", "echo-tcp", 9000,
		map[string]string{
			"caddy-protocol": "tcp",
			"caddy-port":     "15432",
		},
	)
	require.NoError(t, err)
	defer func() { _ = deregisterService(client, "tcp-connect-sidecar") }()

	err = waitForConsulService(client, "tcp-connect-sidecar", 10*time.Second)
	require.NoError(t, err)

	// Wait for caddy-consul to process and create the L4 server
	time.Sleep(5 * time.Second)

	// Verify the L4 server was created in Caddy's config
	server, err := getCaddyTCPServer("consul_tcp_15432")
	require.NoError(t, err, "L4 server consul_tcp_15432 should exist for TCP connect-sidecar route")
	assert.NotNil(t, server)

	// Verify the server has routes with a proxy handler
	routes, _ := server["routes"].([]interface{})
	require.NotEmpty(t, routes, "L4 server should have at least one route")

	routeMap := routes[0].(map[string]interface{})
	handlers, _ := routeMap["handle"].([]interface{})
	require.NotEmpty(t, handlers, "L4 route should have handlers")

	handler := handlers[0].(map[string]interface{})
	assert.Equal(t, "proxy", handler["handler"], "L4 handler should be proxy")

	// Verify upstream points to sidecar bind address (127.0.0.1:9193)
	upstreams, _ := handler["upstreams"].([]interface{})
	require.NotEmpty(t, upstreams)
	upstream := upstreams[0].(map[string]interface{})
	dial := getL4ProxyUpstreamDial(upstream)
	assert.Equal(t, "127.0.0.1:9193", dial,
		"TCP sidecar upstream should point to local sidecar bind address")
}

// --- Post-TCP health check ---

func TestIntegration_TCP_CaddyStaysHealthy(t *testing.T) {
	// After TCP tests, verify Caddy's HTTP endpoints still work
	client := plainHTTPClient()
	resp, err := waitForEndpoint(client, "http://caddy.localdev/", 5*time.Second)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, 200, resp.StatusCode)
}
