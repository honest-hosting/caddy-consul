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
func (rt *RouteTable) Match(host, path string) *CompiledHTTPRoute {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	// Normalize: strip port from host if present (e.g., "example.com:443")
	if idx := strings.LastIndex(host, ":"); idx > 0 {
		host = host[:idx]
	}
	host = strings.ToLower(host)

	if path == "" {
		path = "/"
	}

	var bestMatch *CompiledHTTPRoute
	bestPathLen := -1

	for i := range rt.routes {
		r := &rt.routes[i]

		// Check host match
		if !matchHost(r.Host, host) {
			continue
		}

		// Check path match (prefix matching)
		routePath := r.Path
		if routePath == "" || routePath == "/" {
			// Root path matches everything
			if bestMatch == nil || bestPathLen < 0 {
				bestMatch = r
				bestPathLen = 0
			}
			continue
		}

		if strings.HasPrefix(path, routePath) {
			if len(routePath) > bestPathLen {
				bestMatch = r
				bestPathLen = len(routePath)
			}
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
