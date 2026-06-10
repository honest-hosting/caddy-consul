package caddyconsul

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"go.uber.org/zap"
)

const stateFileName = "state.json"

// persistedServiceState holds the minimal state needed to resume watching a service
// without re-fetching from Consul.
type persistedServiceState struct {
	Name      string            `json:"name"`
	Tags      []string          `json:"tags,omitempty"`
	Meta      map[string]string `json:"meta,omitempty"`
	Instances []ServiceInstance `json:"instances,omitempty"`
	LastIndex uint64            `json:"last_index"`
}

// persistedState holds all state that must survive Caddy config reloads.
// This prevents the watcher from re-querying Consul on reload, avoiding 429s.
type persistedState struct {
	// Catalog state
	CatalogIndex uint64                            `json:"catalog_index"`
	Services     map[string]*persistedServiceState `json:"services,omitempty"`

	// Health state watcher
	HealthStateIndex uint64              `json:"health_state_index"`
	PassingChecks    map[string][]string `json:"passing_checks,omitempty"` // ServiceName → []ServiceID

	// Compiled route state
	HTTPRoutes    []CompiledHTTPRoute `json:"http_routes,omitempty"`
	HTTPRouteHash string              `json:"http_route_hash,omitempty"`

	// Compiled TCP routes — re-opened as listeners by Start() after a reload.
	TCPRoutes []CompiledTCPRoute `json:"tcp_routes,omitempty"`

	// Connect upstream port allocations: service name → local bind port
	UpstreamAllocations map[string]int `json:"upstream_allocations,omitempty"`
}

// stateManager handles reading/writing persisted state to disk.
type stateManager struct {
	mu       sync.Mutex
	filePath string
	logger   *zap.Logger
	state    persistedState
}

// newStateManager creates a state manager. The state file is stored in the
// given data directory. The directory is created if it doesn't exist.
func newStateManager(dataDir string, logger *zap.Logger) *stateManager {
	return &stateManager{
		filePath: filepath.Join(dataDir, stateFileName),
		logger:   logger,
	}
}

// Load reads persisted state from disk. Returns empty state if file doesn't exist.
func (sm *stateManager) Load() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	data, err := os.ReadFile(sm.filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			sm.logger.Warn("failed to read persisted state",
				zap.String("path", sm.filePath),
				zap.Error(err),
			)
		}
		return
	}

	if err := json.Unmarshal(data, &sm.state); err != nil {
		sm.logger.Warn("failed to parse persisted state",
			zap.String("path", sm.filePath),
			zap.Error(err),
		)
		sm.state = persistedState{}
		return
	}

	sm.logger.Info("loaded persisted state",
		zap.String("http_hash", sm.state.HTTPRouteHash),
		zap.Int("services", len(sm.state.Services)),
		zap.Int("http_routes", len(sm.state.HTTPRoutes)),
		zap.Int("tcp_routes", len(sm.state.TCPRoutes)),
		zap.Uint64("catalog_index", sm.state.CatalogIndex),
	)
}

// Save writes the current state to disk.
func (sm *stateManager) Save() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	data, err := json.Marshal(&sm.state)
	if err != nil {
		sm.logger.Warn("failed to marshal state for persistence", zap.Error(err))
		return
	}

	if err := os.MkdirAll(filepath.Dir(sm.filePath), 0o700); err != nil {
		sm.logger.Warn("failed to create data directory",
			zap.String("path", filepath.Dir(sm.filePath)),
			zap.Error(err),
		)
		return
	}

	if err := os.WriteFile(sm.filePath, data, 0o600); err != nil {
		sm.logger.Warn("failed to write persisted state",
			zap.String("path", sm.filePath),
			zap.Error(err),
		)
	}
}

// HTTPRouteHash returns the last known HTTP route table hash.
func (sm *stateManager) HTTPRouteHash() string {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.state.HTTPRouteHash
}

// SetHTTPRouteHash updates the HTTP route table hash.
func (sm *stateManager) SetHTTPRouteHash(hash string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.state.HTTPRouteHash = hash
}

// HTTPRoutes returns the persisted compiled HTTP routes.
func (sm *stateManager) HTTPRoutes() []CompiledHTTPRoute {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.state.HTTPRoutes
}

// SetHTTPRoutes updates the persisted HTTP routes.
func (sm *stateManager) SetHTTPRoutes(routes []CompiledHTTPRoute) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.state.HTTPRoutes = routes
}

// TCPRoutes returns the persisted compiled TCP routes.
func (sm *stateManager) TCPRoutes() []CompiledTCPRoute {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.state.TCPRoutes
}

// SetTCPRoutes updates the persisted TCP routes.
func (sm *stateManager) SetTCPRoutes(routes []CompiledTCPRoute) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.state.TCPRoutes = routes
}

// HealthStateIndex returns the persisted health state watch index.
func (sm *stateManager) HealthStateIndex() uint64 {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.state.HealthStateIndex
}

// SetHealthStateIndex updates the persisted health state index.
func (sm *stateManager) SetHealthStateIndex(idx uint64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.state.HealthStateIndex = idx
}

// PassingChecks returns the persisted passing checks map.
func (sm *stateManager) PassingChecks() map[string][]string {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.state.PassingChecks
}

// SetPassingChecks updates the persisted passing checks.
func (sm *stateManager) SetPassingChecks(checks map[string][]string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.state.PassingChecks = checks
}

// UpstreamAllocations returns the persisted Connect upstream port allocations.
func (sm *stateManager) UpstreamAllocations() map[string]int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.state.UpstreamAllocations
}

// SetUpstreamAllocations updates the persisted upstream port allocations.
func (sm *stateManager) SetUpstreamAllocations(allocs map[string]int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.state.UpstreamAllocations = allocs
}

// CatalogIndex returns the persisted catalog watch index.
func (sm *stateManager) CatalogIndex() uint64 {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.state.CatalogIndex
}

// SetCatalogIndex updates the persisted catalog index.
func (sm *stateManager) SetCatalogIndex(idx uint64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.state.CatalogIndex = idx
}

// ServiceStates returns the persisted service states.
func (sm *stateManager) ServiceStates() map[string]*persistedServiceState {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.state.Services
}

// SetServiceStates updates the persisted service states.
func (sm *stateManager) SetServiceStates(services map[string]*ServiceState) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.state.Services = make(map[string]*persistedServiceState, len(services))
	for name, svc := range services {
		sm.state.Services[name] = &persistedServiceState{
			Name:      svc.Name,
			Tags:      svc.Tags,
			Meta:      svc.Meta,
			Instances: svc.Instances,
			LastIndex: svc.LastIndex,
		}
	}
}

// hashRoutes computes a SHA256 hash of compiled HTTP routes for change detection.
func hashRoutes(routes []CompiledHTTPRoute) string {
	data, err := json.Marshal(routes)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
