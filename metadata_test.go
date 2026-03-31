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
	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
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
	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	assert.Nil(t, routes)
}

func TestParseServiceRoutes_NonIndexedMeta(t *testing.T) {
	meta := map[string]string{
		"caddy-protocol":     "http",
		"caddy-host":         "app.example.com",
		"caddy-path":         "/api",
		"caddy-priority":     "100",
		"caddy-weight":       "5",
		"caddy-strip-prefix": "true",
	}
	svc := &ServiceState{
		Name: "web",
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Weight: 1, Tags: []string{"caddy-consul"}, Meta: meta},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
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
	meta := map[string]string{
		"caddy-route-0-protocol": "http",
		"caddy-route-0-host":     "web.example.com",
		"caddy-route-0-path":     "/",
		"caddy-route-1-protocol": "tcp",
		"caddy-route-1-port":     "5432",
	}
	svc := &ServiceState{
		Name: "multi",
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Tags: []string{"caddy-consul"}, Meta: meta},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 2)

	assert.Equal(t, ProtocolHTTP, routes[0].Protocol)
	assert.Equal(t, "web.example.com", routes[0].Host)
	assert.Equal(t, "/", routes[0].Path)

	assert.Equal(t, ProtocolTCP, routes[1].Protocol)
	assert.Equal(t, 5432, routes[1].Port)
}

func TestParseServiceRoutes_IndexedWinsOverNonIndexed(t *testing.T) {
	meta := map[string]string{
		"caddy-host":             "nonindexed.example.com",
		"caddy-route-0-host":     "indexed.example.com",
		"caddy-route-0-protocol": "http",
	}
	svc := &ServiceState{
		Name: "mixed",
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Tags: []string{"caddy-consul"}, Meta: meta},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, "indexed.example.com", routes[0].Host)
}

func TestParseServiceRoutes_MetadataWinsOverFabio(t *testing.T) {
	svc := &ServiceState{
		Name: "mixed",
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true,
				Tags: []string{"caddy-consul", "urlprefix-fabio.example.com/"},
				Meta: map[string]string{"caddy-host": "meta.example.com"}},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
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
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Tags: []string{
				"urlprefix-app.example.com/",
				"urlprefix-app.example.com/api strip=/api",
			}},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
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
			{Address: "10.0.0.1", Port: 5432, Healthy: true, Tags: []string{"urlprefix-:5432 proto=tcp"}},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1)

	assert.Equal(t, ProtocolTCP, routes[0].Protocol)
	assert.Equal(t, 5432, routes[0].Port)
}

func TestParseServiceRoutes_FabioHTTPS(t *testing.T) {
	svc := &ServiceState{
		Name: "secure",
		Tags: []string{"urlprefix-secure.example.com/ proto=https"},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 443, Healthy: true, Tags: []string{"urlprefix-secure.example.com/ proto=https"}},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, ProtocolHTTPS, routes[0].Protocol)
	assert.Equal(t, "secure.example.com", routes[0].Host)
}

func TestParseServiceRoutes_DefaultModeDirect(t *testing.T) {
	svc := &ServiceState{
		Name: "web",
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Tags: []string{"caddy-consul"},
				Meta: map[string]string{"caddy-host": "web.example.com"}},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, UpstreamDirect, routes[0].UpstreamMode)
}

func TestParseServiceRoutes_DisabledRoute(t *testing.T) {
	svc := &ServiceState{
		Name: "disabled",
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Tags: []string{"caddy-consul"},
				Meta: map[string]string{"caddy-host": "disabled.example.com", "caddy-enabled": "false"}},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	assert.Len(t, routes, 0)
}

