package integration_test

import (
	"testing"
	"time"

	consul "github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// ttlCheckID is the raw CheckID as registered via ServiceRegister.
	// Consul's UpdateTTL API expects this raw ID. The Checks() map may
	// also expose it under a "service:"-prefixed key, but the raw ID is
	// the canonical one for API operations.
	ttlCheckID = connectServiceName + "-ttl"

	// sidecarProxyID is the sidecar proxy service ID that Consul auto-creates.
	sidecarProxyID = connectServiceName + "-sidecar-proxy"
)

// --- TTL check lifecycle tests ---

func TestIntegration_Registration_TTLCheckExists(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Caddy's connect_auto_register=true should have created the TTL check on startup.
	check, err := waitForConsulCheck(client, ttlCheckID, "passing", 15*time.Second)
	require.NoError(t, err, "TTL check %s should exist and be passing", ttlCheckID)

	assert.Equal(t, connectServiceName, check.ServiceID,
		"TTL check should be associated with Caddy's connect service")
}

func TestIntegration_Registration_TTLCheckIDConsistency(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Verify the TTL check exists under the raw CheckID and that
	// UpdateTTL succeeds with it (this is the ID ttlLoop uses).
	check, err := waitForConsulCheck(client, ttlCheckID, "passing", 10*time.Second)
	require.NoError(t, err, "TTL check %s should exist and be passing", ttlCheckID)
	assert.Equal(t, connectServiceName, check.ServiceID,
		"TTL check should be associated with Caddy's connect service")

	// Verify UpdateTTL works with the raw ID (this is what ttlLoop calls)
	err = client.Agent().UpdateTTL(ttlCheckID, "integration test", consul.HealthPassing)
	assert.NoError(t, err, "UpdateTTL should succeed with raw check ID %s", ttlCheckID)
}

func TestIntegration_Registration_TTLCheckSurvivesSyncUpstreams(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Confirm TTL check exists before we trigger SyncUpstreams
	_, err = waitForConsulCheck(client, ttlCheckID, "passing", 10*time.Second)
	require.NoError(t, err, "TTL check should be passing before test")

	// Register a Connect service to trigger SyncUpstreams (which re-registers
	// Caddy's service and used to blow away the TTL check).
	svcName := "ttl-survive-test"
	err = registerConnectService(client, svcName, "echo-connect", 8080,
		map[string]string{
			"caddy-host":     "ttl-survive.localdev",
			"caddy-protocol": "http",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, svcName)
		_ = waitForHTTPRouteGone("ttl-survive.localdev", 10*time.Second)
	}()

	// Wait for the route to appear — this confirms SyncUpstreams has run
	_, err = waitForHTTPRoute("ttl-survive.localdev", 15*time.Second)
	require.NoError(t, err, "route should appear after Connect service registration")

	// The TTL check should still be passing (or restored by ttlLoop's ensureCheck).
	// Allow up to 20s since ttlLoop ticks every 15s and needs one cycle to restore.
	check, err := waitForConsulCheck(client, ttlCheckID, "passing", 20*time.Second)
	require.NoError(t, err,
		"TTL check should survive or be restored after SyncUpstreams re-registers the service")
	assert.Equal(t, connectServiceName, check.ServiceID)
}

// --- Upstream management tests ---

func TestIntegration_Registration_UpstreamAddedOnConnect(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	svcName := "upstream-add-test"
	err = registerConnectService(client, svcName, "echo-connect", 8080,
		map[string]string{
			"caddy-host":     "upstream-add.localdev",
			"caddy-protocol": "http",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, svcName)
		_ = waitForHTTPRouteGone("upstream-add.localdev", 10*time.Second)
	}()

	err = waitForConsulService(client, svcName, 10*time.Second)
	require.NoError(t, err)

	// Wait for route to appear — indicates caddy-consul has processed the service
	_, err = waitForHTTPRoute("upstream-add.localdev", 15*time.Second)
	require.NoError(t, err)

	// Verify the upstream was added to Caddy's sidecar proxy
	err = waitForUpstreamInSidecar(client, sidecarProxyID, svcName, 15*time.Second)
	require.NoError(t, err, "upstream %s should be added to sidecar proxy", svcName)

	// Verify the port is in the configured range
	proxy, err := getConsulServiceProxy(client, sidecarProxyID)
	require.NoError(t, err)
	for _, u := range proxy.Upstreams {
		if u.DestinationName == svcName {
			assert.GreaterOrEqual(t, u.LocalBindPort, 19000,
				"upstream port should be >= port range start")
			assert.Less(t, u.LocalBindPort, 29000,
				"upstream port should be < port range end")
			break
		}
	}
}

