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
// If the service is already registered, it skips re-registration to avoid
// overwriting existing sidecar proxy configurations (e.g., upstream entries).
func (sr *ServiceRegistrar) Register() error {
	// Check if already registered to avoid overwriting sidecar proxy config
	if _, _, err := sr.client.Agent().Service(sr.serviceName, nil); err == nil {
		sr.logger.Info("caddy service already registered in consul, skipping re-registration",
			zap.String("service_name", sr.serviceName),
		)

		// Ensure the TTL check exists (it may have expired while Caddy was down).
		// When a check is created as part of ServiceRegister, Consul prefixes the
		// CheckID with "service:", so the canonical ID is "service:<name>-ttl".
		// We must re-register with that same ID so ttlLoop's updates succeed.
		checkID := "service:" + sr.serviceName + "-ttl"
		err := sr.client.Agent().UpdateTTL(checkID, "caddy-consul healthy", consul.HealthPassing)
		if err != nil {
			sr.logger.Info("TTL check not found, re-registering",
				zap.String("service", sr.serviceName),
			)
			check := &consul.AgentCheckRegistration{
				ID:        checkID,
				Name:      sr.serviceName + " TTL",
				ServiceID: sr.serviceName,
				AgentServiceCheck: consul.AgentServiceCheck{
					TTL:                            "30s",
					Status:                         consul.HealthPassing,
					DeregisterCriticalServiceAfter: "5m",
				},
			}
			if err := sr.client.Agent().CheckRegister(check); err != nil {
				sr.logger.Warn("failed to re-register TTL check",
					zap.String("check_id", checkID),
					zap.Error(err),
				)
			}
		}

		go sr.ttlLoop()
		return nil
	}

	reg := &consul.AgentServiceRegistration{
		ID:   sr.serviceName,
		Name: sr.serviceName,
		Connect: &consul.AgentServiceConnect{
			SidecarService: &consul.AgentServiceRegistration{},
		},
		Check: &consul.AgentServiceCheck{
			CheckID:                        sr.serviceName + "-ttl",
			TTL:                            "30s",
			Status:                         consul.HealthPassing,
			DeregisterCriticalServiceAfter: "5m",
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
// truly shuts down. Use Deregister() for clean removal on process exit.
func (sr *ServiceRegistrar) Stop() {
	sr.stopOnce.Do(func() {
		close(sr.stopCh)
	})
}

// Deregister removes the service and its sidecar proxy from Consul.
// This should only be called on actual process exit, not on config reloads.
func (sr *ServiceRegistrar) Deregister() {
	sr.logger.Info("deregistering caddy service from consul",
		zap.String("service_name", sr.serviceName),
	)
	if err := sr.client.Agent().ServiceDeregister(sr.serviceName); err != nil {
		sr.logger.Warn("failed to deregister service from consul",
			zap.String("service_name", sr.serviceName),
			zap.Error(err),
		)
	}
	// Consul auto-registers the sidecar proxy with this ID
	sidecarID := sr.serviceName + "-sidecar-proxy"
	if err := sr.client.Agent().ServiceDeregister(sidecarID); err != nil {
		sr.logger.Warn("failed to deregister sidecar proxy from consul",
			zap.String("sidecar_id", sidecarID),
			zap.Error(err),
		)
	}
}

// ttlLoop periodically updates the TTL health check to keep the service passing.
// The check ID is always "service:<name>-ttl" — Consul uses this prefix for checks
// created as part of a ServiceRegister call, and we use the same ID when
// re-registering a standalone check after expiry.
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
