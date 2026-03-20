package integration_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Protocol tests ---

func TestIntegration_HTTP1_PlainText(t *testing.T) {
	client := plainHTTPClient()
	resp, err := waitForEndpoint(client, "http://caddy.localdev/", 10*time.Second)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body := readBody(resp)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, body, "caddy-consul integration test")
	assert.Equal(t, 1, resp.ProtoMajor)
}

func TestIntegration_HTTP1_OverTLS(t *testing.T) {
	client := http11Client(testDomain)
	resp, err := waitForEndpoint(client, fmt.Sprintf("https://%s/", testDomain), 10*time.Second)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body := readBody(resp)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, body, "caddy-consul integration test")
	assert.Equal(t, 1, resp.ProtoMajor)
	assert.Equal(t, 1, resp.ProtoMinor)
}

func TestIntegration_HTTP2(t *testing.T) {
	client := http2Client(testDomain)
	resp, err := waitForEndpoint(client, fmt.Sprintf("https://%s/", testDomain), 10*time.Second)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body := readBody(resp)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, body, "caddy-consul integration test")
	assert.Equal(t, 2, resp.ProtoMajor, "expected HTTP/2, got %s", resp.Proto)
}

func TestIntegration_HTTP3(t *testing.T) {
	client := http3Client(testDomain)
	resp, err := waitForEndpoint(client, fmt.Sprintf("https://%s/", testDomain), 15*time.Second)
	require.NoError(t, err, "HTTP/3 request failed — is UDP port 8443 reachable?")
	defer func() { _ = resp.Body.Close() }()

	body := readBody(resp)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, body, "caddy-consul integration test")
	assert.Equal(t, 3, resp.ProtoMajor, "expected HTTP/3, got %s", resp.Proto)
}

// --- Admin API ---

func TestIntegration_CaddyAdminAPI(t *testing.T) {
	config, err := getCaddyConfig()
	require.NoError(t, err)
	assert.NotNil(t, config)
}

// --- Consul service registration ---

func TestIntegration_RegisterHTTPService(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerService(client, "echo-web", "echo-http", 8080,
		[]string{"urlprefix-echo.localdev/"},
		nil,
	)
	require.NoError(t, err)
	defer func() { _ = deregisterService(client, "echo-web") }()

	time.Sleep(3 * time.Second)

	config, err := getCaddyConfig()
	require.NoError(t, err)
	assert.NotNil(t, config)
}

func TestIntegration_RegisterAndDeregisterService(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerService(client, "temp-svc", "echo-http", 8080,
		nil,
		map[string]string{
			"caddy-host":     "temp.localdev",
			"caddy-protocol": "http",
		},
	)
	require.NoError(t, err)

	time.Sleep(3 * time.Second)

	err = deregisterService(client, "temp-svc")
	require.NoError(t, err)

	time.Sleep(3 * time.Second)

	config, err := getCaddyConfig()
	require.NoError(t, err)
	assert.NotNil(t, config)
}

func TestIntegration_MetadataBasedRouting(t *testing.T) {
	client, err := newConsulClient()
	require.NoError(t, err)

	err = registerService(client, "meta-svc", "echo-http", 8080,
		nil,
		map[string]string{
			"caddy-protocol":     "http",
			"caddy-host":         "meta.localdev",
			"caddy-path":         "/api",
			"caddy-strip-prefix": "true",
		},
	)
	require.NoError(t, err)
	defer func() { _ = deregisterService(client, "meta-svc") }()

	time.Sleep(3 * time.Second)

	config, err := getCaddyConfig()
	require.NoError(t, err)
	assert.NotNil(t, config)
}
