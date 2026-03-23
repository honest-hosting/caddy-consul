package caddyconsul

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpstreamManager_AllocatePort_Deterministic(t *testing.T) {
	um := NewUpstreamManager(nil, testLogger(), "test-ingress", 19000, 29000)

	port1 := um.allocatePort("web-app")
	port2 := um.allocatePort("web-app") // same service, same port
	assert.Equal(t, port1, port2, "same service should get same port")
}

func TestUpstreamManager_AllocatePort_UniquePerService(t *testing.T) {
	um := NewUpstreamManager(nil, testLogger(), "test-ingress", 19000, 29000)

	port1 := um.allocatePort("service-a")
	port2 := um.allocatePort("service-b")
	assert.NotEqual(t, port1, port2, "different services should get different ports")
	assert.GreaterOrEqual(t, port1, 19000)
	assert.Less(t, port1, 29000)
	assert.GreaterOrEqual(t, port2, 19000)
	assert.Less(t, port2, 29000)
}

func TestUpstreamManager_AllocatePort_CollisionResolution(t *testing.T) {
	um := NewUpstreamManager(nil, testLogger(), "test-ingress", 19000, 19003)

	// Allocate 3 ports in a range of 3
	ports := make(map[int]bool)
	for _, name := range []string{"svc-a", "svc-b", "svc-c"} {
		port := um.allocatePort(name)
		assert.GreaterOrEqual(t, port, 19000)
		assert.Less(t, port, 19003)
		ports[port] = true
	}
	// All 3 should be unique
	assert.Len(t, ports, 3, "all ports should be unique within range")
}

func TestUpstreamManager_RestoreAllocations(t *testing.T) {
	um := NewUpstreamManager(nil, testLogger(), "test-ingress", 19000, 29000)

	// Restore from persisted state
	um.RestoreAllocations(map[string]int{
		"web-app": 19042,
		"api-svc": 19123,
	})

	// Allocating same service should return restored port
	port := um.allocatePort("web-app")
	assert.Equal(t, 19042, port)

	// New service gets a new port
	port = um.allocatePort("new-svc")
	assert.NotEqual(t, 19042, port)
	assert.NotEqual(t, 19123, port)
}

func TestUpstreamManager_Allocations_Snapshot(t *testing.T) {
	um := NewUpstreamManager(nil, testLogger(), "test-ingress", 19000, 29000)
	um.allocatePort("svc-a")
	um.allocatePort("svc-b")

	allocs := um.Allocations()
	assert.Len(t, allocs, 2)
	assert.Contains(t, allocs, "svc-a")
	assert.Contains(t, allocs, "svc-b")
}

func TestUpstreamManager_SyncUpstreams_WithMockConsul(t *testing.T) {
	// SyncUpstreams requires a real Consul client to read/write registrations.
	// This test verifies the port allocation logic without Consul.
	um := NewUpstreamManager(nil, testLogger(), "test-ingress", 19000, 29000)

	// Allocate ports for services (simulating what SyncUpstreams does internally)
	port1 := um.allocatePort("web-app")
	port2 := um.allocatePort("api-svc")

	require.NotEqual(t, port1, port2)
	require.GreaterOrEqual(t, port1, 19000)
	require.GreaterOrEqual(t, port2, 19000)

	allocs := um.Allocations()
	assert.Equal(t, port1, allocs["web-app"])
	assert.Equal(t, port2, allocs["api-svc"])
}

func TestUpstreamManager_ScaleTo1000(t *testing.T) {
	um := NewUpstreamManager(nil, testLogger(), "test-ingress", 19000, 29000)

	ports := make(map[int]bool)
	for i := 0; i < 1000; i++ {
		name := fmt.Sprintf("service-%d", i)
		port := um.allocatePort(name)
		assert.GreaterOrEqual(t, port, 19000)
		assert.Less(t, port, 29000)
		ports[port] = true
	}
	// All 1000 should be unique
	assert.Len(t, ports, 1000, "all 1000 ports should be unique")
}
