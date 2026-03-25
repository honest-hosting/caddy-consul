package caddyconsul

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompile_EmptyRoutes(t *testing.T) {
	rc := NewRouteCompiler(testLogger())
	result := rc.Compile(nil)

	assert.Empty(t, result.HTTPRoutes)
	assert.Empty(t, result.TCPRoutes)
	assert.Empty(t, result.Conflicts)
}

func TestCompile_SingleHTTPRoute(t *testing.T) {
	rc := NewRouteCompiler(testLogger())
	routes := []RouteDefinition{
		{
			ServiceName: "web",
			Protocol:    ProtocolHTTP,
			Host:        "app.example.com",
			Path:        "/",
			Upstreams: []Upstream{
				{Address: "10.0.0.1:8080", Healthy: true},
			},
		},
	}

	result := rc.Compile(routes)
	require.Len(t, result.HTTPRoutes, 1)
	assert.Equal(t, "app.example.com", result.HTTPRoutes[0].Host)
	assert.Equal(t, "/", result.HTTPRoutes[0].Path)
	assert.Empty(t, result.Conflicts)
}

func TestCompile_SingleTCPRoute(t *testing.T) {
	rc := NewRouteCompiler(testLogger())
	routes := []RouteDefinition{
		{
			ServiceName: "postgres",
			Protocol:    ProtocolTCP,
			Port:        5432,
			Upstreams: []Upstream{
				{Address: "10.0.0.1:5432", Healthy: true},
			},
		},
	}

	result := rc.Compile(routes)
	require.Len(t, result.TCPRoutes, 1)
	assert.Equal(t, 5432, result.TCPRoutes[0].Port)
	assert.Empty(t, result.Conflicts)
}

func TestCompile_DuplicateHostPath_FirstSeenWins(t *testing.T) {
	rc := NewRouteCompiler(testLogger())
	routes := []RouteDefinition{
		{
			ServiceName: "alpha",
			Protocol:    ProtocolHTTP,
			Host:        "app.example.com",
			Path:        "/api",
			Upstreams:   []Upstream{{Address: "10.0.0.1:8080", Healthy: true}},
		},
		{
			ServiceName: "beta",
			Protocol:    ProtocolHTTP,
			Host:        "app.example.com",
			Path:        "/api",
			Upstreams:   []Upstream{{Address: "10.0.0.2:8080", Healthy: true}},
		},
	}

	result := rc.Compile(routes)
	require.Len(t, result.HTTPRoutes, 1)
	assert.Equal(t, "alpha", result.HTTPRoutes[0].ServiceName)

	require.Len(t, result.Conflicts, 1)
	assert.Equal(t, ConflictDuplicateHostPath, result.Conflicts[0].Type)
	assert.Equal(t, "alpha", result.Conflicts[0].Winner.ServiceName)
	assert.Equal(t, "beta", result.Conflicts[0].Loser.ServiceName)
}

func TestCompile_PriorityWins(t *testing.T) {
	rc := NewRouteCompiler(testLogger())
	routes := []RouteDefinition{
		{
			ServiceName: "low-priority",
			Protocol:    ProtocolHTTP,
			Host:        "app.example.com",
			Path:        "/",
			Priority:    10,
			Upstreams:   []Upstream{{Address: "10.0.0.1:8080", Healthy: true}},
		},
		{
			ServiceName: "high-priority",
			Protocol:    ProtocolHTTP,
			Host:        "app.example.com",
			Path:        "/",
			Priority:    100,
			Upstreams:   []Upstream{{Address: "10.0.0.2:8080", Healthy: true}},
		},
	}

	result := rc.Compile(routes)
	require.Len(t, result.HTTPRoutes, 1)
	assert.Equal(t, "high-priority", result.HTTPRoutes[0].ServiceName)

	require.Len(t, result.Conflicts, 1)
	assert.Equal(t, "low-priority", result.Conflicts[0].Loser.ServiceName)
}

