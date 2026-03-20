package caddyconsul

import (
	"bytes"
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

func TestReconciler_Apply_InjectsHTTPRoutes(t *testing.T) {
	initial := map[string]interface{}{
		"apps": map[string]interface{}{
			"http": map[string]interface{}{
				"servers": map[string]interface{}{
					"srv0": map[string]interface{}{
						"listen": []interface{}{":80", ":443"},
						"routes": []interface{}{
							map[string]interface{}{
								"handle": []interface{}{
									map[string]interface{}{"handler": "static_response", "body": "static"},
								},
							},
						},
					},
				},
			},
		},
	}

	ts := mockCaddyAdmin(initial)
	defer ts.Close()

	rec := NewReconciler(testLogger(), ts.Listener.Addr().String())

	err := rec.Apply(&CompiledConfig{
		HTTPRoutes: []CompiledHTTPRoute{
			{
				Host:        "app.example.com",
				Path:        "/api",
				ServiceName: "web",
				Upstreams:   []Upstream{{Address: "10.0.0.1:8080"}},
			},
		},
	})
	require.NoError(t, err)

	// Read routes back from the store
	store := ts // we can't access store directly, read via API
	resp, err := http.Get(ts.URL + "/config/apps/http/servers/srv0/routes")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_ = store

	var routes []interface{}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &routes))

	// 1 static + 1 consul
	assert.Len(t, routes, 2)
}

func TestReconciler_Apply_PreservesStaticRoutes(t *testing.T) {
	initial := map[string]interface{}{
		"apps": map[string]interface{}{
			"http": map[string]interface{}{
				"servers": map[string]interface{}{
					"srv0": map[string]interface{}{
						"listen": []interface{}{":80"},
						"routes": []interface{}{
							map[string]interface{}{
								"handle": []interface{}{
									map[string]interface{}{"handler": "static_response", "body": "i-am-static"},
								},
							},
						},
					},
				},
			},
		},
	}

	ts := mockCaddyAdmin(initial)
	defer ts.Close()

	rec := NewReconciler(testLogger(), ts.Listener.Addr().String())

	// First apply: adds svc-a
	err := rec.Apply(&CompiledConfig{
		HTTPRoutes: []CompiledHTTPRoute{
			{Host: "a.com", ServiceName: "svc-a", Upstreams: []Upstream{{Address: "10.0.0.1:80"}}},
		},
	})
	require.NoError(t, err)

	// Second apply: replaces svc-a with svc-b
	err = rec.Apply(&CompiledConfig{
		HTTPRoutes: []CompiledHTTPRoute{
			{Host: "b.com", ServiceName: "svc-b", Upstreams: []Upstream{{Address: "10.0.0.2:80"}}},
		},
	})
	require.NoError(t, err)

	resp, err := http.Get(ts.URL + "/config/apps/http/servers/srv0/routes")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	var routes []interface{}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &routes))

	// 1 static + 1 consul (svc-b only)
	assert.Len(t, routes, 2)

	// Verify the consul route has host=b.com
	consulRoute := routes[1].(map[string]interface{})
	matchList := consulRoute["match"].([]interface{})
	match := matchList[0].(map[string]interface{})
	hosts := match["host"].([]interface{})
	assert.Equal(t, "b.com", hosts[0])
}

func TestReconciler_Apply_PreservesExternallyAddedRoutes(t *testing.T) {
	initial := map[string]interface{}{
		"apps": map[string]interface{}{
			"http": map[string]interface{}{
				"servers": map[string]interface{}{
					"srv0": map[string]interface{}{
						"listen": []interface{}{":80"},
						"routes": []interface{}{
							map[string]interface{}{
								"handle": []interface{}{
									map[string]interface{}{"handler": "static_response", "body": "original"},
								},
							},
						},
					},
				},
			},
		},
	}

	ts := mockCaddyAdmin(initial)
	defer ts.Close()

	rec := NewReconciler(testLogger(), ts.Listener.Addr().String())

	// First apply
	err := rec.Apply(&CompiledConfig{
		HTTPRoutes: []CompiledHTTPRoute{
			{Host: "consul.local", ServiceName: "svc", Upstreams: []Upstream{{Address: "10.0.0.1:80"}}},
		},
	})
	require.NoError(t, err)

	// Simulate an external admin API consumer adding a route
	externalRoute := map[string]interface{}{
		"handle": []interface{}{
			map[string]interface{}{"handler": "static_response", "body": "external"},
		},
	}
	resp, err := http.Get(ts.URL + "/config/apps/http/servers/srv0/routes")
	require.NoError(t, err)
	var currentRoutes []interface{}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = json.Unmarshal(body, &currentRoutes)
	currentRoutes = append(currentRoutes, externalRoute)
	patchBody, _ := json.Marshal(currentRoutes)
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/config/apps/http/servers/srv0/routes", bytes.NewReader(patchBody))
	req.Header.Set("Content-Type", "application/json")
	patchResp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = patchResp.Body.Close()

	// Second apply: same consul route
	err = rec.Apply(&CompiledConfig{
		HTTPRoutes: []CompiledHTTPRoute{
			{Host: "consul.local", ServiceName: "svc", Upstreams: []Upstream{{Address: "10.0.0.1:80"}}},
		},
	})
	require.NoError(t, err)

	resp, err = http.Get(ts.URL + "/config/apps/http/servers/srv0/routes")
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	var routes []interface{}
	_ = json.Unmarshal(body, &routes)

	// 3: original static + externally-added + consul
	assert.Len(t, routes, 3)
}

func TestReconciler_Apply_InjectsTCPRoutes(t *testing.T) {
	initial := map[string]interface{}{
		"apps": map[string]interface{}{
			"http": map[string]interface{}{
				"servers": map[string]interface{}{
					"srv0": map[string]interface{}{
						"listen": []interface{}{":80"},
						"routes": []interface{}{},
					},
				},
			},
		},
	}

	ts := mockCaddyAdmin(initial)
	defer ts.Close()

	rec := NewReconciler(testLogger(), ts.Listener.Addr().String())

	err := rec.Apply(&CompiledConfig{
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

func TestReconciler_Apply_RemovesTCPRoutesWhenEmpty(t *testing.T) {
	initial := map[string]interface{}{
		"apps": map[string]interface{}{
			"http": map[string]interface{}{
				"servers": map[string]interface{}{
					"srv0": map[string]interface{}{
						"listen": []interface{}{":80"},
						"routes": []interface{}{},
					},
				},
			},
		},
	}

	ts := mockCaddyAdmin(initial)
	defer ts.Close()

	rec := NewReconciler(testLogger(), ts.Listener.Addr().String())

	// Add TCP route
	err := rec.Apply(&CompiledConfig{
		TCPRoutes: []CompiledTCPRoute{
			{Port: 5432, ServiceName: "pg", Upstreams: []Upstream{{Address: "10.0.0.1:5432"}}},
		},
	})
	require.NoError(t, err)

	// Remove all TCP routes
	err = rec.Apply(&CompiledConfig{})
	require.NoError(t, err)

	resp, err := http.Get(ts.URL + "/config/apps/layer4/servers/consul_tcp_5432")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
