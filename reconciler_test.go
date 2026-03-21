package caddyconsul

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReconciler_WaitForAdminAPI_Success(t *testing.T) {
	ts := mockCaddyAdmin(nil)
	defer ts.Close()

	r := NewReconciler(testLogger(), ts.Listener.Addr().String())
	err := r.waitForAdminAPI()
	assert.NoError(t, err)
	assert.True(t, r.adminReady.Load())
}

func TestReconciler_WaitForAdminAPI_Failure(t *testing.T) {
	r := NewReconciler(testLogger(), "127.0.0.1:1")
	r.maxRetries = 2
	err := r.waitForAdminAPI()
	assert.Error(t, err)
	assert.False(t, r.adminReady.Load())
}

// configStore is a thread-safe in-memory config tree for the mock Caddy admin API.
type configStore struct {
	mu   sync.RWMutex
	data map[string]interface{}
}

func newConfigStore(initial map[string]interface{}) *configStore {
	if initial == nil {
		initial = make(map[string]interface{})
	}
	return &configStore{data: initial}
}

func (s *configStore) getPath(path string) (interface{}, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	parts := splitConfigPath(path)
	var current interface{} = s.data
	for _, p := range parts {
		m, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		current, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func (s *configStore) setPath(path string, value interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	parts := splitConfigPath(path)
	if len(parts) == 0 {
		if m, ok := value.(map[string]interface{}); ok {
			s.data = m
		}
		return
	}

	current := s.data
	for _, p := range parts[:len(parts)-1] {
		next, ok := current[p].(map[string]interface{})
		if !ok {
			next = make(map[string]interface{})
			current[p] = next
		}
		current = next
	}
	current[parts[len(parts)-1]] = value
}

func (s *configStore) deletePath(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	parts := splitConfigPath(path)
	if len(parts) == 0 {
		return
	}

	current := s.data
	for _, p := range parts[:len(parts)-1] {
		next, ok := current[p].(map[string]interface{})
		if !ok {
			return
		}
		current = next
	}
	delete(current, parts[len(parts)-1])
}

func splitConfigPath(path string) []string {
	path = strings.TrimPrefix(path, "/config/")
	path = strings.TrimPrefix(path, "/config")
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

// mockCaddyAdmin creates a test server that simulates the Caddy admin API
// with path-based GET/PATCH/PUT/DELETE/POST on /config/...
func mockCaddyAdmin(initial map[string]interface{}) *httptest.Server {
	store := newConfigStore(initial)

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		switch r.Method {
		case http.MethodGet:
			val, ok := store.getPath(path)
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(val)

		case http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			var val interface{}
			_ = json.Unmarshal(body, &val)
			store.setPath(path, val)
			w.WriteHeader(http.StatusOK)

		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			var val interface{}
			_ = json.Unmarshal(body, &val)
			store.setPath(path, val)
			w.WriteHeader(http.StatusOK)

		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			var val interface{}
			_ = json.Unmarshal(body, &val)
			store.setPath(path, val)
			w.WriteHeader(http.StatusOK)

		case http.MethodDelete:
			store.deletePath(path)
			w.WriteHeader(http.StatusOK)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
}

func TestReconciler_ApplyTCP_InjectsTCPRoutes(t *testing.T) {
	ts := mockCaddyAdmin(nil)
	defer ts.Close()

	rec := NewReconciler(testLogger(), ts.Listener.Addr().String())

	err := rec.ApplyTCP(&CompiledConfig{
		TCPRoutes: []CompiledTCPRoute{
			{Port: 5432, ServiceName: "postgres", Upstreams: []Upstream{{Address: "10.0.0.1:5432"}}},
		},
	})
	require.NoError(t, err)

	resp, err := http.Get(ts.URL + "/config/apps/layer4/servers/consul_tcp_5432")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestReconciler_ApplyTCP_RemovesTCPRoutesWhenEmpty(t *testing.T) {
	ts := mockCaddyAdmin(nil)
	defer ts.Close()

	rec := NewReconciler(testLogger(), ts.Listener.Addr().String())

	// Add TCP route
	err := rec.ApplyTCP(&CompiledConfig{
		TCPRoutes: []CompiledTCPRoute{
			{Port: 5432, ServiceName: "pg", Upstreams: []Upstream{{Address: "10.0.0.1:5432"}}},
		},
	})
	require.NoError(t, err)

	// Remove all TCP routes
	err = rec.ApplyTCP(&CompiledConfig{})
	require.NoError(t, err)

	resp, err := http.Get(ts.URL + "/config/apps/layer4/servers/consul_tcp_5432")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestReconciler_TCPState_PersistAndRestore(t *testing.T) {
	ts := mockCaddyAdmin(nil)
	defer ts.Close()

	// First reconciler creates TCP servers
	rec1 := NewReconciler(testLogger(), ts.Listener.Addr().String())
	err := rec1.ApplyTCP(&CompiledConfig{
		TCPRoutes: []CompiledTCPRoute{
			{Port: 5432, ServiceName: "pg", Upstreams: []Upstream{{Address: "10.0.0.1:5432"}}},
		},
	})
	require.NoError(t, err)

	// Get state to persist
	hashes, names := rec1.TCPState()
	assert.NotEmpty(t, hashes)
	assert.Contains(t, names, "consul_tcp_5432")

	// Simulate reload: create new reconciler with restored state
	rec2 := NewReconciler(testLogger(), ts.Listener.Addr().String())
	rec2.RestoreTCPState(hashes, names)

	// Apply same TCP routes — should detect unchanged and skip
	err = rec2.ApplyTCP(&CompiledConfig{
		TCPRoutes: []CompiledTCPRoute{
			{Port: 5432, ServiceName: "pg", Upstreams: []Upstream{{Address: "10.0.0.1:5432"}}},
		},
	})
	require.NoError(t, err)

	// Verify server still exists
	resp, err := http.Get(ts.URL + "/config/apps/layer4/servers/consul_tcp_5432")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
