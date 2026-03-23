package caddyconsul

import (
	"fmt"
	"hash/fnv"
	"sync"

	consul "github.com/hashicorp/consul/api"
	"go.uber.org/zap"
)

// UpstreamManager dynamically manages sidecar proxy upstream registrations in
// Consul. When caddy-consul discovers a Connect service, it allocates a local
// port and adds the upstream to the sidecar's Proxy.Upstreams. Envoy detects
// the change via xDS and opens a local listener on that port.
type UpstreamManager struct {
	client      *consul.Client
	logger      *zap.Logger
	serviceName string // Caddy's connect service name
	portStart   int
	portEnd     int

	// Port allocations: service name → local bind port
	allocations map[string]int
	mu          sync.Mutex
}

// NewUpstreamManager creates a new UpstreamManager.
func NewUpstreamManager(
	client *consul.Client,
	logger *zap.Logger,
	serviceName string,
	portStart, portEnd int,
) *UpstreamManager {
	return &UpstreamManager{
		client:      client,
		logger:      logger,
		serviceName: serviceName,
		portStart:   portStart,
		portEnd:     portEnd,
		allocations: make(map[string]int),
	}
}

// RestoreAllocations restores port allocations from persisted state.
func (um *UpstreamManager) RestoreAllocations(allocations map[string]int) {
	um.mu.Lock()
	defer um.mu.Unlock()
	for k, v := range allocations {
		um.allocations[k] = v
	}
	if len(allocations) > 0 {
		um.logger.Info("restored upstream port allocations",
			zap.Int("count", len(allocations)),
		)
	}
}

// Allocations returns the current port allocations for persistence.
func (um *UpstreamManager) Allocations() map[string]int {
	um.mu.Lock()
	defer um.mu.Unlock()
	result := make(map[string]int, len(um.allocations))
	for k, v := range um.allocations {
		result[k] = v
	}
	return result
}

// SyncUpstreams ensures the sidecar proxy registration's Proxy.Upstreams
// matches the desired set of Connect services. Adds missing upstreams with
// allocated ports, removes stale ones. Makes a single Consul API call.
//
// Returns true if the registration was updated (upstreams changed).
func (um *UpstreamManager) SyncUpstreams(desired map[string]bool) (bool, error) {
	um.mu.Lock()
	defer um.mu.Unlock()

	// Read current service registration
	svc, _, err := um.client.Agent().Service(um.serviceName, nil)
	if err != nil {
		return false, fmt.Errorf("failed to read service registration %s: %w", um.serviceName, err)
	}
	if svc == nil {
		return false, fmt.Errorf("service %s not registered", um.serviceName)
	}

	// Build current upstream set from registration
	currentUpstreams := make(map[string]consul.Upstream)
	if svc.Proxy != nil {
		for _, u := range svc.Proxy.Upstreams {
			currentUpstreams[u.DestinationName] = u
		}
	}

	// Determine what needs to change
	var toAdd []string
	var toRemove []string

	for name := range desired {
		if _, exists := currentUpstreams[name]; !exists {
			toAdd = append(toAdd, name)
		}
	}

	for name := range currentUpstreams {
		if !desired[name] {
			toRemove = append(toRemove, name)
		}
	}

	if len(toAdd) == 0 && len(toRemove) == 0 {
		return false, nil // no changes needed
	}

	// Allocate ports for new upstreams
	for _, name := range toAdd {
		port := um.allocatePort(name)
		currentUpstreams[name] = consul.Upstream{
			DestinationName: name,
			LocalBindPort:   port,
		}
		um.logger.Info("allocated upstream port",
			zap.String("service", name),
			zap.Int("port", port),
		)
	}

	// Remove stale upstreams
	for _, name := range toRemove {
		delete(currentUpstreams, name)
		delete(um.allocations, name)
		um.logger.Info("removed upstream",
			zap.String("service", name),
		)
	}

	// Build updated upstream list
	var upstreams []consul.Upstream
	for _, u := range currentUpstreams {
		upstreams = append(upstreams, u)
	}

	// Update the service registration with new upstreams.
	// We re-register the full service to preserve all existing fields
	// while updating only the Proxy.Upstreams.
	reg := &consul.AgentServiceRegistration{
		ID:   svc.ID,
		Name: svc.Service,
		Port: svc.Port,
		Tags: svc.Tags,
		Meta: svc.Meta,
		Connect: &consul.AgentServiceConnect{
			SidecarService: &consul.AgentServiceRegistration{
				Proxy: &consul.AgentServiceConnectProxyConfig{
					Upstreams: upstreams,
				},
			},
		},
	}

	if err := um.client.Agent().ServiceRegister(reg); err != nil {
		return false, fmt.Errorf("failed to update sidecar upstreams: %w", err)
	}

	um.logger.Info("synced sidecar upstreams",
		zap.Int("total", len(upstreams)),
		zap.Int("added", len(toAdd)),
		zap.Int("removed", len(toRemove)),
	)

	return true, nil
}

// allocatePort assigns a local bind port for a service.
// Uses deterministic hashing with collision resolution.
// Must be called with um.mu held.
func (um *UpstreamManager) allocatePort(serviceName string) int {
	// Check if already allocated (from persisted state)
	if port, ok := um.allocations[serviceName]; ok {
		return port
	}

	rangeSize := um.portEnd - um.portStart
	if rangeSize <= 0 {
		rangeSize = 10000
	}

	// Deterministic hash-based port assignment
	h := fnv.New32a()
	_, _ = h.Write([]byte(serviceName))
	basePort := um.portStart + int(h.Sum32())%rangeSize

	// Build set of used ports for collision detection
	usedPorts := make(map[int]bool, len(um.allocations))
	for _, port := range um.allocations {
		usedPorts[port] = true
	}

	// Find free port (increment on collision)
	port := basePort
	for usedPorts[port] {
		port++
		if port >= um.portEnd {
			port = um.portStart // wrap around
		}
		if port == basePort {
			// Full range exhausted (shouldn't happen with 10,000 ports)
			break
		}
	}

	um.allocations[serviceName] = port
	return port
}