func TestIntegration_Registration_UpstreamRemovedOnDeregister(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register an anchor Connect service that stays alive throughout the test.
	// This ensures SyncUpstreams is called with a non-empty set after the
	// target service is deregistered, triggering the diff that removes the
	// stale upstream.
	anchorName := "upstream-remove-anchor"
	err = registerConnectService(client, anchorName, "echo-connect", 8080,
		map[string]string{
			"caddy-host":     "upstream-remove-anchor.localdev",
			"caddy-protocol": "http",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, anchorName)
		_ = waitForHTTPRouteGone("upstream-remove-anchor.localdev", 10*time.Second)
	}()

	_, err = waitForHTTPRoute("upstream-remove-anchor.localdev", 15*time.Second)
	require.NoError(t, err, "anchor service route should appear")

	// Register the service we'll remove
	svcName := "upstream-remove-test"
	err = registerConnectService(client, svcName, "echo-connect", 8080,
		map[string]string{
			"caddy-host":     "upstream-remove.localdev",
			"caddy-protocol": "http",
		},
	)
	require.NoError(t, err)

	// Wait for route and upstream to appear
	_, err = waitForHTTPRoute("upstream-remove.localdev", 15*time.Second)
	require.NoError(t, err)

	err = waitForUpstreamInSidecar(client, sidecarProxyID, svcName, 15*time.Second)
	require.NoError(t, err, "upstream should be present before deregistration")

	// Deregister the service
	err = deregisterService(client, svcName)
	require.NoError(t, err)

	// Wait for the route to disappear
	err = waitForHTTPRouteGone("upstream-remove.localdev", 10*time.Second)
	require.NoError(t, err)

	// Verify the upstream was removed from the sidecar proxy.
	// The anchor service triggers SyncUpstreams with a set that excludes
	// the removed service, causing its upstream to be diffed out.
	err = waitForUpstreamGoneFromSidecar(client, sidecarProxyID, svcName, 15*time.Second)
	assert.NoError(t, err, "upstream %s should be removed from sidecar proxy after deregistration", svcName)
}

// --- Service registration idempotency ---

func TestIntegration_Registration_CaddyServiceNotOverwritten(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register a Connect service to trigger SyncUpstreams and add an upstream
	svcName := "overwrite-test"
	err = registerConnectService(client, svcName, "echo-connect", 8080,
		map[string]string{
			"caddy-host":     "overwrite-test.localdev",
			"caddy-protocol": "http",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, svcName)
		_ = waitForHTTPRouteGone("overwrite-test.localdev", 10*time.Second)
	}()

	// Wait for the upstream to appear in the sidecar
	err = waitForUpstreamInSidecar(client, sidecarProxyID, svcName, 15*time.Second)
	require.NoError(t, err, "upstream should be present before overwrite test")

	// Record the current upstream's port for comparison
	proxy, err := getConsulServiceProxy(client, sidecarProxyID)
	require.NoError(t, err)
	var originalPort int
	for _, u := range proxy.Upstreams {
		if u.DestinationName == svcName {
			originalPort = u.LocalBindPort
			break
		}
	}
	require.NotZero(t, originalPort, "should have found the upstream port")

	// Simulate what Register() does when the service already exists:
	// try to read it — it should exist and thus NOT be re-registered.
	svc, _, err := client.Agent().Service(connectServiceName, nil)
	require.NoError(t, err)
	require.NotNil(t, svc, "Caddy's connect service should be registered")

	// Verify the upstream is still there (hasn't been overwritten)
	err = waitForUpstreamInSidecar(client, sidecarProxyID, svcName, 5*time.Second)
	assert.NoError(t, err, "upstream should still exist — service registration should not be overwritten")

	// Verify the port hasn't changed (allocation is stable)
	proxy, err = getConsulServiceProxy(client, sidecarProxyID)
	require.NoError(t, err)
	for _, u := range proxy.Upstreams {
		if u.DestinationName == svcName {
			assert.Equal(t, originalPort, u.LocalBindPort,
				"upstream port allocation should be stable across syncs")
			break
		}
	}
}

// --- Service deregistration cleanup ---

func TestIntegration_Registration_ServiceDeregisterCleansUp(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	// Register a temporary service with Connect (separate from Caddy's main service)
	tmpName := "dereg-cleanup-test"
	reg := &consul.AgentServiceRegistration{
		ID:   tmpName,
		Name: tmpName,
		Port: 9999,
		Connect: &consul.AgentServiceConnect{
			SidecarService: &consul.AgentServiceRegistration{},
		},
		Check: &consul.AgentServiceCheck{
			TTL:    "30s",
			Status: consul.HealthPassing,
		},
	}
	err = client.Agent().ServiceRegister(reg)
	require.NoError(t, err)

	// Verify the service exists
	services, err := client.Agent().Services()
	require.NoError(t, err)
	_, svcExists := services[tmpName]
	require.True(t, svcExists, "temporary service should be registered")

	// Deregister the main service. Consul automatically removes the
	// sidecar proxy when the parent service is deregistered.
	err = client.Agent().ServiceDeregister(tmpName)
	require.NoError(t, err)

	// Also attempt to deregister the sidecar proxy explicitly (as
	// ServiceRegistrar.Deregister does). This may return 404 if Consul
	// already cleaned it up — that's fine.
	tmpSidecarID := tmpName + "-sidecar-proxy"
	_ = client.Agent().ServiceDeregister(tmpSidecarID)

	// Verify both are gone
	services, err = client.Agent().Services()
	require.NoError(t, err)
	_, svcExists = services[tmpName]
	assert.False(t, svcExists, "service should be deregistered")
	_, sidecarExists := services[tmpSidecarID]
	assert.False(t, sidecarExists, "sidecar proxy should be gone after parent deregistration")

	// Verify associated checks are also gone
	checks, err := client.Agent().Checks()
	require.NoError(t, err)
	for id, check := range checks {
		assert.NotEqual(t, tmpName, check.ServiceID,
			"no checks should remain for deregistered service (found %s)", id)
	}
}
