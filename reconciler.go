package caddyconsul

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

const (
	defaultAdminMaxRetries = 10
	adminReadyBaseDelay    = 500 * time.Millisecond
)

// Reconciler applies compiled TCP routes to Caddy's running config via the Admin API.
// HTTP routes are handled in-memory by the consul_proxy handler and do NOT use this.
//
// TCP routes require the admin API because new listeners (ports) cannot be created
// without a config change. TCP route changes are infrequent (only when TCP services
// are added/removed). TCP state is persisted across reloads to prevent cascading
// reload cycles.
type Reconciler struct {
	logger     *zap.Logger
	adminAddr  string
	mu         sync.Mutex
	httpClient *http.Client
	adminReady atomic.Bool
	maxRetries int

	// ownedTCPServerNames tracks L4 server names we previously created (consul_tcp_<port>).
	ownedTCPServerNames map[string]bool

	// lastTCPServerHashes tracks hashes of TCP server configs we last applied.
	lastTCPServerHashes map[string]string
}

// NewReconciler creates a new Reconciler targeting the given Caddy admin address.
func NewReconciler(logger *zap.Logger, adminAddr string) *Reconciler {
	return &Reconciler{
		logger:              logger,
		adminAddr:           adminAddr,
		maxRetries:          defaultAdminMaxRetries,
		ownedTCPServerNames: make(map[string]bool),
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// RestoreTCPState restores TCP reconciler state from a previous reload cycle.
// This prevents the reconciler from re-applying identical TCP servers and
// triggering another unnecessary config reload.
//
// After restoring, it verifies that the servers actually exist in the live
// Caddy config. If any are missing (e.g., after a reload wiped them), the
// hashes are cleared so the reconciler will re-create them.
func (r *Reconciler) RestoreTCPState(hashes map[string]string, names []string) {
	if len(names) > 0 {
		r.ownedTCPServerNames = make(map[string]bool, len(names))
		for _, name := range names {
			r.ownedTCPServerNames[name] = true
		}
	}
	if len(hashes) > 0 {
		r.lastTCPServerHashes = hashes
	}
}

// VerifyTCPServersExist checks that persisted TCP servers actually exist in
// the live Caddy config. If any are missing, clears the hash state so they
// get re-created on the next reconciliation.
func (r *Reconciler) VerifyTCPServersExist() {
	if len(r.ownedTCPServerNames) == 0 {
		return
	}

	for name := range r.ownedTCPServerNames {
		path := fmt.Sprintf("/config/apps/layer4/servers/%s", name)
		resp, err := r.adminGet(path)
		if err != nil {
			// Admin API not ready yet — clear hashes to force re-apply
			r.lastTCPServerHashes = nil
			r.logger.Debug("admin API not ready, will re-apply TCP servers")
			return
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		// Check for 404, null body, or non-200 status
		if resp.StatusCode != http.StatusOK || string(body) == "null" {
			r.logger.Info("persisted TCP server not found in live config, will re-create",
				zap.String("server", name),
			)
			r.lastTCPServerHashes = nil
			return
		}
	}
}

// TCPState returns the current TCP state for persistence across reloads.
func (r *Reconciler) TCPState() (hashes map[string]string, names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	hashes = make(map[string]string, len(r.lastTCPServerHashes))
	for k, v := range r.lastTCPServerHashes {
		hashes[k] = v
	}

	names = make([]string, 0, len(r.ownedTCPServerNames))
	for name := range r.ownedTCPServerNames {
		names = append(names, name)
	}
	return
}

// waitForAdminAPI waits for the Caddy admin API to become reachable.
func (r *Reconciler) waitForAdminAPI() error {
	if r.adminReady.Load() {
		return nil
	}

	delay := adminReadyBaseDelay
	for attempt := 1; attempt <= r.maxRetries; attempt++ {
		err := r.pingAdmin()
		if err == nil {
			r.adminReady.Store(true)
			r.logger.Info("caddy admin API is ready",
				zap.String("address", r.adminAddr),
			)
			return nil
		}

		if attempt == r.maxRetries {
			return fmt.Errorf("caddy admin API not reachable at %s after %d attempts: %w",
				r.adminAddr, r.maxRetries, err)
		}

		r.logger.Warn("caddy admin API not ready yet, retrying",
			zap.String("address", r.adminAddr),
			zap.Int("attempt", attempt),
			zap.Duration("retry_in", delay),
			zap.Error(err),
		)
		time.Sleep(delay)
		delay = min(delay*2, 5*time.Second)
	}

	return fmt.Errorf("caddy admin API not reachable at %s", r.adminAddr)
}

// pingAdmin does a single connectivity check to the admin API.
func (r *Reconciler) pingAdmin() error {
	resp, err := r.adminGet("/config/")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("admin API returned status %d", resp.StatusCode)
	}
	return nil
}

// ApplyTCP reconciles TCP routes with Caddy's live L4 config via the admin API.
// HTTP routes are handled in-memory by the consul_proxy handler and do not use this.
func (r *Reconciler) ApplyTCP(compiled *CompiledConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.waitForAdminAPI(); err != nil {
		return fmt.Errorf("admin API unavailable: %w", err)
	}

	// Verify persisted TCP servers still exist in live config
	r.VerifyTCPServersExist()

	start := time.Now()

	if err := r.reconcileTCPRoutes(compiled.TCPRoutes); err != nil {
		return fmt.Errorf("failed to reconcile TCP routes: %w", err)
	}

	r.logger.Info("TCP routes reconciled",
		zap.Int("tcp_routes", len(compiled.TCPRoutes)),
		zap.Duration("duration", time.Since(start)),
	)

	return nil
}

// reconcileTCPRoutes manages L4 server entries for TCP routes.
// Compares desired state against live state and only mutates if changed.
func (r *Reconciler) reconcileTCPRoutes(routes []CompiledTCPRoute) error {
	newNames := make(map[string]bool)

	// Build the desired state
	grouped := GroupTCPRoutesByPort(routes)
	desiredServers := make(map[string][]byte) // name → JSON
	for port, portRoutes := range grouped {
		serverJSON, err := BuildTCPServerJSON(port, portRoutes)
		if err != nil {
			return err
		}
		serverName := fmt.Sprintf("consul_tcp_%d", port)
		desiredServers[serverName] = serverJSON
		newNames[serverName] = true
	}

	// Check if anything changed from what we previously set
	if r.tcpServersUnchanged(desiredServers) {
		r.logger.Debug("TCP routes unchanged, skipping admin API calls")
		return nil
	}

	// Remove previously-owned servers that are no longer needed
	for name := range r.ownedTCPServerNames {
		if _, stillNeeded := desiredServers[name]; !stillNeeded {
			path := fmt.Sprintf("/config/apps/layer4/servers/%s", name)
			_ = r.adminDelete(path)
		}
	}

	if len(desiredServers) > 0 {
		r.ensureLayer4Servers()

		for serverName, serverJSON := range desiredServers {
			path := fmt.Sprintf("/config/apps/layer4/servers/%s", serverName)
			if err := r.putJSON(path, serverJSON); err != nil {
				if err := r.patchJSON(path, json.RawMessage(serverJSON)); err != nil {
					return fmt.Errorf("failed to set TCP server %s: %w", serverName, err)
				}
			}
		}
	}

	r.ownedTCPServerNames = newNames
	r.lastTCPServerHashes = r.hashTCPServers(desiredServers)
	return nil
}

// tcpServersUnchanged checks if the desired TCP servers match what we last applied.
func (r *Reconciler) tcpServersUnchanged(desired map[string][]byte) bool {
	if r.lastTCPServerHashes == nil {
		return false
	}
	current := r.hashTCPServers(desired)
	if len(current) != len(r.lastTCPServerHashes) {
		return false
	}
	for name, hash := range current {
		if r.lastTCPServerHashes[name] != hash {
			return false
		}
	}
	return true
}

// hashTCPServers returns a name→hash map for TCP server configs.
func (r *Reconciler) hashTCPServers(servers map[string][]byte) map[string]string {
	hashes := make(map[string]string, len(servers))
	for name, data := range servers {
		hashes[name] = hashJSON(data)
	}
	return hashes
}

// --- Admin API helpers ---

func (r *Reconciler) adminURL(path string) string {
	addr := r.adminAddr
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	return addr + path
}

func (r *Reconciler) adminGet(path string) (*http.Response, error) {
	return r.httpClient.Get(r.adminURL(path))
}

func (r *Reconciler) adminDelete(path string) error {
	req, err := http.NewRequest(http.MethodDelete, r.adminURL(path), nil)
	if err != nil {
		return err
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("DELETE %s returned %d", path, resp.StatusCode)
	}
	return nil
}

func (r *Reconciler) patchJSON(path string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPatch, r.adminURL(path), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PATCH %s returned %d: %s", path, resp.StatusCode, string(body))
	}
	return nil
}

func (r *Reconciler) putJSON(path string, data []byte) error {
	req, err := http.NewRequest(http.MethodPut, r.adminURL(path), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PUT %s returned %d: %s", path, resp.StatusCode, string(body))
	}
	return nil
}

// ensureConfigPath ensures a config path exists by attempting to create it.
// ensureLayer4Servers ensures the /config/apps/layer4/servers path exists.
// This is needed because caddy-l4 may not be initialized in the config yet.
func (r *Reconciler) ensureLayer4Servers() {
	// Check if it already exists
	resp, err := r.adminGet("/config/apps/layer4")
	if err == nil {
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return // already exists
		}
	}

	// Create layer4 app with empty servers via POST to /config/apps/layer4
	data := []byte(`{"servers":{}}`)
	req, err := http.NewRequest(http.MethodPost, r.adminURL("/config/apps/layer4"), bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	postResp, err := r.httpClient.Do(req)
	if err != nil {
		return
	}
	_, _ = io.ReadAll(postResp.Body)
	_ = postResp.Body.Close()
}

// --- Hashing ---

func hashJSON(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func hashInterface(v interface{}) string {
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return hashJSON(data)
}
