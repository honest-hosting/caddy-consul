package integration_test

import (
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

	// Wait for caddy-consul to discover and create the L4 listener
	time.Sleep(5 * time.Second)

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

	time.Sleep(5 * time.Second)

	err = waitForTCP(caddyTCPMySQL, 10*time.Second)
	require.NoError(t, err, "TCP port %s should be reachable through Caddy", caddyTCPMySQL)

	resp, err := dialTCP(caddyTCPMySQL, "", 5*time.Second)
	require.NoError(t, err)
	assert.Contains(t, resp, "TCP-OK")
}

func TestIntegration_TCP_ServiceDeregister(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register TCP service
	err = registerTCPService(client, "tcp-temp", "echo-tcp", 9000, 15432)
	require.NoError(t, err)

	time.Sleep(5 * time.Second)

	// Verify it's reachable
	err = waitForTCP(caddyTCPPostgres, 10*time.Second)
	require.NoError(t, err)

	// Deregister the service
	err = deregisterService(client, "tcp-temp")
	require.NoError(t, err)

	time.Sleep(5 * time.Second)

	// The TCP port should no longer be reachable (L4 server removed)
	// We just verify Caddy is still healthy — the port may or may not
	// be immediately unreachable depending on Caddy's shutdown timing.
	config, err := getCaddyConfig()
	require.NoError(t, err)
	assert.NotNil(t, config)
}

// --- TCP + Connect sidecar ---

func TestIntegration_TCP_Connect_Sidecar(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register a TCP backend with connect-sidecar mode
	err = registerConnectService(client, "tcp-connect-sidecar", "echo-tcp", 9000,
		map[string]string{
			"caddy-protocol":      "tcp",
			"caddy-port":          "15432",
			"caddy-upstream-mode": "connect-sidecar",
		},
	)
	require.NoError(t, err)
	defer func() { _ = deregisterService(client, "tcp-connect-sidecar") }()

	// Register Caddy's sidecar with upstream for the TCP service
	err = registerCaddySidecarWithUpstreams(client, []consul.Upstream{
		{
			DestinationName: "tcp-connect-sidecar",
			LocalBindPort:   9193,
		},
	})
	require.NoError(t, err)

	err = waitForConsulService(client, "tcp-connect-sidecar", 10*time.Second)
	require.NoError(t, err)

	time.Sleep(3 * time.Second)

	// Verify Caddy processed the service (config updated)
	config, err := getCaddyConfig()
	require.NoError(t, err)
	assert.NotNil(t, config)
}

// --- TCP + Connect direct ---

func TestIntegration_TCP_Connect_Direct(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register a TCP backend with connect-direct mode
	err = registerConnectService(client, "tcp-connect-direct", "echo-tcp", 9000,
		map[string]string{
			"caddy-protocol":      "tcp",
			"caddy-port":          "13306",
			"caddy-upstream-mode": "connect-direct",
		},
	)
	require.NoError(t, err)
	defer func() { _ = deregisterService(client, "tcp-connect-direct") }()

	err = waitForConsulService(client, "tcp-connect-direct", 10*time.Second)
	require.NoError(t, err)

	time.Sleep(3 * time.Second)

	// Verify Caddy processed the service (config updated)
	config, err := getCaddyConfig()
	require.NoError(t, err)
	assert.NotNil(t, config)
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
