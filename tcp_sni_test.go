package caddyconsul

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"
)

// captureClientHello runs a TLS client handshake over an in-memory pipe and
// returns the raw bytes of the first TLS record (the ClientHello) it sends.
func captureClientHello(t *testing.T, serverName string) []byte {
	t.Helper()
	cli, srv := net.Pipe()
	go func() {
		cfg := &tls.Config{InsecureSkipVerify: true, ServerName: serverName}
		_ = tls.Client(cli, cfg).Handshake() // fails once we close srv; ignore
		_ = cli.Close()
	}()

	_ = srv.SetReadDeadline(time.Now().Add(2 * time.Second))
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(srv, hdr); err != nil {
		t.Fatalf("read record header: %v", err)
	}
	n := int(hdr[3])<<8 | int(hdr[4])
	body := make([]byte, n)
	if _, err := io.ReadFull(srv, body); err != nil {
		t.Fatalf("read record body: %v", err)
	}
	_ = srv.Close()
	return append(hdr, body...)
}

func TestExtractSNI(t *testing.T) {
	for _, name := range []string{
		"example.klmh.co",
		"svc.wp1.us-west.hhost.io",
		"a.b.c.d.klm1.us-west.my-wp.io",
	} {
		record := captureClientHello(t, name)
		sni, ok := extractSNI(record[5:]) // strip 5-byte record header
		if !ok || sni != name {
			t.Fatalf("extractSNI(%q) = (%q, %v), want (%q, true)", name, sni, ok, name)
		}
	}
}

func TestExtractSNI_None(t *testing.T) {
	record := captureClientHello(t, "") // no SNI sent
	sni, ok := extractSNI(record[5:])
	if ok || sni != "" {
		t.Fatalf("extractSNI(no SNI) = (%q, %v), want (\"\", false)", sni, ok)
	}
}

func TestExtractSNI_Garbage(t *testing.T) {
	for _, b := range [][]byte{
		nil,
		{0x01},
		{0x01, 0, 0, 0, 0x03, 0x03}, // truncated
		bytes.Repeat([]byte{0xff}, 64),
	} {
		if sni, ok := extractSNI(b); ok || sni != "" {
			t.Fatalf("extractSNI(garbage %x) = (%q, %v), want (\"\", false)", b, sni, ok)
		}
	}
}

func TestPeekClientHelloSNI(t *testing.T) {
	record := captureClientHello(t, "svc.wp1.us-west.hhost.io")
	extra := []byte("payload-after-the-handshake")

	c1, c2 := net.Pipe()
	go func() {
		_, _ = c1.Write(record)
		_, _ = c1.Write(extra)
	}()

	sni, buffered, isTLS, err := peekClientHelloSNI(c2, 2*time.Second)
	if err != nil {
		t.Fatalf("peek err: %v", err)
	}
	if !isTLS {
		t.Fatal("isTLS = false, want true")
	}
	if sni != "svc.wp1.us-west.hhost.io" {
		t.Fatalf("sni = %q", sni)
	}
	if !bytes.Equal(buffered, record) {
		t.Fatalf("buffered (%d bytes) != original ClientHello record (%d bytes)", len(buffered), len(record))
	}

	// prefixConn must replay `buffered` verbatim, then stream the live `extra`.
	pc := newPrefixConn(c2, buffered)
	got := make([]byte, len(record)+len(extra))
	if _, err := io.ReadFull(pc, got); err != nil {
		t.Fatalf("replay read: %v", err)
	}
	if !bytes.Equal(got[:len(record)], record) {
		t.Fatal("replayed prefix != original ClientHello")
	}
	if !bytes.Equal(got[len(record):], extra) {
		t.Fatal("post-prefix stream != live data")
	}
	_ = c1.Close()
	_ = c2.Close()
}

func TestPeekClientHelloSNI_NonTLS(t *testing.T) {
	c1, c2 := net.Pipe()
	go func() { _, _ = c1.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")) }()

	sni, buffered, isTLS, err := peekClientHelloSNI(c2, 2*time.Second)
	if err != nil {
		t.Fatalf("peek err: %v", err)
	}
	if isTLS {
		t.Fatal("isTLS = true for HTTP input, want false")
	}
	if sni != "" {
		t.Fatalf("sni = %q for non-TLS, want empty", sni)
	}
	if !bytes.Equal(buffered, []byte("GET /")) {
		t.Fatalf("buffered = %q, want %q", buffered, "GET /")
	}
	_ = c1.Close()
	_ = c2.Close()
}

func TestPeekClientHelloSNI_Timeout(t *testing.T) {
	c1, c2 := net.Pipe()
	defer func() { _ = c1.Close() }()
	defer func() { _ = c2.Close() }()

	start := time.Now()
	_, _, _, err := peekClientHelloSNI(c2, 100*time.Millisecond) // c1 never writes
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout took %v, expected ~100ms", elapsed)
	}
}

func TestNewPrefixConn_EmptyPrefixReturnsSame(t *testing.T) {
	_, c2 := net.Pipe()
	defer func() { _ = c2.Close() }()
	if got := newPrefixConn(c2, nil); got != c2 {
		t.Fatal("newPrefixConn with empty prefix should return the original conn unwrapped")
	}
}
