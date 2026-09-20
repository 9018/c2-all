package transport

// ja4_doublehello_test.go — verifies the double-hello (JA4 ALPN alignment)
// flow in browserTLSConnect: with the probe roll forced on, the first
// handshake must offer h2,http/1.1 (real-Chrome JA4, part1 "h2"); the edge
// negotiating h2 must cause an immediate close and a re-dial whose hello
// offers http/1.1 only, and the returned connection must be http/1.1.
// With the roll forced off, a single h1-only hello must be presented.

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
	"sync"
	"testing"
	"time"
)

// doubleHelloServer accepts conns, completes the server side of TLS with
// NextProtos h2,http/1.1, and records the negotiated protocol per conn.
func doubleHelloServer(t *testing.T, cert tls.Certificate) (net.Listener, *[]string, *sync.Mutex) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := &[]string{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				tc := tls.Server(c, &tls.Config{
					Certificates: []tls.Certificate{cert},
					NextProtos:   []string{"h2", "http/1.1"},
				})
				herr := tc.Handshake()
				if herr != nil && herr != io.EOF && herr != io.ErrUnexpectedEOF {
					// a client closing mid-handshake is expected for the
					// probe path only after the server finished; anything
					// else is logged, not fatal
					mu.Lock()
					*seen = append(*seen, "handshake-err:"+herr.Error())
					mu.Unlock()
					c.Close()
					return
				}
				np := tc.ConnectionState().NegotiatedProtocol
				mu.Lock()
				*seen = append(*seen, np)
				mu.Unlock()
				// read until the client hangs up (the probe path closes
				// without sending anything)
				buf := make([]byte, 512)
				for {
					if _, rerr := tc.Read(buf); rerr != nil {
						break
					}
				}
				tc.Close()
			}(c)
		}
	}()
	return ln, seen, &mu
}

func doubleHelloCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(11),
		Subject:      pkix.Name{CommonName: "ja4-doublehello"},
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		DNSNames:  []string{"127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, _ := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	parsed, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// awaitConns polls the server-side record until len reaches want or the
// timeout expires (the server goroutine records a beat after the client
// returns, so a single read would race).
func awaitConns(mu *sync.Mutex, seen *[]string, want int) []string {
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		out := append([]string{}, *seen...)
		mu.Unlock()
		if len(out) >= want || time.Now().After(deadline) {
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDoubleHelloH2Probe(t *testing.T) {
	cert, pool := doubleHelloCert(t)
	relayTLSRootCAs = pool
	defer func() { relayTLSRootCAs = nil }()

	ln, seen, mu := doubleHelloServer(t, cert)
	defer ln.Close()
	addr := ln.Addr().String()

	t.Run("probe on: h2 hello then h1 retry", func(t *testing.T) {
		oldRoll := h2ProbeRoll
		h2ProbeRoll = func() bool { return true }
		defer func() { h2ProbeRoll = oldRoll }()

		mu.Lock(); *seen = nil; mu.Unlock()
		conn, err := browserTLSConnect(func() (net.Conn, error) {
			return net.DialTimeout("tcp4", addr, 5*time.Second)
		}, addr)
		if err != nil {
			t.Fatalf("browserTLSConnect: %v", err)
		}
		defer conn.Close()
		if np := conn.ConnectionState().NegotiatedProtocol; np != "http/1.1" {
			t.Fatalf("returned conn negotiates %q, want http/1.1", np)
		}
		conns := awaitConns(mu, seen, 2)
		if len(conns) != 2 {
			t.Fatalf("want 2 server-side connections (h2 probe + h1 retry), got %v", conns)
		}
		if conns[0] != "h2" {
			t.Fatalf("first hello must negotiate h2 (Chrome-consistent offer), got %q", conns[0])
		}
		if conns[1] != "http/1.1" {
			t.Fatalf("retry hello must negotiate http/1.1, got %q", conns[1])
		}
	})

	t.Run("probe off: single h1-only hello", func(t *testing.T) {
		oldRoll := h2ProbeRoll
		h2ProbeRoll = func() bool { return false }
		defer func() { h2ProbeRoll = oldRoll }()

		mu.Lock(); *seen = nil; mu.Unlock()
		conn, err := browserTLSConnect(func() (net.Conn, error) {
			return net.DialTimeout("tcp4", addr, 5*time.Second)
		}, addr)
		if err != nil {
			t.Fatalf("browserTLSConnect: %v", err)
		}
		defer conn.Close()
		if np := conn.ConnectionState().NegotiatedProtocol; np != "http/1.1" {
			t.Fatalf("returned conn negotiates %q, want http/1.1", np)
		}
		conns := awaitConns(mu, seen, 1)
		if len(conns) != 1 || conns[0] != "http/1.1" {
			t.Fatalf("want exactly one http/1.1 connection, got %v", conns)
		}
	})

	_ = context.Background
}
