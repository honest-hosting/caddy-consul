package caddyconsul

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

// tcpBindHost is the address all dynamic TCP listeners bind to. Matches the
// historical behavior of the admin-API path (`listen :<port>`).
const tcpBindHost = "0.0.0.0"

// defaultTCPDrain is the grace period for in-flight connections when a port is
// removed before they are force-closed.
const defaultTCPDrain = 30 * time.Second

// tcpListener is a single dynamic listener plus the bookkeeping needed to drain
// it. Created/owned exclusively by TCPListenerManager.
type tcpListener struct {
	port   int
	ln     net.Listener // pooled (caddy fakeCloseListener); Close() decrements refcount
	active int64        // atomic: in-flight handler goroutines
	conns  sync.Map     // net.Conn -> struct{}; live conns, for force-close on drain
}

// TCPListenerManager owns the set of dynamic TCP listeners and reconciles them
// against the desired port set derived from the TCPTable. Listeners are opened
// via Caddy's pooled NetworkAddress.Listen so they survive base-config reloads
// (see CADDY-CONSUL-TCP-RELOAD-FIX-PLAN.md §6, spike-verified). No Caddy admin
// API is involved, so reconciliation never triggers a config reload.
type TCPListenerManager struct {
	ctx      context.Context // satisfied by caddy.Context at provision; used for pooled Listen
	logger   *zap.Logger
	table    *TCPTable
	proxy    *tcpProxy
	metrics  *MetricsCollector // nil-safe; nil when metrics disabled
	drain    time.Duration
	reserved map[int]bool // ports we must never bind (80, 443, admin, ...)

	mu     sync.Mutex
	active map[int]*tcpListener
}

// NewTCPListenerManager creates a manager. `reserved` is a denylist of ports
// the manager will refuse to bind; if nil, {80, 443} are reserved by default.
func NewTCPListenerManager(ctx context.Context, logger *zap.Logger, table *TCPTable, proxy *tcpProxy, metrics *MetricsCollector, drain time.Duration, reserved map[int]bool) *TCPListenerManager {
	if logger == nil {
		logger = zap.NewNop()
	}
	if drain <= 0 {
		drain = defaultTCPDrain
	}
	if reserved == nil {
		reserved = map[int]bool{80: true, 443: true}
	}
	return &TCPListenerManager{
		ctx:      ctx,
		logger:   logger,
		table:    table,
		proxy:    proxy,
		metrics:  metrics,
		drain:    drain,
		reserved: reserved,
		active:   make(map[int]*tcpListener),
	}
}

// Reconcile opens listeners for newly-desired ports and closes ones no longer
// desired. Idempotent and serialized. A failure to open one port is non-fatal —
// it is logged and retried on the next reconcile; the first such error is
// returned for visibility.
//
// MUST be called synchronously from the app's Start() (with the persisted port
// set) so the pooled-listener refcount hands off from the outgoing instance
// before it stops — see §6.
func (m *TCPListenerManager) Reconcile(desiredPorts []int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	desired := make(map[int]bool, len(desiredPorts))
	for _, p := range desiredPorts {
		if m.reserved[p] {
			m.logger.Warn("tcp: refusing to bind reserved port", zap.Int("port", p))
			continue
		}
		desired[p] = true
	}

	var firstErr error
	for p := range desired {
		if _, ok := m.active[p]; ok {
			continue // already listening
		}
		l, err := m.openPort(p)
		if err != nil {
			m.logger.Error("tcp: failed to open listener (will retry next converge)",
				zap.Int("port", p), zap.Error(err))
			m.metrics.IncTCPListenError()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		m.active[p] = l
		go m.acceptLoop(l)
		m.metrics.IncTCPListenerOpen()
		m.logger.Info("tcp: listener opened", zap.Int("port", p))
	}

	for p, l := range m.active {
		if desired[p] {
			continue
		}
		delete(m.active, p)
		m.logger.Info("tcp: listener closing", zap.Int("port", p), zap.Duration("drain", m.drain))
		m.stopListener(l)
		m.metrics.IncTCPListenerClose()
	}

	// Set-gauge written only here (single writer): on a reload the incoming
	// instance's Reconcile sets the live count; the outgoing instance's Close()
	// deliberately does not touch it, avoiding a 2->0 race on the shared gauge.
	m.metrics.SetTCPListenersActive(len(m.active))
	return firstErr
}

// Close stops every listener (used on app Stop()). Listeners are closed
// synchronously (prompt refcount decrement for the reload handoff); in-flight
// connections drain in the background.
func (m *TCPListenerManager) Close() {
	m.mu.Lock()
	ls := make([]*tcpListener, 0, len(m.active))
	for p, l := range m.active {
		ls = append(ls, l)
		delete(m.active, p)
	}
	m.mu.Unlock()

	for _, l := range ls {
		m.stopListener(l)
	}
}

// ActivePorts returns the sorted set of ports currently being listened on.
func (m *TCPListenerManager) ActivePorts() []int {
	m.mu.Lock()
	ports := make([]int, 0, len(m.active))
	for p := range m.active {
		ports = append(ports, p)
	}
	m.mu.Unlock()
	sort.Ints(ports)
	return ports
}

// openPort binds a pooled listener for the given port. Caller holds m.mu.
func (m *TCPListenerManager) openPort(port int) (*tcpListener, error) {
	na, err := caddy.ParseNetworkAddress(fmt.Sprintf("tcp/%s:%d", tcpBindHost, port))
	if err != nil {
		return nil, fmt.Errorf("parse address: %w", err)
	}
	lnAny, err := na.Listen(m.ctx, 0, net.ListenConfig{})
	if err != nil {
		return nil, err
	}
	ln, ok := lnAny.(net.Listener)
	if !ok {
		if c, isCloser := lnAny.(interface{ Close() error }); isCloser {
			_ = c.Close()
		}
		return nil, fmt.Errorf("port %d: pooled listener is not a stream net.Listener (%T)", port, lnAny)
	}
	return &tcpListener{port: port, ln: ln}, nil
}

// acceptLoop accepts connections until the listener is closed, dispatching each
// to the proxy. Each in-flight handler is tracked for drain.
func (m *TCPListenerManager) acceptLoop(l *tcpListener) {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return // listener closed (port removed or shutdown)
		}
		atomic.AddInt64(&l.active, 1)
		l.conns.Store(conn, struct{}{})
		m.metrics.IncTCPConnAccepted()
		go func(c net.Conn) {
			defer atomic.AddInt64(&l.active, -1)
			defer l.conns.Delete(c)
			defer m.metrics.DecTCPConnActive()
			m.proxy.handle(l.port, c)
		}(conn)
	}
}

// stopListener closes the listener immediately (stops accepting, decrements the
// pool refcount) then drains in-flight connections in the background: wait up to
// m.drain for natural completion, then force-close any stragglers (grace-then-force).
func (m *TCPListenerManager) stopListener(l *tcpListener) {
	_ = l.ln.Close()
	go func() {
		deadline := time.Now().Add(m.drain)
		for atomic.LoadInt64(&l.active) > 0 && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if atomic.LoadInt64(&l.active) > 0 {
			forced := 0
			l.conns.Range(func(k, _ any) bool {
				if c, ok := k.(net.Conn); ok {
					_ = c.Close()
					forced++
				}
				return true
			})
			m.metrics.AddTCPForceClosed(forced)
			m.logger.Info("tcp: force-closed connections after drain grace",
				zap.Int("port", l.port), zap.Int("count", forced))
		}
	}()
}
