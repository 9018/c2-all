package transport

// ja4_peek_analyze_test.go — computes the JA4 of a captured ClientHello
// (e.g. a real Chromium wss:// hello from the /tmp/wspeek harness) using
// the same wire parser as TestJA4OfOurHello. Gated by JA4_PEEK_FILE.

import (
	"encoding/binary"
	"net"
	"os"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

func TestJA4AnalyzePeekedHello(t *testing.T) {
	path := os.Getenv("JA4_PEEK_FILE")
	if path == "" {
		t.Skip("set JA4_PEEK_FILE")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fp, alpn, perr := ja4FromRaw(raw[5:]) // strip the record header
	if perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	t.Logf("%s: JA4=%s alpn_first=%s", path, fp, alpn)
}

func TestJA4ExtListDiff(t *testing.T) {
	path := os.Getenv("JA4_PEEK_FILE")
	if path == "" {
		t.Skip("set JA4_PEEK_FILE")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// dump the extension TYPE list of the captured hello
	types := extTypesOf(raw[5:])
	t.Logf("captured (%d exts): %v", len(types), types)

	specs, err := browserHelloSpecs(alpnH1)
	if err != nil {
		t.Fatal(err)
	}
	for i := range specs {
		conn, derr := net.DialTimeout("tcp4", os.Getenv("ECH_PROBE_HOST")+":443", 8*time.Second)
		if derr != nil {
			t.Skipf("dial: %v", derr)
		}
		cfg := &utls.Config{ServerName: os.Getenv("ECH_PROBE_HOST")}
		u := utls.UClient(conn, cfg, utls.HelloCustom)
		if aerr := u.ApplyPreset(specs[i]); aerr != nil {
			t.Fatal(aerr)
		}
		u.SetDeadline(time.Now().Add(10 * time.Second))
		if herr := u.Handshake(); herr != nil {
			t.Skipf("handshake: %v", herr)
		}
		ours := extTypesOf(u.HandshakeState.Hello.Raw)
		t.Logf("parrot[%d] (%d exts): %v", i, len(ours), ours)
		u.Close()
	}
}

// extTypesOf parses a raw ClientHello handshake message and returns the
// extension TYPE list in wire order (GREASE excluded).
func extTypesOf(raw []byte) []uint16 {
	b := raw[4:]
	b = b[2:]  // version
	b = b[32:] // random
	sid := int(b[0])
	b = b[1+sid:]
	cs := int(binary.BigEndian.Uint16(b))
	b = b[2+cs:]
	cm := int(b[0])
	b = b[1+cm:]
	if len(b) < 2 {
		return nil
	}
	extLen := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	var out []uint16
	for i := 0; i+4 <= extLen && i+4 <= len(b); {
		et := binary.BigEndian.Uint16(b[i:])
		elen := int(binary.BigEndian.Uint16(b[i+2:]))
		if et&0x0f0f != 0x0a0a {
			out = append(out, et)
		}
		i += 4 + elen
	}
	return out
}
