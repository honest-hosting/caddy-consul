package caddyconsul

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	consul "github.com/hashicorp/consul/api"
)

const (
	DefaultConsulAddr          = "127.0.0.1:8500"
	DefaultConsulScheme        = "http"
	DefaultHealthPolicy        = "passing"
	DefaultConflictPolicy      = "reject"
	DefaultServiceProxyEnable  = true
	DefaultConnectProxyEnable  = false
	DefaultDebounce            = "500ms"
	DefaultConnectAutoRegister = true
	DefaultMaxConcurrentChecks = 5
	DefaultCaddyAdminAPI       = "localhost:2019"

	// MaxServiceNameLen is the max length for a Consul service name (DNS label).
	MaxServiceNameLen = 63
)

var validServiceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)

func init() {
	httpcaddyfile.RegisterGlobalOption("consul", parseConsulGlobalOption)
}

// parseConsulGlobalOption parses the global consul option in Caddyfile.
func parseConsulGlobalOption(d *caddyfile.Dispenser, _ interface{}) (interface{}, error) {
	cr := new(ConsulRouter)
	if err := cr.UnmarshalCaddyfile(d); err != nil {
		return nil, err
	}
	return httpcaddyfile.App{
		Name:  "consul",
		Value: caddyconfig.JSON(cr, nil),
	}, nil
}

// UnmarshalCaddyfile sets up the ConsulRouter from Caddyfile tokens.
//
//	consul {
//	    address 127.0.0.1:8500
//	    token {env.CONSUL_HTTP_TOKEN}
//	    scheme https
//	    datacenter dc1
//	    tls_ca /path/to/ca.pem
//	    tls_cert /path/to/cert.pem
//	    tls_key /path/to/key.pem
//	    insecure_skip_verify false
//	    service_proxy_enable true
//	    health_policy passing
//	    conflict_policy reject
//	    connect_proxy_enable false
//	    connect_service_name my-ingress
//	    connect_auto_register true
//	    max_concurrent_checks 5
//	    debounce 500ms
//	    caddy_admin_api localhost:2019
//	    metrics /metrics/consul
//	}
func (cr *ConsulRouter) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume "consul"

	for d.NextBlock(0) {
		switch d.Val() {
		case "address":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cr.ConsulAddr = d.Val()

		case "token":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cr.ConsulToken = d.Val()

		case "scheme":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cr.ConsulScheme = d.Val()
			if cr.ConsulScheme != "http" && cr.ConsulScheme != "https" {
				return d.Errf("scheme must be 'http' or 'https', got '%s'", cr.ConsulScheme)
			}

		case "datacenter":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cr.ConsulDC = d.Val()

		case "tls_ca":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cr.ConsulTLSCA = d.Val()

		case "tls_cert":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cr.ConsulTLSCert = d.Val()

		case "tls_key":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cr.ConsulTLSKey = d.Val()

		case "insecure_skip_verify":
			if !d.NextArg() {
				return d.ArgErr()
			}
			switch d.Val() {
			case "true":
				cr.ConsulTLSSkipVerify = true
			case "false":
				cr.ConsulTLSSkipVerify = false
			default:
				return d.Errf("insecure_skip_verify must be 'true' or 'false', got '%s'", d.Val())
			}

		case "service_proxy_enable":
			if !d.NextArg() {
				return d.ArgErr()
			}
			switch d.Val() {
			case "true":
				cr.ServiceProxyEnable = boolPtr(true)
			case "false":
				cr.ServiceProxyEnable = boolPtr(false)
			default:
				return d.Errf("service_proxy_enable must be 'true' or 'false', got '%s'", d.Val())
			}

		case "health_policy":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cr.HealthPolicy = d.Val()

		case "conflict_policy":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cr.ConflictPolicy = d.Val()

		case "connect_proxy_enable":
			if !d.NextArg() {
				return d.ArgErr()
			}
			switch d.Val() {
			case "true":
				cr.ConnectProxyEnable = boolPtr(true)
			case "false":
				cr.ConnectProxyEnable = boolPtr(false)
			default:
				return d.Errf("connect_proxy_enable must be 'true' or 'false', got '%s'", d.Val())
			}

		case "connect_service_name":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cr.ConnectServiceName = d.Val()

		case "connect_auto_register":
			if !d.NextArg() {
				return d.ArgErr()
			}
			switch d.Val() {
			case "true":
				cr.ConnectAutoRegister = boolPtr(true)
			case "false":
				cr.ConnectAutoRegister = boolPtr(false)
			default:
				return d.Errf("connect_auto_register must be 'true' or 'false', got '%s'", d.Val())
			}

		case "max_concurrent_checks":
			if !d.NextArg() {
				return d.ArgErr()
			}
			val, err := strconv.Atoi(d.Val())
			if err != nil || val < 1 {
				return d.Errf("max_concurrent_checks must be a positive integer, got '%s'", d.Val())
			}
			cr.MaxConcurrentChecks = val

		case "debounce":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cr.DebounceDuration = d.Val()

		case "caddy_admin_api":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cr.CaddyAdminAPI = d.Val()

		case "metrics":
			if !d.NextArg() {
				return d.ArgErr()
			}
			cr.Metrics = d.Val()

		default:
			return d.Errf("unrecognized consul option: %s", d.Val())
		}
	}

	return nil
}

