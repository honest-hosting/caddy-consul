package caddyconsul

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouteTable_Match_ExactHost(t *testing.T) {
	rt := NewRouteTable()
	rt.Update([]CompiledHTTPRoute{
		{Host: "app.example.com", Path: "/", ServiceName: "app"},
		{Host: "api.example.com", Path: "/", ServiceName: "api"},
	})

	match := rt.Match("app.example.com", "/anything")
	require.NotNil(t, match)
	assert.Equal(t, "app", match.ServiceName)

	match = rt.Match("api.example.com", "/")
	require.NotNil(t, match)
	assert.Equal(t, "api", match.ServiceName)
}

func TestRouteTable_Match_WildcardHost(t *testing.T) {
	rt := NewRouteTable()
	rt.Update([]CompiledHTTPRoute{
		{Host: "*.example.com", Path: "/", ServiceName: "wildcard"},
	})

	match := rt.Match("sub.example.com", "/")
	require.NotNil(t, match)
	assert.Equal(t, "wildcard", match.ServiceName)

	// Wildcard should NOT match the bare domain
	match = rt.Match("example.com", "/")
	assert.Nil(t, match)
}

func TestRouteTable_Match_LongestPathWins(t *testing.T) {
	rt := NewRouteTable()
	rt.Update([]CompiledHTTPRoute{
		{Host: "app.example.com", Path: "/", ServiceName: "root"},
		{Host: "app.example.com", Path: "/api", ServiceName: "api"},
		{Host: "app.example.com", Path: "/api/v2", ServiceName: "api-v2"},
	})

	match := rt.Match("app.example.com", "/api/v2/users")
	require.NotNil(t, match)
	assert.Equal(t, "api-v2", match.ServiceName)

	match = rt.Match("app.example.com", "/api/v1/users")
	require.NotNil(t, match)
	assert.Equal(t, "api", match.ServiceName)

	match = rt.Match("app.example.com", "/other")
	require.NotNil(t, match)
	assert.Equal(t, "root", match.ServiceName)
}

func TestRouteTable_Match_EmptyHost_MatchesAll(t *testing.T) {
	rt := NewRouteTable()
	rt.Update([]CompiledHTTPRoute{
		{Host: "", Path: "/", ServiceName: "catch-all"},
	})

	match := rt.Match("anything.com", "/")
	require.NotNil(t, match)
	assert.Equal(t, "catch-all", match.ServiceName)
}

func TestRouteTable_Match_NoMatch(t *testing.T) {
	rt := NewRouteTable()
	rt.Update([]CompiledHTTPRoute{
		{Host: "app.example.com", Path: "/", ServiceName: "app"},
	})

	match := rt.Match("other.com", "/")
	assert.Nil(t, match)
}

func TestRouteTable_Match_StripsPort(t *testing.T) {
	rt := NewRouteTable()
	rt.Update([]CompiledHTTPRoute{
		{Host: "app.example.com", Path: "/", ServiceName: "app"},
	})

	match := rt.Match("app.example.com:443", "/")
	require.NotNil(t, match)
	assert.Equal(t, "app", match.ServiceName)
}

func TestRouteTable_Match_CaseInsensitive(t *testing.T) {
	rt := NewRouteTable()
	rt.Update([]CompiledHTTPRoute{
		{Host: "App.Example.COM", Path: "/", ServiceName: "app"},
	})

	match := rt.Match("app.example.com", "/")
	require.NotNil(t, match)
	assert.Equal(t, "app", match.ServiceName)
}

func TestRouteTable_Update_Atomic(t *testing.T) {
	rt := NewRouteTable()

	rt.Update([]CompiledHTTPRoute{
		{Host: "old.com", Path: "/", ServiceName: "old"},
	})
	assert.Equal(t, 1, rt.Len())

	rt.Update([]CompiledHTTPRoute{
		{Host: "new.com", Path: "/", ServiceName: "new"},
	})
	assert.Equal(t, 1, rt.Len())

	match := rt.Match("old.com", "/")
	assert.Nil(t, match)

	match = rt.Match("new.com", "/")
	require.NotNil(t, match)
	assert.Equal(t, "new", match.ServiceName)
}

func TestRouteTable_Routes_Snapshot(t *testing.T) {
	rt := NewRouteTable()
	rt.Update([]CompiledHTTPRoute{
		{Host: "a.com", ServiceName: "a"},
		{Host: "b.com", ServiceName: "b"},
	})

	snapshot := rt.Routes()
	assert.Len(t, snapshot, 2)

	// Modifying snapshot should not affect the table
	snapshot[0].Host = "modified"
	match := rt.Match("a.com", "/")
	require.NotNil(t, match)
	assert.Equal(t, "a", match.ServiceName)
}

func TestMatchHost(t *testing.T) {
	tests := []struct {
		pattern string
		host    string
		want    bool
	}{
		{"", "anything.com", true},
		{"app.com", "app.com", true},
		{"app.com", "other.com", false},
		{"*.example.com", "sub.example.com", true},
		{"*.example.com", "deep.sub.example.com", true},
		{"*.example.com", "example.com", false},
		{"APP.COM", "app.com", true},
	}

	for _, tt := range tests {
		got := matchHost(tt.pattern, tt.host)
		assert.Equal(t, tt.want, got, "matchHost(%q, %q)", tt.pattern, tt.host)
	}
}
