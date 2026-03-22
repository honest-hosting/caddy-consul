package caddyconsul

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

var indexedKeyRegex = regexp.MustCompile(`^caddy-route-(\d+)-(.+)$`)

// ParseServiceRoutes extracts RouteDefinitions from a service's metadata and tags.
// Metadata keys take precedence over tags. If indexed keys (caddy-route-N-*) exist,
// non-indexed keys (caddy-*) are ignored.
func ParseServiceRoutes(svc *ServiceState, logger *zap.Logger) []RouteDefinition {
	if svc == nil || len(svc.Instances) == 0 {
		return nil
	}

	// Merge metadata from all instances (first instance wins for conflicts)
	meta := mergeInstanceMeta(svc)

	// Check for indexed keys first
	indexedRoutes := parseIndexedMeta(meta)
	hasIndexed := len(indexedRoutes) > 0

	// Check for non-indexed caddy-* metadata
	hasNonIndexed := hasNonIndexedMeta(meta)

	// Check for Fabio urlprefix- tags
	fabioRoutes, skippedPortRedirects := parseFabioTags(svc.Tags)
	hasFabio := len(fabioRoutes) > 0

	if skippedPortRedirects > 0 {
		logger.Warn("skipped port-qualified redirect tags (Caddy handles HTTP→HTTPS automatically)",
			zap.String("service", svc.Name),
			zap.Int("skipped", skippedPortRedirects),
		)
	}

	if hasIndexed && hasNonIndexed {
		logger.Warn("service has both indexed (caddy-route-N-*) and non-indexed (caddy-*) metadata; using indexed keys only",
			zap.String("service", svc.Name),
		)
	}

	if (hasIndexed || hasNonIndexed) && hasFabio {
		logger.Warn("service has both caddy metadata and Fabio urlprefix tags; using metadata only",
			zap.String("service", svc.Name),
		)
	}

	var routes []RouteDefinition

	switch {
	case hasIndexed:
		routes = indexedRoutes
		// Indexed routes use merged metadata — shared upstreams
		var upstreams []Upstream
		for _, inst := range svc.Instances {
			if !inst.Healthy || inst.Address == "" {
				continue
			}
			if !instanceHasRoutingConfig(inst, true) {
				continue
			}
			upstreams = append(upstreams, Upstream{
				Address: fmt.Sprintf("%s:%d", inst.Address, inst.Port),
				Weight:  inst.Weight,
				Healthy: true,
			})
		}
		if len(upstreams) == 0 {
			return nil
		}
		for i := range routes {
			routes[i].ServiceName = svc.Name
			routes[i].Upstreams = upstreams
			routes[i].UpstreamMode = UpstreamDirect
		}

	case hasNonIndexed:
		// Non-indexed metadata: build effective metadata per instance by merging
		// service-level meta with instance-level meta (instance overrides service).
		// Group instances by their effective routing key (host+port+protocol) to
		// handle both shared metadata (multiple upstreams) and per-instance
		// metadata (e.g., multiple minecraft servers with different ports).
		type routeKey struct {
			host     string
			path     string
			port     int
			protocol string
		}
		routeMap := make(map[routeKey]*RouteDefinition)
		var routeOrder []routeKey

		for _, inst := range svc.Instances {
			if !inst.Healthy || inst.Address == "" {
				continue
			}

			// Build effective metadata: service-level + instance-level (instance wins)
			effectiveMeta := make(map[string]string)
			for k, v := range svc.Meta {
				effectiveMeta[k] = v
			}
			for k, v := range inst.Meta {
				effectiveMeta[k] = v
			}

			if !hasNonIndexedMeta(effectiveMeta) {
				continue
			}

			rd := parseNonIndexedMeta(effectiveMeta)
			key := routeKey{
				host:     rd.Host,
				path:     rd.Path,
				port:     rd.Port,
				protocol: string(rd.Protocol),
			}

			upstream := Upstream{
				Address: fmt.Sprintf("%s:%d", inst.Address, inst.Port),
				Weight:  inst.Weight,
				Healthy: true,
			}

			if existing, ok := routeMap[key]; ok {
				// Same route key — add as additional upstream
				existing.Upstreams = append(existing.Upstreams, upstream)
			} else {
				rd.ServiceName = svc.Name
				rd.UpstreamMode = UpstreamDirect
				rd.Upstreams = []Upstream{upstream}
				routeMap[key] = &rd
				routeOrder = append(routeOrder, key)
			}
		}

		for _, key := range routeOrder {
			routes = append(routes, *routeMap[key])
		}

	case hasFabio:
		routes = fabioRoutes
		// Fabio routes: only instances with urlprefix- tags are upstreams
		var upstreams []Upstream
		for _, inst := range svc.Instances {
			if !inst.Healthy || inst.Address == "" {
				continue
			}
			if !instanceHasRoutingConfig(inst, false) {
				continue
			}
			upstreams = append(upstreams, Upstream{
				Address: fmt.Sprintf("%s:%d", inst.Address, inst.Port),
				Weight:  inst.Weight,
				Healthy: true,
			})
		}
		if len(upstreams) == 0 {
			return nil
		}
		for i := range routes {
			routes[i].ServiceName = svc.Name
			routes[i].Upstreams = upstreams
			routes[i].UpstreamMode = UpstreamDirect
		}

	default:
		return nil
	}

	// Filter out disabled routes
	var enabled []RouteDefinition
	for _, r := range routes {
		if r.Enabled {
			enabled = append(enabled, r)
		}
	}

	return enabled
}