// applyDefaults fills in default values, using environment variables as fallback.
func (cr *ConsulRouter) applyDefaults() {
	if cr.ConsulAddr == "" {
		cr.ConsulAddr = envOrDefault("CONSUL_HTTP_ADDR", DefaultConsulAddr)
	}
	if cr.ConsulToken == "" {
		cr.ConsulToken = os.Getenv("CONSUL_HTTP_TOKEN")
	}
	if cr.ConsulScheme == "" {
		if os.Getenv("CONSUL_HTTP_SSL") == "true" {
			cr.ConsulScheme = "https"
		} else {
			cr.ConsulScheme = DefaultConsulScheme
		}
	}
	if cr.ConsulTLSCA == "" {
		cr.ConsulTLSCA = os.Getenv("CONSUL_CACERT")
	}
	if cr.ConsulTLSCert == "" {
		cr.ConsulTLSCert = os.Getenv("CONSUL_CLIENT_CERT")
	}
	if cr.ConsulTLSKey == "" {
		cr.ConsulTLSKey = os.Getenv("CONSUL_CLIENT_KEY")
	}
	if cr.ServiceProxyEnable == nil {
		cr.ServiceProxyEnable = boolPtr(DefaultServiceProxyEnable)
	}
	if cr.HealthPolicy == "" {
		cr.HealthPolicy = DefaultHealthPolicy
	}
	if cr.ConflictPolicy == "" {
		cr.ConflictPolicy = DefaultConflictPolicy
	}
	if cr.ConnectProxyEnable == nil {
		cr.ConnectProxyEnable = boolPtr(DefaultConnectProxyEnable)
	}
	if cr.DebounceDuration == "" {
		cr.DebounceDuration = DefaultDebounce
	}
	if cr.MaxConcurrentChecks == 0 {
		cr.MaxConcurrentChecks = DefaultMaxConcurrentChecks
	}
	if cr.ConnectServiceName == "" {
		cr.ConnectServiceName = defaultConnectServiceName()
	}
	if cr.ConnectAutoRegister == nil {
		cr.ConnectAutoRegister = boolPtr(DefaultConnectAutoRegister)
	}
	if cr.CaddyAdminAPI == "" {
		cr.CaddyAdminAPI = DefaultCaddyAdminAPI
	}
}

