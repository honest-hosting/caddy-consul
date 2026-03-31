package integration_test

import (
	"fmt"
	"testing"
	"time"

	consul "github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegration_InstanceFiltering_OnlyTaggedInstances verifies that when
// multiple instances are registered under the same Consul service name, only
// instances with the "caddy-consul" tag are included as upstreams.
func TestIntegration_InstanceFiltering_OnlyTaggedInstances(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	svcName := "multi-inst-filter"
	host := "multi-inst-filter.localdev"

	// Instance 1: has caddy-consul tag and routing metadata — should be upstream
	reg1 := &consul.AgentServiceRegistration{
		ID:      svcName + "-web",
		Name:    svcName,
		Address: "echo-http",
		Port:    8080,
		Tags:    []string{"caddy-consul"},
		Meta: map[string]string{
			"caddy-host":     host,
			"caddy-protocol": "http",
		},
		Check: &consul.AgentServiceCheck{
			TCP:      "echo-http:8080",
			Interval: "1s",
			Timeout:  "1s",
		},
	}

	// Instance 2: no caddy-consul tag — should NOT be upstream
	reg2 := &consul.AgentServiceRegistration{
		ID:      svcName + "-metrics",
		Name:    svcName,
		Address: "echo-http",
		Port:    9090,
		Tags:    []string{"metrics"},
		Meta: map[string]string{
			"external-source": "nomad",
		},
		Check: &consul.AgentServiceCheck{
			TCP:      "echo-http:8080",
			Interval: "1s",
			Timeout:  "1s",
		},
	}

	// Instance 3: no caddy-consul tag — should NOT be upstream
	reg3 := &consul.AgentServiceRegistration{
		ID:      svcName + "-database",
		Name:    svcName,
		Address: "echo-http",
		Port:    5432,
		Tags:    []string{"database"},
		Meta: map[string]string{
			"external-source": "nomad",
		},
		Check: &consul.AgentServiceCheck{
			TCP:      "echo-http:8080",
			Interval: "1s",
			Timeout:  "1s",
		},
	}

	require.NoError(t, client.Agent().ServiceRegister(reg1))
	require.NoError(t, client.Agent().ServiceRegister(reg2))
	require.NoError(t, client.Agent().ServiceRegister(reg3))
	defer func() {
		_ = client.Agent().ServiceDeregister(reg1.ID)
		_ = client.Agent().ServiceDeregister(reg2.ID)
		_ = client.Agent().ServiceDeregister(reg3.ID)
		_ = waitForHTTPRouteGone(host, 10*time.Second)
	}()

	// Wait for route to appear in caddy-consul's route table
	route, err := waitForHTTPRoute(host, 15*time.Second)
	require.NoError(t, err, "route for %s should appear", host)

	// Verify only the tagged instance is an upstream
	upstreams := getRouteUpstreams(route)
	require.Len(t, upstreams, 1, "only the instance with caddy-consul tag should be an upstream")
	assert.Contains(t, upstreams[0], ":8080",
		"upstream should be the web instance on port 8080, not metrics (9090) or database (5432)")
}

// TestIntegration_InstanceFiltering_IndexedMeta_OnlyTaggedInstances tests the
// indexed metadata path (caddy-route-N-*) with multiple instances.
func TestIntegration_InstanceFiltering_IndexedMeta_OnlyTaggedInstances(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	svcName := "multi-inst-indexed"
	host := "multi-inst-indexed.localdev"

	// Instance 1: has caddy-consul tag — should be upstream
	reg1 := &consul.AgentServiceRegistration{
		ID:      svcName + "-web",
		Name:    svcName,
		Address: "echo-http",
		Port:    8080,
		Tags:    []string{"caddy-consul"},
		Meta: map[string]string{
			"caddy-route-0-host":     host,
			"caddy-route-0-protocol": "http",
		},
		Check: &consul.AgentServiceCheck{
			TCP:      "echo-http:8080",
			Interval: "1s",
			Timeout:  "1s",
		},
	}

	// Instance 2: no caddy-consul tag — should NOT be upstream
	reg2 := &consul.AgentServiceRegistration{
		ID:      svcName + "-sidecar",
		Name:    svcName,
		Address: "echo-http",
		Port:    9090,
		Tags:    []string{"sidecar"},
		Check: &consul.AgentServiceCheck{
			TCP:      "echo-http:8080",
			Interval: "1s",
			Timeout:  "1s",
		},
	}

	require.NoError(t, client.Agent().ServiceRegister(reg1))
	require.NoError(t, client.Agent().ServiceRegister(reg2))
	defer func() {
		_ = client.Agent().ServiceDeregister(reg1.ID)
		_ = client.Agent().ServiceDeregister(reg2.ID)
		_ = waitForHTTPRouteGone(host, 10*time.Second)
	}()

	route, err := waitForHTTPRoute(host, 15*time.Second)
	require.NoError(t, err, "route for %s should appear", host)

	upstreams := getRouteUpstreams(route)
	require.Len(t, upstreams, 1, "only the tagged instance should be an upstream")
	assert.Contains(t, upstreams[0], ":8080")
}

// TestIntegration_InstanceFiltering_FabioTag_OnlyURLPrefixInstances tests that
// Fabio-style routing also filters at the instance level.
func TestIntegration_InstanceFiltering_FabioTag_OnlyURLPrefixInstances(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	svcName := "multi-inst-fabio"
	host := "multi-inst-fabio.localdev"

	// Instance 1: has urlprefix tag — should be upstream
	reg1 := &consul.AgentServiceRegistration{
		ID:      svcName + "-web",
		Name:    svcName,
		Address: "echo-http",
		Port:    8080,
		Tags:    []string{fmt.Sprintf("urlprefix-%s/", host)},
		Check: &consul.AgentServiceCheck{
			TCP:      "echo-http:8080",
			Interval: "1s",
			Timeout:  "1s",
		},
	}

	// Instance 2: no urlprefix tag — should NOT be upstream
	reg2 := &consul.AgentServiceRegistration{
		ID:      svcName + "-worker",
		Name:    svcName,
		Address: "echo-http",
		Port:    9090,
		Tags:    []string{"worker"},
		Check: &consul.AgentServiceCheck{
			TCP:      "echo-http:8080",
			Interval: "1s",
			Timeout:  "1s",
		},
	}

	require.NoError(t, client.Agent().ServiceRegister(reg1))
	require.NoError(t, client.Agent().ServiceRegister(reg2))
	defer func() {
		_ = client.Agent().ServiceDeregister(reg1.ID)
		_ = client.Agent().ServiceDeregister(reg2.ID)
		_ = waitForHTTPRouteGone(host, 10*time.Second)
	}()

	route, err := waitForHTTPRoute(host, 15*time.Second)
	require.NoError(t, err, "route for %s should appear", host)

	upstreams := getRouteUpstreams(route)
	require.Len(t, upstreams, 1, "only the instance with urlprefix tag should be an upstream")
	assert.Contains(t, upstreams[0], ":8080")
}