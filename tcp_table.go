package caddyconsul

import (
	"strings"
	"sync"
)

// TCPTable is a thread-safe in-memory TCP route table shared between the consul
// app (writer, via onServicesChanged) and the per-port accept loops (readers).
// It mirrors RouteTable (the HTTP path): updates replace the whole map under a
// write lock, so readers holding a *tcpPortRoutes pointer always see a
// consistent snapshot and never race with a concurrent Update.
//
// Routes are keyed by listener port. A port carries >1 route only when it is
// SNI-discriminated (TLS passthrough); plain TCP ports carry exactly one.
type TCPTable struct {
	mu     sync.RWMutex
	byPort map[int]*tcpPortRoutes
}

// tcpPortRoutes holds the routes for a single listener port plus a precomputed
// flag for whether any of them require SNI inspection. Instances are immutable
// once published by Update (Update always builds fresh ones), so reader goroutines
// may hold a pointer without locking.
type tcpPortRoutes struct {
	port   int
	routes []CompiledTCPRoute
	anySNI bool // true if any route has a non-empty SNI (=> peek ClientHello)
}

// NewTCPTable returns an empty table.
func NewTCPTable() *TCPTable {
	return &TCPTable{byPort: make(map[int]*tcpPortRoutes)}
}

// Update atomically replaces the entire route set, grouping by port and
// precomputing anySNI per port.
func (t *TCPTable) Update(routes []CompiledTCPRoute) {
	byPort := make(map[int]*tcpPortRoutes)
	for _, r := range routes {
		pr := byPort[r.Port]
		if pr == nil {
			pr = &tcpPortRoutes{port: r.Port}
			byPort[r.Port] = pr
		}
		pr.routes = append(pr.routes, r)
		if r.SNI != "" {
			pr.anySNI = true
		}
	}

	t.mu.Lock()
	t.byPort = byPort
	t.mu.Unlock()
}

// Lookup returns the routes for a port (read snapshot). The returned pointer is
// safe to hold without locking; it is never mutated after publication.
func (t *TCPTable) Lookup(port int) (*tcpPortRoutes, bool) {
	t.mu.RLock()
	pr, ok := t.byPort[port]
	t.mu.RUnlock()
	return pr, ok
}

// Ports returns the set of listener ports currently desired — the input to the
// listener manager's reconcile.
func (t *TCPTable) Ports() []int {
	t.mu.RLock()
	ports := make([]int, 0, len(t.byPort))
	for p := range t.byPort {
		ports = append(ports, p)
	}
	t.mu.RUnlock()
	return ports
}

// Len returns the number of active listener ports.
func (t *TCPTable) Len() int {
	t.mu.RLock()
	n := len(t.byPort)
	t.mu.RUnlock()
	return n
}

// Snapshot returns a flattened copy of all routes (for the admin debug endpoint).
func (t *TCPTable) Snapshot() []CompiledTCPRoute {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []CompiledTCPRoute
	for _, pr := range t.byPort {
		out = append(out, pr.routes...)
	}
	return out
}

// match selects the route for a connection on this port given the (possibly
// empty) SNI peeked from the ClientHello. Precedence: exact SNI > wildcard SNI
// > empty-SNI default route. For a plain-TCP port (anySNI false) the single
// route is returned regardless of sni. Returns nil if nothing matches.
func (p *tcpPortRoutes) match(sni string) *CompiledTCPRoute {
	if !p.anySNI {
		if len(p.routes) > 0 {
			return &p.routes[0]
		}
		return nil
	}

	sni = strings.ToLower(sni)
	var wildcard, dflt *CompiledTCPRoute
	for i := range p.routes {
		r := &p.routes[i]
		switch {
		case r.SNI == "":
			if dflt == nil {
				dflt = r
			}
		case strings.ToLower(r.SNI) == sni:
			return r // exact match wins outright
		case strings.HasPrefix(r.SNI, "*.") && matchHost(r.SNI, sni):
			if wildcard == nil {
				wildcard = r
			}
		}
	}
	if wildcard != nil {
		return wildcard
	}
	return dflt // may be nil
}
