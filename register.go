package caddyconsul

import (
	"fmt"
	"strings"
	"sync"
	"time"

	consul "github.com/hashicorp/consul/api"
	"go.uber.org/zap"
)

const (
	// TTL check parameters. The check is renewed every ttlRenewInterval; the TTL
	// itself is ttlDuration. Renewing comfortably inside the TTL means a single
	// missed tick won't flap the service to critical.
	ttlDuration      = "30s"
	ttlRenewInterval = 15 * time.Second

	// Backoff bounds for the registration retry loop while Consul is unreachable
	// (the agent isn't up yet at boot, or a transient outage). Capped so we keep
	// trying indefinitely without busy-looping.
	regBackoffMin = 1 * time.Second
	regBackoffMax = 30 * time.Second
)

// ServiceRegistrar handles auto-registration of Caddy as a service in Consul
// with a Connect sidecar proxy definition. This is required for Connect mode —
// without registration, Caddy has no mesh identity and no sidecar proxy exists
// for upstream resolution.
//
// Registration is owned by a single background goroutine (see Start) that is
// fully self-healing and tolerant of any order-of-operations between Caddy and
// the Consul agent:
//
//   - If Consul is unreachable when Caddy starts (e.g. the agent comes up AFTER
//     Caddy), it retries with capped backoff until registration succeeds, rather
//     than failing once and wedging permanently.
//   - Once registered, it keeps the TTL check passing and re-registers the
//     service (and its sidecar) automatically if either later disappears — e.g.
//     a SyncUpstreams re-register that dropped the check, an agent restart that
//     lost the registration, or a manual deregister.
//
// A Consul outage therefore degrades Connect routing gracefully; it never
// crashes Caddy, which is also serving non-Connect traffic.
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

// Start launches the background maintenance loop and returns immediately. It
// never blocks and never reports a fatal error: registration is retried until
// it succeeds and is continuously repaired thereafter. Safe to call once per
// process; subsequent registration work is idempotent.
func (sr *ServiceRegistrar) Start() {
	go sr.maintainLoop()
}

// maintainLoop owns the registration for the life of the process. Phase 1 gets
// the service registered (retrying through an unreachable Consul); Phase 2 keeps
// the TTL check alive and repairs the registration if it drifts.
func (sr *ServiceRegistrar) maintainLoop() {
	// Phase 1: become registered, backing off while Consul is unreachable.
	backoff := regBackoffMin
	for {
		if sr.stopped() {
			return
		}
		if err := sr.ensureRegistered(); err != nil {
			sr.logger.Warn("consul registration not yet successful; will retry",
				zap.String("service_name", sr.serviceName),
				zap.Duration("retry_in", backoff),
				zap.Error(err),
			)
			if sr.sleep(backoff) {
				return // stopped during backoff
			}
			backoff *= 2
			if backoff > regBackoffMax {
				backoff = regBackoffMax
			}
			continue
		}
		break
	}

	// Phase 2: keep the TTL check passing; repair the registration if it drifts
	// (check removed by a SyncUpstreams re-register, service lost on an agent
	// restart, Consul flap, etc.). Errors here are never fatal — we simply try
	// again on the next tick so a transient outage tears nothing down.
	ticker := time.NewTicker(ttlRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-sr.stopCh:
			return
		case <-ticker.C:
			if err := sr.client.Agent().UpdateTTL(sr.checkID(), "caddy-consul healthy", consul.HealthPassing); err != nil {
				sr.logger.Info("TTL update failed; re-ensuring registration",
					zap.String("service_name", sr.serviceName),
					zap.Error(err),
				)
				if err := sr.ensureRegistered(); err != nil {
					sr.logger.Warn("re-registration attempt failed; will retry on next tick",
						zap.String("service_name", sr.serviceName),
						zap.Error(err),
					)
				}
			}
		}
	}
}

