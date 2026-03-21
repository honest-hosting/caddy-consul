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

// persistedState holds checksums that survive Caddy config reloads.
// Written to disk after each reconciliation, read on Provision.
type persistedState struct {
	HTTPRouteHash   string            `json:"http_route_hash,omitempty"`
	TCPServerHashes map[string]string `json:"tcp_server_hashes,omitempty"`
	TCPServerNames  []string          `json:"tcp_server_names,omitempty"`
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
	}

	sm.logger.Debug("loaded persisted state",
		zap.String("http_hash", sm.state.HTTPRouteHash),
		zap.Int("tcp_servers", len(sm.state.TCPServerNames)),
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

// TCPState returns the persisted TCP server hashes and names.
func (sm *stateManager) TCPState() (map[string]string, []string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.state.TCPServerHashes, sm.state.TCPServerNames
}

// SetTCPState updates the persisted TCP state.
func (sm *stateManager) SetTCPState(hashes map[string]string, names []string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.state.TCPServerHashes = hashes
	sm.state.TCPServerNames = names
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
