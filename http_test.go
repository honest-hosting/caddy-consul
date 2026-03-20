package caddyconsul

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildHTTPRouteJSON_Basic(t *testing.T) {
	route := CompiledHTTPRoute{
		Host:        "app.example.com",
		Path:        "/api",
		ServiceName: "web",
		Upstreams: []Upstream{
			{Address: "10.0.0.1:8080"},
			{Address: "10.0.0.2:8080"},
		},
	}

	data, err := BuildHTTPRouteJSON(route)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result))

	// Check matchers
	matchList := result["match"].([]interface{})
	require.Len(t, matchList, 1)
	match := matchList[0].(map[string]interface{})
	hosts := match["host"].([]interface{})
	assert.Equal(t, "app.example.com", hosts[0])
	paths := match["path"].([]interface{})
	assert.Equal(t, "/api*", paths[0])

	// Check handlers
	handlers := result["handle"].([]interface{})
	require.Len(t, handlers, 1) // no strip-prefix
	handler := handlers[0].(map[string]interface{})
	assert.Equal(t, "reverse_proxy", handler["handler"])

	upstreams := handler["upstreams"].([]interface{})
	assert.Len(t, upstreams, 2)
}

func TestBuildHTTPRouteJSON_StripPrefix(t *testing.T) {
	route := CompiledHTTPRoute{
		Host:        "app.example.com",
		Path:        "/api",
		StripPrefix: true,
		ServiceName: "web",
		Upstreams: []Upstream{
			{Address: "10.0.0.1:8080"},
		},
	}

	data, err := BuildHTTPRouteJSON(route)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result))

	handlers := result["handle"].([]interface{})
	require.Len(t, handlers, 2) // rewrite + reverse_proxy

	rewrite := handlers[0].(map[string]interface{})
	assert.Equal(t, "rewrite", rewrite["handler"])
	assert.Equal(t, "/api", rewrite["strip_path_prefix"])
}

func TestBuildHTTPRouteJSON_NoHost(t *testing.T) {
	route := CompiledHTTPRoute{
		Path:        "/",
		ServiceName: "web",
		Upstreams: []Upstream{
			{Address: "10.0.0.1:8080"},
		},
	}

	data, err := BuildHTTPRouteJSON(route)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result))

	// No matchers when path is just /
	_, hasMatch := result["match"]
	assert.False(t, hasMatch)
}

func TestBuildHTTPRoutesJSON_Multiple(t *testing.T) {
	routes := []CompiledHTTPRoute{
		{
			Host:        "a.example.com",
			ServiceName: "a",
			Upstreams:   []Upstream{{Address: "10.0.0.1:8080"}},
		},
		{
			Host:        "b.example.com",
			ServiceName: "b",
			Upstreams:   []Upstream{{Address: "10.0.0.2:8080"}},
		},
	}

	results, err := BuildHTTPRoutesJSON(routes)
	require.NoError(t, err)
	assert.Len(t, results, 2)
}
