package caddyconsul

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p
}

// startBlackhole accepts and drains connections but never writes or closes them,
// keeping the proxied connection alive (for drain testing).
func startBlackhole(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { _, _ = io.Copy(io.Discard, c) }(c)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func newManager(t *testing.T, tbl *TCPTable, drain time.Duration, reserved map[int]bool) *TCPListenerManager {
	t.Helper()
	proxy := newTCPProxy(zap.NewNop(), tbl, nil, time.Second, 2*time.Second)
	return NewTCPListenerManager(context.Background(), zap.NewNop(), tbl, proxy, nil, drain, reserved)
}

func dialEcho(t *testing.T, port int, msg string) error {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte(msg)); err != nil {
		return err
	}
	got := make([]byte, len(msg))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		return err
	}
	if string(got) != msg {
		return fmt.Errorf("echo = %q, want %q", got, msg)
	}
	return nil
}

func TestListenerManager_OpenAndProxy(t *testing.T) {
	echoAddr, stop := startEchoServer(t)
	defer stop()

	port := freePort(t)
	tbl := NewTCPTable()
	tbl.Update([]CompiledTCPRoute{{Port: port, Upstreams: []Upstream{up(echoAddr)}, ServiceName: "echo"}})
	m := newManager(t, tbl, time.Second, nil)
	defer m.Close()

	if err := m.Reconcile([]int{port}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := dialEcho(t, port, "round-trip"); err != nil {
		t.Fatalf("proxy round-trip failed: %v", err)
	}
}

func TestListenerManager_ReconcileAddRemove(t *testing.T) {
	echoAddr, stop := startEchoServer(t)
	defer stop()

	p1, p2 := freePort(t), freePort(t)
	tbl := NewTCPTable()
	tbl.Update([]CompiledTCPRoute{
		{Port: p1, Upstreams: []Upstream{up(echoAddr)}, ServiceName: "a"},
		{Port: p2, Upstreams: []Upstream{up(echoAddr)}, ServiceName: "b"},
	})
	m := newManager(t, tbl, 200*time.Millisecond, nil)
	defer m.Close()

	if err := m.Reconcile([]int{p1, p2}); err != nil {
		t.Fatalf("reconcile open: %v", err)
	}
	if err := dialEcho(t, p1, "p1"); err != nil {
		t.Fatalf("p1 should work: %v", err)
	}
	if err := dialEcho(t, p2, "p2"); err != nil {
		t.Fatalf("p2 should work: %v", err)
	}

	// drop p1
	tbl.Update([]CompiledTCPRoute{{Port: p2, Upstreams: []Upstream{up(echoAddr)}, ServiceName: "b"}})
	if err := m.Reconcile([]int{p2}); err != nil {
		t.Fatalf("reconcile remove: %v", err)
	}
	// p1 should refuse new connections; give the listener a moment to close.
	time.Sleep(150 * time.Millisecond)
	if err := dialEcho(t, p1, "p1"); err == nil {
		t.Fatal("p1 should be closed after removal")
	}
	if err := dialEcho(t, p2, "still-up"); err != nil {
		t.Fatalf("p2 should still work: %v", err)
	}
}

func TestListenerManager_ReservedPortRefused(t *testing.T) {
	port := freePort(t)
	tbl := NewTCPTable()
	tbl.Update([]CompiledTCPRoute{{Port: port, Upstreams: []Upstream{up("x:1")}, ServiceName: "x"}})
	m := newManager(t, tbl, time.Second, map[int]bool{port: true})
	defer m.Close()

	if err := m.Reconcile([]int{port}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := m.ActivePorts(); len(got) != 0 {
		t.Fatalf("reserved port should not be opened, active = %v", got)
	}
}

func TestListenerManager_Close(t *testing.T) {
	echoAddr, stop := startEchoServer(t)
	defer stop()

	port := freePort(t)
	tbl := NewTCPTable()
	tbl.Update([]CompiledTCPRoute{{Port: port, Upstreams: []Upstream{up(echoAddr)}, ServiceName: "echo"}})
	m := newManager(t, tbl, 200*time.Millisecond, nil)

	if err := m.Reconcile([]int{port}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := dialEcho(t, port, "before-close"); err != nil {
		t.Fatalf("should work before close: %v", err)
	}
	m.Close()
	time.Sleep(150 * time.Millisecond)
	if err := dialEcho(t, port, "after-close"); err == nil {
		t.Fatal("listener should be closed after Close()")
	}
}

func TestListenerManager_DrainForceClose(t *testing.T) {
	bhAddr, stop := startBlackhole(t)
	defer stop()

	port := freePort(t)
	tbl := NewTCPTable()
	tbl.Update([]CompiledTCPRoute{{Port: port, Upstreams: []Upstream{up(bhAddr)}, ServiceName: "bh"}})
	const drain = 200 * time.Millisecond
	m := newManager(t, tbl, drain, nil)
	defer m.Close()

	if err := m.Reconcile([]int{port}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	client, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()
	// let the proxy fully establish & register the in-flight conn
	time.Sleep(150 * time.Millisecond)

	// remove the port — in-flight conn should be force-closed after the grace
	tbl.Update(nil)
	if err := m.Reconcile(nil); err != nil {
		t.Fatalf("reconcile remove: %v", err)
	}

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	start := time.Now()
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected in-flight conn to be force-closed after drain grace")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("force-close took %v, expected ~drain (%v)", elapsed, drain)
	}
}

// TestListenerManager_ReloadHandoff proves the §6 handoff at the component
// level: a NEW manager opens the SAME port (pool dedupe, no EADDRINUSE) before
// the OLD manager closes, and the listener keeps serving NEW connections with
// zero downtime across the handoff.
func TestListenerManager_ReloadHandoff(t *testing.T) {
	echoAddr, stop := startEchoServer(t)
	defer stop()
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	mkManager := func() *TCPListenerManager {
		tbl := NewTCPTable()
		tbl.Update([]CompiledTCPRoute{{Port: port, Upstreams: []Upstream{up(echoAddr)}, ServiceName: "echo"}})
		return newManager(t, tbl, 500*time.Millisecond, nil)
	}

	// OLD instance up and serving
	mOld := mkManager()
	if err := mOld.Reconcile([]int{port}); err != nil {
		t.Fatalf("OLD reconcile: %v", err)
	}
	if err := dialEcho(t, port, "pre-handoff"); err != nil {
		t.Fatalf("pre-handoff echo: %v", err)
	}

	// NEW instance opens the SAME port BEFORE old stops (start-new-before-stop-old)
	mNew := mkManager()
	if err := mNew.Reconcile([]int{port}); err != nil {
		t.Fatalf("NEW reconcile on shared port (pool dedupe expected, no EADDRINUSE): %v", err)
	}
	defer mNew.Close()

	// OLD stops — its listener Close decrements the refcount, but NEW holds it,
	// so the socket stays bound.
	mOld.Close()
	time.Sleep(150 * time.Millisecond)

	// A NEW connection after the handoff must still be served (by NEW).
	if _, err := net.DialTimeout("tcp", addr, time.Second); err != nil {
		t.Fatalf("post-handoff dial failed — listener did not survive: %v", err)
	}
	if err := dialEcho(t, port, "post-handoff"); err != nil {
		t.Fatalf("post-handoff echo failed — handoff broke serving: %v", err)
	}

	// Sanity: once NEW also closes, the port is fully released.
	mNew.Close()
	time.Sleep(150 * time.Millisecond)
	if l, err := net.Listen("tcp", addr); err != nil {
		t.Fatalf("port not released after both closed: %v", err)
	} else {
		_ = l.Close()
	}
}