// validate checks that all configuration values are valid.
func (cr *ConsulRouter) validate() error {
	switch cr.HealthPolicy {
	case "passing", "warning", "any":
	default:
		return fmt.Errorf("health_policy must be 'passing', 'warning', or 'any', got '%s'", cr.HealthPolicy)
	}

	switch cr.ConflictPolicy {
	case "reject", "first-wins":
	default:
		return fmt.Errorf("conflict_policy must be 'reject' or 'first-wins', got '%s'", cr.ConflictPolicy)
	}

	if _, err := time.ParseDuration(cr.DebounceDuration); err != nil {
		return fmt.Errorf("invalid debounce duration '%s': %w", cr.DebounceDuration, err)
	}

	if cr.ConsulScheme != "http" && cr.ConsulScheme != "https" {
		return fmt.Errorf("scheme must be 'http' or 'https', got '%s'", cr.ConsulScheme)
	}

	if cr.ConsulTLSCert != "" || cr.ConsulTLSKey != "" {
		if cr.ConsulTLSCert == "" || cr.ConsulTLSKey == "" {
			return fmt.Errorf("both tls_cert and tls_key must be specified together")
		}
	}

	if len(cr.ConnectServiceName) > MaxServiceNameLen {
		return fmt.Errorf("connect_service_name '%s' exceeds max length of %d characters",
			cr.ConnectServiceName, MaxServiceNameLen)
	}
	if cr.ConnectServiceName != "" && !validServiceNameRe.MatchString(cr.ConnectServiceName) {
		return fmt.Errorf("connect_service_name '%s' contains invalid characters (must be lowercase alphanumeric and hyphens, cannot start/end with hyphen)",
			cr.ConnectServiceName)
	}

	return nil
}

// defaultConnectServiceName generates a service name from the hostname.
// Format: <hostname>-caddy-consul, truncated to MaxServiceNameLen, lowercased,
// with invalid characters replaced by hyphens.
func defaultConnectServiceName() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown"
	}

	name := strings.ToLower(hostname) + "-caddy-consul"

	// Replace invalid characters with hyphens
	var b strings.Builder
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteRune(c)
		} else {
			b.WriteRune('-')
		}
	}
	name = b.String()

	// Truncate
	if len(name) > MaxServiceNameLen {
		name = name[:MaxServiceNameLen]
	}

	// Trim leading/trailing hyphens
	name = strings.Trim(name, "-")

	if name == "" {
		name = "caddy-consul"
	}

	return name
}

// parsedHealthPolicy returns the typed HealthPolicy.
func (cr *ConsulRouter) parsedHealthPolicy() HealthPolicy {
	switch cr.HealthPolicy {
	case "warning":
		return HealthPolicyWarning
	case "any":
		return HealthPolicyAny
	default:
		return HealthPolicyPassing
	}
}

// parsedDebounceDuration returns the parsed debounce duration.
func (cr *ConsulRouter) parsedDebounceDuration() time.Duration {
	d, _ := time.ParseDuration(cr.DebounceDuration) // already validated
	return d
}

// newConsulClient creates a Consul API client from the router configuration.
func (cr *ConsulRouter) newConsulClient() (*consul.Client, error) {
	cfg := consul.DefaultConfig()
	cfg.Address = cr.ConsulAddr
	cfg.Scheme = cr.ConsulScheme
	cfg.Token = cr.ConsulToken
	cfg.Datacenter = cr.ConsulDC

	if cr.ConsulTLSCert != "" || cr.ConsulTLSCA != "" || cr.ConsulTLSSkipVerify {
		cfg.TLSConfig = consul.TLSConfig{
			CertFile:           cr.ConsulTLSCert,
			KeyFile:            cr.ConsulTLSKey,
			CAFile:             cr.ConsulTLSCA,
			InsecureSkipVerify: cr.ConsulTLSSkipVerify,
		}
	}

	return consul.NewClient(cfg)
}

// boolPtr returns a pointer to the given bool value.
func boolPtr(v bool) *bool { return &v }

// boolVal returns the value of a *bool, defaulting to false if nil.
func boolVal(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
