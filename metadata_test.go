package caddyconsul

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testLogger() *zap.Logger {
	return zap.NewNop()
}

func TestParseServiceRoutes_NoInstances(t *testing.T) {
	svc := &ServiceState{Name: "web"}
	routes := ParseServiceRoutes(svc, "direct", testLogger())
	assert.Nil(t, routes)
}

func TestParseServiceRoutes_NoHealthyInstances(t *testing.T) {
	svc := &ServiceState{
		Name: "web",
		Meta: map[string]string{"caddy-host": "example.com"},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: false},
		},
	}
	routes := ParseServiceRoutes(svc, "direct", testLogger())
	assert.Nil(t, routes)
}

func TestParseServiceRoutes_NonIndexedMeta(t *testing.T) {
	svc := &ServiceState{
		Name: "web",
		Meta: map[string]string{
			"caddy-protocol":     "http",
			"caddy-host":         "app.example.com",
			"caddy-path":         "/api",
			"caddy-priority":     "100",
			"caddy-weight":       "5",
			"caddy-strip-prefix": "true",
		},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Weight: 1},
		},
	}

	routes := ParseServiceRoutes(svc, "direct", testLogger())
	require.Len(t, routes, 1)

	r := routes[0]
	assert.Equal(t, "web", r.ServiceName)
	assert.Equal(t, ProtocolHTTP, r.Protocol)
	assert.Equal(t, "app.example.com", r.Host)
	assert.Equal(t, "/api", r.Path)
	assert.Equal(t, 100, r.Priority)
	assert.Equal(t, 5, r.Weight)
	assert.True(t, r.StripPrefix)
	assert.Equal(t, UpstreamDirect, r.UpstreamMode)
	assert.Len(t, r.Upstreams, 1)
	assert.Equal(t, "10.0.0.1:8080", r.Upstreams[0].Address)
}

func TestParseServiceRoutes_IndexedMeta(t *testing.T) {
	svc := &ServiceState{
		Name: "multi",
		Meta: map[string]string{
			"caddy-route-0-protocol": "http",
			"caddy-route-0-host":     "web.example.com",
			"caddy-route-0-path":     "/",
			"caddy-route-1-protocol": "tcp",
			"caddy-route-1-port":     "5432",
		},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true},
		},
	}

	routes := ParseServiceRoutes(svc, "direct", testLogger())
	require.Len(t, routes, 2)

	assert.Equal(t, ProtocolHTTP, routes[0].Protocol)
	assert.Equal(t, "web.example.com", routes[0].Host)
	assert.Equal(t, "/", routes[0].Path)

	assert.Equal(t, ProtocolTCP, routes[1].Protocol)
	assert.Equal(t, 5432, routes[1].Port)
}

func TestParseServiceRoutes_IndexedWinsOverNonIndexed(t *testing.T) {
	svc := &ServiceState{
		Name: "mixed",
		Meta: map[string]string{
			"caddy-host":             "nonindexed.example.com",
			"caddy-route-0-host":     "indexed.example.com",
			"caddy-route-0-protocol": "http",
		},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true},
		},
	}

	routes := ParseServiceRoutes(svc, "direct", testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, "indexed.example.com", routes[0].Host)
}

func TestParseServiceRoutes_MetadataWinsOverFabio(t *testing.T) {
	svc := &ServiceState{
		Name: "mixed",
		Tags: []string{"urlprefix-fabio.example.com/"},
		Meta: map[string]string{
			"caddy-host": "meta.example.com",
		},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true},
		},
	}

	routes := ParseServiceRoutes(svc, "direct", testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, "meta.example.com", routes[0].Host)
}

