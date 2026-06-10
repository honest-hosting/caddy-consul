package caddyconsul

import (
	"io"
	"math/rand"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	defaultTCPPeekTimeout = 5 * time.Second
	defaultTCPDialTimeout = 10 * time.Second
)

// tcpProxy handles a single accepted TCP connection: it matches the connection
// to a route (peeking the ClientHello SNI only when the port needs it), picks a
// healthy upstream, dials it, and proxies bytes bidirectionally. It holds no
// listeners — the listener manager owns those and calls handle() per connection.
type tcpProxy struct {
	logger      *zap.Logger
	table       *TCPTable
	metrics     *MetricsCollector // nil-safe; nil when metrics disabled
	peekTimeout time.Duration
	dialTimeout time.Duration
}

func newTCPProxy(logger *zap.Logger, table *TCPTable, metrics *MetricsCollector, peekTimeout, dialTimeout time.Duration) *tcpProxy {
	if logger == nil {
		logger = zap.NewNop()
	}
	if peekTimeout <= 0 {
		peekTimeout = defaultTCPPeekTimeout
	}
	if dialTimeout <= 0 {
		dialTimeout = defaultTCPDialTimeout
	}
	return &tcpProxy{logger: logger, table: table, metrics: metrics, peekTimeout: peekTimeout, dialTimeout: dialTimeout}
}

// handle proxies one accepted connection for the given listener port. It takes
// ownership of `down` and always closes it before returning.
func (p *tcpProxy) handle(port int, down net.Conn) {
	defer func() { _ = down.Close() }()

	pr, ok := p.table.Lookup(port)
	if !ok {
		return // port removed between accept and handle
	}

	client := down
	var sni string
	if pr.anySNI {
		s, buffered, isTLS, err := peekClientHelloSNI(down, p.peekTimeout)
		if err != nil {
			p.logger.Debug("tcp: ClientHello peek failed", zap.Int("port", port), zap.Error(err))
			return
		}
		sni = s
		if !isTLS {
			p.logger.Debug("tcp: non-TLS connection on SNI-routed port", zap.Int("port", port))
		}
		// Replay the peeked bytes to the upstream — passthrough must forward the
		// original ClientHello verbatim.
		client = newPrefixConn(down, buffered)
	}

	route := pr.match(sni)
	if route == nil {
		p.logger.Debug("tcp: no matching route", zap.Int("port", port), zap.String("sni", sni))
		p.metrics.IncTCPNoRoute()
		return
	}

	upstream := pickUpstream(route.Upstreams)
	if upstream == nil {
		p.logger.Warn("tcp: no healthy upstream",
			zap.Int("port", port), zap.String("service", route.ServiceName))
		p.metrics.IncTCPNoUpstream()
		return
	}

	up, err := net.DialTimeout("tcp", upstream.Address, p.dialTimeout)
	if err != nil {
		p.logger.Warn("tcp: upstream dial failed",
			zap.Int("port", port), zap.String("service", route.ServiceName),
			zap.String("upstream", upstream.Address), zap.Error(err))
		p.metrics.IncTCPDialError()
		return
	}

	proxyBidir(client, up)
}

// pickUpstream selects a healthy upstream weighted by Weight (default 1 when
// unset). Returns nil if none are healthy. The compiler already filters to
// healthy upstreams, but we re-check defensively.
func pickUpstream(ups []Upstream) *Upstream {
	total := 0
	healthy := make([]*Upstream, 0, len(ups))
	for i := range ups {
		if !ups[i].Healthy {
			continue
		}
		healthy = append(healthy, &ups[i])
		w := ups[i].Weight
		if w <= 0 {
			w = 1
		}
		total += w
	}
	switch {
	case len(healthy) == 0:
		return nil
	case len(healthy) == 1 || total <= 0:
		return healthy[rand.Intn(len(healthy))]
	}
	n := rand.Intn(total)
	for _, u := range healthy {
		w := u.Weight
		if w <= 0 {
			w = 1
		}
		if n < w {
			return u
		}
		n -= w
	}
	return healthy[len(healthy)-1]
}

// proxyBidir copies bytes in both directions until both sides reach EOF, using
// half-close (CloseWrite) so each peer observes the other's EOF, then closes
// both connections. Mirrors the caddy-l4 proxy copy loop.
func proxyBidir(down, up net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(up, down)
		halfCloseWrite(up)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(down, up)
		halfCloseWrite(down)
	}()
	wg.Wait()
	_ = down.Close()
	_ = up.Close()
}

// halfCloseWrite signals EOF to the peer by closing only the write half when the
// connection supports it (TCP). For connections without half-close (e.g.
// net.Pipe in tests) it nudges the peer read to unblock so the copy goroutine
// can exit.
func halfCloseWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.SetReadDeadline(time.Now())
}