func TestCompile_DifferentPaths_NoConflict(t *testing.T) {
	rc := NewRouteCompiler(testLogger())
	routes := []RouteDefinition{
		{
			ServiceName: "api",
			Protocol:    ProtocolHTTP,
			Host:        "app.example.com",
			Path:        "/api",
			Upstreams:   []Upstream{{Address: "10.0.0.1:8080", Healthy: true}},
		},
		{
			ServiceName: "web",
			Protocol:    ProtocolHTTP,
			Host:        "app.example.com",
			Path:        "/web",
			Upstreams:   []Upstream{{Address: "10.0.0.2:8080", Healthy: true}},
		},
	}

	result := rc.Compile(routes)
	assert.Len(t, result.HTTPRoutes, 2)
	assert.Empty(t, result.Conflicts)
}

func TestCompile_DuplicateTCPPort_FirstSeenWins(t *testing.T) {
	rc := NewRouteCompiler(testLogger())
	routes := []RouteDefinition{
		{
			ServiceName: "pg-primary",
			Protocol:    ProtocolTCP,
			Port:        5432,
			Upstreams:   []Upstream{{Address: "10.0.0.1:5432", Healthy: true}},
		},
		{
			ServiceName: "pg-replica",
			Protocol:    ProtocolTCP,
			Port:        5432,
			Upstreams:   []Upstream{{Address: "10.0.0.2:5432", Healthy: true}},
		},
	}

	result := rc.Compile(routes)
	require.Len(t, result.TCPRoutes, 1)
	assert.Equal(t, "pg-primary", result.TCPRoutes[0].ServiceName)

	require.Len(t, result.Conflicts, 1)
	assert.Equal(t, ConflictDuplicatePortSNI, result.Conflicts[0].Type)
}

func TestCompile_TCPSamePotDifferentSNI_NoConflict(t *testing.T) {
	rc := NewRouteCompiler(testLogger())
	routes := []RouteDefinition{
		{
			ServiceName: "db1",
			Protocol:    ProtocolTLSPassthrough,
			Port:        5432,
			Host:        "db1.example.com",
			Upstreams:   []Upstream{{Address: "10.0.0.1:5432", Healthy: true}},
		},
		{
			ServiceName: "db2",
			Protocol:    ProtocolTLSPassthrough,
			Port:        5432,
			Host:        "db2.example.com",
			Upstreams:   []Upstream{{Address: "10.0.0.2:5432", Healthy: true}},
		},
	}

	result := rc.Compile(routes)
	assert.Len(t, result.TCPRoutes, 2)
	assert.Empty(t, result.Conflicts)
}

func TestCompile_NoHealthyUpstreams_Skipped(t *testing.T) {
	rc := NewRouteCompiler(testLogger())
	routes := []RouteDefinition{
		{
			ServiceName: "down",
			Protocol:    ProtocolHTTP,
			Host:        "down.example.com",
			Upstreams:   []Upstream{{Address: "10.0.0.1:8080", Healthy: false}},
		},
	}

	result := rc.Compile(routes)
	assert.Empty(t, result.HTTPRoutes)
	assert.Empty(t, result.Conflicts)
}

func TestCompile_UnknownProtocol_Warning(t *testing.T) {
	rc := NewRouteCompiler(testLogger())
	routes := []RouteDefinition{
		{
			ServiceName: "weird",
			Protocol:    Protocol("udp"),
			Upstreams:   []Upstream{{Address: "10.0.0.1:8080", Healthy: true}},
		},
	}

	result := rc.Compile(routes)
	assert.Empty(t, result.HTTPRoutes)
	assert.Empty(t, result.TCPRoutes)
	require.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], "unknown protocol")
}

func TestCompile_StripPrefix(t *testing.T) {
	rc := NewRouteCompiler(testLogger())
	routes := []RouteDefinition{
		{
			ServiceName: "api",
			Protocol:    ProtocolHTTP,
			Host:        "app.example.com",
			Path:        "/api",
			StripPrefix: true,
			Upstreams:   []Upstream{{Address: "10.0.0.1:8080", Healthy: true}},
		},
	}

	result := rc.Compile(routes)
	require.Len(t, result.HTTPRoutes, 1)
	assert.True(t, result.HTTPRoutes[0].StripPrefix)
}

