package caddyconsul

import (
	"fmt"
	"sync"
	"time"

	consul "github.com/hashicorp/consul/api"
	"go.uber.org/zap"
)

// ServiceRegistrar handles auto-registration of Caddy as a service in Consul
// with a Connect sidecar proxy definition. This is required for both sidecar
// and direct Connect modes — without registration, Caddy has no mesh identity.
//
// The registrar keeps the TTL health check alive via a background goroutine.
// Registration is idempotent — safe to call on every Caddy config reload.
type ServiceRegistrar struct {
	client      *consul.Client
	logger      *zap.Logger
	serviceName string

	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewServiceRegistrar creates a new ServiceRegistrar.
func NewServiceRegistrar(client *consul.Client, logger *zap.Logger, serviceName string) *ServiceRegistrar {
	return &ServiceRegistrar{
		client:      client,
		logger:      logger,
		serviceName: serviceName,
		stopCh:      make(chan struct{}),
	}
}

// Register registers Caddy as a service in Consul with Connect enabled
// and starts a background goroutine to keep the TTL health check alive.
// Safe to call multiple times (idempotent).
func (sr *ServiceRegistrar) Register() error {
	reg := &consul.AgentServiceRegistration{
		ID:   sr.serviceName,
		Name: sr.serviceName,
		Connect: &consul.AgentServiceConnect{
			SidecarService: &consul.AgentServiceRegistration{},
		},
		Check: &consul.AgentServiceCheck{
			CheckID: sr.serviceName + "-ttl",
			TTL:     "30s",
			Status:  consul.HealthPassing,
		},
	}

	if err := sr.client.Agent().ServiceRegister(reg); err != nil {
		return fmt.Errorf("failed to register service %s: %w", sr.serviceName, err)
	}

	sr.logger.Info("auto-registered caddy service in consul",
		zap.String("service_name", sr.serviceName),
	)

	// Start TTL updater
	go sr.ttlLoop()

	return nil
}

// Stop stops the TTL updater. Does NOT deregister the service — registration
// persists across config reloads. The TTL will eventually expire if Caddy
// truly shuts down.
func (sr *ServiceRegistrar) Stop() {
	sr.stopOnce.Do(func() {
		close(sr.stopCh)
	})
}

// ttlLoop periodically updates the TTL health check to keep the service passing.
func (sr *ServiceRegistrar) ttlLoop() {
	checkID := "service:" + sr.serviceName + "-ttl"
	ticker := time.NewTicker(15 * time.Second) // update every 15s for a 30s TTL
	defer ticker.Stop()

	for {
		select {
		case <-sr.stopCh:
			return
		case <-ticker.C:
			if err := sr.client.Agent().UpdateTTL(checkID, "caddy-consul healthy", consul.HealthPassing); err != nil {
				sr.logger.Warn("failed to update TTL health check",
					zap.String("check_id", checkID),
					zap.Error(err),
				)
			}
		}
	}
}
