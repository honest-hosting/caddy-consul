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

func TestBuildHTTPRouteJSON_NoTransport(t *testing.T) {
	route := CompiledHTTPRoute{
		Host:        "plain.example.com",
		ServiceName: "web-plain",
		Upstreams:   []Upstream{{Address: "10.0.0.1:8080"}},
		// No TLS fields
	}

	data, err := BuildHTTPRouteJSON(route)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result))

	handler := result["handle"].([]interface{})[0].(map[string]interface{})
	_, hasTransport := handler["transport"]
	assert.False(t, hasTransport, "non-connect route should not have a transport field")
}

func TestBuildHTTPRouteJSON_Redirect(t *testing.T) {
	route := CompiledHTTPRoute{
		Host:         "old.example.com",
		Path:         "/",
		ServiceName:  "redirect-svc",
		RedirectCode: 301,
		RedirectURL:  "https://new.example.com{http.request.uri}",
	}

	data, err := BuildHTTPRouteJSON(route)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result))

	// Should have a static_response handler, not reverse_proxy
	handlers := result["handle"].([]interface{})
	require.Len(t, handlers, 1)
	handler := handlers[0].(map[string]interface{})
	assert.Equal(t, "static_response", handler["handler"])
	assert.Equal(t, "301", handler["status_code"])

	// Check Location header
	headers := handler["headers"].(map[string]interface{})
	location := headers["Location"].([]interface{})
	require.Len(t, location, 1)
	assert.Equal(t, "https://new.example.com{http.request.uri}", location[0])
}

func TestBuildHTTPRouteJSON_RedirectNoCache(t *testing.T) {
	route := CompiledHTTPRoute{
		Host:            "old.example.com",
		Path:            "/",
		ServiceName:     "redirect-svc",
		RedirectCode:    301,
		RedirectURL:     "https://new.example.com{http.request.uri}",
		RedirectNoCache: true,
	}

	data, err := BuildHTTPRouteJSON(route)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result))

	handler := result["handle"].([]interface{})[0].(map[string]interface{})
	headers := handler["headers"].(map[string]interface{})

	cc := headers["Cache-Control"].([]interface{})
	require.Len(t, cc, 1)
	assert.Equal(t, "no-cache, no-store, must-revalidate", cc[0])

	pragma := headers["Pragma"].([]interface{})
	require.Len(t, pragma, 1)
	assert.Equal(t, "no-cache", pragma[0])

	expires := headers["Expires"].([]interface{})
	require.Len(t, expires, 1)
	assert.Equal(t, "0", expires[0])
}

func TestBuildHTTPRouteJSON_RedirectNoCache_DefaultOmitsHeaders(t *testing.T) {
	route := CompiledHTTPRoute{
		Host:         "old.example.com",
		Path:         "/",
		ServiceName:  "redirect-svc",
		RedirectCode: 301,
		RedirectURL:  "https://new.example.com{http.request.uri}",
	}

	data, err := BuildHTTPRouteJSON(route)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result))

	handler := result["handle"].([]interface{})[0].(map[string]interface{})
	headers := handler["headers"].(map[string]interface{})

	_, hasCC := headers["Cache-Control"]
	assert.False(t, hasCC)
	_, hasPragma := headers["Pragma"]
	assert.False(t, hasPragma)
	_, hasExpires := headers["Expires"]
	assert.False(t, hasExpires)
}

func TestBuildHTTPRouteJSON_RedirectNoUpstreams(t *testing.T) {
	// Redirect routes should work even with zero upstreams
	route := CompiledHTTPRoute{
		Host:         "redir.example.com",
		ServiceName:  "redir",
		RedirectCode: 302,
		RedirectURL:  "https://target.example.com/",
	}

	data, err := BuildHTTPRouteJSON(route)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result))

	handler := result["handle"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, "static_response", handler["handler"])
	assert.Equal(t, "302", handler["status_code"])
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