func TestParseServiceRoutes_FabioTags(t *testing.T) {
	svc := &ServiceState{
		Name: "legacy",
		Tags: []string{
			"urlprefix-app.example.com/",
			"urlprefix-app.example.com/api strip=/api",
		},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true},
		},
	}

	routes := ParseServiceRoutes(svc, "direct", testLogger())
	require.Len(t, routes, 2)

	assert.Equal(t, "app.example.com", routes[0].Host)
	assert.Equal(t, "/", routes[0].Path)
	assert.False(t, routes[0].StripPrefix)

	assert.Equal(t, "app.example.com", routes[1].Host)
	assert.Equal(t, "/api", routes[1].Path)
	assert.True(t, routes[1].StripPrefix)
}

func TestParseServiceRoutes_FabioTCP(t *testing.T) {
	svc := &ServiceState{
		Name: "postgres",
		Tags: []string{"urlprefix-:5432 proto=tcp"},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 5432, Healthy: true},
		},
	}

	routes := ParseServiceRoutes(svc, "direct", testLogger())
	require.Len(t, routes, 1)

	assert.Equal(t, ProtocolTCP, routes[0].Protocol)
	assert.Equal(t, 5432, routes[0].Port)
}

func TestParseServiceRoutes_FabioHTTPS(t *testing.T) {
	svc := &ServiceState{
		Name: "secure",
		Tags: []string{"urlprefix-secure.example.com/ proto=https"},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 443, Healthy: true},
		},
	}

	routes := ParseServiceRoutes(svc, "direct", testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, ProtocolHTTPS, routes[0].Protocol)
	assert.Equal(t, "secure.example.com", routes[0].Host)
}

func TestParseServiceRoutes_NoExplicitMode_DefaultsDirect(t *testing.T) {
	svc := &ServiceState{
		Name: "web",
		Meta: map[string]string{"caddy-host": "web.example.com"},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true},
		},
	}

	// Even with global mode "sidecar", no explicit mode → direct (no mesh)
	routes := ParseServiceRoutes(svc, "sidecar", testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, UpstreamDirect, routes[0].UpstreamMode)
}

func TestParseServiceRoutes_BareConnect_GlobalSidecar(t *testing.T) {
	svc := &ServiceState{
		Name: "mesh-svc",
		Meta: map[string]string{
			"caddy-host":          "mesh.example.com",
			"caddy-upstream-mode": "connect",
		},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true},
		},
	}

	routes := ParseServiceRoutes(svc, "sidecar", testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, UpstreamConnectSidecar, routes[0].UpstreamMode)
}

func TestParseServiceRoutes_BareConnect_GlobalDirect(t *testing.T) {
	svc := &ServiceState{
		Name: "mesh-svc",
		Meta: map[string]string{
			"caddy-host":          "mesh.example.com",
			"caddy-upstream-mode": "connect",
		},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true},
		},
	}

	routes := ParseServiceRoutes(svc, "direct", testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, UpstreamConnectDirect, routes[0].UpstreamMode)
}

func TestParseServiceRoutes_ExplicitConnectSidecar(t *testing.T) {
	svc := &ServiceState{
		Name: "mesh-svc",
		Meta: map[string]string{
			"caddy-host":          "mesh.example.com",
			"caddy-upstream-mode": "connect-sidecar",
		},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true},
		},
	}

	// Explicit connect-sidecar overrides global mode
	routes := ParseServiceRoutes(svc, "direct", testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, UpstreamConnectSidecar, routes[0].UpstreamMode)
}

func TestParseServiceRoutes_ExplicitConnectDirect(t *testing.T) {
	svc := &ServiceState{
		Name: "mesh-svc",
		Meta: map[string]string{
			"caddy-host":          "mesh.example.com",
			"caddy-upstream-mode": "connect-direct",
		},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true},
		},
	}

	// Explicit connect-direct overrides global mode
	routes := ParseServiceRoutes(svc, "sidecar", testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, UpstreamConnectDirect, routes[0].UpstreamMode)
}

func TestParseServiceRoutes_DisabledRoute(t *testing.T) {
	svc := &ServiceState{
		Name: "disabled",
		Meta: map[string]string{
			"caddy-host":    "disabled.example.com",
			"caddy-enabled": "false",
		},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true},
		},
	}

	routes := ParseServiceRoutes(svc, "direct", testLogger())
	assert.Len(t, routes, 0)
}