func TestParseServiceRoutes_MultipleUpstreams(t *testing.T) {
	meta := map[string]string{"caddy-host": "example.com"}
	svc := &ServiceState{
		Name: "web",
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Weight: 3, Tags: []string{"caddy-consul"}, Meta: meta},
			{Address: "10.0.0.2", Port: 8080, Healthy: true, Weight: 1, Tags: []string{"caddy-consul"}, Meta: meta},
			{Address: "10.0.0.3", Port: 8080, Healthy: false, Tags: []string{"caddy-consul"}, Meta: meta}, // unhealthy, excluded
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1)
	assert.Len(t, routes[0].Upstreams, 2)
	assert.Equal(t, "10.0.0.1:8080", routes[0].Upstreams[0].Address)
	assert.Equal(t, 3, routes[0].Upstreams[0].Weight)
}

func TestParseServiceRoutes_PerInstanceMetadata(t *testing.T) {
	// Multiple instances under the same service name, each with unique
	// caddy-port metadata (e.g., minecraft servers). Each should produce
	// its own TCP route.
	svc := &ServiceState{
		Name: "minecraft",
		Tags: []string{"caddy-consul"},
		Instances: []ServiceInstance{
			{
				ID:      "minecraft-server-a",
				Address: "172.16.46.120",
				Port:    25565,
				Healthy: true,
				Weight:  1,
				Meta:    map[string]string{"caddy-protocol": "tcp", "caddy-port": "25565"},
			},
			{
				ID:      "minecraft-server-b",
				Address: "172.16.46.120",
				Port:    25566,
				Healthy: true,
				Weight:  1,
				Meta:    map[string]string{"caddy-protocol": "tcp", "caddy-port": "25566"},
			},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 2, "each instance with unique caddy-port should produce its own route")

	// Verify each route has a distinct port and one upstream
	ports := map[int]bool{}
	for _, r := range routes {
		assert.Equal(t, ProtocolTCP, r.Protocol)
		assert.Equal(t, "minecraft", r.ServiceName)
		require.Len(t, r.Upstreams, 1)
		ports[r.Port] = true
	}
	assert.True(t, ports[25565], "should have route for port 25565")
	assert.True(t, ports[25566], "should have route for port 25566")
}

func TestParseServiceRoutes_SharedMetadata_MultipleUpstreams(t *testing.T) {
	// Multiple instances with the SAME caddy metadata should produce
	// one route with multiple upstreams (load balancing).
	meta := map[string]string{"caddy-host": "app.example.com"}
	svc := &ServiceState{
		Name: "web",
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Weight: 1, Tags: []string{"caddy-consul"}, Meta: meta},
			{Address: "10.0.0.2", Port: 8080, Healthy: true, Weight: 1, Tags: []string{"caddy-consul"}, Meta: meta},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1, "same metadata should produce one route")
	assert.Len(t, routes[0].Upstreams, 2, "both healthy instances should be upstreams")
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
			result, _ := parseFabioTag(tt.tag)
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

func TestParseFabioTag_Redirect(t *testing.T) {
	rd, _ := parseFabioTag("urlprefix-www.habitat.dev/ redirect=301,https://www.honesthosting.io$path")
	require.NotNil(t, rd)
	assert.Equal(t, "www.habitat.dev", rd.Host)
	assert.Equal(t, "/", rd.Path)
	assert.Equal(t, 301, rd.RedirectCode)
	assert.Equal(t, "https://www.honesthosting.io{http.request.uri}", rd.RedirectURL)
	assert.True(t, rd.IsRedirect())
}

func TestParseFabioTag_RedirectWithPort80_Dropped(t *testing.T) {
	// :80 redirect tags are HTTP→HTTPS redirects handled by Caddy automatically.
	// They should be dropped to prevent redirect loops.
	rd, portRedir := parseFabioTag("urlprefix-www.habitat.dev:80/ redirect=301,https://www.honesthosting.io$path")
	assert.True(t, portRedir, "should be flagged as port redirect")
	assert.Nil(t, rd, ":80 redirect should be dropped (Caddy handles HTTP→HTTPS)")
}

func TestParseFabioTag_RedirectWithPort443_Dropped(t *testing.T) {
	rd, portRedir := parseFabioTag("urlprefix-secure.example.com:443/ redirect=302,https://other.com$path")
	assert.True(t, portRedir)
	assert.Nil(t, rd, ":443 redirect should be dropped")
}

func TestParseFabioTag_RedirectWithoutPort_Kept(t *testing.T) {
	// Redirect WITHOUT :port qualifier is a real cross-domain redirect — keep it
	rd, portRedir := parseFabioTag("urlprefix-old.example.com/ redirect=301,https://new.example.com$path")
	assert.False(t, portRedir, "no port qualifier — should not be flagged")
	require.NotNil(t, rd)
	assert.Equal(t, "old.example.com", rd.Host)
	assert.Equal(t, 301, rd.RedirectCode)
	assert.Equal(t, "https://new.example.com{http.request.uri}", rd.RedirectURL)
}

func TestParseFabioTag_PortStripNonStandard(t *testing.T) {
	rd, _ := parseFabioTag("urlprefix-app.example.com:8080/")
	require.NotNil(t, rd)
	assert.Equal(t, "app.example.com:8080", rd.Host) // non-standard port kept
}

func TestParseServiceRoutes_NativeRedirect(t *testing.T) {
	svc := &ServiceState{
		Name: "redirect-svc",
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Tags: []string{"caddy-consul"},
				Meta: map[string]string{
					"caddy-host":          "old.example.com",
					"caddy-redirect-code": "301",
					"caddy-redirect-url":  "https://new.example.com{http.request.uri}",
				}},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, "old.example.com", routes[0].Host)
	assert.Equal(t, 301, routes[0].RedirectCode)
	assert.Equal(t, "https://new.example.com{http.request.uri}", routes[0].RedirectURL)
	assert.True(t, routes[0].IsRedirect())
}

