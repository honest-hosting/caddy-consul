package caddyconsul

import (
	"io"
	"net"
	"time"

	"golang.org/x/crypto/cryptobyte"
)

const (
	tlsRecordTypeHandshake  = 0x16
	tlsHandshakeClientHello = 0x01
	tlsRecordHeaderLen      = 5
	maxClientHelloRecord    = 16384 // max TLS plaintext record payload (2^14)

	extensionServerName uint16 = 0 // RFC 6066
)

// peekClientHelloSNI reads the leading TLS record from conn, extracts the SNI
// server name from the ClientHello (if present), and returns the exact bytes
// consumed so they can be replayed to the upstream. TLS passthrough must forward
// the original ClientHello untouched, so the caller wraps conn with
// newPrefixConn(conn, buffered) before proxying.
//
// Returns:
//   - sni:      the SNI host_name ("" if absent or unparseable)
//   - buffered: every byte read from conn (replay these to the upstream)
//   - isTLS:    whether the first record looked like a TLS handshake
//   - err:      a read error (timeout/EOF); buffered holds whatever was read
//
// A read deadline of `timeout` guards against a client that connects but never
// sends a ClientHello. The deadline is cleared before returning so it does not
// leak into the subsequent proxy copy.
//
// Limitation: only the first TLS record is read (up to 16 KiB). A ClientHello
// fragmented across multiple records, or whose SNI sits after a very large
// extension (e.g. post-quantum key shares pushing past one record), yields
// sni="" — the caller then falls back to the empty-SNI/default route. This
// matches the practical envelope of caddy-l4's own 8 KiB match buffer.
func peekClientHelloSNI(conn net.Conn, timeout time.Duration) (sni string, buffered []byte, isTLS bool, err error) {
	if timeout > 0 {
		if e := conn.SetReadDeadline(time.Now().Add(timeout)); e == nil {
			defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
		}
	}

	hdr := make([]byte, tlsRecordHeaderLen)
	if _, err = io.ReadFull(conn, hdr); err != nil {
		return "", hdr[:0], false, err
	}
	buffered = hdr

	// Record type must be a handshake; otherwise this isn't TLS.
	if hdr[0] != tlsRecordTypeHandshake {
		return "", buffered, false, nil
	}

	recLen := int(hdr[3])<<8 | int(hdr[4])
	if recLen <= 0 || recLen > maxClientHelloRecord {
		// Implausible length — treat as non-TLS rather than read garbage.
		return "", buffered, false, nil
	}

	body := make([]byte, recLen)
	if _, err = io.ReadFull(conn, body); err != nil {
		return "", buffered, true, err
	}
	buffered = append(buffered, body...)

	// The record payload should begin with a ClientHello handshake message.
	if len(body) < 4 || body[0] != tlsHandshakeClientHello {
		return "", buffered, true, nil
	}

	sni, _ = extractSNI(body)
	return sni, buffered, true, nil
}

// extractSNI parses just enough of a TLS ClientHello to pull out the SNI
// host_name. `hs` must start at the handshake message (type byte + uint24
// length + body), i.e. the TLS record payload. Returns ("", false) if no SNI is
// present or the message cannot be parsed.
//
// This is a minimal port of the server_name path from mholt/caddy-l4's
// parsehello.go (itself derived from the Go stdlib); only the SNI extension is
// read — every other field is skipped.
func extractSNI(hs []byte) (string, bool) {
	s := cryptobyte.String(hs)

	if !s.Skip(4) { // handshake type (1) + uint24 length (3)
		return "", false
	}
	if !s.Skip(2) { // client_version
		return "", false
	}
	if !s.Skip(32) { // random
		return "", false
	}

	var sessionID cryptobyte.String
	if !s.ReadUint8LengthPrefixed(&sessionID) {
		return "", false
	}
	var cipherSuites cryptobyte.String
	if !s.ReadUint16LengthPrefixed(&cipherSuites) {
		return "", false
	}
	var compressionMethods cryptobyte.String
	if !s.ReadUint8LengthPrefixed(&compressionMethods) {
		return "", false
	}

	if s.Empty() {
		return "", false // ClientHello without extensions => no SNI
	}

	var extensions cryptobyte.String
	if !s.ReadUint16LengthPrefixed(&extensions) {
		return "", false
	}

	for !extensions.Empty() {
		var extType uint16
		var extData cryptobyte.String
		if !extensions.ReadUint16(&extType) || !extensions.ReadUint16LengthPrefixed(&extData) {
			return "", false
		}
		if extType != extensionServerName {
			continue
		}
		// server_name extension (RFC 6066, Section 3).
		var nameList cryptobyte.String
		if !extData.ReadUint16LengthPrefixed(&nameList) || nameList.Empty() {
			return "", false
		}
		for !nameList.Empty() {
			var nameType uint8
			var serverName cryptobyte.String
			if !nameList.ReadUint8(&nameType) ||
				!nameList.ReadUint16LengthPrefixed(&serverName) ||
				serverName.Empty() {
				return "", false
			}
			if nameType == 0 { // host_name
				return string(serverName), true
			}
		}
	}
	return "", false
}

// prefixConn is a net.Conn whose Read first drains a buffered prefix (the bytes
// consumed while peeking the ClientHello) before reading from the underlying
// connection. This lets the proxy forward the original ClientHello to the
// upstream verbatim for TLS passthrough.
type prefixConn struct {
	net.Conn
	prefix []byte
	off    int
}

// newPrefixConn wraps c so that `prefix` is replayed before c's own data.
// If prefix is empty, c is returned unchanged.
func newPrefixConn(c net.Conn, prefix []byte) net.Conn {
	if len(prefix) == 0 {
		return c
	}
	return &prefixConn{Conn: c, prefix: prefix}
}

func (p *prefixConn) Read(b []byte) (int, error) {
	if p.off < len(p.prefix) {
		n := copy(b, p.prefix[p.off:])
		p.off += n
		return n, nil
	}
	return p.Conn.Read(b)
}

// CloseWrite forwards a half-close to the underlying connection when it supports
// one (e.g. *net.TCPConn), so the proxy can signal EOF to the upstream after the
// replayed ClientHello and live stream are done.
func (p *prefixConn) CloseWrite() error {
	if cw, ok := p.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
