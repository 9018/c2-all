//go:build linux
// +build linux

// Package transport — ja4_probe_test.go
//
// Captures the agent's actual ClientHello bytes (a real handshake to the
// relay with the channel's browser specs) and computes the JA4 fingerprint
// from the wire bytes. JA4 part 1 encodes ALPN — the one component where
// our mimicry diverges from real Chrome (we offer http/1.1 only; Chrome
// offers h2,http/1.1). Skips without ECH_PROBE_HOST.
package transport

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// ja4FromRaw parses a ClientHello and computes JA4 (part1_12hex_12hex).
// raw is the ClientHello HANDSHAKE MESSAGE (type+len+body, no record
// header) — utls's PubClientHelloMsg.Raw. Returns the offered ALPN.
func ja4FromRaw(raw []byte) (fp, alpn string, err error) {
	if len(raw) < 43 || raw[0] != 0x01 {
		return "", "", fmt.Errorf("not a ClientHello (%d bytes, type=%x)", len(raw), raw[0])
	}
	b := raw[4:] // skip handshake type + 3-byte length
	b = b[2:]    // client version
	b = b[32:]   // random
	sidLen := int(b[0])
	b = b[1+sidLen:]
	csLen := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	cipherBytes := b[:csLen]
	b = b[csLen:]
	cmLen := int(b[0])
	b = b[1+cmLen:]

	// extensions
	if len(b) < 2 {
		return "", "", fmt.Errorf("no extensions vector")
	}
	extLen := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	extTypes := map[uint16]bool{}
	var ciphers []string
	for i := 0; i < csLen; i += 2 {
		v := binary.BigEndian.Uint16(cipherBytes[i:])
		if v&0x0f0f == 0x0a0a { // GREASE — JA4 excludes it
			continue
		}
		ciphers = append(ciphers, fmt.Sprintf("%04x", v))
	}
	alpnFirst := "00"
	for i := 0; i+4 <= extLen && i+4 <= len(b); {
		et := binary.BigEndian.Uint16(b[i:])
		elen := int(binary.BigEndian.Uint16(b[i+2:]))
		if et&0x0f0f != 0x0a0a { // GREASE excluded from the JA4 list too
			extTypes[et] = true
		}
		if et == 16 { // ALPN
			// ext data: 2-byte total len, then entries of (len8 + name)
			p := b[i+4 : i+4+elen]
			if len(p) >= 2 {
				q := p[2:]
				if len(q) >= 1 {
					pn := int(q[0])
					if pn <= len(q)-1 {
						name := string(q[1 : 1+pn])
						if strings.HasPrefix(name, "http/1") {
							alpnFirst = "h1"
						} else if name == "h2" {
							alpnFirst = "h2"
						} else {
							alpnFirst = name
						}
					}
				}
			}
		}
		i += 4 + elen
	}

	extList := make([]int, 0, len(extTypes))
	for t := range extTypes {
		extList = append(extList, int(t))
	}
	sort.Ints(extList)

	part1 := fmt.Sprintf("t13d%02d%02d%s", len(ciphers), len(extTypes), alpnFirst)

	sort.Strings(ciphers)
	ch := sha256.Sum256([]byte(strings.Join(ciphers, ",")))
	part2 := hex.EncodeToString(ch[:])[:12]

	extStrs := make([]string, 0, len(extList))
	for _, e := range extList {
		extStrs = append(extStrs, fmt.Sprintf("%04x", e))
	}
	sort.Strings(extStrs)
	eh := sha256.Sum256([]byte(strings.Join(extStrs, ",")))
	part3 := hex.EncodeToString(eh[:])[:12]

	return part1 + "_" + part2 + "_" + part3, alpnFirst, nil
}

func TestJA4OfOurHello(t *testing.T) {
	host := os.Getenv("ECH_PROBE_HOST")
	if host == "" {
		t.Skip("set ECH_PROBE_HOST to run")
	}
	for _, alpn := range [][]string{alpnH1, alpnH2Capable} {
		specs, err := browserHelloSpecs(alpn)
		if err != nil || len(specs) == 0 {
			t.Fatalf("browserHelloSpecs: %v", err)
		}
		seen := map[string]string{}
		for i, spec := range specs {
			conn, derr := net.DialTimeout("tcp4", host+":443", 10*time.Second)
			if derr != nil {
				t.Fatalf("dial: %v", derr)
			}
			cfg := &utls.Config{ServerName: host}
			u := utls.UClient(conn, cfg, utls.HelloCustom)
			if aerr := u.ApplyPreset(spec); aerr != nil {
				t.Fatalf("apply preset: %v", aerr)
			}
			u.SetDeadline(time.Now().Add(12 * time.Second))
			if herr := u.Handshake(); herr != nil {
				t.Fatalf("handshake spec[%d]: %v", i, herr)
			}
			raw := u.HandshakeState.Hello.Raw
			fp, gotAlpn, perr := ja4FromRaw(raw)
			if perr != nil {
				t.Fatalf("parse spec[%d]: %v", i, perr)
			}
			neg := u.ConnectionState().NegotiatedProtocol
			seen[fp] = fmt.Sprintf("spec[%d]", i)
			t.Logf("alpn=%v spec[%d]: JA4=%s (offered %s, edge picked %q)", alpn, i, fp, gotAlpn, neg)
			u.Close()
		}
	}
	t.Log("h2-capable hellos must end part1 with 'h2' (real Chrome); h1-only hellos end 'h1'")
}