func TestParseServiceRoutes_IndexedRedirect(t *testing.T) {
	meta := map[string]string{
		"caddy-route-0-host":          "old.example.com",
		"caddy-route-0-redirect-code": "301",
		"caddy-route-0-redirect-url":  "https://new.example.com{http.request.uri}",
		"caddy-route-1-host":          "app.example.com",
		"caddy-route-1-path":          "/api",
	}
	svc := &ServiceState{
		Name: "multi-redirect",
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Tags: []string{"caddy-consul"}, Meta: meta},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 2)
	assert.True(t, routes[0].IsRedirect())
	assert.Equal(t, 301, routes[0].RedirectCode)
	assert.False(t, routes[1].IsRedirect())
	assert.Equal(t, "app.example.com", routes[1].Host)
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

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	assert.Nil(t, routes)
}

func TestParseServiceRoutes_IndexedSNI(t *testing.T) {
	meta := map[string]string{
		"caddy-route-0-protocol": "tls-passthrough",
		"caddy-route-0-port":     "5432",
		"caddy-route-0-sni":      "db.example.com",
	}
	svc := &ServiceState{
		Name: "db",
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 5432, Healthy: true, Tags: []string{"caddy-consul"}, Meta: meta},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, ProtocolTLSPassthrough, routes[0].Protocol)
	assert.Equal(t, "db.example.com", routes[0].Host) // SNI maps to Host
	assert.Equal(t, 5432, routes[0].Port)
}

// --- NodeName propagation tests ---

func TestParseServiceRoutes_NodeNamePropagated_Indexed(t *testing.T) {
	meta := map[string]string{
		"caddy-route-0-protocol": "tcp",
		"caddy-route-0-port":     "5432",
	}
	svc := &ServiceState{
		Name: "tcp-svc",
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 5432, Healthy: true, NodeName: "node-a", Tags: []string{"caddy-consul"}, Meta: meta},
			{Address: "10.0.0.2", Port: 5432, Healthy: true, NodeName: "node-b", Tags: []string{"caddy-consul"}, Meta: meta},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1)
	require.Len(t, routes[0].Upstreams, 2)
	assert.Equal(t, "node-a", routes[0].Upstreams[0].NodeName)
	assert.Equal(t, "node-b", routes[0].Upstreams[1].NodeName)
}

