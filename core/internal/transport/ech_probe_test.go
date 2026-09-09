// Package transport — ech_probe_test.go
//
// Live probe for the ECH (Encrypted Client Hello) path against the relay
// Worker's Cloudflare zone. Verifies the three building blocks before the
// agent wires them together:
//
//  1. fetching the ECHConfigList from the DNS HTTPS (type 65) record via a
//     DoH POST (public 1.1.1.1 here; the agent uses its own relay /dns),
//  2. parsing the ECH public name out of the config,
//  3. a uTLS handshake with a browser spec where the OUTER ClientHello
//     carries the public name while the encrypted INNER keeps the real
//     SNI — asserting the real hostname never appears in plaintext.
package transport

import (
	"bytes"
	"crypto/tls"
	"io"
	"os"
	"net"
	"reflect"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	utls "github.com/refraction-networking/utls"
)

func probeFetchECHConfigList(t *testing.T, host string) ([]byte, string) {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(host), dns.TypeHTTPS)
	wire, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	doh := os.Getenv("ECH_PROBE_DOH")
	if doh == "" {
		doh = "https://relay.zhpmt8ymbc.kdns.fr/dns?secret=" + os.Getenv("EMP_SHARED_SECRET")
	}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Post(doh, "application/dns-message", bytes.NewReader(wire))
	if err != nil {
		t.Skipf("DoH %s unreachable: %v", doh, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	answer := new(dns.Msg)
	if err := answer.Unpack(body); err != nil {
		t.Fatal(err)
	}
	for _, rr := range answer.Answer {
		if https, ok := rr.(*dns.HTTPS); ok {
			for _, kv := range https.Value {
				if ech, ok := kv.(*dns.SVCBECHConfig); ok {
					pub, err := echPublicNameOf(ech.ECH)
					if err != nil {
						t.Fatalf("parse public name: %v", err)
					}
					return ech.ECH, pub
				}
			}
		}
	}
	t.Skipf("no HTTPS record with an ech= param for %s (ECH not published?)", host)
	return nil, ""
}

// TestECHHandshakeToRelay dials the relay with a Chrome spec + real ECH and
// asserts that ECH was accepted and that the real hostname is nowhere in
// the plaintext outer ClientHello. Set ECH_PROBE_HOST to run.
func TestECHHandshakeToRelay(t *testing.T) {
	host := "relay.zhpmt8ymbc.kdns.fr"
	if h := os.Getenv("ECH_PROBE_HOST"); h != "" {
		host = h
	}
	echList, publicName := probeFetchECHConfigList(t, host)
	if publicName == "" {
		t.Fatalf("empty public name")
	}
	t.Logf("ECH public name: %s (config %d bytes)", publicName, len(echList))

	conn, err := net.DialTimeout("tcp4", host+":443", 8*time.Second)
	if err != nil {
		t.Skipf("dial %s: %v", host, err)
	}
	defer conn.Close()

	cfg := &utls.Config{
		ServerName:                         host,
		RootCAs:                             relayTLSRootCAs,
		MinVersion:                         utls.VersionTLS13,
		EncryptedClientHelloConfigList:      echList,
		InsecureSkipVerify:                 false,
	}
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		t.Fatal(err)
	}
	// pin ALPN like the WS channel does
	for i, ext := range spec.Extensions {
		if _, ok := ext.(*utls.ALPNExtension); ok {
			spec.Extensions[i] = &utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}}
		}
	}
	// outer SNI must be the ECH public name — the library does not do this
	// for the spec-based path
	for i, ext := range spec.Extensions {
		if _, ok := ext.(*utls.SNIExtension); ok {
			spec.Extensions[i] = &utls.SNIExtension{ServerName: publicName}
		}
	}
	// marker extension the ECH marshaler replaces with the real payload.
	// Chrome/Firefox specs already carry a GREASE ECH ext — appending a
	// second one yields a malformed outer (two ECH extensions) which the
	// server rejects with decode_error.
	hasECH := false
	for _, ext := range spec.Extensions {
		if _, ok := ext.(utls.EncryptedClientHelloExtension); ok {
			hasECH = true
			break
		}
	}
	if !hasECH {
		spec.Extensions = append(spec.Extensions, &utls.GREASEEncryptedClientHelloExtension{})
	}

	specName := os.Getenv("ECH_PROBE_SPEC")
	if specName == "" {
		specName = "chrome"
	}
	var uconn *utls.UConn
	switch specName {
	case "go":
		uconn = utls.UClient(conn, cfg, utls.HelloGolang)
	default:
		if specName == "chrome" {
			// keep spec from Chrome_Auto above
		} else {
			var id utls.ClientHelloID
			switch specName {
			case "firefox":
				id = utls.HelloFirefox_Auto
			case "ios":
				id = utls.HelloIOS_Auto
			}
			spec, err = utls.UTLSIdToSpec(id)
			if err != nil {
				t.Fatal(err)
			}
			// Firefox specs carry Fake* placeholder extensions whose bytes
			// the ECH inner would reference via outer_extensions — drop them
			kept := spec.Extensions[:0]
			for _, ext := range spec.Extensions {
				tname := reflect.TypeOf(ext).Elem().Name()
				if strings.HasPrefix(tname, "Fake") {
					continue
				}
				kept = append(kept, ext)
			}
			spec.Extensions = kept
			for i, ext := range spec.Extensions {
				if _, ok := ext.(*utls.ALPNExtension); ok {
					spec.Extensions[i] = &utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}}
				}
				if _, ok := ext.(*utls.SNIExtension); ok {
					spec.Extensions[i] = &utls.SNIExtension{ServerName: publicName}
				}
			}
			spec.Extensions = append(spec.Extensions, &utls.GREASEEncryptedClientHelloExtension{})
		}
		uconn = utls.UClient(conn, cfg, utls.HelloCustom)
		if err := uconn.ApplyPreset(&spec); err != nil {
			t.Fatal(err)
		}
	}
	if specName != "go" {
		// (spec already prepared above for chrome)
	}
	t.Logf("using spec: %s", specName)
	uconn.SetDeadline(time.Now().Add(12 * time.Second))

	// inspect the marshaled OUTER ClientHello before sending it
	if err := uconn.BuildHandshakeStateWithoutSession(); err != nil {
		t.Fatalf("build handshake state: %v", err)
	}
	raw := uconn.HandshakeState.Hello.Raw
	if bytes.Contains(raw, []byte(strings.TrimSuffix(host, "."))) {
		t.Fatalf("FATAL: real hostname appears in the plaintext outer ClientHello")
	}
	if !bytes.Contains(raw, []byte(publicName)) {
		t.Fatalf("outer ClientHello does not contain the public name %s", publicName)
	}
	t.Logf("outer ClientHello: %d bytes, SNI masked as %s", len(raw), publicName)

	if err := uconn.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	cs := uconn.ConnectionState()
	if !cs.ECHAccepted {
		t.Fatalf("server did NOT accept ECH (handshake fell back to plaintext SNI)")
	}
	t.Logf("ECHAccepted=%v TLS=%s proto=%q", cs.ECHAccepted, tls.VersionName(cs.Version), cs.NegotiatedProtocol)
}
