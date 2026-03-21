package caddyconsul

import (
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnmarshalCaddyfile_Minimal(t *testing.T) {
	input := `consul {
	}`
	d := caddyfile.NewTestDispenser(input)
	cr := &ConsulRouter{}
	err := cr.UnmarshalCaddyfile(d)
	require.NoError(t, err)
}

func TestUnmarshalCaddyfile_AllOptions(t *testing.T) {
	input := `consul {
		address 10.0.0.1:8500
		token my-secret-token
		scheme https
		datacenter dc1
		tls_ca /etc/consul/ca.pem
		tls_cert /etc/consul/cert.pem
		tls_key /etc/consul/key.pem
		insecure_skip_verify true
		service_proxy_enable true
		health_policy warning
		conflict_policy first-wins
		connect_proxy_enable true
		debounce 1s
		metrics /metrics/consul
	}`
	d := caddyfile.NewTestDispenser(input)
	cr := &ConsulRouter{}
	err := cr.UnmarshalCaddyfile(d)
	require.NoError(t, err)

	assert.Equal(t, "10.0.0.1:8500", cr.ConsulAddr)
	assert.Equal(t, "my-secret-token", cr.ConsulToken)
	assert.Equal(t, "https", cr.ConsulScheme)
	assert.Equal(t, "dc1", cr.ConsulDC)
	assert.Equal(t, "/etc/consul/ca.pem", cr.ConsulTLSCA)
	assert.Equal(t, "/etc/consul/cert.pem", cr.ConsulTLSCert)
	assert.Equal(t, "/etc/consul/key.pem", cr.ConsulTLSKey)
	assert.True(t, cr.ConsulTLSSkipVerify)
	assert.True(t, boolVal(cr.ServiceProxyEnable))
	assert.Equal(t, "warning", cr.HealthPolicy)
	assert.Equal(t, "first-wins", cr.ConflictPolicy)
	assert.True(t, boolVal(cr.ConnectProxyEnable))
	assert.Equal(t, "1s", cr.DebounceDuration)
	assert.Equal(t, "/metrics/consul", cr.Metrics)
}

func TestUnmarshalCaddyfile_InvalidScheme(t *testing.T) {
	input := `consul {
		scheme ftp
	}`
	d := caddyfile.NewTestDispenser(input)
	cr := &ConsulRouter{}
	err := cr.UnmarshalCaddyfile(d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scheme must be 'http' or 'https'")
}

func TestUnmarshalCaddyfile_UnrecognizedOption(t *testing.T) {
	input := `consul {
		bogus_option value
	}`
	d := caddyfile.NewTestDispenser(input)
	cr := &ConsulRouter{}
	err := cr.UnmarshalCaddyfile(d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized consul option")
}

func TestUnmarshalCaddyfile_MissingArg(t *testing.T) {
	input := `consul {
		address
	}`
	d := caddyfile.NewTestDispenser(input)
	cr := &ConsulRouter{}
	err := cr.UnmarshalCaddyfile(d)
	require.Error(t, err)
}

func TestApplyDefaults(t *testing.T) {
	cr := &ConsulRouter{}
	cr.applyDefaults()

	assert.Equal(t, DefaultConsulAddr, cr.ConsulAddr)
	assert.Equal(t, DefaultConsulScheme, cr.ConsulScheme)
	assert.Equal(t, DefaultHealthPolicy, cr.HealthPolicy)
	assert.Equal(t, DefaultConflictPolicy, cr.ConflictPolicy)
	assert.Equal(t, DefaultServiceProxyEnable, boolVal(cr.ServiceProxyEnable))
	assert.Equal(t, DefaultConnectProxyEnable, boolVal(cr.ConnectProxyEnable))
	assert.Equal(t, DefaultDebounce, cr.DebounceDuration)
}

func TestApplyDefaults_EnvVarFallback(t *testing.T) {
	t.Setenv("CONSUL_HTTP_ADDR", "consul.local:8500")
	t.Setenv("CONSUL_HTTP_TOKEN", "env-token")
	t.Setenv("CONSUL_HTTP_SSL", "true")
	t.Setenv("CONSUL_CACERT", "/env/ca.pem")
	t.Setenv("CONSUL_CLIENT_CERT", "/env/cert.pem")
	t.Setenv("CONSUL_CLIENT_KEY", "/env/key.pem")

	cr := &ConsulRouter{}
	cr.applyDefaults()

	assert.Equal(t, "consul.local:8500", cr.ConsulAddr)
	assert.Equal(t, "env-token", cr.ConsulToken)
	assert.Equal(t, "https", cr.ConsulScheme)
	assert.Equal(t, "/env/ca.pem", cr.ConsulTLSCA)
	assert.Equal(t, "/env/cert.pem", cr.ConsulTLSCert)
	assert.Equal(t, "/env/key.pem", cr.ConsulTLSKey)
}

func TestApplyDefaults_CaddyfileOverridesEnv(t *testing.T) {
	t.Setenv("CONSUL_HTTP_ADDR", "env-addr:8500")

	cr := &ConsulRouter{ConsulAddr: "caddyfile-addr:8500"}
	cr.applyDefaults()

	assert.Equal(t, "caddyfile-addr:8500", cr.ConsulAddr)
}

func TestValidate_Valid(t *testing.T) {
	cr := &ConsulRouter{}
	cr.applyDefaults()
	assert.NoError(t, cr.validate())
}

func TestValidate_InvalidHealthPolicy(t *testing.T) {
	cr := &ConsulRouter{HealthPolicy: "bogus", ConflictPolicy: "reject", DebounceDuration: "500ms", ConsulScheme: "http"}
	assert.Error(t, cr.validate())
}

func TestValidate_InvalidConflictPolicy(t *testing.T) {
	cr := &ConsulRouter{HealthPolicy: "passing", ConflictPolicy: "bogus", DebounceDuration: "500ms", ConsulScheme: "http"}
	assert.Error(t, cr.validate())
}

func TestValidate_InvalidDebounce(t *testing.T) {
	cr := &ConsulRouter{HealthPolicy: "passing", ConflictPolicy: "reject", DebounceDuration: "not-a-duration", ConsulScheme: "http"}
	assert.Error(t, cr.validate())
}

func TestValidate_TLSCertWithoutKey(t *testing.T) {
	cr := &ConsulRouter{
		HealthPolicy: "passing", ConflictPolicy: "reject",
		DebounceDuration: "500ms", ConsulScheme: "http",
		ConsulTLSCert: "/path/cert.pem",
	}
	err := cr.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tls_cert and tls_key must be specified together")
}

func TestParsedHealthPolicy(t *testing.T) {
	tests := []struct {
		input    string
		expected HealthPolicy
	}{
		{"passing", HealthPolicyPassing},
		{"warning", HealthPolicyWarning},
		{"any", HealthPolicyAny},
		{"unknown", HealthPolicyPassing},
	}

	for _, tt := range tests {
		cr := &ConsulRouter{HealthPolicy: tt.input}
		assert.Equal(t, tt.expected, cr.parsedHealthPolicy(), "input: %s", tt.input)
	}
}

func TestParsedDebounceDuration(t *testing.T) {
	cr := &ConsulRouter{DebounceDuration: "1s"}
	assert.Equal(t, 1_000_000_000, int(cr.parsedDebounceDuration()))
}