func TestCompile_MixedProtocols(t *testing.T) {
	rc := NewRouteCompiler(testLogger())
	routes := []RouteDefinition{
		{
			ServiceName: "web",
			Protocol:    ProtocolHTTP,
			Host:        "app.example.com",
			Path:        "/",
			Upstreams:   []Upstream{{Address: "10.0.0.1:8080", Healthy: true}},
		},
		{
			ServiceName: "postgres",
			Protocol:    ProtocolTCP,
			Port:        5432,
			Upstreams:   []Upstream{{Address: "10.0.0.2:5432", Healthy: true}},
		},
	}

	result := rc.Compile(routes)
	assert.Len(t, result.HTTPRoutes, 1)
	assert.Len(t, result.TCPRoutes, 1)
	assert.Empty(t, result.Conflicts)
}

func TestCompile_RedirectRoute_NoUpstreamsNeeded(t *testing.T) {
	rc := NewRouteCompiler(testLogger())
	routes := []RouteDefinition{
		{
			ServiceName:  "redirect-svc",
			Protocol:     ProtocolHTTP,
			Host:         "old.example.com",
			Path:         "/",
			RedirectCode: 301,
			RedirectURL:  "https://new.example.com{http.request.uri}",
			Enabled:      true,
			// No upstreams — redirect routes don't need them
		},
	}

	result := rc.Compile(routes)
	require.Len(t, result.HTTPRoutes, 1)
	assert.Equal(t, 301, result.HTTPRoutes[0].RedirectCode)
	assert.Equal(t, "https://new.example.com{http.request.uri}", result.HTTPRoutes[0].RedirectURL)
	assert.Empty(t, result.HTTPRoutes[0].Upstreams)
}

func TestCompile_RedirectAndProxy_Coexist(t *testing.T) {
	rc := NewRouteCompiler(testLogger())
	routes := []RouteDefinition{
		{
			ServiceName:  "redirect-svc",
			Protocol:     ProtocolHTTP,
			Host:         "old.example.com",
			Path:         "/",
			RedirectCode: 301,
			RedirectURL:  "https://new.example.com{http.request.uri}",
			Enabled:      true,
		},
		{
			ServiceName: "proxy-svc",
			Protocol:    ProtocolHTTP,
			Host:        "app.example.com",
			Path:        "/",
			Upstreams:   []Upstream{{Address: "10.0.0.1:8080", Healthy: true}},
			Enabled:     true,
		},
	}

	result := rc.Compile(routes)
	assert.Len(t, result.HTTPRoutes, 2)
	assert.Empty(t, result.Conflicts)
}

// --- FilterTCPRoutesByNode tests ---

func TestFilterTCPRoutesByNode_MatchingNode(t *testing.T) {
	routes := []RouteDefinition{
		{
			ServiceName: "postgres",
			Protocol:    ProtocolTCP,
			Port:        5432,
			Upstreams: []Upstream{
				{Address: "10.0.0.1:5432", Healthy: true, NodeName: "node-a"},
				{Address: "10.0.0.2:5432", Healthy: true, NodeName: "node-b"},
			},
		},
	}

	filtered := FilterTCPRoutesByNode(routes, "node-a")
	require.Len(t, filtered, 1)
	// All upstreams preserved (not just local)
	assert.Len(t, filtered[0].Upstreams, 2)
}

func TestFilterTCPRoutesByNode_NoMatchingNode(t *testing.T) {
	routes := []RouteDefinition{
		{
			ServiceName: "postgres",
			Protocol:    ProtocolTCP,
			Port:        5432,
			Upstreams: []Upstream{
				{Address: "10.0.0.1:5432", Healthy: true, NodeName: "node-a"},
				{Address: "10.0.0.2:5432", Healthy: true, NodeName: "node-b"},
			},
		},
	}

	filtered := FilterTCPRoutesByNode(routes, "node-c")
	assert.Empty(t, filtered)
}

