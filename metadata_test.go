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
	routes := ParseServiceRoutes(svc, testLogger())
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
	routes := ParseServiceRoutes(svc, testLogger())
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

	routes := ParseServiceRoutes(svc, testLogger())
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

	routes := ParseServiceRoutes(svc, testLogger())
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

	routes := ParseServiceRoutes(svc, testLogger())
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

	routes := ParseServiceRoutes(svc, testLogger())
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

	routes := ParseServiceRoutes(svc, testLogger())
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

	routes := ParseServiceRoutes(svc, testLogger())
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

	routes := ParseServiceRoutes(svc, testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, ProtocolHTTPS, routes[0].Protocol)
	assert.Equal(t, "secure.example.com", routes[0].Host)
}

func TestParseServiceRoutes_DefaultModeDirect(t *testing.T) {
	svc := &ServiceState{
		Name: "web",
		Meta: map[string]string{
			"caddy-host": "web.example.com",
		},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true},
		},
	}

	routes := ParseServiceRoutes(svc, testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, UpstreamDirect, routes[0].UpstreamMode)
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

	routes := ParseServiceRoutes(svc, testLogger())
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

	routes := ParseServiceRoutes(svc, testLogger())
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

	routes := ParseServiceRoutes(svc, testLogger())
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
	svc := &ServiceState{
		Name: "web",
		Meta: map[string]string{
			"caddy-host": "app.example.com",
		},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true, Weight: 1},
			{Address: "10.0.0.2", Port: 8080, Healthy: true, Weight: 1},
		},
	}

	routes := ParseServiceRoutes(svc, testLogger())
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
		Meta: map[string]string{
			"caddy-host":          "old.example.com",
			"caddy-redirect-code": "301",
			"caddy-redirect-url":  "https://new.example.com{http.request.uri}",
		},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true},
		},
	}

	routes := ParseServiceRoutes(svc, testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, "old.example.com", routes[0].Host)
	assert.Equal(t, 301, routes[0].RedirectCode)
	assert.Equal(t, "https://new.example.com{http.request.uri}", routes[0].RedirectURL)
	assert.True(t, routes[0].IsRedirect())
}

func TestParseServiceRoutes_IndexedRedirect(t *testing.T) {
	svc := &ServiceState{
		Name: "multi-redirect",
		Meta: map[string]string{
			"caddy-route-0-host":          "old.example.com",
			"caddy-route-0-redirect-code": "301",
			"caddy-route-0-redirect-url":  "https://new.example.com{http.request.uri}",
			"caddy-route-1-host":          "app.example.com",
			"caddy-route-1-path":          "/api",
		},
		Instances: []ServiceInstance{
			{Address: "10.0.0.1", Port: 8080, Healthy: true},
		},
	}

	routes := ParseServiceRoutes(svc, testLogger())
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

	routes := ParseServiceRoutes(svc, testLogger())
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

	routes := ParseServiceRoutes(svc, testLogger())
	require.Len(t, routes, 1)
	assert.Equal(t, ProtocolTLSPassthrough, routes[0].Protocol)
	assert.Equal(t, "db.example.com", routes[0].Host) // SNI maps to Host
	assert.Equal(t, 5432, routes[0].Port)
}
