package integration_test

import (
	"testing"
	"time"

	caddyconsul "github.com/honest-hosting/caddy-consul"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegration_L4Mode_NodeNameFromConsul verifies that the Consul agent's
// node name can be retrieved and matches what service instances report.
func TestIntegration_L4Mode_NodeNameFromConsul(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Get the local agent's node name (same API call caddy-consul uses)
	self, err := client.Agent().Self()
	require.NoError(t, err)

	cfg, ok := self["Config"]
	require.True(t, ok, "Agent self should have Config section")

	nodeName, ok := cfg["NodeName"].(string)
	require.True(t, ok, "Config.NodeName should be a string")
	assert.NotEmpty(t, nodeName, "Consul node name should not be empty")

	t.Logf("Local Consul node name: %s", nodeName)

	// Register a non-TCP service (HTTP) to avoid triggering L4 admin API
	// reloads that destabilize later tests. We only need to verify node name
	// consistency, not TCP routing.
	svcName := "l4mode-node-test"
	err = registerService(client, svcName, "echo-http", 8080,
		[]string{"caddy-consul"},
		map[string]string{
			"caddy-host": "l4mode-node-test.localdev",
		},
	)
	require.NoError(t, err)
	defer func() { _ = deregisterService(client, svcName) }()

	err = waitForConsulService(client, svcName, 10*time.Second)
	require.NoError(t, err)

	// Fetch the service's health entries directly
	entries, _, err := client.Health().Service(svcName, "", true, nil)
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	// Verify the node name matches
	assert.Equal(t, nodeName, entries[0].Node.Node,
		"Service instance node name should match agent self node name")
}

// TestIntegration_L4Mode_FilterTCPRoutesByNode_EndToEnd uses the real Consul
// agent's node name and verifies that FilterTCPRoutesByNode correctly keeps/drops
// routes based on node locality.
func TestIntegration_L4Mode_FilterTCPRoutesByNode_EndToEnd(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Get the local node name
	self, err := client.Agent().Self()
	require.NoError(t, err)
	localNodeName := self["Config"]["NodeName"].(string)
	require.NotEmpty(t, localNodeName)

	// Build route definitions simulating services on different nodes
	routes := []caddyconsul.RouteDefinition{
		{
			ServiceName: "l4mode-local",
			Protocol:    caddyconsul.ProtocolTCP,
			Port:        18432,
			Upstreams: []caddyconsul.Upstream{
				{Address: "echo-tcp:9000", Healthy: true, NodeName: localNodeName},
			},
		},
		{
			ServiceName: "l4mode-remote",
			Protocol:    caddyconsul.ProtocolTCP,
			Port:        18433,
			Upstreams: []caddyconsul.Upstream{
				{Address: "10.99.99.1:9000", Healthy: true, NodeName: "remote-node-that-does-not-exist"},
			},
		},
		{
			// HTTP route should always pass through
			ServiceName: "l4mode-http",
			Protocol:    caddyconsul.ProtocolHTTP,
			Host:        "l4mode.example.com",
			Upstreams: []caddyconsul.Upstream{
				{Address: "10.99.99.2:8080", Healthy: true, NodeName: "remote-node-that-does-not-exist"},
			},
		},
	}

	// Filter with local node name
	filtered := caddyconsul.FilterTCPRoutesByNode(routes, localNodeName)

	// Should keep: l4mode-local (has local upstream) and l4mode-http (HTTP, unaffected)
	// Should drop: l4mode-remote (no local upstream)
	require.Len(t, filtered, 2, "Should keep local TCP + HTTP routes, drop remote TCP")

	var names []string
	for _, r := range filtered {
		names = append(names, r.ServiceName)
	}
	assert.Contains(t, names, "l4mode-local")
	assert.Contains(t, names, "l4mode-http")
	assert.NotContains(t, names, "l4mode-remote")
}

// TestIntegration_L4Mode_FilterWithNodeOverride verifies that the l4_node_hostname
// override correctly controls which routes are kept.
func TestIntegration_L4Mode_FilterWithNodeOverride(t *testing.T) {
	routes := []caddyconsul.RouteDefinition{
		{
			ServiceName: "svc-a",
			Protocol:    caddyconsul.ProtocolTCP,
			Port:        20000,
			Upstreams: []caddyconsul.Upstream{
				{Address: "10.0.0.1:5432", Healthy: true, NodeName: "custom-hostname-override"},
			},
		},
		{
			ServiceName: "svc-b",
			Protocol:    caddyconsul.ProtocolTCP,
			Port:        20001,
			Upstreams: []caddyconsul.Upstream{
				{Address: "10.0.0.2:5432", Healthy: true, NodeName: "other-node"},
			},
		},
	}

	// Simulate l4_node_hostname = "custom-hostname-override"
	filtered := caddyconsul.FilterTCPRoutesByNode(routes, "custom-hostname-override")
	require.Len(t, filtered, 1)
	assert.Equal(t, "svc-a", filtered[0].ServiceName)

	// With the other node name
	filtered = caddyconsul.FilterTCPRoutesByNode(routes, "other-node")
	require.Len(t, filtered, 1)
	assert.Equal(t, "svc-b", filtered[0].ServiceName)
}
