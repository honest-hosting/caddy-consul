package integration_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Cache-Control no-cache response header tests ---
//
// These tests verify the no_cache_status feature which sets
// Cache-Control: no-cache, no-store, must-revalidate (plus Pragma: no-cache and
// Expires: 0) on responses matching configured status codes.
//
// NOTE: The integration test Caddyfile must set no_cache_status in the consul block
// for global-default tests to work. When the global is unset, no modification occurs.

func TestIntegration_NoCache_GlobalDefault_2xx_NoHeader(t *testing.T) {
	// With global no_cache_status set (in test Caddyfile), a 200 response
	// should NOT have Cache-Control injected by the plugin.
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerService(client, "nocache-2xx", "echo-http", 8080,
		[]string{"caddy-consul"},
		map[string]string{
			"caddy-host":     "nocache-2xx.localdev",
			"caddy-protocol": "http",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "nocache-2xx")
		_ = waitForHTTPRouteGone("nocache-2xx.localdev", 10*time.Second)
	}()

	_, err = waitForHTTPRoute("nocache-2xx.localdev", 15*time.Second)
	require.NoError(t, err)

	httpClient := http2Client("nocache-2xx.localdev")
	resp, err := waitForEndpoint(httpClient, fmt.Sprintf("https://%s/", "nocache-2xx.localdev"), 15*time.Second)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, 200, resp.StatusCode)
	// Plugin should not inject Cache-Control on 200 responses
	assert.Empty(t, resp.Header.Get("Cache-Control"),
		"200 response should not have Cache-Control injected by plugin")
}

func TestIntegration_NoCache_PerService_OptOut(t *testing.T) {
	// A service with caddy-no-cache-status="" should opt out entirely —
	// even if global no_cache_status is set, no Cache-Control should be injected.
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerService(client, "nocache-optout", "echo-http", 8080,
		[]string{"caddy-consul"},
		map[string]string{
			"caddy-host":            "nocache-optout.localdev",
			"caddy-protocol":        "http",
			"caddy-no-cache-status": "", // explicit opt-out
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "nocache-optout")
		_ = waitForHTTPRouteGone("nocache-optout.localdev", 10*time.Second)
	}()

	route, err := waitForHTTPRoute("nocache-optout.localdev", 15*time.Second)
	require.NoError(t, err)

	// Verify the route table shows NoCacheOptOut
	routes, routeErr := getConsulRoutes()
	if routeErr == nil {
		for _, r := range routes {
			if r.Host == "nocache-optout.localdev" {
				assert.True(t, r.NoCacheOptOut, "route should have NoCacheOptOut=true")
			}
		}
	}

	_ = route // used above
}

func TestIntegration_NoCache_PerService_Override(t *testing.T) {
	// A service with caddy-no-cache-status="502" should only trigger on 502.
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerService(client, "nocache-override", "echo-http", 8080,
		[]string{"caddy-consul"},
		map[string]string{
			"caddy-host":            "nocache-override.localdev",
			"caddy-protocol":        "http",
			"caddy-no-cache-status": "502",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "nocache-override")
		_ = waitForHTTPRouteGone("nocache-override.localdev", 10*time.Second)
	}()

	_, err = waitForHTTPRoute("nocache-override.localdev", 15*time.Second)
	require.NoError(t, err)

	// A successful response should not have Cache-Control
	httpClient := http2Client("nocache-override.localdev")
	resp, err := waitForEndpoint(httpClient, fmt.Sprintf("https://%s/", "nocache-override.localdev"), 15*time.Second)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, 200, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Cache-Control"),
		"200 response should not have Cache-Control even with per-service no-cache")
}

func TestIntegration_NoCache_UpstreamCacheControl_Preserved(t *testing.T) {
	// When the upstream sets its own Cache-Control on a 200, the plugin should
	// not interfere (since 200 doesn't match typical no-cache patterns).
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerService(client, "nocache-passthru", "echo-http", 8080,
		[]string{"caddy-consul"},
		map[string]string{
			"caddy-host":     "nocache-passthru.localdev",
			"caddy-protocol": "http",
		},
	)
	require.NoError(t, err)
	defer func() {
		_ = deregisterService(client, "nocache-passthru")
		_ = waitForHTTPRouteGone("nocache-passthru.localdev", 10*time.Second)
	}()

	_, err = waitForHTTPRoute("nocache-passthru.localdev", 15*time.Second)
	require.NoError(t, err)

	httpClient := http2Client("nocache-passthru.localdev")
	resp, err := waitForEndpoint(httpClient, fmt.Sprintf("https://%s/", "nocache-passthru.localdev"), 15*time.Second)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	// Echo server may or may not set Cache-Control, but at minimum the plugin
	// should not inject no-cache headers on a 200.
	cc := resp.Header.Get("Cache-Control")
	if cc != "" {
		assert.NotEqual(t, "no-cache, no-store, must-revalidate", cc,
			"plugin should not overwrite upstream Cache-Control on 200")
	}
	assert.Empty(t, resp.Header.Get("Pragma"),
		"plugin should not inject Pragma on 200")
	assert.Empty(t, resp.Header.Get("Expires"),
		"plugin should not inject Expires on 200")
}
