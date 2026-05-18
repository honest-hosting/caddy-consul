package caddyconsul

import (
	"encoding/json"
	"fmt"
)

// BuildTCPRouteJSON builds the Caddy L4 JSON config fragment for a TCP route.
func BuildTCPRouteJSON(route CompiledTCPRoute) (json.RawMessage, error) {
	// Build upstreams
	upstreams := make([]map[string]interface{}, 0, len(route.Upstreams))
	for _, u := range route.Upstreams {
		upstreams = append(upstreams, map[string]interface{}{
			"dial": []string{u.Address},
		})
	}

	// Build matchers for SNI if present
	var matchers []map[string]interface{}
	if route.SNI != "" {
		matchers = []map[string]interface{}{
			{
				"tls": map[string]interface{}{
					"sni": []string{route.SNI},
				},
			},
		}
	}

	// Build handler
	var handler map[string]interface{}
	if route.Passthrough {
		handler = map[string]interface{}{
			"handler":   "proxy",
			"upstreams": upstreams,
		}
	} else {
		handler = map[string]interface{}{
			"handler":   "proxy",
			"upstreams": upstreams,
		}
	}

	routeObj := map[string]interface{}{
		"handle": []map[string]interface{}{handler},
	}

	if len(matchers) > 0 {
		routeObj["match"] = matchers
	}

	data, err := json.Marshal(routeObj)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal TCP route for service %s: %w", route.ServiceName, err)
	}

	return data, nil
}

// BuildTCPServerJSON builds a complete L4 server config for a given port with its routes.
func BuildTCPServerJSON(port int, routes []CompiledTCPRoute) (json.RawMessage, error) {
	var routeJSONs []json.RawMessage
	for _, r := range routes {
		data, err := BuildTCPRouteJSON(r)
		if err != nil {
			return nil, err
		}
		routeJSONs = append(routeJSONs, data)
	}

	server := map[string]interface{}{
		"listen": []string{fmt.Sprintf(":%d", port)},
		"routes": routeJSONs,
	}

	data, err := json.Marshal(server)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal TCP server for port %d: %w", port, err)
	}

	return data, nil
}

// GroupTCPRoutesByPort groups compiled TCP routes by their port number.
func GroupTCPRoutesByPort(routes []CompiledTCPRoute) map[int][]CompiledTCPRoute {
	grouped := make(map[int][]CompiledTCPRoute)
	for _, r := range routes {
		grouped[r.Port] = append(grouped[r.Port], r)
	}
	return grouped
}
