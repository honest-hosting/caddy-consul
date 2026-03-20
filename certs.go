package caddyconsul

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/certmagic"
	consul "github.com/hashicorp/consul/api"
	"go.uber.org/zap"
)

const (
	certStoragePrefix = "consul-connect/certs/"
	certRefreshRatio  = 0.7 // refresh when 70% of TTL has elapsed
)

// CertManager fetches and rotates Connect leaf certificates and CA roots
// using the Consul Agent API, storing them via Caddy's storage API.
type CertManager struct {
	client      *consul.Client
	logger      *zap.Logger
	serviceName string
	storage     certmagic.Storage

	leaf    *LeafCert
	caRoots []byte // concatenated CA root PEM
	mu      sync.RWMutex

	stopCh chan struct{}
	stopOnce sync.Once
}

// LeafCert holds a Connect leaf certificate and its metadata.
type LeafCert struct {
	CertPEM     []byte
	KeyPEM      []byte
	ValidBefore time.Time
}

// NewCertManager creates a new CertManager.
func NewCertManager(client *consul.Client, logger *zap.Logger, serviceName string, ctx caddy.Context) *CertManager {
	return &CertManager{
		client:      client,
		logger:      logger,
		serviceName: serviceName,
		storage:     ctx.Storage(),
		stopCh:      make(chan struct{}),
	}
}

// Start begins the background cert rotation goroutine.
func (cm *CertManager) Start() {
	// Fetch initial certs
	if err := cm.refreshLeafCert(); err != nil {
		cm.logger.Warn("failed to fetch initial connect leaf cert (will retry)",
			zap.String("service", cm.serviceName),
			zap.Error(err),
		)
	}
	if err := cm.refreshCARoots(); err != nil {
		cm.logger.Warn("failed to fetch initial connect CA roots (will retry)",
			zap.String("service", cm.serviceName),
			zap.Error(err),
		)
	}

	go cm.rotationLoop()
}

// Stop stops the cert rotation goroutine.
func (cm *CertManager) Stop() {
	cm.stopOnce.Do(func() {
		close(cm.stopCh)
	})
}

// GetLeafCert returns the current leaf certificate, or nil if not available.
func (cm *CertManager) GetLeafCert() *LeafCert {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.leaf
}

// GetCARoots returns the current CA root certificates PEM, or nil if not available.
func (cm *CertManager) GetCARoots() []byte {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.caRoots
}

// rotationLoop periodically refreshes certs before they expire.
func (cm *CertManager) rotationLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-cm.stopCh:
			return
		case <-ticker.C:
			cm.mu.RLock()
			leaf := cm.leaf
			cm.mu.RUnlock()

			// Refresh leaf cert if approaching expiry
			if leaf == nil || cm.shouldRefresh(leaf) {
				if err := cm.refreshLeafCert(); err != nil {
					cm.logger.Warn("failed to refresh connect leaf cert",
						zap.Error(err),
					)
				}
			}

			// Periodically refresh CA roots
			if err := cm.refreshCARoots(); err != nil {
				cm.logger.Warn("failed to refresh connect CA roots",
					zap.Error(err),
				)
			}
		}
	}
}

// shouldRefresh returns true if the cert should be refreshed (past 70% of TTL).
func (cm *CertManager) shouldRefresh(leaf *LeafCert) bool {
	remaining := time.Until(leaf.ValidBefore)
	total := leaf.ValidBefore.Sub(time.Now().Add(-remaining))
	threshold := time.Duration(float64(total) * certRefreshRatio)
	return remaining < total-threshold
}

// refreshLeafCert fetches a new leaf certificate from the Consul Agent API.
func (cm *CertManager) refreshLeafCert() error {
	leaf, _, err := cm.client.Agent().ConnectCALeaf(cm.serviceName, nil)
	if err != nil {
		return fmt.Errorf("failed to fetch leaf cert for %s: %w", cm.serviceName, err)
	}

	newLeaf := &LeafCert{
		CertPEM:     []byte(leaf.CertPEM),
		KeyPEM:      []byte(leaf.PrivateKeyPEM),
		ValidBefore: leaf.ValidBefore,
	}

	// Store in Caddy storage
	certKey := certStoragePrefix + cm.serviceName + "-cert.pem"
	keyKey := certStoragePrefix + cm.serviceName + "-key.pem"

	ctx := context.Background()
	if err := cm.storage.Store(ctx, certKey, newLeaf.CertPEM); err != nil {
		return fmt.Errorf("failed to store leaf cert: %w", err)
	}
	if err := cm.storage.Store(ctx, keyKey, newLeaf.KeyPEM); err != nil {
		return fmt.Errorf("failed to store leaf key: %w", err)
	}

	cm.mu.Lock()
	cm.leaf = newLeaf
	cm.mu.Unlock()

	cm.logger.Debug("refreshed connect leaf cert",
		zap.String("service", cm.serviceName),
		zap.Time("valid_before", newLeaf.ValidBefore),
	)

	return nil
}

// refreshCARoots fetches the CA root certificates from the Consul Agent API.
func (cm *CertManager) refreshCARoots() error {
	roots, _, err := cm.client.Agent().ConnectCARoots(nil)
	if err != nil {
		return fmt.Errorf("failed to fetch CA roots: %w", err)
	}

	var caPEM []byte
	for _, root := range roots.Roots {
		caPEM = append(caPEM, []byte(root.RootCertPEM)...)
		caPEM = append(caPEM, '\n')
	}

	// Store in Caddy storage
	caKey := certStoragePrefix + "connect-ca.pem"
	ctx := context.Background()
	if err := cm.storage.Store(ctx, caKey, caPEM); err != nil {
		return fmt.Errorf("failed to store CA roots: %w", err)
	}

	cm.mu.Lock()
	cm.caRoots = caPEM
	cm.mu.Unlock()

	cm.logger.Debug("refreshed connect CA roots",
		zap.Int("root_count", len(roots.Roots)),
	)

	return nil
}
