package agentutils

import (
	"context"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// externalIPSources are public "echo your IP" endpoints. They are queried
// over plain HTTPS with the system root store — this is not C2 traffic, so
// the standard Go TLS fingerprint is fine here (same trust level as the
// NCSI connectivity probe).
var externalIPSources = []string{
	"https://api.ip.sb/ip",
	"https://ifconfig.me/ip",
	"https://api.ipify.org",
	"https://icanhazip.com",
	"https://checkip.amazonaws.com",
	"https://ipinfo.io/ip",
}

// GetExternalIP resolves this machine's public egress IP: it picks 3 random
// sources from the pool and races them concurrently, returning the first
// valid answer. Returns "" when all fail (e.g. no direct internet access).
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
