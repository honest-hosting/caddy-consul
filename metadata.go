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
//
// Each instance is processed independently — its effective metadata is built by
// merging service-level meta (svc.Meta) with instance-level meta (inst.Meta wins).
// Routes are parsed from that per-instance metadata, and the instance becomes the
// sole upstream for its own routes. Instances that produce identical route
// definitions (same host, path, protocol, port, etc.) are grouped into one route
// with multiple upstreams for load balancing.
//
// Precedence per instance: indexed (caddy-route-N-*) > non-indexed (caddy-*) > Fabio (urlprefix-).
//
// An instance is only considered if it has the serviceTag/connectTag in its Tags,
// or has caddy-* keys in its own Meta, or has urlprefix-* in its own Tags.
func ParseServiceRoutes(svc *ServiceState, serviceTag, connectTag string, logger *zap.Logger) []RouteDefinition {
	if svc == nil || len(svc.Instances) == 0 {
		return nil
	}

	// Phase 1: Extract routes from each instance independently.
	type instanceRoute struct {
		route    RouteDefinition
		upstream Upstream
	}
	var allRoutes []instanceRoute

	for _, inst := range svc.Instances {
		if !inst.Healthy || inst.Address == "" {
			continue
		}

		// Eligibility: instance must signal routing intent via tag or metadata.
		instHasTag := instanceHasTag(inst, serviceTag) || instanceHasTag(inst, connectTag)
		instHasCaddyMeta := hasAnyCaddyMeta(inst.Meta)
		instHasFabioTags := hasAnyFabioTag(inst.Tags)
		if !instHasTag && !instHasCaddyMeta && !instHasFabioTags {
			continue
		}

		// Use the instance's own metadata for route parsing. svc.Meta is NOT
		// used as a base because it is an aggregate of all instance metadata
		// built by the watcher (first-instance-wins merge), not a distinct
		// service-level configuration. Merging it would cross-contaminate
		// routing config between unrelated instances.
		effectiveMeta := inst.Meta

		upstream := Upstream{
			Address:  fmt.Sprintf("%s:%d", inst.Address, inst.Port),
			Weight:   inst.Weight,
			Healthy:  true,
			NodeName: inst.NodeName,
		}

		// Determine routing source for this instance (indexed > non-indexed > fabio).
		indexedRoutes := parseIndexedMeta(effectiveMeta)
		hasIndexed := len(indexedRoutes) > 0
		hasNonIndexed := hasNonIndexedMeta(effectiveMeta)
		fabioRoutes, skippedPortRedirects := parseFabioTags(inst.Tags)
		hasFabio := len(fabioRoutes) > 0

		if skippedPortRedirects > 0 {
			logger.Warn("skipped port-qualified redirect tags (Caddy handles HTTP→HTTPS automatically)",
				zap.String("service", svc.Name),
				zap.String("instance", inst.ID),
				zap.Int("skipped", skippedPortRedirects),
			)
		}

		if hasIndexed && hasNonIndexed {
			logger.Debug("instance has both indexed and non-indexed metadata; using indexed keys only",
				zap.String("service", svc.Name),
				zap.String("instance", inst.ID),
			)
		}

		if (hasIndexed || hasNonIndexed) && hasFabio {
			logger.Debug("instance has both caddy metadata and Fabio urlprefix tags; using metadata only",
				zap.String("service", svc.Name),
				zap.String("instance", inst.ID),
			)
		}

		var routes []RouteDefinition
		switch {
		case hasIndexed:
			routes = indexedRoutes
		case hasNonIndexed:
			rd := parseNonIndexedMeta(effectiveMeta)
			routes = []RouteDefinition{rd}
		case hasFabio:
			routes = fabioRoutes
		default:
			continue
		}

		for _, r := range routes {
			r.ServiceName = svc.Name
			r.UpstreamMode = UpstreamDirect
			allRoutes = append(allRoutes, instanceRoute{route: r, upstream: upstream})
		}
	}

	if len(allRoutes) == 0 {
		return nil
	}

	// Phase 2: Group identical routes so multiple instances serving the same
	// route definition share upstreams (load balancing).
	type routeSignature struct {
		protocol     Protocol
		host         string
		path         string
		port         int
		priority     int
		weight       int
		stripPrefix  bool
		enabled      bool
		redirectCode int
		redirectURL  string
	}
	type groupedRoute struct {
		route     RouteDefinition
		upstreams []Upstream
	}
	routeMap := make(map[routeSignature]*groupedRoute)
	var routeOrder []routeSignature

	for _, ir := range allRoutes {
		sig := routeSignature{
			protocol:     ir.route.Protocol,
			host:         ir.route.Host,
			path:         ir.route.Path,
			port:         ir.route.Port,
			priority:     ir.route.Priority,
			weight:       ir.route.Weight,
			stripPrefix:  ir.route.StripPrefix,
			enabled:      ir.route.Enabled,
			redirectCode: ir.route.RedirectCode,
			redirectURL:  ir.route.RedirectURL,
		}
		if existing, ok := routeMap[sig]; ok {
			existing.upstreams = append(existing.upstreams, ir.upstream)
		} else {
			routeMap[sig] = &groupedRoute{
				route:     ir.route,
				upstreams: []Upstream{ir.upstream},
			}
			routeOrder = append(routeOrder, sig)
		}
	}

	// Phase 3: Assemble final routes, filtering disabled.
	var enabled []RouteDefinition
	for _, sig := range routeOrder {
		gr := routeMap[sig]
		if !gr.route.Enabled {
			continue
		}
		gr.route.Upstreams = gr.upstreams
		enabled = append(enabled, gr.route)
	}

	return enabled
}

// instanceHasTag returns true if the instance's own Tags contain the given tag.
func instanceHasTag(inst ServiceInstance, tag string) bool {
	if tag == "" {
		return false
	}
	for _, t := range inst.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// hasAnyCaddyMeta returns true if the metadata map contains any caddy-* keys.
func hasAnyCaddyMeta(meta map[string]string) bool {
	for k := range meta {
		if strings.HasPrefix(k, "caddy-") {
			return true
		}
	}
	return false
}

// hasAnyFabioTag returns true if the tag list contains any urlprefix-* tags.
func hasAnyFabioTag(tags []string) bool {
	for _, tag := range tags {
		if strings.HasPrefix(tag, "urlprefix-") {
			return true
		}
	}
	return false
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