func TestParseServiceRoutes_NodeNamePropagated_NonIndexed(t *testing.T) {
	svc := &ServiceState{
		Name: "tcp-svc",
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 5432, Healthy: true, NodeName: "worker-01", Tags: []string{"caddy-consul"},
				Meta: map[string]string{"caddy-protocol": "tcp", "caddy-port": "5432"}},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1)
	require.Len(t, routes[0].Upstreams, 1)
	assert.Equal(t, "worker-01", routes[0].Upstreams[0].NodeName)
}

func TestParseServiceRoutes_NodeNamePropagated_Fabio(t *testing.T) {
	svc := &ServiceState{
		Name: "tcp-svc",
		Tags: []string{"urlprefix-:5432 proto=tcp"},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 9000, Healthy: true, NodeName: "node-x",
				Tags: []string{"urlprefix-:5432 proto=tcp"}},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1)
	require.Len(t, routes[0].Upstreams, 1)
	assert.Equal(t, "node-x", routes[0].Upstreams[0].NodeName)
}

// --- Instance-level tag filtering tests ---

func TestParseServiceRoutes_Indexed_OnlyTaggedInstancesAreUpstreams(t *testing.T) {
	// Service with 3 instances but only 1 has the caddy-consul tag.
	// The metrics sidecar and database container should NOT be upstreams.
	svc := &ServiceState{
		Name: "frontend",
		Instances: []ServiceInstance{
			{
				ID: "frontend-web", Address: "10.0.0.1", Port: 25112,
				Healthy: true, Weight: 1,
				Tags: []string{"caddy-consul", "frontend"},
				Meta: map[string]string{
					"caddy-route-0-protocol": "http",
					"caddy-route-0-host":     "frontend.example.com",
				},
			},
			{
				ID: "frontend-metrics", Address: "10.0.0.1", Port: 28902,
				Healthy: true, Weight: 1,
				Tags: []string{"caddy-mywordpress-metrics"},
			},
			{
				ID: "frontend-db", Address: "10.0.0.1", Port: 27933,
				Healthy: true, Weight: 1,
				Tags: []string{"database"},
			},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1)
	require.Len(t, routes[0].Upstreams, 1, "only the instance with caddy-consul tag should be an upstream")
	assert.Equal(t, "10.0.0.1:25112", routes[0].Upstreams[0].Address)
}

func TestParseServiceRoutes_NonIndexed_OnlyTaggedInstancesAreUpstreams(t *testing.T) {
	svc := &ServiceState{
		Name: "app",
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Weight: 1,
				Tags: []string{"caddy-consul"},
				Meta: map[string]string{"caddy-host": "app.example.com"}},
			{Address: "10.0.0.2", Port: 9090, Healthy: true, Weight: 1,
				Tags: []string{"monitoring"}},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1)
	require.Len(t, routes[0].Upstreams, 1, "only the instance with caddy-consul tag should be an upstream")
	assert.Equal(t, "10.0.0.1:8080", routes[0].Upstreams[0].Address)
}

func TestParseServiceRoutes_ConnectTagInstanceIncluded(t *testing.T) {
	// Instance with the connect tag should also be included as an upstream.
	svc := &ServiceState{
		Name: "mesh-svc",
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Weight: 1,
				Tags: []string{"caddy-consul-connect"},
				Meta: map[string]string{"caddy-host": "mesh.example.com"}},
			{Address: "10.0.0.2", Port: 9090, Healthy: true, Weight: 1,
				Tags: []string{"sidecar"}},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1)
	require.Len(t, routes[0].Upstreams, 1)
	assert.Equal(t, "10.0.0.1:8080", routes[0].Upstreams[0].Address)
}