// ensureRegistered makes Consul's state match what Connect needs: the main
// service registered with a sidecar proxy, plus a passing TTL check. It is
// idempotent and safe to call repeatedly.
//
// It returns an error ONLY when Consul is unreachable, so the caller can back
// off and retry. A missing service, missing sidecar, or missing check is
// repaired in-line and is NOT reported as an error.
func (sr *ServiceRegistrar) ensureRegistered() error {
	// Is the main service present?
	if _, _, err := sr.client.Agent().Service(sr.serviceName, nil); err != nil {
		if isNotFound(err) {
			return sr.registerService() // absent → create it (with sidecar + check)
		}
		return fmt.Errorf("querying service %s: %w", sr.serviceName, err) // unreachable → retry
	}

	// Main service exists. Ensure the Connect sidecar proxy exists too. Consul
	// auto-creates "<name>-sidecar-proxy" from the SidecarService stanza, but it
	// can be absent if a prior registration omitted Connect or the sidecar was
	// removed. If missing, re-register the full service to recreate it. (This is
	// the gap that previously left the main service present but no sidecar, so
	// upstream resolution 404'd forever.)
	sidecarID := sr.serviceName + "-sidecar-proxy"
	if _, _, err := sr.client.Agent().Service(sidecarID, nil); err != nil {
		if isNotFound(err) {
			sr.logger.Info("connect sidecar proxy missing; re-registering service to recreate it",
				zap.String("service_name", sr.serviceName),
				zap.String("sidecar_id", sidecarID),
			)
			return sr.registerService()
		}
		return fmt.Errorf("querying sidecar %s: %w", sidecarID, err)
	}

	// Service + sidecar present. Make sure the TTL check exists and is passing.
	// Reaching here means the two queries above succeeded, so Consul is
	// reachable; a failing UpdateTTL here means the check is missing (e.g. a
	// SyncUpstreams re-register dropped it), so recreate it.
	if err := sr.client.Agent().UpdateTTL(sr.checkID(), "caddy-consul healthy", consul.HealthPassing); err != nil {
		sr.ensureCheck()
	}
	return nil
}

// registerService performs the full service registration, including the Connect
// sidecar service and the TTL check. ServiceRegister is idempotent in Consul.
func (sr *ServiceRegistrar) registerService() error {
	reg := &consul.AgentServiceRegistration{
		ID:   sr.serviceName,
		Name: sr.serviceName,
		Connect: &consul.AgentServiceConnect{
			SidecarService: &consul.AgentServiceRegistration{},
		},
		Check: &consul.AgentServiceCheck{
			CheckID: sr.checkID(),
			TTL:     ttlDuration,
			Status:  consul.HealthPassing,
		},
	}
	if err := sr.client.Agent().ServiceRegister(reg); err != nil {
		return fmt.Errorf("registering service %s: %w", sr.serviceName, err)
	}
	sr.logger.Info("registered caddy service in consul",
		zap.String("service_name", sr.serviceName),
	)
	return nil
}

// ensureCheck (re)registers the TTL check against the already-registered service.
// Used when the service exists but its check was removed (e.g. by a SyncUpstreams
// re-register that omitted the check). Best-effort: a failure is logged and
// retried on the next maintenance tick.
func (sr *ServiceRegistrar) ensureCheck() {
	check := &consul.AgentCheckRegistration{
		ID:        sr.checkID(),
		Name:      sr.serviceName + " TTL",
		ServiceID: sr.serviceName,
		AgentServiceCheck: consul.AgentServiceCheck{
			TTL:    ttlDuration,
			Status: consul.HealthPassing,
		},
	}
	if err := sr.client.Agent().CheckRegister(check); err != nil {
		sr.logger.Warn("failed to (re)register TTL check",
			zap.String("check_id", sr.checkID()),
			zap.Error(err),
		)
		return
	}
	sr.logger.Info("restored TTL check",
		zap.String("check_id", sr.checkID()),
	)
}

// Stop stops the maintenance loop. It does NOT deregister the service —
// registration persists across config reloads (Stop is called on every reload,
// not just shutdown). The TTL will expire naturally if Caddy truly exits. Use
// Deregister for clean removal on process exit.
func (sr *ServiceRegistrar) Stop() {
	sr.stopOnce.Do(func() {
		close(sr.stopCh)
	})
}

// Deregister removes the service and its sidecar proxy from Consul. This should
// only be called on actual process exit, not on config reloads.
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
	// Consul auto-registers the sidecar proxy with this ID.
	sidecarID := sr.serviceName + "-sidecar-proxy"
	if err := sr.client.Agent().ServiceDeregister(sidecarID); err != nil {
		sr.logger.Warn("failed to deregister sidecar proxy from consul",
			zap.String("sidecar_id", sidecarID),
			zap.Error(err),
		)
	}
}

// checkID is the raw CheckID used with ServiceRegister and UpdateTTL. Consul's
// UpdateTTL API expects this raw ID, NOT the "service:"-prefixed form shown in
// the UI/Checks() map.
func (sr *ServiceRegistrar) checkID() string {
	return sr.serviceName + "-ttl"
}

// stopped reports whether Stop has been called, without blocking.
func (sr *ServiceRegistrar) stopped() bool {
	select {
	case <-sr.stopCh:
		return true
	default:
		return false
	}
}

// sleep waits for d, or until Stop is called. Returns true if it was stopped.
func (sr *ServiceRegistrar) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-sr.stopCh:
		return true
	case <-t.C:
		return false
	}
}

// isNotFound reports whether a Consul API error is a 404 (service/check absent),
// as opposed to an unreachable agent. The Consul API client returns untyped
// errors of the form "Unexpected response code: 404 (...)".
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Unexpected response code: 404")
}
