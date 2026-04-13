package caddyconsul

import (
	"fmt"
	"sort"

	"go.uber.org/zap"
)

// RouteCompiler converts RouteDefinitions into compiled Caddy config structures.
type RouteCompiler struct {
	logger *zap.Logger
}

// NewRouteCompiler creates a new RouteCompiler.
func NewRouteCompiler(logger *zap.Logger) *RouteCompiler {
	return &RouteCompiler{logger: logger}
}

// Compile takes a set of RouteDefinitions and produces a CompiledConfig.
// It groups routes by protocol, detects conflicts, and applies resolution rules.
func (rc *RouteCompiler) Compile(routes []RouteDefinition) *CompiledConfig {
	result := &CompiledConfig{}

	// Sort routes for deterministic conflict resolution (by service name)
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Priority != routes[j].Priority {
			return routes[i].Priority > routes[j].Priority // higher priority first
		}
		return routes[i].ServiceName < routes[j].ServiceName // alphabetical for same priority
	})

	// Separate by protocol
	var httpRoutes, tcpRoutes []RouteDefinition
	for _, r := range routes {
		switch r.Protocol {
		case ProtocolHTTP, ProtocolHTTPS:
			httpRoutes = append(httpRoutes, r)
		case ProtocolTCP, ProtocolTLSPassthrough:
			tcpRoutes = append(tcpRoutes, r)
		default:
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("unknown protocol '%s' for service %s, skipping", r.Protocol, r.ServiceName))
		}
	}

	result.HTTPRoutes, result.Conflicts = rc.compileHTTPRoutes(httpRoutes)

	tcpCompiled, tcpConflicts := rc.compileTCPRoutes(tcpRoutes)
	result.TCPRoutes = tcpCompiled
	result.Conflicts = append(result.Conflicts, tcpConflicts...)

	return result
}

// compileHTTPRoutes groups and deduplicates HTTP routes by host+path.
func (rc *RouteCompiler) compileHTTPRoutes(routes []RouteDefinition) ([]CompiledHTTPRoute, []Conflict) {
	var compiled []CompiledHTTPRoute
	var conflicts []Conflict

	// Track host+path claims (routes are already sorted by priority/name)
	claimed := make(map[string]*RouteDefinition)

	for i := range routes {
		r := &routes[i]
		key := httpRouteKey(r.Host, r.Path)

		if winner, exists := claimed[key]; exists {
			conflicts = append(conflicts, Conflict{
				Type:   ConflictDuplicateHostPath,
				Winner: winner,
				Loser:  r,
				Reason: fmt.Sprintf("route for %s already claimed by service %s (first-seen wins)", key, winner.ServiceName),
			})
			continue
		}

		claimed[key] = r

		// Resolve per-service no-cache matcher
		noCacheMatcher, noCacheOptOut := rc.resolveNoCacheMatcher(r)

		// Redirect routes don't need upstreams
		if r.IsRedirect() {
			compiled = append(compiled, CompiledHTTPRoute{
				Host:           r.Host,
				Path:           r.Path,
				StripPrefix:    r.StripPrefix,
				ServiceName:    r.ServiceName,
				Via:            r.Via,
				RedirectCode:   r.RedirectCode,
				RedirectURL:    r.RedirectURL,
				NoCacheMatcher: noCacheMatcher,
				NoCacheOptOut:  noCacheOptOut,
			})
			continue
		}

		// Filter to healthy upstreams only
		var healthy []Upstream
		for _, u := range r.Upstreams {
			if u.Healthy {
				healthy = append(healthy, u)
			}
		}

		if len(healthy) == 0 {
			rc.logger.Debug("skipping route with no healthy upstreams",
				zap.String("service", r.ServiceName),
				zap.String("host", r.Host),
				zap.String("path", r.Path),
			)
			continue
		}

		compiled = append(compiled, CompiledHTTPRoute{
			Host:           r.Host,
			Path:           r.Path,
			Upstreams:      healthy,
			StripPrefix:    r.StripPrefix,
			ServiceName:    r.ServiceName,
			Via:            r.Via,
			NoCacheMatcher: noCacheMatcher,
			NoCacheOptOut:  noCacheOptOut,
		})
	}

	return compiled, conflicts
}

