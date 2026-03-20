package caddyconsul

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

const (
	defaultAdminMaxRetries = 10
	adminReadyBaseDelay    = 500 * time.Millisecond
)

// Reconciler applies compiled routes to Caddy's running config via the Admin API.
//
// It uses targeted PATCH operations on specific config paths (e.g.,
// /config/apps/http/servers/srv0/routes) rather than POST /load, which would
// replace the entire config and re-provision all apps — including the consul app
// itself, causing a restart loop.
//
// Ownership tracking: routes we inject are tracked by content hash. On each
// reconciliation we read the live routes, remove ours (by hash match), append
// the new set, and PATCH back.
type Reconciler struct {
	logger     *zap.Logger
	adminAddr  string
	mu         sync.Mutex
	httpClient *http.Client
	adminReady atomic.Bool
	maxRetries int

	// ownedHTTPRouteHashes tracks content hashes of HTTP routes we previously injected.
	ownedHTTPRouteHashes map[string]bool

	// ownedTCPServerNames tracks L4 server names we previously created (consul_tcp_<port>).
	ownedTCPServerNames map[string]bool

	// lastTCPServerHashes tracks hashes of TCP server configs we last applied.
	lastTCPServerHashes map[string]string

	// httpServerName is the Caddy HTTP server we inject routes into, discovered on first apply.
	httpServerName string
}

// NewReconciler creates a new Reconciler targeting the given Caddy admin address.
func NewReconciler(logger *zap.Logger, adminAddr string) *Reconciler {
	return &Reconciler{
		logger:               logger,
		adminAddr:            adminAddr,
		maxRetries:           defaultAdminMaxRetries,
		ownedHTTPRouteHashes: make(map[string]bool),
		ownedTCPServerNames:  make(map[string]bool),
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
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

// Apply reconciles the compiled routes with Caddy's live config using
// targeted PATCH operations. This avoids replacing the entire config
// (which would re-provision the consul app and cause a restart loop).
func (r *Reconciler) Apply(compiled *CompiledConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.waitForAdminAPI(); err != nil {
		return fmt.Errorf("admin API unavailable: %w", err)
	}

	start := time.Now()

	// Discover the HTTP server name on first call
	if r.httpServerName == "" {
		name, err := r.discoverHTTPServer()
		if err != nil {
			return fmt.Errorf("failed to discover HTTP server: %w", err)
		}
		r.httpServerName = name
		r.logger.Info("discovered HTTP server for route injection",
			zap.String("server", name),
		)
	}

	// Reconcile HTTP routes
	if err := r.reconcileHTTPRoutes(compiled.HTTPRoutes); err != nil {
		return fmt.Errorf("failed to reconcile HTTP routes: %w", err)
	}

	// Reconcile TCP routes
	if err := r.reconcileTCPRoutes(compiled.TCPRoutes); err != nil {
		return fmt.Errorf("failed to reconcile TCP routes: %w", err)
	}

	r.logger.Info("routes reconciled successfully",
		zap.Int("http_routes", len(compiled.HTTPRoutes)),
		zap.Int("tcp_routes", len(compiled.TCPRoutes)),
		zap.Int("conflicts", len(compiled.Conflicts)),
		zap.Duration("duration", time.Since(start)),
	)

	return nil
}

// discoverHTTPServer finds the first HTTP server listening on :80 or :443.
func (r *Reconciler) discoverHTTPServer() (string, error) {
	resp, err := r.adminGet("/config/apps/http/servers")
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /config/apps/http/servers returned %d: %s", resp.StatusCode, string(body))
	}

	var servers map[string]json.RawMessage
	if err := json.Unmarshal(body, &servers); err != nil {
		return "", fmt.Errorf("failed to parse servers: %w", err)
	}

	// Prefer server listening on common ports
	var firstName string
	for name, raw := range servers {
		if firstName == "" {
			firstName = name
		}
		var srv struct {
			Listen []string `json:"listen"`
		}
		if err := json.Unmarshal(raw, &srv); err != nil {
			continue
		}
		for _, addr := range srv.Listen {
			if addr == ":80" || addr == ":443" || addr == ":8080" || addr == ":8443" {
				return name, nil
			}
		}
	}

	if firstName != "" {
		return firstName, nil
	}

	return "", fmt.Errorf("no HTTP servers found in Caddy config")
}

// reconcileHTTPRoutes reads the live routes, removes our old ones, appends new ones,
// and PATCHes the routes array back. Skips the PATCH if nothing changed.
func (r *Reconciler) reconcileHTTPRoutes(routes []CompiledHTTPRoute) error {
	routesPath := fmt.Sprintf("/config/apps/http/servers/%s/routes", r.httpServerName)

	// Read current routes
	liveRoutes, err := r.readJSONArray(routesPath)
	if err != nil {
		return fmt.Errorf("failed to read live routes: %w", err)
	}

	// Hash the live state before changes
	liveHash := hashInterface(liveRoutes)

	// Remove routes we previously injected (by hash)
	var kept []interface{}
	for _, route := range liveRoutes {
		h := hashInterface(route)
		if r.ownedHTTPRouteHashes[h] {
			continue
		}
		kept = append(kept, route)
	}

	// Build and append new consul routes
	newHashes := make(map[string]bool)
	for _, route := range routes {
		routeJSON, err := BuildHTTPRouteJSON(route)
		if err != nil {
			return err
		}
		var routeMap interface{}
		if err := json.Unmarshal(routeJSON, &routeMap); err != nil {
			return err
		}
		kept = append(kept, routeMap)
		newHashes[hashJSON(routeJSON)] = true
	}

	// Skip PATCH if nothing changed — avoids triggering a Caddy config reload
	newHash := hashInterface(kept)
	if liveHash == newHash {
		r.logger.Debug("HTTP routes unchanged, skipping PATCH")
		r.ownedHTTPRouteHashes = newHashes
		return nil
	}

	if err := r.patchJSON(routesPath, kept); err != nil {
		return fmt.Errorf("failed to PATCH routes: %w", err)
	}

	r.ownedHTTPRouteHashes = newHashes
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
	return fmt.Sprintf("http://%s%s", r.adminAddr, path)
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

// readJSONArray reads a JSON array from a config path.
func (r *Reconciler) readJSONArray(path string) ([]interface{}, error) {
	resp, err := r.adminGet(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d: %s", path, resp.StatusCode, string(body))
	}

	var arr []interface{}
	if err := json.Unmarshal(body, &arr); err != nil {
		return nil, fmt.Errorf("failed to parse array at %s: %w", path, err)
	}

	return arr, nil
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
