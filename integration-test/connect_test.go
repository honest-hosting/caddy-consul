package integration_test

import (
	"testing"
	"time"

	consul "github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Identity tests ---
// Run first since other tests depend on auto-registration working.

func TestIntegration_Connect_CustomServiceName(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// The Caddyfile sets connect_service_name=caddy-test-ingress with connect_auto_register=true.
	// Poll until it appears (auto-registration happens async in Start()).
	deadline := time.Now().Add(15 * time.Second)
	var found bool
	for time.Now().Before(deadline) {
		services, err := client.Agent().Services()
		if err == nil {
			if _, exists := services[connectServiceName]; exists {
				found = true
				break
			}
		}
		time.Sleep(1 * time.Second)
	}

	assert.True(t, found, "service %s should be auto-registered in Consul", connectServiceName)
}

// --- Sidecar mode tests ---

func TestIntegration_Connect_Sidecar_ServiceRegistered(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register the backend service with Connect + sidecar
	err = registerConnectService(client, "echo-connect-sidecar", "echo-connect", 8080,
		map[string]string{
			"caddy-host":          "sidecar.localdev",
			"caddy-protocol":      "http",
			"caddy-upstream-mode": "connect-sidecar",
		},
	)
	require.NoError(t, err)
	defer func() { _ = deregisterService(client, "echo-connect-sidecar") }()

	// Register Caddy's sidecar with an upstream for the backend.
	// This is separate from auto-registration — it adds upstream entries
	// to the sidecar proxy config that caddy-consul reads.
	err = registerCaddySidecarWithUpstreams(client, []consul.Upstream{
		{
			DestinationName: "echo-connect-sidecar",
			LocalBindPort:   9191,
		},
	})
	require.NoError(t, err)
	// Don't defer deregister of connectServiceName — auto-registration owns it

	err = waitForConsulService(client, "echo-connect-sidecar", 10*time.Second)
	require.NoError(t, err)

	time.Sleep(3 * time.Second)

	config, err := getCaddyConfig()
	require.NoError(t, err)
	assert.NotNil(t, config)
}

func TestIntegration_Connect_Sidecar_NoUpstream(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register backend with connect-sidecar mode
	err = registerConnectService(client, "echo-no-upstream", "echo-connect", 8080,
		map[string]string{
			"caddy-host":          "no-upstream.localdev",
			"caddy-protocol":      "http",
			"caddy-upstream-mode": "connect-sidecar",
		},
	)
	require.NoError(t, err)
	defer func() { _ = deregisterService(client, "echo-no-upstream") }()

	// Register Caddy's sidecar WITHOUT the upstream for this service
	err = registerCaddySidecarWithUpstreams(client, []consul.Upstream{
		{
			DestinationName: "some-other-service",
			LocalBindPort:   9999,
		},
	})
	require.NoError(t, err)

	time.Sleep(3 * time.Second)

	// The route should NOT be injected (no upstream in sidecar).
	// Caddy should still be running fine.
	config, err := getCaddyConfig()
	require.NoError(t, err)
	assert.NotNil(t, config)
}

func TestIntegration_Connect_Sidecar_ServiceDeregister(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerConnectService(client, "echo-dereg", "echo-connect", 8080,
		map[string]string{
			"caddy-host":          "dereg.localdev",
			"caddy-protocol":      "http",
			"caddy-upstream-mode": "connect-sidecar",
		},
	)
	require.NoError(t, err)

	err = registerCaddySidecarWithUpstreams(client, []consul.Upstream{
		{DestinationName: "echo-dereg", LocalBindPort: 9192},
	})
	require.NoError(t, err)

	time.Sleep(3 * time.Second)

	// Deregister the backend
	err = deregisterService(client, "echo-dereg")
	require.NoError(t, err)

	time.Sleep(3 * time.Second)

	// Caddy should still be healthy
	config, err := getCaddyConfig()
	require.NoError(t, err)
	assert.NotNil(t, config)
}

// --- Direct mode tests ---
// These rely on auto-registration (connect_auto_register=true in Caddyfile)
// to provide Caddy's mesh identity. No need to register connectServiceName manually.

func TestIntegration_Connect_Direct_ServiceRegistered(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register a backend service with connect-direct mode
	err = registerConnectService(client, "echo-connect-direct", "echo-connect", 8080,
		map[string]string{
			"caddy-host":          "direct.localdev",
			"caddy-protocol":      "http",
			"caddy-upstream-mode": "connect-direct",
		},
	)
	require.NoError(t, err)
	defer func() { _ = deregisterService(client, "echo-connect-direct") }()

	err = waitForConsulService(client, "echo-connect-direct", 10*time.Second)
	require.NoError(t, err)

	time.Sleep(3 * time.Second)

	config, err := getCaddyConfig()
	require.NoError(t, err)
	assert.NotNil(t, config)
}

func TestIntegration_Connect_Direct_CertAvailable(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Auto-registration should have registered caddy-test-ingress.
	// Give time for cert manager to fetch leaf cert.
	time.Sleep(5 * time.Second)

	// Verify we can fetch a leaf cert for our service identity
	leaf, _, err := client.Agent().ConnectCALeaf(connectServiceName, nil)
	require.NoError(t, err, "should be able to fetch leaf cert for %s", connectServiceName)
	assert.NotEmpty(t, leaf.CertPEM)
	assert.NotEmpty(t, leaf.PrivateKeyPEM)
}

// --- Intention tests ---

func TestIntegration_Connect_Intention_DefaultDeny(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	err = setIntention(client, "*", "*", "deny")
	require.NoError(t, err)
	defer func() { _ = deleteIntention(client, "*", "*") }()

	intentions, _, err := client.Connect().Intentions(nil)
	require.NoError(t, err)

	found := false
	for _, i := range intentions {
		if i.SourceName == "*" && i.DestinationName == "*" && i.Action == "deny" {
			found = true
			break
		}
	}
	assert.True(t, found, "default deny intention should exist")
}

func TestIntegration_Connect_Intention_AllowSpecific(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerConnectService(client, "echo-intent-allow", "echo-connect", 8080, nil)
	require.NoError(t, err)
	defer func() { _ = deregisterService(client, "echo-intent-allow") }()

	// Default deny + specific allow
	err = setIntention(client, "*", "*", "deny")
	require.NoError(t, err)
	defer func() { _ = deleteIntention(client, "*", "*") }()

	err = setIntention(client, connectServiceName, "echo-intent-allow", "allow")
	require.NoError(t, err)
	defer func() { _ = deleteIntention(client, connectServiceName, "echo-intent-allow") }()

	intentions, _, err := client.Connect().Intentions(nil)
	require.NoError(t, err)

	found := false
	for _, i := range intentions {
		if i.SourceName == connectServiceName && i.DestinationName == "echo-intent-allow" && i.Action == "allow" {
			found = true
			break
		}
	}
	assert.True(t, found, "allow intention from %s to echo-intent-allow should exist", connectServiceName)
}

func TestIntegration_Connect_Intention_DenySpecific(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerConnectService(client, "echo-intent-deny", "echo-connect", 8080, nil)
	require.NoError(t, err)
	defer func() { _ = deregisterService(client, "echo-intent-deny") }()

	// Wildcard allow + specific deny
	err = setIntention(client, "*", "*", "allow")
	require.NoError(t, err)
	defer func() { _ = deleteIntention(client, "*", "*") }()

	err = setIntention(client, connectServiceName, "echo-intent-deny", "deny")
	require.NoError(t, err)
	defer func() { _ = deleteIntention(client, connectServiceName, "echo-intent-deny") }()

	intentions, _, err := client.Connect().Intentions(nil)
	require.NoError(t, err)

	found := false
	for _, i := range intentions {
		if i.SourceName == connectServiceName && i.DestinationName == "echo-intent-deny" && i.Action == "deny" {
			found = true
			break
		}
	}
	assert.True(t, found, "deny intention from %s to echo-intent-deny should exist", connectServiceName)
}

func TestIntegration_Connect_Intention_WildcardAllow(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	err = setIntention(client, connectServiceName, "*", "allow")
	require.NoError(t, err)
	defer func() { _ = deleteIntention(client, connectServiceName, "*") }()

	intentions, _, err := client.Connect().Intentions(nil)
	require.NoError(t, err)

	found := false
	for _, i := range intentions {
		if i.SourceName == connectServiceName && i.DestinationName == "*" && i.Action == "allow" {
			found = true
			break
		}
	}
	assert.True(t, found, "wildcard allow intention from %s should exist", connectServiceName)
}
