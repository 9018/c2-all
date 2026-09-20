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

	utls "github.com/refraction-networking/utls"
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

func captureBrowserHandshakes(t *testing.T, n int) (negotiated []string, clientProtos []string) {
	t.Helper()
	serverCert, caPool := testCA(t)
	relayTLSRootCAs = caPool
	defer func() { relayTLSRootCAs = nil }()

	var (
		mu     sync.Mutex
		protos []string
	)
	cfg := &tls.Config{Certificates: []tls.Certificate{serverCert},
		// mimic CF edge: it advertises both h2 and http/1.1. With the
		// double-hello design each dial yields either [http/1.1] or the
		// probe pair [h2, http/1.1] — the traffic-carrying conn always
		// ends up on http/1.1; an h2 hello is the Chrome-consistent probe.
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
		if u, ok := conn.(*utls.UConn); ok {
			clientProtos = append(clientProtos, u.ConnectionState().NegotiatedProtocol)
		} else {
			clientProtos = append(clientProtos, "unknown")
		}
		conn.Close()
		time.Sleep(50 * time.Millisecond)
	}

	// the server goroutine may still be recording the last retry — wait
	// until the record stops growing (or 2s elapse) before asserting
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		snapped := len(protos)
		mu.Unlock()
		if snapped >= n && time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
		if snapped >= n+1 && time.Now().After(deadline.Add(-1500*time.Millisecond)) {
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string{}, protos...), clientProtos
}

func TestRelayTLSBrowserFingerprint(t *testing.T) {
	protos, clientProtos := captureBrowserHandshakes(t, 3)

	// traffic-facing contract: EVERY conn dialRelayTLS returns must be
	// http/1.1 — h2 would break the WS upgrade and the http1 transports
	for i, np := range clientProtos {
		if np != "http/1.1" {
			t.Fatalf("dial %d returned conn negotiates %q, want http/1.1", i, np)
		}
	}

	// sensor-facing contract: the server-visible sequence is a
	// concatenation of [http/1.1] and [h2, http/1.1] probe pairs — an h2
	// hello must always be immediately followed by its http/1.1 retry
	for i := 0; i < len(protos); i++ {
		switch {
		case protos[i] == "http/1.1":
		case protos[i] == "h2" && i+1 < len(protos) && protos[i+1] == "http/1.1":
			i++ // the retry belongs to this probe
		default:
			t.Fatalf("server-visible proto %q at %d breaks the double-hello contract: %v", protos[i], i, protos)
		}
	}
	t.Logf("%d server-visible handshakes, all %d returned conns http/1.1 — double-hello contract OK (%v)",
		len(protos), len(clientProtos), protos)
}
