package caddyconsul

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/zap"
)

// TestMetrics_NilSafe verifies every recorder is a no-op (no panic) on a nil
// collector — the disabled-metrics path that every hot-path call site relies on.
func TestMetrics_NilSafe(t *testing.T) {
	var m *MetricsCollector // nil == metrics disabled

	m.IncDebounce()
	m.ObserveReconcile(time.Second)
	m.IncReconcileError()
	m.IncConflict("duplicate_port_sni")
	m.SetServicesTotal(3)
	m.SetRoutesTotal("tcp", 2)
	m.SetTCPListenersActive(2)
	m.IncTCPListenerOpen()
	m.IncTCPListenerClose()
	m.IncTCPListenError()
	m.IncTCPConnAccepted()
	m.DecTCPConnActive()
	m.AddTCPForceClosed(3)
	m.AddTCPForceClosed(0) // no-op guard
	m.IncTCPNoRoute()
	m.IncTCPNoUpstream()
	m.IncTCPDialError()
	// Reaching here without a panic is the assertion.
}

func TestMetrics_TCPProxy_NoRoute(t *testing.T) {
	m := newMetricsCollector(zap.NewNop())

	const port = 18500
	tbl := NewTCPTable()
	tbl.Update([]CompiledTCPRoute{
		{Port: port, SNI: "known.klmh.co", Passthrough: true, Upstreams: []Upstream{up("127.0.0.1:1")}, ServiceName: "k"},
	})
	p := newTCPProxy(zap.NewNop(), tbl, m, time.Second, time.Second)

	hello := captureClientHello(t, "unmatched.example.org")
	client, server := socketPair(t)
	defer func() { _ = client.Close() }()
	go p.handle(port, server)
	if _, err := client.Write(hello); err != nil {
		t.Fatal(err)
	}
	// handle drops the conn after recording; client read returns EOF.
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = client.Read(make([]byte, 1))

	if got := testutil.ToFloat64(m.TCPNoRoute); got != 1 {
		t.Fatalf("TCPNoRoute = %v, want 1", got)
	}
}

func TestMetrics_TCPProxy_DialError(t *testing.T) {
	m := newMetricsCollector(zap.NewNop())

	const port = 18501
	deadAddr := func() string { // a port nothing listens on → connection refused
		p := freePort(t)
		return netJoin(p)
	}()
	tbl := NewTCPTable()
	tbl.Update([]CompiledTCPRoute{{Port: port, Upstreams: []Upstream{up(deadAddr)}, ServiceName: "dead"}})
	p := newTCPProxy(zap.NewNop(), tbl, m, time.Second, 500*time.Millisecond)

	client, server := socketPair(t)
	defer func() { _ = client.Close() }()
	go p.handle(port, server)
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = client.Read(make([]byte, 1)) // blocks until handle closes after dial failure

	if got := testutil.ToFloat64(m.TCPUpstreamDialErrors); got != 1 {
		t.Fatalf("TCPUpstreamDialErrors = %v, want 1", got)
	}
}

func TestMetrics_ListenerManager_Records(t *testing.T) {
	m := newMetricsCollector(zap.NewNop())
	echoAddr, stop := startEchoServer(t)
	defer stop()

	port := freePort(t)
	tbl := NewTCPTable()
	tbl.Update([]CompiledTCPRoute{{Port: port, Upstreams: []Upstream{up(echoAddr)}, ServiceName: "echo"}})
	proxy := newTCPProxy(zap.NewNop(), tbl, m, time.Second, 2*time.Second)
	mgr := NewTCPListenerManager(context.Background(), zap.NewNop(), tbl, proxy, m, 200*time.Millisecond, nil)

	if err := mgr.Reconcile([]int{port}); err != nil {
		t.Fatalf("reconcile open: %v", err)
	}
	if got := testutil.ToFloat64(m.TCPListenerOpens); got != 1 {
		t.Fatalf("TCPListenerOpens = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.TCPListenersActive); got != 1 {
		t.Fatalf("TCPListenersActive = %v, want 1", got)
	}

	if err := dialEcho(t, port, "metric-conn"); err != nil {
		t.Fatalf("proxy round-trip: %v", err)
	}
	if got := testutil.ToFloat64(m.TCPConnectionsTotal); got < 1 {
		t.Fatalf("TCPConnectionsTotal = %v, want >= 1", got)
	}

	// remove the port
	tbl.Update(nil)
	if err := mgr.Reconcile(nil); err != nil {
		t.Fatalf("reconcile remove: %v", err)
	}
	if got := testutil.ToFloat64(m.TCPListenerCloses); got != 1 {
		t.Fatalf("TCPListenerCloses = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.TCPListenersActive); got != 0 {
		t.Fatalf("TCPListenersActive after remove = %v, want 0", got)
	}
	mgr.Close()
}

func TestMetrics_ConvergeRecorders(t *testing.T) {
	m := newMetricsCollector(zap.NewNop())
	m.IncDebounce()
	m.IncDebounce()
	m.SetServicesTotal(7)
	m.SetRoutesTotal("tcp", 4)
	m.IncConflict("duplicate_port_sni")

	if got := testutil.ToFloat64(m.DebounceEvents); got != 2 {
		t.Fatalf("DebounceEvents = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.ServicesTotal); got != 7 {
		t.Fatalf("ServicesTotal = %v, want 7", got)
	}
}

func netJoin(port int) string {
	return (&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}).String()
}
