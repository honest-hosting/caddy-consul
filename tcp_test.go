package caddyconsul

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildTCPRouteJSON_Basic(t *testing.T) {
	route := CompiledTCPRoute{
		Port:        5432,
		ServiceName: "postgres",
		Upstreams: []Upstream{
			{Address: "10.0.0.1:5432"},
		},
	}

	data, err := BuildTCPRouteJSON(route)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result))

	handlers := result["handle"].([]interface{})
	require.Len(t, handlers, 1)
	handler := handlers[0].(map[string]interface{})
	assert.Equal(t, "proxy", handler["handler"])
}

func TestBuildTCPRouteJSON_WithSNI(t *testing.T) {
	route := CompiledTCPRoute{
		Port:        5432,
		SNI:         "db.example.com",
		ServiceName: "postgres",
		Upstreams: []Upstream{
			{Address: "10.0.0.1:5432"},
		},
	}

	data, err := BuildTCPRouteJSON(route)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result))

	matchers := result["match"].([]interface{})
	require.Len(t, matchers, 1)
	matcher := matchers[0].(map[string]interface{})
	tlsMatcher := matcher["tls"].(map[string]interface{})
	sniList := tlsMatcher["sni"].([]interface{})
	assert.Equal(t, "db.example.com", sniList[0])
}

func TestBuildTCPServerJSON(t *testing.T) {
	routes := []CompiledTCPRoute{
		{
			Port:        5432,
			ServiceName: "postgres",
			Upstreams:   []Upstream{{Address: "10.0.0.1:5432"}},
		},
	}

	data, err := BuildTCPServerJSON(5432, routes)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result))

	listen := result["listen"].([]interface{})
	assert.Equal(t, ":5432", listen[0])

	serverRoutes := result["routes"].([]interface{})
	assert.Len(t, serverRoutes, 1)
}

func TestGroupTCPRoutesByPort(t *testing.T) {
	routes := []CompiledTCPRoute{
		{Port: 5432, ServiceName: "pg1"},
		{Port: 5432, ServiceName: "pg2", SNI: "pg2.example.com"},
		{Port: 3306, ServiceName: "mysql"},
	}

	grouped := GroupTCPRoutesByPort(routes)
	assert.Len(t, grouped, 2)
	assert.Len(t, grouped[5432], 2)
	assert.Len(t, grouped[3306], 1)
}
