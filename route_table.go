package caddyconsul

import (
	"strings"
	"sync"
)

// RouteTable is a thread-safe in-memory HTTP route table shared between the
// consul app (writer) and the consul_proxy handler (reader). It is updated
// whenever the watcher detects service changes and the compiler produces new
// compiled routes. The handler reads from it on every HTTP request.
type RouteTable struct {
	mu     sync.RWMutex
	routes []CompiledHTTPRoute
}

// NewRouteTable creates an empty RouteTable.
func NewRouteTable() *RouteTable {
	return &RouteTable{}
}

// Update replaces the entire route set atomically. Routes should already be
// sorted by priority (handled by the compiler).
func (rt *RouteTable) Update(routes []CompiledHTTPRoute) {
	rt.mu.Lock()
	rt.routes = routes
	rt.mu.Unlock()
}

// Match finds the best matching route for the given host and path.
// Returns nil if no route matches.
//
// Matching rules:
//  1. Exact host match takes priority over wildcard
//  2. Longest path prefix wins among same-host matches
//  3. Routes are pre-sorted by priority from the compiler
//  4. Port-aware: if a route pattern includes a port, the request must match
//     on host:port; if the pattern has no port, only the hostname is compared
func (rt *RouteTable) Match(host, path string) *CompiledHTTPRoute {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	// Normalize host to lowercase, preserving port
	host = strings.ToLower(host)

	// Split host and port for routes that don't specify a port
	hostOnly := host
	if idx := strings.LastIndex(host, ":"); idx > 0 {
		hostOnly = host[:idx]
	}

	if path == "" {
		path = "/"
	}

	var bestMatch *CompiledHTTPRoute
	bestPathLen := -1
	bestHasPort := false // port-specific routes beat portless routes

	for i := range rt.routes {
		r := &rt.routes[i]

		// Check host match: if the route pattern contains a port, match
		// against the full host:port; otherwise match hostname only.
		hasPort := routeHasPort(r.Host)
		if hasPort {
			if !matchHost(r.Host, host) {
				continue
			}
		} else {
			if !matchHost(r.Host, hostOnly) {
				continue
			}
		}

		// Check path match (prefix matching)
		routePath := r.Path
		pathLen := 0
		if routePath == "" || routePath == "/" {
			// Root path matches everything
			pathLen = 0
		} else if strings.HasPrefix(path, routePath) {
			pathLen = len(routePath)
		} else {
			continue
		}

		// Prefer: (1) port-specific over portless, (2) longest path
		if bestMatch == nil ||
			(hasPort && !bestHasPort) ||
			(hasPort == bestHasPort && pathLen > bestPathLen) {
			bestMatch = r
			bestPathLen = pathLen
			bestHasPort = hasPort
		}
	}

	return bestMatch
}

// Routes returns a snapshot of the current routes (for debugging/metrics).
func (rt *RouteTable) Routes() []CompiledHTTPRoute {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	snapshot := make([]CompiledHTTPRoute, len(rt.routes))
	copy(snapshot, rt.routes)
	return snapshot
}

// Len returns the number of routes in the table.
func (rt *RouteTable) Len() int {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return len(rt.routes)
}

// routeHasPort returns true if the host pattern contains a port suffix (e.g. ":8443").
// Wildcard patterns like "*.example.com:8443" are also detected.
func routeHasPort(pattern string) bool {
	if pattern == "" {
		return false
	}
	// A port is present if the last ":" is followed only by digits
	idx := strings.LastIndex(pattern, ":")
	if idx < 0 || idx == len(pattern)-1 {
		return false
	}
	for _, c := range pattern[idx+1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// matchHost checks if a route's host pattern matches the request host.
// Supports exact match and wildcard (*.example.com).
func matchHost(pattern, host string) bool {
	if pattern == "" {
		return true // empty pattern matches all hosts
	}
	pattern = strings.ToLower(pattern)

	if strings.HasPrefix(pattern, "*.") {
		// Wildcard: *.example.com matches sub.example.com but not example.com
		suffix := pattern[1:] // ".example.com"
		return strings.HasSuffix(host, suffix) && host != suffix[1:]
	}

	return pattern == host
}