// compileTCPRoutes groups and deduplicates TCP routes by port+SNI.
func (rc *RouteCompiler) compileTCPRoutes(routes []RouteDefinition) ([]CompiledTCPRoute, []Conflict) {
	var compiled []CompiledTCPRoute
	var conflicts []Conflict

	// Track port+SNI claims
	claimed := make(map[string]*RouteDefinition)

	for i := range routes {
		r := &routes[i]
		key := tcpRouteKey(r.Port, r.Host)

		if winner, exists := claimed[key]; exists {
			conflicts = append(conflicts, Conflict{
				Type:   ConflictDuplicatePortSNI,
				Winner: winner,
				Loser:  r,
				Reason: fmt.Sprintf("TCP route for %s already claimed by service %s (first-seen wins)", key, winner.ServiceName),
			})
			continue
		}

		claimed[key] = r

		var healthy []Upstream
		for _, u := range r.Upstreams {
			if u.Healthy {
				healthy = append(healthy, u)
			}
		}

		if len(healthy) == 0 {
			rc.logger.Debug("skipping TCP route with no healthy upstreams",
				zap.String("service", r.ServiceName),
				zap.Int("port", r.Port),
			)
			continue
		}

		compiled = append(compiled, CompiledTCPRoute{
			Port:        r.Port,
			SNI:         r.Host,
			Upstreams:   healthy,
			Passthrough: r.Protocol == ProtocolTLSPassthrough,
			ServiceName: r.ServiceName,
		})
	}

	return compiled, conflicts
}

// resolveNoCacheMatcher parses the per-service no-cache-status metadata.
// Returns (matcher, optOut):
//   - metadata absent: (nil, false) → handler falls through to global
//   - metadata present but empty: (nil, true) → service opted out
//   - metadata present with valid spec: (parsed matcher, false) → per-service override
//   - metadata present with invalid spec: (nil, false) → log warning, fall through to global
func (rc *RouteCompiler) resolveNoCacheMatcher(r *RouteDefinition) (*StatusMatcher, bool) {
	if !r.HasNoCacheStatus {
		return nil, false
	}
	if r.NoCacheStatusRaw == "" {
		return nil, true // explicit opt-out
	}
	matcher, err := ParseStatusMatcher(r.NoCacheStatusRaw)
	if err != nil {
		rc.logger.Warn("invalid caddy-no-cache-status, falling back to global",
			zap.String("service", r.ServiceName),
			zap.String("value", r.NoCacheStatusRaw),
			zap.Error(err),
		)
		return nil, false
	}
	return matcher, false
}

func httpRouteKey(host, path string) string {
	if host == "" {
		host = "*"
	}
	if path == "" {
		path = "/"
	}
	return fmt.Sprintf("%s%s", host, path)
}

// FilterTCPRoutesByNode returns only routes that should be materialized on the
// given node. TCP/TLS-passthrough routes are kept only if at least one upstream
// runs on nodeName. When kept, ALL upstreams are preserved (not just local ones)
// so Caddy can load-balance across the full set. HTTP/HTTPS routes pass through
// unchanged. If nodeName is empty, all routes pass through (safety fallback).
func FilterTCPRoutesByNode(routes []RouteDefinition, nodeName string) []RouteDefinition {
	if nodeName == "" {
		return routes
	}

	filtered := make([]RouteDefinition, 0, len(routes))
	for _, r := range routes {
		if r.Protocol != ProtocolTCP && r.Protocol != ProtocolTLSPassthrough {
			// Non-TCP routes are not affected by l4_mode
			filtered = append(filtered, r)
			continue
		}

		// Check if any upstream is on this node
		hasLocal := false
		for _, u := range r.Upstreams {
			if u.NodeName == nodeName {
				hasLocal = true
				break
			}
		}

		if hasLocal {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

func tcpRouteKey(port int, sni string) string {
	if sni == "" {
		return fmt.Sprintf(":%d", port)
	}
	return fmt.Sprintf(":%d/%s", port, sni)
}
