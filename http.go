package caddyconsul

import (
	"encoding/json"
	"fmt"
)

// BuildHTTPRouteJSON builds the Caddy JSON config fragment for an HTTP route.
func BuildHTTPRouteJSON(route CompiledHTTPRoute) (json.RawMessage, error) {
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

	if route.RedirectCode > 0 && route.RedirectURL != "" {
		// Redirect route: static_response with Location header
		headers := map[string]interface{}{
			"Location": []string{route.RedirectURL},
		}
		if route.RedirectNoCache {
			headers["Cache-Control"] = []string{"no-cache, no-store, must-revalidate"}
			headers["Pragma"] = []string{"no-cache"}
			headers["Expires"] = []string{"0"}
		}
		handlers = append(handlers, map[string]interface{}{
			"handler":     "static_response",
			"status_code": fmt.Sprintf("%d", route.RedirectCode),
			"headers":     headers,
		})
	} else {
		// Proxy route: reverse_proxy with upstreams
		upstreams := make([]map[string]interface{}, 0, len(route.Upstreams))
		for _, u := range route.Upstreams {
			upstreams = append(upstreams, map[string]interface{}{
				"dial": u.Address,
			})
		}

		// Add strip-prefix rewrite if needed
		if route.StripPrefix && route.Path != "" && route.Path != "/" {
			handlers = append(handlers, map[string]interface{}{
				"handler":           "rewrite",
				"strip_path_prefix": route.Path,
			})
		}

		reverseProxy := map[string]interface{}{
			"handler":   "reverse_proxy",
			"upstreams": upstreams,
			"health_checks": map[string]interface{}{
				"passive": map[string]interface{}{
					"fail_duration": "30s",
				},
			},
		}

		handlers = append(handlers, reverseProxy)
	}

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