func TestParseServiceRoutes_MultipleUpstreams(t *testing.T) {
	svc := &ServiceState{
		Name: "web",
		Meta: map[string]string{"caddy-host": "example.com"},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Weight: 3},
			{Address: "10.0.0.2", Port: 8080, Healthy: true, Weight: 1},
			{Address: "10.0.0.3", Port: 8080, Healthy: false}, // unhealthy, excluded
		},
	}

	routes := ParseServiceRoutes(svc, "direct", testLogger())
	require.Len(t, routes, 1)
	assert.Len(t, routes[0].Upstreams, 2)
	assert.Equal(t, "10.0.0.1:8080", routes[0].Upstreams[0].Address)
	assert.Equal(t, 3, routes[0].Upstreams[0].Weight)
}

func TestParseFabioTag_Basic(t *testing.T) {
	tests := []struct {
		tag      string
		expected *RouteDefinition
	}{
		{
			tag: "urlprefix-example.com/",
			expected: &RouteDefinition{
				Protocol: ProtocolHTTP,
				Host:     "example.com",
				Path:     "/",
				Enabled:  true,
			},
		},
		{
			tag: "urlprefix-example.com/api",
			expected: &RouteDefinition{
				Protocol: ProtocolHTTP,
				Host:     "example.com",
				Path:     "/api",
				Enabled:  true,
			},
		},
		{
			tag: "urlprefix-:5432 proto=tcp",
			expected: &RouteDefinition{
				Protocol: ProtocolTCP,
				Port:     5432,
				Enabled:  true,
			},
		},
		{
			tag: "urlprefix-example.com/api strip=/api",
			expected: &RouteDefinition{
				Protocol:    ProtocolHTTP,
				Host:        "example.com",
				Path:        "/api",
				StripPrefix: true,
				Enabled:     true,
			},
		},
		{
			tag:      "urlprefix-",
			expected: nil,
		},
		{
			tag:      "urlprefix-:0 proto=tcp",
			expected: nil,
		},
		// Note: "not-a-urlprefix" is filtered by parseFabioTags before reaching parseFabioTag.
		// parseFabioTag itself does not check the prefix — it's a low-level parser.
	}

	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			result := parseFabioTag(tt.tag)
			if tt.expected == nil {
				assert.Nil(t, result)
			} else {
				require.NotNil(t, result)
				assert.Equal(t, tt.expected.Protocol, result.Protocol)
				assert.Equal(t, tt.expected.Host, result.Host)
				assert.Equal(t, tt.expected.Path, result.Path)
				assert.Equal(t, tt.expected.Port, result.Port)
				assert.Equal(t, tt.expected.StripPrefix, result.StripPrefix)
			}
		})
	}
}

func TestParseServiceRoutes_NoRoutingMetadata(t *testing.T) {
	svc := &ServiceState{
		Name: "plain",
		Tags: []string{"version:1.0", "env:prod"},
		Meta: map[string]string{"team": "backend"},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true},
		},
	}

	routes := ParseServiceRoutes(svc, "direct", testLogger())
	assert.Nil(t, routes)
}

func TestParseServiceRoutes_IndexedSNI(t *testing.T) {
	svc := &ServiceState{
		Name: "db",
		Meta: map[string]string{
			"caddy-route-0-protocol": "tls-passthrough",
			"caddy-route-0-port":     "5432",
			"caddy-route-0-sni":      "db.example.com",
		},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 5432, Healthy: true},
		},
	}

	routes := ParseServiceRoutes(svc, "direct", testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, ProtocolTLSPassthrough, routes[0].Protocol)
	assert.Equal(t, "db.example.com", routes[0].Host) // SNI maps to Host
	assert.Equal(t, 5432, routes[0].Port)
}
