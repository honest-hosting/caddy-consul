package caddyconsul

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// BuildHTTPRouteJSON builds the Caddy JSON config fragment for an HTTP route.
func BuildHTTPRouteJSON(route CompiledHTTPRoute) (json.RawMessage, error) {
	// Build upstreams
	upstreams := make([]map[string]interface{}, 0, len(route.Upstreams))
	for _, u := range route.Upstreams {
		upstreams = append(upstreams, map[string]interface{}{
			"dial": u.Address,
		})
	}

	// Build matchers
	match := make(map[string]interface{})
	if route.Host != "" {
		match["host"] = []string{route.Host}
	}
	if route.Path != "" && route.Path != "/" {
		match["path"] = []string{route.Path + "*"}
	}

	// Build handlers
	var handlers []map[string]interface{}

	// Add strip-prefix rewrite if needed
	if route.StripPrefix && route.Path != "" && route.Path != "/" {
		handlers = append(handlers, map[string]interface{}{
			"handler":          "rewrite",
			"strip_path_prefix": route.Path,
		})
	}

	// Add reverse proxy handler
	reverseProxy := map[string]interface{}{
		"handler":   "reverse_proxy",
		"upstreams": upstreams,
		"health_checks": map[string]interface{}{
			"passive": map[string]interface{}{
				"fail_duration": "30s",
			},
		},
	}

	// Add TLS transport for Connect direct mode
	if len(route.TLSCertPEM) > 0 && len(route.TLSKeyPEM) > 0 {
		tlsConfig := map[string]interface{}{
			"protocol": "http",
			"tls": map[string]interface{}{
				"client_certificate_pem": base64.StdEncoding.EncodeToString(route.TLSCertPEM),
				"client_certificate_key_pem": base64.StdEncoding.EncodeToString(route.TLSKeyPEM),
				"insecure_skip_verify": false,
			},
		}
		if len(route.TLSCACertPEM) > 0 {
			tlsConfig["tls"].(map[string]interface{})["root_ca_pem_files"] = []string{
				base64.StdEncoding.EncodeToString(route.TLSCACertPEM),
			}
		}
		reverseProxy["transport"] = tlsConfig
	}

	handlers = append(handlers, reverseProxy)

	// Build route object
	routeObj := map[string]interface{}{
		"handle": handlers,
	}

	if len(match) > 0 {
		routeObj["match"] = []map[string]interface{}{match}
	}

	data, err := json.Marshal(routeObj)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal HTTP route for service %s: %w", route.ServiceName, err)
	}

	return data, nil
}

// BuildHTTPRoutesJSON builds an array of Caddy JSON route configs for all HTTP routes.
func BuildHTTPRoutesJSON(routes []CompiledHTTPRoute) ([]json.RawMessage, error) {
	var result []json.RawMessage
	for _, r := range routes {
		data, err := BuildHTTPRouteJSON(r)
		if err != nil {
			return nil, err
		}
		result = append(result, data)
	}
	return result, nil
}
