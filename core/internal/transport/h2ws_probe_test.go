//go:build linux
// +build linux

// Package transport — h2ws_probe_test.go
//
// Phase A of T2 (WebSocket-over-HTTP/2): probes whether the CF edge
// accepts an RFC 8441 extended CONNECT against our relay Worker. Flow:
// browser-spec uTLS hello offering h2,http/1.1 -> h2 preface + SETTINGS ->
// extended CONNECT (:protocol websocket) on stream 1 -> read the response
// headers -> WS ping/pong inside DATA frames. Run with ECH_PROBE_HOST +
// H2WS_PROBE=1. Skips without them.
package transport

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func TestH2WSProbe(t *testing.T) {
	host := os.Getenv("ECH_PROBE_HOST")
	if host == "" || os.Getenv("H2WS_PROBE") == "" {
		t.Skip("set ECH_PROBE_HOST + H2WS_PROBE=1 to run")
	}

	// 1. TCP + browser-spec TLS offering h2,http/1.1
	conn, err := net.DialTimeout("tcp4", host+":443", 10*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	cfg := &utls.Config{ServerName: host}
	specs, err := browserHelloSpecs(alpnH2Capable)
	if err != nil || len(specs) == 0 {
		t.Fatalf("specs: %v", err)
	}
	u := utls.UClient(conn, cfg, utls.HelloCustom)
	if err := u.ApplyPreset(specs[0]); err != nil { // Chrome spec
		t.Fatalf("preset: %v", err)
	}
	u.SetDeadline(time.Now().Add(15 * time.Second))
	if err := u.Handshake(); err != nil {
		t.Fatalf("tls: %v", err)
	}
	np := u.ConnectionState().NegotiatedProtocol
	t.Logf("TLS negotiated: %q", np)
	if np != "h2" {
		t.Fatalf("edge did not negotiate h2 (%q) — h2ws path dead here", np)
	}

	// 2. h2 preface + SETTINGS
	u.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := u.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatalf("preface: %v", err)
	}
	fr := http2.NewFramer(u, u)
	fr.ReadMetaHeaders = hpack.NewDecoder(4096, func(hf hpack.HeaderField) {})
	if err := fr.WriteSettings(
		http2.Setting{ID: http2.SettingInitialWindowSize, Val: 8 << 20},
	); err != nil {
		t.Fatalf("settings: %v", err)
	}
	if err := fr.WriteWindowUpdate(0, 16<<20); err != nil {
		t.Fatalf("conn window: %v", err)
	}

	// 3. extended CONNECT headers on stream 1
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	var hb bytes.Buffer
	he := hpack.NewEncoder(&hb)
	for _, hf := range []hpack.HeaderField{
		{Name: ":method", Value: "CONNECT"},
		{Name: ":protocol", Value: "websocket"},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: host},
		{Name: ":path", Value: "/ws/t2-probe-room"},
		{Name: "sec-websocket-version", Value: "13"},
		{Name: "sec-websocket-key", Value: base64.StdEncoding.EncodeToString(key)},
		{Name: "user-agent", Value: BrowserUserAgent()},
		{Name: "origin", Value: "https://" + host},
	} {
		if err := he.WriteField(hf); err != nil {
			t.Fatalf("hpack: %v", err)
		}
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID: 1, BlockFragment: hb.Bytes(), EndHeaders: true,
	}); err != nil {
		t.Fatalf("headers: %v", err)
	}

	// 4. read frames until the response HEADERS
	var status string
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		switch ff := f.(type) {
		case *http2.SettingsFrame:
			if !ff.Header().Flags.Has(http2.FlagSettingsAck) {
				// does the edge advertise RFC 8441 support at all?
				if v, _ := ff.Value(http2.SettingEnableConnectProtocol); v != 0 {
					t.Logf("server SETTINGS: ENABLE_CONNECT_PROTOCOL=1 (RFC 8441 advertised)")
				} else {
					t.Logf("server SETTINGS: ENABLE_CONNECT_PROTOCOL NOT advertised — Chrome would never attempt h2-WS here")
				}
				// also log a few interesting ones
				for _, s := range []http2.SettingID{http2.SettingMaxFrameSize, http2.SettingInitialWindowSize, http2.SettingMaxConcurrentStreams} {
					if v, ok := ff.Value(s); ok {
						t.Logf("server SETTINGS: %v=%d", s, v)
					}
				}
				if err := fr.WriteSettingsAck(); err != nil {
					t.Fatalf("settings ack: %v", err)
				}
			}
		case *http2.MetaHeadersFrame:
			for _, hf := range ff.Fields {
				if hf.Name == ":status" {
					status = hf.Value
				}
				t.Logf("resp header: %s: %s", hf.Name, hf.Value)
			}
			t.Logf("response headers on stream %d (END_STREAM=%v)", ff.Header().StreamID,
				ff.Header().Flags.Has(http2.FlagHeadersEndStream))
			if status == "" {
				t.Fatalf("no :status in response")
			}
			goto gotHeaders
		case *http2.GoAwayFrame:
			t.Fatalf("GOAWAY errcode=%v debug=%q", ff.ErrCode, ff.DebugData())
		case *http2.PingFrame:
			if !ff.Flags.Has(http2.FlagPingAck) {
				_ = fr.WritePing(true, ff.Data)
			}
		}
	}
gotHeaders:
	t.Logf("extended CONNECT status: %s", status)

	// 5. WS ping inside DATA if accepted
	if status == "200" {
		// masked WS ping frame: FIN|opcode=0x9, mask bit, len 9, payload keepalive
		mask := make([]byte, 4)
		_, _ = rand.Read(mask)
		payload := []byte("keepalive")
		frame := []byte{0x89, 0x80 | byte(len(payload))}
		frame = append(frame, mask...)
		for i, b := range payload {
			frame = append(frame, b^mask[i%4])
		}
		if err := fr.WriteData(1, false, frame); err != nil {
			t.Fatalf("ws ping data: %v", err)
		}
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			u.SetReadDeadline(time.Now().Add(3 * time.Second))
			f, err := fr.ReadFrame()
			if err != nil {
				t.Logf("read while waiting pong: %v", err)
				if os.IsTimeout(err) || strings.Contains(err.Error(), "timeout") {
					continue
				}
				break
			}
			switch ff := f.(type) {
			case *http2.DataFrame:
				// unmask? server frames are NOT masked (server->client)
				if len(ff.Data()) >= 2 && ff.Data()[0]&0x0f == 0x0a {
					t.Logf("WS PONG received inside DATA: %x", ff.Data()[:min(len(ff.Data()), 12)])
					t.Log("RFC 8441 WORKS against this relay Worker — T2 is GO")
					return
				}
				t.Logf("data frame: %d bytes", len(ff.Data()))
			case *http2.MetaHeadersFrame:
				for _, hf := range ff.Fields {
					t.Logf("trailers: %s: %s", hf.Name, hf.Value)
				}
			case *http2.PingFrame:
				if !ff.Flags.Has(http2.FlagPingAck) {
					_ = fr.WritePing(true, ff.Data)
				}
			}
		}
		t.Log("WS accepted but no pong — partial support?")
	} else {
		t.Logf("CF rejected extended CONNECT (%s) — Workers h2-WS NOT supported; T2 fallback stays h1", status)
	}
	_ = io.EOF
}
