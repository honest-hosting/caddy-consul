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

	// Register Caddy's sidecar with an upstream for the backend FIRST,
	// so the upstream entry exists when the watcher processes the backend.
	err = registerCaddySidecarWithUpstreams(client, []consul.Upstream{
		{
			DestinationName: "echo-connect-sidecar",
			LocalBindPort:   9191,
		},
	})
	require.NoError(t, err)

	// Register the backend service with Connect + sidecar
	err = registerConnectService(client, "echo-connect-sidecar", "echo-connect", 8080,
		map[string]string{
			"caddy-host":     "sidecar.localdev",
			"caddy-protocol": "http",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "echo-connect-sidecar")
		_ = waitForHTTPRouteGone("sidecar.localdev", 10*time.Second)
	}()

	err = waitForConsulService(client, "echo-connect-sidecar", 10*time.Second)
	require.NoError(t, err)

	// Verify the route was injected into Caddy's config
	route, err := waitForHTTPRoute("sidecar.localdev", 15*time.Second)
	require.NoError(t, err, "connect-sidecar route for sidecar.localdev should be injected")

	// Verify it has a reverse_proxy handler
	handler, ok := getReverseProxyHandler(route)
	require.True(t, ok, "route should have a reverse_proxy handler")

	// In sidecar mode, upstream should be localhost with dynamically allocated port
	upstreams := getReverseProxyUpstreams(handler)
	require.NotEmpty(t, upstreams, "should have upstreams")
	assert.Contains(t, upstreams[0], "127.0.0.1:",
		"sidecar route upstream should point to localhost sidecar bind address")
	assert.NotContains(t, upstreams[0], "echo-connect",
		"sidecar route upstream should NOT point to direct service address")

	// Sidecar mode should NOT have TLS transport (sidecar handles mTLS)
	assert.False(t, reverseProxyHasTLSTransport(handler),
		"sidecar mode route should not have TLS transport (sidecar handles mTLS)")
}

func TestIntegration_Connect_Sidecar_AutoUpstream(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register backend with connect proxy enabled.
	// With dynamic upstream management, caddy-consul automatically adds the
	// upstream to the sidecar registration — no manual sidecar config needed.
	err = registerConnectService(client, "echo-auto-upstream", "echo-connect", 8080,
		map[string]string{
			"caddy-host":     "auto-upstream.localdev",
			"caddy-protocol": "http",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "echo-auto-upstream")
		_ = waitForHTTPRouteGone("auto-upstream.localdev", 10*time.Second)
	}()

	err = waitForConsulService(client, "echo-auto-upstream", 10*time.Second)
	require.NoError(t, err)

	// The route should appear with a sidecar address (localhost with dynamically allocated port)
	route, err := waitForHTTPRoute("auto-upstream.localdev", 15*time.Second)
	require.NoError(t, err, "connect route should be injected with auto-managed upstream")

	handler, ok := getReverseProxyHandler(route)
	require.True(t, ok)

	upstreams := getReverseProxyUpstreams(handler)
	require.NotEmpty(t, upstreams)
	assert.Contains(t, upstreams[0], "127.0.0.1:",
		"connect route should use sidecar localhost address")
	assert.NotContains(t, upstreams[0], "echo-connect",
		"connect route should NOT use direct service address")
}

func TestIntegration_Connect_Sidecar_ServiceDeregister(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerConnectService(client, "echo-dereg", "echo-connect", 8080,
		map[string]string{
			"caddy-host":     "dereg.localdev",
			"caddy-protocol": "http",
		},
	)
	require.NoError(t, err)

	err = registerCaddySidecarWithUpstreams(client, []consul.Upstream{
		{DestinationName: "echo-dereg", LocalBindPort: 9192},
	})
	require.NoError(t, err)

	// Verify the route appears first
	_, err = waitForHTTPRoute("dereg.localdev", 15*time.Second)
	require.NoError(t, err, "route for dereg.localdev should appear after registration")

	// Deregister the backend
	err = deregisterService(client, "echo-dereg")
	require.NoError(t, err)

	// Verify the route disappears
	err = waitForHTTPRouteGone("dereg.localdev", 10*time.Second)
	assert.NoError(t, err, "route for dereg.localdev should disappear after deregistration")
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
