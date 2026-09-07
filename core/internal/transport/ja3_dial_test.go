package transport

// ja3_dial_test.go — one-shot helper: dial 127.0.0.1:18443 through dialRelayTLS.
// The server is a raw-TCP peeker: it saves the raw ClientHello bytes to
// /tmp/hello_agent_<n>.bin, then completes the TLS handshake so the client is
// happy. Run with JA3_DIAL=1. Chromium (or any browser) hitting the same port
// gets the same treatment, producing /tmp/hello_browser_<n>.bin for comparison.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

// replayConn first replays the captured bytes, then passes through live traffic.
type replayConn struct {
	net.Conn
	pre []byte
	off int
}

func (p *replayConn) Read(b []byte) (int, error) {
	if p.off < len(p.pre) {
		n := copy(b, p.pre[p.off:])
		p.off += n
		return n, nil
	}
	return p.Conn.Read(b)
}

func peekAndSave(c net.Conn, path string) (net.Conn, error) {
	// io.ReadFull directly on the TCP conn consumes exactly what we ask for;
	// anything else stays kernel-buffered for the real TLS layer afterwards.
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return nil, err
	}
	recLen := int(hdr[3])<<8 | int(hdr[4])
	body := make([]byte, recLen)
	if _, err := io.ReadFull(c, body); err != nil {
		return nil, err
	}
	saved := append(hdr, body...)
	os.WriteFile(path, saved, 0644)
	return &replayConn{Conn: c, pre: saved}, nil
}

func servePeek(t *testing.T, cert tls.Certificate) net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:18443")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		n := 0
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n++
			go func(c net.Conn, n int) {
				pc, err := peekAndSave(c, "/tmp/hello_n"+strconv.Itoa(n)+".bin")
				if err != nil {
					c.Close()
					return
				}
				tc := tls.Server(pc, &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}})
				if err := tc.Handshake(); err != nil {
					t.Logf("server handshake err: %v", err)
				} else {
					t.Logf("server handshake OK, alpn=%q", tc.ConnectionState().NegotiatedProtocol)
				}
				tc.Close()
			}(c, n)
		}
	}()
	return ln
}

func TestJa3Dial(t *testing.T) {
	if os.Getenv("JA3_DIAL") == "" {
		t.Skip("helper only")
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: "ja3-probe"},
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		DNSNames: []string{"127.0.0.1"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, _ := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	parsed, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	relayTLSRootCAs = pool
	defer func() { relayTLSRootCAs = nil }()

	ln := servePeek(t, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key})
	defer ln.Close()

	for i := 0; i < 3; i++ {
		t.Logf("dial %d starting at %s", i, time.Now().Format("15:04:05.000"))
		conn, err := dialRelayTLS(context.Background(), "tcp", "127.0.0.1:18443")
		if err != nil {
			t.Logf("agent dial %d err: %v", i, err)
			continue
		}
		conn.Close()
		t.Logf("dial %d done", i)
		time.Sleep(300 * time.Millisecond)
	}
	// keep server alive briefly so browser dials (if any) are served too
	time.Sleep(4 * time.Second)
}