// instanceHasRoutingConfig checks if a specific service instance has routing
// configuration. For Fabio-based routing, checks for urlprefix- tags. For
// metadata-based routing, all instances of the service are considered valid
// (metadata is merged across instances).
func instanceHasRoutingConfig(inst ServiceInstance, usesMetadata bool) bool {
	if usesMetadata {
		// Metadata-based routing: all instances are valid upstreams since
		// metadata is defined at the service level, not per-instance.
		return true
	}
	// Fabio tag-based routing: only instances with urlprefix- tags
	for _, tag := range inst.Tags {
		if strings.HasPrefix(tag, "urlprefix-") {
			return true
		}
	}
	return false
}

// mergeInstanceMeta collects metadata from all instances. Service-level meta takes
// precedence (from svc.Meta), then individual instance meta is merged.
func mergeInstanceMeta(svc *ServiceState) map[string]string {
	merged := make(map[string]string)

	// Instance-level meta (first instance wins for duplicates)
	for _, inst := range svc.Instances {
		for k, v := range inst.Meta {
			if _, exists := merged[k]; !exists {
				merged[k] = v
			}
		}
	}

	// Service-level meta overrides instance-level
	for k, v := range svc.Meta {
		merged[k] = v
	}

	return merged
}

// hasNonIndexedMeta checks if there are any non-indexed caddy-* metadata keys.
func hasNonIndexedMeta(meta map[string]string) bool {
	for k := range meta {
		if strings.HasPrefix(k, "caddy-") && !indexedKeyRegex.MatchString(k) {
			return true
		}
	}
	return false
}

// parseNonIndexedMeta parses non-indexed caddy-* metadata keys into a single RouteDefinition.
func parseNonIndexedMeta(meta map[string]string) RouteDefinition {
	rd := RouteDefinition{
		Protocol: ProtocolHTTP,
		Enabled:  true,
	}

	if v, ok := meta["caddy-protocol"]; ok {
		rd.Protocol = Protocol(v)
	}
	if v, ok := meta["caddy-host"]; ok {
		rd.Host = v
	}
	if v, ok := meta["caddy-path"]; ok {
		rd.Path = v
	}
	if v, ok := meta["caddy-port"]; ok {
		if port, err := strconv.Atoi(v); err == nil {
			rd.Port = port
		}
	}
	if v, ok := meta["caddy-priority"]; ok {
		if pri, err := strconv.Atoi(v); err == nil {
			rd.Priority = pri
		}
	}
	if v, ok := meta["caddy-weight"]; ok {
		if w, err := strconv.Atoi(v); err == nil {
			rd.Weight = w
		}
	}
	if v, ok := meta["caddy-strip-prefix"]; ok {
		rd.StripPrefix = v == "true"
	}
	if v, ok := meta["caddy-redirect-code"]; ok {
		if code, err := strconv.Atoi(v); err == nil {
			rd.RedirectCode = code
		}
	}
	if v, ok := meta["caddy-redirect-url"]; ok {
		rd.RedirectURL = v
	}
	if v, ok := meta["caddy-enabled"]; ok {
		rd.Enabled = v != "false"
	}

	return rd
}

// parseIndexedMeta parses indexed caddy-route-N-* metadata keys into multiple RouteDefinitions.
func parseIndexedMeta(meta map[string]string) []RouteDefinition {
	// Group by index
	indexed := make(map[int]map[string]string)
	for k, v := range meta {
		matches := indexedKeyRegex.FindStringSubmatch(k)
		if matches == nil {
			continue
		}
		idx, err := strconv.Atoi(matches[1])
		if err != nil {
			continue
		}
		field := matches[2]
		if indexed[idx] == nil {
			indexed[idx] = make(map[string]string)
		}
		indexed[idx][field] = v
	}

	if len(indexed) == 0 {
		return nil
	}

	// Sort by index for deterministic order
	var indices []int
	for idx := range indexed {
		indices = append(indices, idx)
	}
	sort.Ints(indices)

	var routes []RouteDefinition
	for _, idx := range indices {
		fields := indexed[idx]
		rd := RouteDefinition{
			Protocol: ProtocolHTTP,
			Enabled:  true,
		}

		if v, ok := fields["protocol"]; ok {
			rd.Protocol = Protocol(v)
		}
		if v, ok := fields["host"]; ok {
			rd.Host = v
		}
		if v, ok := fields["path"]; ok {
			rd.Path = v
		}
		if v, ok := fields["port"]; ok {
			if port, err := strconv.Atoi(v); err == nil {
				rd.Port = port
			}
		}
		if v, ok := fields["sni"]; ok {
			rd.Host = v // SNI maps to Host for TCP/TLS routes
		}
		if v, ok := fields["priority"]; ok {
			if pri, err := strconv.Atoi(v); err == nil {
				rd.Priority = pri
			}
		}
		if v, ok := fields["weight"]; ok {
			if w, err := strconv.Atoi(v); err == nil {
				rd.Weight = w
			}
		}
		if v, ok := fields["strip-prefix"]; ok {
			rd.StripPrefix = v == "true"
		}
		if v, ok := fields["redirect-code"]; ok {
			if code, err := strconv.Atoi(v); err == nil {
				rd.RedirectCode = code
			}
		}
		if v, ok := fields["redirect-url"]; ok {
			rd.RedirectURL = v
		}
		if v, ok := fields["enabled"]; ok {
			rd.Enabled = v != "false"
		}

		routes = append(routes, rd)
	}

	return routes
}

