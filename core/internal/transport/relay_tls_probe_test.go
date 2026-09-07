package transport

// relay_tls_probe_test.go — verifies that relay WSS outbound ClientHellos are
// randomized per connection (no stable Go JA3 fingerprint).
//
// dialRelayTLS verifies the server certificate (InsecureSkipVerify=false), so
// the client handshake will *fail* against a self-signed test server — that's
// fine: the ClientHello is sent before verification, so the server-side
// GetConfigForClient callback still captures it. We assert that two separate
// dials produce different cipher-suite orders (the core of a JA3 string).

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"sync"
	"testing"
	"time"
)

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "probe.local"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:         true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func captureHellos(t *testing.T, n int) []*tls.ClientHelloInfo {
	t.Helper()
	var (
		mu     sync.Mutex
		hellos []*tls.ClientHelloInfo
	)
	cert := selfSigned(t)
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(hi *tls.ClientHelloInfo) (*tls.Config, error) {
			mu.Lock()
			hellos = append(hellos, hi)
			mu.Unlock()
			return nil, nil
		},
	}
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
				buf := make([]byte, 1)
				_, _ = c.Read(buf) // handshake proceeds; cert will be rejected client-side
				c.Close()
			}()
		}
	}()

	for i := 0; i < n; i++ {
		conn, err := dialRelayTLS(context.Background(), "tcp", ln.Addr().String())
		if err == nil {
			conn.Close() // unexpected but harmless
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(hellos) < n {
		t.Fatalf("captured %d hellos, want %d", len(hellos), n)
	}
	return hellos
}

func TestRelayTLSHelloRandomized(t *testing.T) {
	hellos := captureHellos(t, 3)
	suites := make([]string, 3)
	for i, hi := range hellos {
		t.Logf("handshake %d: %d cipher suites, versions=%v", i, len(hi.CipherSuites), hi.SupportedVersions)
		suites[i] = fmtCipherIDs(hi.CipherSuites)
	}
	if suites[0] == suites[1] && suites[1] == suites[2] {
		t.Fatalf("three handshakes produced identical cipher-suite ordering (stable fingerprint!): %s", suites[0])
	}
	t.Logf("JA3-relevant cipher ordering differs across handshakes: randomized OK")
}

func fmtCipherIDs(ids []uint16) string {
	s := ""
	for _, id := range ids {
		s += string(rune(id>>8)) + string(rune(id&0xff))
	}
	return s
}
