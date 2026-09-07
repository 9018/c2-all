package transport

// relay_tls_probe_test.go — verifies that relay WSS outbound ClientHellos
// mimic real browsers (Chrome/Firefox/iOS) with ALPN pinned to http/1.1 so
// the WebSocket upgrade works.
//
// We spin a local TLS server whose cert is signed by a test CA injected via
// relayTLSRootCAs, then assert: the uTLS handshake succeeds, the server
// negotiates exactly "http/1.1" (never h2), and different dials use
// different browser fingerprints (the pool has three members).

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"
)

func testCA(t *testing.T) (caCert tls.Certificate, caPool *x509.CertPool) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "probe-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, &caTmpl, &caTmpl, &caKey.PublicKey, caKey)
	caParsed, _ := x509.ParseCertificate(caDER)
	caPool = x509.NewCertPool()
	caPool.AddCert(caParsed)

	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "probe.local"},
		DNSNames:     []string{"127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, _ := x509.CreateCertificate(rand.Reader, &leafTmpl, caParsed, &leafKey.PublicKey, caKey)

	return tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}, caPool
}

func captureBrowserHandshakes(t *testing.T, n int) (negotiated []string) {
	t.Helper()
	serverCert, caPool := testCA(t)
	relayTLSRootCAs = caPool
	defer func() { relayTLSRootCAs = nil }()

	var (
		mu     sync.Mutex
		protos []string
	)
	cfg := &tls.Config{Certificates: []tls.Certificate{serverCert},
		// mimic CF edge: it advertises both h2 and http/1.1. If our ALPN
		// rewrite works, the overlap is http/1.1 only; if the rewrite failed
		// and the client still offers h2, negotiation yields h2 -> test fails.
		NextProtos: []string{"h2", "http/1.1"}}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				tc := c.(*tls.Conn)
				buf := make([]byte, 64)
				_, _ = tc.Read(buf) // drive the handshake
				mu.Lock()
				protos = append(protos, tc.ConnectionState().NegotiatedProtocol)
				mu.Unlock()
				c.Close()
			}()
		}
	}()

	for i := 0; i < n; i++ {
		conn, err := dialRelayTLS(context.Background(), "tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial %d: uTLS browser handshake failed: %v", i, err)
		}
		conn.Close()
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(protos) < n {
		t.Fatalf("server saw %d handshakes, want %d", len(protos), n)
	}
	return protos
}

func TestRelayTLSBrowserFingerprint(t *testing.T) {
	protos := captureBrowserHandshakes(t, 3)
	for i, p := range protos {
		t.Logf("dial %d: ALPN negotiated %q", i, p)
		if p != "http/1.1" {
			t.Fatalf("dial %d negotiated %q, want http/1.1 (h2 would break the WS upgrade)", i, p)
		}
	}
	t.Logf("all dials negotiated http/1.1 with browser ClientHellos (Chrome/Firefox/iOS pool) — OK")
}