// parseFabioTags parses Fabio-compatible urlprefix- tags.
// Supported formats:
//
//	urlprefix-host.example.com/
//	urlprefix-host.example.com/path strip=/path
//	urlprefix-:5432 proto=tcp
func parseFabioTags(tags []string) (routes []RouteDefinition, skippedPortRedirects int) {
	for _, tag := range tags {
		if !strings.HasPrefix(tag, "urlprefix-") {
			continue
		}

		rd, portRedirect := parseFabioTag(tag)
		if portRedirect {
			skippedPortRedirects++
			continue
		}
		if rd != nil {
			routes = append(routes, *rd)
		}
	}

	return routes, skippedPortRedirects
}

// parseFabioTag parses a single Fabio urlprefix- tag.
// Returns the RouteDefinition and a flag indicating if this was a port-qualified
// redirect that should be skipped (Caddy handles HTTP→HTTPS automatically).
func parseFabioTag(tag string) (rd *RouteDefinition, portRedirect bool) {
	// Remove "urlprefix-" prefix
	value := strings.TrimPrefix(tag, "urlprefix-")
	if value == "" {
		return nil, false
	}

	// Split into URL part and modifiers
	parts := strings.Fields(value)
	urlPart := parts[0]

	// Parse modifiers
	modifiers := make(map[string]string)
	for _, part := range parts[1:] {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 {
			modifiers[kv[0]] = kv[1]
		}
	}

	rd = &RouteDefinition{
		Protocol: ProtocolHTTP,
		Enabled:  true,
	}

	// Parse protocol modifier
	if proto, ok := modifiers["proto"]; ok {
		switch proto {
		case "tcp":
			rd.Protocol = ProtocolTCP
		case "https":
			rd.Protocol = ProtocolHTTPS
		default:
			rd.Protocol = ProtocolHTTP
		}
	}

	// Parse strip modifier
	if _, ok := modifiers["strip"]; ok {
		rd.StripPrefix = true
	}

	// Parse redirect modifier: redirect=CODE,URL
	if redir, ok := modifiers["redirect"]; ok {
		parts := strings.SplitN(redir, ",", 2)
		if len(parts) == 2 {
			if code, err := strconv.Atoi(parts[0]); err == nil && code >= 300 && code < 400 {
				rd.RedirectCode = code
				// Replace Fabio's $path variable with Caddy's request URI placeholder
				rd.RedirectURL = strings.ReplaceAll(parts[1], "$path", "{http.request.uri}")
			}
		}
	}

	// Parse URL part based on protocol
	if rd.Protocol == ProtocolTCP {
		// TCP format: :port
		if strings.HasPrefix(urlPart, ":") {
			portStr := strings.TrimPrefix(urlPart, ":")
			port, err := strconv.Atoi(portStr)
			if err != nil || port <= 0 {
				return nil, false
			}
			rd.Port = port
		} else {
			return nil, false
		}
	} else {
		// HTTP format: host/path or host (may include :port)
		rawHost := ""
		if idx := strings.IndexByte(urlPart, '/'); idx >= 0 {
			rawHost = urlPart[:idx]
			rd.Path = urlPart[idx:]
		} else {
			rawHost = urlPart
			rd.Path = "/"
		}

		// Detect :80/:443 redirect tags — these are HTTP→HTTPS redirects
		// that Caddy handles automatically. Since consul_proxy only runs on
		// the HTTPS server, these would cause redirect loops. Drop them.
		hadStandardPort := strings.HasSuffix(rawHost, ":80") || strings.HasSuffix(rawHost, ":443")
		rd.Host = stripStandardPort(rawHost)

		if hadStandardPort && rd.RedirectCode > 0 {
			return nil, true
		}
	}

	return rd, false
}

// stripStandardPort removes :80 or :443 suffixes from a hostname.
func stripStandardPort(host string) string {
	if strings.HasSuffix(host, ":80") {
		return strings.TrimSuffix(host, ":80")
	}
	if strings.HasSuffix(host, ":443") {
		return strings.TrimSuffix(host, ":443")
	}
	return host
}
