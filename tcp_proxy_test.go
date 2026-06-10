package caddyconsul

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
)

// startEchoServer accepts TCP connections and echoes everything back.
func startEchoServer(t *testing.T) (addr string, stop func()) {
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
			go func(c net.Conn) { _, _ = io.Copy(c, c); _ = c.Close() }(c)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// startTagServer writes a fixed tag on connect (then drains), so a client can
// learn which backend it was routed to.
func startTagServer(t *testing.T, tag string) (addr string, stop func()) {
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
			go func(c net.Conn) {
				_, _ = c.Write([]byte(tag))
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}(c)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// socketPair returns a connected pair of real TCP conns (both *net.TCPConn, so
// CloseWrite works). client is the dialing side; server is handed to the proxy.
func socketPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	type res struct {
		c net.Conn
		e error
	}
	ch := make(chan res, 1)
	go func() { c, e := ln.Accept(); ch <- res{c, e} }()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.e != nil {
		t.Fatal(r.e)
	}
	return client, r.c
}

func TestProxy_PlainTCP_Echo(t *testing.T) {
	upAddr, stopUp := startEchoServer(t)
	defer stopUp()

	const port = 15432
	tbl := NewTCPTable()
	tbl.Update([]CompiledTCPRoute{{Port: port, Upstreams: []Upstream{up(upAddr)}, ServiceName: "echo"}})
	p := newTCPProxy(zap.NewNop(), tbl, nil, time.Second, 2*time.Second)

	client, server := socketPair(t)
	defer func() { _ = client.Close() }()
	go p.handle(port, server)

	msg := []byte("hello-tcp-passthrough")
	if _, err := client.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("echo = %q, want %q", got, msg)
	}
}

func TestProxy_SNIRouting(t *testing.T) {
	upA, stopA := startTagServer(t, "AAAA")
	defer stopA()
	upB, stopB := startTagServer(t, "BBBB")
	defer stopB()

	const port = 8443
	tbl := NewTCPTable()
	tbl.Update([]CompiledTCPRoute{
		{Port: port, SNI: "a.klmh.co", Passthrough: true, Upstreams: []Upstream{up(upA)}, ServiceName: "A"},
		{Port: port, SNI: "b.klmh.co", Passthrough: true, Upstreams: []Upstream{up(upB)}, ServiceName: "B"},
	})
	p := newTCPProxy(zap.NewNop(), tbl, nil, time.Second, 2*time.Second)

	for _, tc := range []struct{ sni, wantTag string }{
		{"a.klmh.co", "AAAA"},
		{"b.klmh.co", "BBBB"},
	} {
		hello := captureClientHello(t, tc.sni)
		client, server := socketPair(t)
		go p.handle(port, server)
		if _, err := client.Write(hello); err != nil {
			t.Fatalf("write hello: %v", err)
		}
		got := make([]byte, 4)
		_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(client, got); err != nil {
			t.Fatalf("sni %s: read tag: %v", tc.sni, err)
		}
		if string(got) != tc.wantTag {
			t.Fatalf("sni %s routed to %q, want %q", tc.sni, got, tc.wantTag)
		}
		_ = client.Close()
	}
}

func TestProxy_NoMatchingRoute_ClosesConn(t *testing.T) {
	upA, stopA := startTagServer(t, "AAAA")
	defer stopA()

	const port = 8444
	tbl := NewTCPTable()
	// Only an SNI route exists; a ClientHello for a different SNI with no
	// default route should match nothing and the conn should be dropped.
	tbl.Update([]CompiledTCPRoute{
		{Port: port, SNI: "known.klmh.co", Passthrough: true, Upstreams: []Upstream{up(upA)}, ServiceName: "A"},
	})
	p := newTCPProxy(zap.NewNop(), tbl, nil, time.Second, time.Second)

	hello := captureClientHello(t, "unknown.example.org")
	client, server := socketPair(t)
	defer func() { _ = client.Close() }()
	done := make(chan struct{})
	go func() { p.handle(port, server); close(done) }()
	if _, err := client.Write(hello); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handle did not return for unmatched SNI")
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected client read to fail after proxy dropped the conn")
	}
}

func TestProxy_NoHealthyUpstream_ClosesConn(t *testing.T) {
	const port = 15999
	tbl := NewTCPTable()
	tbl.Update([]CompiledTCPRoute{
		{Port: port, Upstreams: []Upstream{{Address: "192.0.2.1:1", Healthy: false}}, ServiceName: "down"},
	})
	p := newTCPProxy(zap.NewNop(), tbl, nil, time.Second, time.Second)

	client, server := socketPair(t)
	defer func() { _ = client.Close() }()
	done := make(chan struct{})
	go func() { p.handle(port, server); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handle did not return for no-upstream route")
	}
}

func TestPickUpstream(t *testing.T) {
	// none healthy
	if u := pickUpstream([]Upstream{{Address: "x:1", Healthy: false}}); u != nil {
		t.Fatalf("pickUpstream(all unhealthy) = %v, want nil", u)
	}
	// only healthy ones are ever returned
	ups := []Upstream{
		{Address: "healthy:1", Healthy: true, Weight: 1},
		{Address: "unhealthy:2", Healthy: false, Weight: 100},
	}
	for i := 0; i < 50; i++ {
		if u := pickUpstream(ups); u == nil || u.Address != "healthy:1" {
			t.Fatalf("pickUpstream returned %v, want healthy:1", u)
		}
	}
	// weighting: a 99:1 split should overwhelmingly favor the heavy upstream
	ups = []Upstream{
		{Address: "heavy:1", Healthy: true, Weight: 99},
		{Address: "light:1", Healthy: true, Weight: 1},
	}
	heavy := 0
	const n = 2000
	for i := 0; i < n; i++ {
		if pickUpstream(ups).Address == "heavy:1" {
			heavy++
		}
	}
	if heavy < n*8/10 { // expect ~99%, assert > 80% to avoid flakiness
		t.Fatalf("weighted pick favored heavy only %d/%d times, expected the large majority", heavy, n)
	}
}