func TestFilterTCPRoutesByNode_MixedNodes(t *testing.T) {
	routes := []RouteDefinition{
		{
			ServiceName: "postgres",
			Protocol:    ProtocolTCP,
			Port:        5432,
			Upstreams: []Upstream{
				{Address: "10.0.0.1:5432", Healthy: true, NodeName: "node-a"},
			},
		},
		{
			ServiceName: "redis",
			Protocol:    ProtocolTCP,
			Port:        6379,
			Upstreams: []Upstream{
				{Address: "10.0.0.2:6379", Healthy: true, NodeName: "node-b"},
			},
		},
	}

	filtered := FilterTCPRoutesByNode(routes, "node-a")
	require.Len(t, filtered, 1)
	assert.Equal(t, "postgres", filtered[0].ServiceName)
}

func TestFilterTCPRoutesByNode_HTTPRoutesUnaffected(t *testing.T) {
	routes := []RouteDefinition{
		{
			ServiceName: "web",
			Protocol:    ProtocolHTTP,
			Host:        "app.example.com",
			Upstreams: []Upstream{
				{Address: "10.0.0.1:8080", Healthy: true, NodeName: "node-b"},
			},
		},
		{
			ServiceName: "postgres",
			Protocol:    ProtocolTCP,
			Port:        5432,
			Upstreams: []Upstream{
				{Address: "10.0.0.2:5432", Healthy: true, NodeName: "node-b"},
			},
		},
	}

	// Filter for node-a: HTTP route should pass through, TCP should be dropped
	filtered := FilterTCPRoutesByNode(routes, "node-a")
	require.Len(t, filtered, 1)
	assert.Equal(t, ProtocolHTTP, filtered[0].Protocol)
	assert.Equal(t, "web", filtered[0].ServiceName)
}

func TestFilterTCPRoutesByNode_EmptyNodeName(t *testing.T) {
	routes := []RouteDefinition{
		{
			ServiceName: "postgres",
			Protocol:    ProtocolTCP,
			Port:        5432,
			Upstreams: []Upstream{
				{Address: "10.0.0.1:5432", Healthy: true, NodeName: "node-a"},
			},
		},
	}

	// Empty node name = no filtering (safety fallback)
	filtered := FilterTCPRoutesByNode(routes, "")
	assert.Len(t, filtered, 1)
}

func TestFilterTCPRoutesByNode_TLSPassthrough(t *testing.T) {
	routes := []RouteDefinition{
		{
			ServiceName: "tls-svc",
			Protocol:    ProtocolTLSPassthrough,
			Port:        8443,
			Host:        "tls.example.com",
			Upstreams: []Upstream{
				{Address: "10.0.0.1:8443", Healthy: true, NodeName: "node-a"},
			},
		},
	}

	// TLS passthrough routes should also be filtered
	filtered := FilterTCPRoutesByNode(routes, "node-b")
	assert.Empty(t, filtered)

	filtered = FilterTCPRoutesByNode(routes, "node-a")
	require.Len(t, filtered, 1)
	assert.Equal(t, "tls-svc", filtered[0].ServiceName)
}

func TestFilterTCPRoutesByNode_PreservesAllUpstreams(t *testing.T) {
	routes := []RouteDefinition{
		{
			ServiceName: "postgres",
			Protocol:    ProtocolTCP,
			Port:        5432,
			Upstreams: []Upstream{
				{Address: "10.0.0.1:5432", Healthy: true, NodeName: "node-a"},
				{Address: "10.0.0.2:5432", Healthy: true, NodeName: "node-b"},
				{Address: "10.0.0.3:5432", Healthy: true, NodeName: "node-c"},
			},
		},
	}

	// Even though only node-a is local, all 3 upstreams should be kept
	filtered := FilterTCPRoutesByNode(routes, "node-a")
	require.Len(t, filtered, 1)
	assert.Len(t, filtered[0].Upstreams, 3)
}