func TestParseServiceRoutes_InstanceWithCaddyMetaButNoTag(t *testing.T) {
	// An instance that has caddy-* metadata on itself (not service-level)
	// should be included even without the service tag — the metadata
	// proves routing intent.
	svc := &ServiceState{
		Name: "self-describing",
		Tags: []string{"caddy-consul"},
		Meta: map[string]string{},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Weight: 1,
				Tags: []string{"caddy-consul"},
				Meta: map[string]string{"caddy-host": "self.example.com"}},
			{Address: "10.0.0.2", Port: 9090, Healthy: true, Weight: 1,
				Meta: map[string]string{"caddy-host": "self.example.com"}},
			{Address: "10.0.0.3", Port: 7070, Healthy: true, Weight: 1,
				Tags: []string{"unrelated"},
				Meta: map[string]string{"team": "backend"}},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1)
	// Instance 1 matches via caddy-consul tag, instance 2 matches via caddy-* meta,
	// instance 3 has neither.
	assert.Len(t, routes[0].Upstreams, 2, "instances with tag or caddy-* meta should be upstreams")
}

func TestParseServiceRoutes_UnhealthyTaggedInstanceExcluded(t *testing.T) {
	// Even with the correct tag, unhealthy instances must not be upstreams.
	meta := map[string]string{"caddy-host": "web.example.com"}
	svc := &ServiceState{
		Name: "web",
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Tags: []string{"caddy-consul"}, Meta: meta},
			{Address: "10.0.0.2", Port: 8080, Healthy: false, Tags: []string{"caddy-consul"}, Meta: meta},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1)
	require.Len(t, routes[0].Upstreams, 1, "unhealthy instance should be excluded even with correct tag")
	assert.Equal(t, "10.0.0.1:8080", routes[0].Upstreams[0].Address)
}

func TestParseServiceRoutes_NoTaggedInstances_ReturnsNil(t *testing.T) {
	// Service has metadata but no instances carry the service tag or caddy-* meta.
	// Should return nil (no routes) since there are no valid upstreams.
	svc := &ServiceState{
		Name: "orphaned",
		Meta: map[string]string{
			"caddy-route-0-host":     "orphaned.example.com",
			"caddy-route-0-protocol": "http",
		},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 9090, Healthy: true, Tags: []string{"metrics"}},
			{Address: "10.0.0.2", Port: 7070, Healthy: true, Tags: []string{"database"}},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	assert.Nil(t, routes, "no routes when no instances have the service tag or caddy-* meta")
}

// --- Instance-first routing tests ---

