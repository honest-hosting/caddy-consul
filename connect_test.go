package caddyconsul

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	consul "github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockConsulAgent creates a test Consul HTTP server that responds to
// Agent API endpoints used by SidecarResolver.
func mockConsulAgent(handler http.HandlerFunc) (*consul.Client, *httptest.Server) {
	ts := httptest.NewServer(handler)
	cfg := consul.DefaultConfig()
	cfg.Address = ts.Listener.Addr().String()
	client, _ := consul.NewClient(cfg)
	return client, ts
}

// --- SidecarResolver ---

func TestSidecarResolver_ResolveUpstreams_Success(t *testing.T) {
	// Mock Consul agent returning a sidecar proxy with an upstream for "web-backend"
	client, ts := mockConsulAgent(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agent/service/my-ingress-sidecar-proxy" {
			resp := consul.AgentService{
				ID:      "my-ingress-sidecar-proxy",
				Service: "my-ingress-sidecar-proxy",
				Proxy: &consul.AgentServiceConnectProxyConfig{
					Upstreams: []consul.Upstream{
						{
							DestinationName: "web-backend",
							LocalBindPort:   9191,
						},
						{
							DestinationName: "other-service",
							LocalBindPort:   9192,
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
	defer ts.Close()

	sr := NewSidecarResolver(client, testLogger(), "my-ingress")

	route := &RouteDefinition{
		ServiceName: "web-backend",
		Upstreams: []Upstream{
			{Address: "10.0.0.1:8080", Healthy: true},
			{Address: "10.0.0.2:8080", Healthy: true},
		},
	}

	err := sr.ResolveUpstreams(route)
	require.NoError(t, err)

	// Upstreams should be replaced with the sidecar's local bind address
	require.Len(t, route.Upstreams, 1)
	assert.Equal(t, "127.0.0.1:9191", route.Upstreams[0].Address)
	assert.True(t, route.Upstreams[0].Healthy)
}

func TestSidecarResolver_ResolveUpstreams_CustomBindAddress(t *testing.T) {
	client, ts := mockConsulAgent(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agent/service/my-ingress-sidecar-proxy" {
			resp := consul.AgentService{
				Proxy: &consul.AgentServiceConnectProxyConfig{
					Upstreams: []consul.Upstream{
						{
							DestinationName:  "web-backend",
							LocalBindAddress: "0.0.0.0",
							LocalBindPort:    9191,
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
	defer ts.Close()

	sr := NewSidecarResolver(client, testLogger(), "my-ingress")
	route := &RouteDefinition{
		ServiceName: "web-backend",
		Upstreams:   []Upstream{{Address: "10.0.0.1:8080"}},
	}

	err := sr.ResolveUpstreams(route)
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0:9191", route.Upstreams[0].Address)
}

func TestSidecarResolver_ResolveUpstreams_ServiceNotInUpstreams(t *testing.T) {
	client, ts := mockConsulAgent(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agent/service/my-ingress-sidecar-proxy" {
			resp := consul.AgentService{
				Proxy: &consul.AgentServiceConnectProxyConfig{
					Upstreams: []consul.Upstream{
						{DestinationName: "other-service", LocalBindPort: 9999},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
	defer ts.Close()

	sr := NewSidecarResolver(client, testLogger(), "my-ingress")
	route := &RouteDefinition{
		ServiceName: "web-backend",
		Upstreams:   []Upstream{{Address: "10.0.0.1:8080"}},
	}

	err := sr.ResolveUpstreams(route)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no upstream entry for service web-backend")
}

func TestSidecarResolver_ResolveUpstreams_SidecarNotFound(t *testing.T) {
	client, ts := mockConsulAgent(func(w http.ResponseWriter, r *http.Request) {
		// Return 404 for sidecar proxy
		http.Error(w, "unknown service", http.StatusNotFound)
	})
	defer ts.Close()

	sr := NewSidecarResolver(client, testLogger(), "my-ingress")
	route := &RouteDefinition{
		ServiceName: "web-backend",
		Upstreams:   []Upstream{{Address: "10.0.0.1:8080"}},
	}

	err := sr.ResolveUpstreams(route)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to query sidecar proxy")
}

func TestSidecarResolver_ResolveUpstreams_ZeroBindPort(t *testing.T) {
	client, ts := mockConsulAgent(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agent/service/my-ingress-sidecar-proxy" {
			resp := consul.AgentService{
				Proxy: &consul.AgentServiceConnectProxyConfig{
					Upstreams: []consul.Upstream{
						{DestinationName: "web-backend", LocalBindPort: 0},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
	defer ts.Close()

	sr := NewSidecarResolver(client, testLogger(), "my-ingress")
	route := &RouteDefinition{
		ServiceName: "web-backend",
		Upstreams:   []Upstream{{Address: "10.0.0.1:8080"}},
	}

	err := sr.ResolveUpstreams(route)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no bind port")
}
