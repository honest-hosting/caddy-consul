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
			Host:        r.Host,
			Path:        r.Path,
			Upstreams:   healthy,
			StripPrefix: r.StripPrefix,
			ServiceName: r.ServiceName,
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

func httpRouteKey(host, path string) string {
	if host == "" {
		host = "*"
	}
	if path == "" {
		path = "/"
	}
	return fmt.Sprintf("%s%s", host, path)
}

func tcpRouteKey(port int, sni string) string {
	if sni == "" {
		return fmt.Sprintf(":%d", port)
	}
	return fmt.Sprintf(":%d/%s", port, sni)
}