func TestParseServiceRoutes_MixedInstanceMeta_DifferentRoutes(t *testing.T) {
	// Core bug scenario: 3 instances under one service, each with different
	// caddy metadata. Should produce 3 separate routes (2 HTTP + 1 TCP),
	// each with only its own instance as upstream.
	svc := &ServiceState{
		Name: "mywp-site",
		Tags: []string{"caddy-consul"},
		Meta: map[string]string{
			// Service-level meta is the union from the watcher — but each
			// instance's own meta should take precedence for its own route.
			"caddy-host":             "filemanager.example.com:8443",
			"caddy-port":             "40000",
			"caddy-protocol":         "http",
			"caddy-route-0-host":     "frontend.example.com:8443",
			"caddy-route-0-protocol": "http",
			"external-source":        "nomad",
		},
		Instances: []ServiceInstance{
			{
				ID: "filemanager-frontend", Address: "10.0.0.1", Port: 26662,
				Healthy: true, Weight: 1, Tags: []string{"caddy-consul"},
				Meta: map[string]string{
					"caddy-host":     "filemanager.example.com:8443",
					"caddy-protocol": "http",
					"external-source": "nomad",
				},
			},
			{
				ID: "filemanager-sftp", Address: "10.0.0.1", Port: 29117,
				Healthy: true, Weight: 1, Tags: []string{"caddy-consul"},
				Meta: map[string]string{
					"caddy-port":     "40000",
					"caddy-protocol": "tcp",
					"external-source": "nomad",
				},
			},
			{
				ID: "frontend", Address: "10.0.0.1", Port: 26283,
				Healthy: true, Weight: 1, Tags: []string{"caddy-consul", "frontend"},
				Meta: map[string]string{
					"caddy-route-0-host":     "frontend.example.com:8443",
					"caddy-route-0-protocol": "http",
					"external-source":        "nomad",
				},
			},
			{
				ID: "frontend-metrics", Address: "10.0.0.1", Port: 26749,
				Healthy: true, Weight: 1, Tags: []string{"caddy-mywordpress-metrics"},
				Meta: map[string]string{"external-source": "nomad"},
			},
			{
				ID: "database", Address: "10.0.0.1", Port: 30905,
				Healthy: true, Weight: 1, Tags: []string{"database"},
				Meta: map[string]string{"external-source": "nomad"},
			},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())

	// Should produce 3 routes: HTTP for filemanager, TCP for sftp, HTTP for frontend
	require.Len(t, routes, 3, "should produce 3 separate routes for 3 differently-configured instances")

	// Build a lookup by protocol+host+port for easy assertions
	type routeID struct {
		protocol Protocol
		host     string
		port     int
	}
	routeByID := make(map[routeID]RouteDefinition)
	for _, r := range routes {
		routeByID[routeID{r.Protocol, r.Host, r.Port}] = r
	}

	// Filemanager HTTP route
	fm, ok := routeByID[routeID{ProtocolHTTP, "filemanager.example.com:8443", 0}]
	require.True(t, ok, "should have HTTP route for filemanager")
	require.Len(t, fm.Upstreams, 1)
	assert.Equal(t, "10.0.0.1:26662", fm.Upstreams[0].Address)

	// SFTP TCP route
	sftp, ok := routeByID[routeID{ProtocolTCP, "", 40000}]
	require.True(t, ok, "should have TCP route on port 40000")
	require.Len(t, sftp.Upstreams, 1)
	assert.Equal(t, "10.0.0.1:29117", sftp.Upstreams[0].Address)

	// Frontend HTTP route (indexed)
	fe, ok := routeByID[routeID{ProtocolHTTP, "frontend.example.com:8443", 0}]
	require.True(t, ok, "should have HTTP route for frontend")
	require.Len(t, fe.Upstreams, 1)
	assert.Equal(t, "10.0.0.1:26283", fe.Upstreams[0].Address)
}

func TestParseServiceRoutes_MixedIndexedAndNonIndexed_AcrossInstances(t *testing.T) {
	// One instance uses indexed metadata, another uses non-indexed.
	// Each should produce its own route independently.
	svc := &ServiceState{
		Name: "mixed-styles",
		Instances: []ServiceInstance{
			{
				ID: "indexed-inst", Address: "10.0.0.1", Port: 8080,
				Healthy: true, Weight: 1, Tags: []string{"caddy-consul"},
				Meta: map[string]string{
					"caddy-route-0-host":     "indexed.example.com",
					"caddy-route-0-protocol": "http",
				},
			},
			{
				ID: "nonindexed-inst", Address: "10.0.0.2", Port: 9090,
				Healthy: true, Weight: 1, Tags: []string{"caddy-consul"},
				Meta: map[string]string{
					"caddy-host":     "nonindexed.example.com",
					"caddy-protocol": "http",
				},
			},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 2, "each instance should produce its own route")

	hosts := map[string]string{}
	for _, r := range routes {
		require.Len(t, r.Upstreams, 1)
		hosts[r.Host] = r.Upstreams[0].Address
	}
	assert.Equal(t, "10.0.0.1:8080", hosts["indexed.example.com"])
	assert.Equal(t, "10.0.0.2:9090", hosts["nonindexed.example.com"])
}

func TestParseServiceRoutes_FabioPerInstance_DifferentURLPrefixes(t *testing.T) {
	// Two instances with different urlprefix tags should produce separate routes.
	svc := &ServiceState{
		Name: "multi-fabio",
		Tags: []string{
			"urlprefix-app.example.com/",
			"urlprefix-api.example.com/",
		},
		Instances: []ServiceInstance{
			{
				ID: "app", Address: "10.0.0.1", Port: 8080,
				Healthy: true, Weight: 1,
				Tags: []string{"urlprefix-app.example.com/"},
			},
			{
				ID: "api", Address: "10.0.0.2", Port: 9090,
				Healthy: true, Weight: 1,
				Tags: []string{"urlprefix-api.example.com/"},
			},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 2, "each instance should produce its own Fabio route")

	hosts := map[string]string{}
	for _, r := range routes {
		require.Len(t, r.Upstreams, 1, "each Fabio route should have only its own instance")
		hosts[r.Host] = r.Upstreams[0].Address
	}
	assert.Equal(t, "10.0.0.1:8080", hosts["app.example.com"])
	assert.Equal(t, "10.0.0.2:9090", hosts["api.example.com"])
}

func TestParseServiceRoutes_ServiceMetaAsDefault_InstanceOverride(t *testing.T) {
	// Service has caddy-protocol: http. Instance overrides with caddy-protocol: tcp.
	svc := &ServiceState{
		Name: "override-test",
		Meta: map[string]string{
			"caddy-protocol": "http",
			"caddy-host":     "override.example.com",
		},
		Instances: []ServiceInstance{
			{
				ID: "tcp-inst", Address: "10.0.0.1", Port: 5432,
				Healthy: true, Weight: 1, Tags: []string{"caddy-consul"},
				Meta: map[string]string{
					"caddy-protocol": "tcp",
					"caddy-port":     "5432",
				},
			},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, ProtocolTCP, routes[0].Protocol, "instance meta should override service meta")
	assert.Equal(t, 5432, routes[0].Port)
}

func TestParseServiceRoutes_UntaggedInstanceExcluded_EvenWithServiceMeta(t *testing.T) {
	// Service has caddy-host in svc.Meta. Instance has no tag, no caddy-* meta.
	// Should NOT produce a route (the eligibility guard prevents inheritance).
	svc := &ServiceState{
		Name: "guard-test",
		Meta: map[string]string{
			"caddy-host": "guard.example.com",
		},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Tags: []string{"worker"}},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	assert.Nil(t, routes, "untagged instance without caddy-* meta should not inherit service-level routes")
}

func TestParseServiceRoutes_IdenticalRoutes_GroupedUpstreams(t *testing.T) {
	// Two instances with identical routing config (same host, same protocol)
	// should be grouped into one route with two upstreams.
	meta := map[string]string{
		"caddy-route-0-host":     "grouped.example.com",
		"caddy-route-0-protocol": "http",
	}
	svc := &ServiceState{
		Name: "grouped",
		Instances: []ServiceInstance{
			{
				ID: "inst-a", Address: "10.0.0.1", Port: 8080,
				Healthy: true, Weight: 1, Tags: []string{"caddy-consul"}, Meta: meta,
			},
			{
				ID: "inst-b", Address: "10.0.0.2", Port: 8080,
				Healthy: true, Weight: 1, Tags: []string{"caddy-consul"}, Meta: meta,
			},
		},
	}

	routes := ParseServiceRoutes(svc, "caddy-consul", "caddy-consul-connect", testLogger())
	require.Len(t, routes, 1, "identical routes should be grouped")
	assert.Len(t, routes[0].Upstreams, 2, "grouped route should have both upstreams")
	assert.Equal(t, "grouped.example.com", routes[0].Host)
}
