package c2transport

import (
	"strings"
	"testing"

	"github.com/jm33-m0/emp3r0r/core/internal/agent/base/common"
	"github.com/jm33-m0/emp3r0r/core/internal/def"
)

func TestRotateDoH(t *testing.T) {
	doH := "https://relay-a.example/dns?secret=s"
	origDoH := common.RuntimeConfig.DoHServer
	defer func() {
		common.RuntimeConfig.DoHServer = origDoH
	}()

	// derivation helper
	if got := deriveDoH("wss://relay-b.example/ws/room-b?role=agent&secret=s");
		got != "https://relay-b.example/dns?secret=s" {
		t.Fatalf("deriveDoH: got %q", got)
	}
	if got := deriveDoH("wss://relay-b.example/ws/room-b?role=agent"); got != "" {
		t.Fatalf("deriveDoH without secret must be empty, got %q", got)
	}

	// same host as failed endpoint -> DoH config follows (resolver swap itself
	// needs a live DoH backend, so only the config string is asserted here)
	common.RuntimeConfig.DoHServer = doH
	rotateDoH("wss://relay-a.example/ws/room-a?role=agent&secret=s",
		"wss://1.1.1.1/ws/room-b?role=agent&secret=s")
	if common.RuntimeConfig.DoHServer != "https://1.1.1.1/dns?secret=s" {
		t.Fatalf("DoH should be re-homed, got %s", common.RuntimeConfig.DoHServer)
	}

	// DoH host differs from failed endpoint -> untouched
	common.RuntimeConfig.DoHServer = "https://9.9.9.9/dns-query"
	rotateDoH("wss://relay-a.example/ws/room-a?role=agent&secret=s",
		"wss://relay-b.example/ws/room-b?role=agent&secret=s")
	if common.RuntimeConfig.DoHServer != "https://9.9.9.9/dns-query" {
		t.Fatalf("unrelated DoH must stay, got %s", common.RuntimeConfig.DoHServer)
	}

	// no DoH configured -> no-op
	common.RuntimeConfig.DoHServer = ""
	rotateDoH("wss://relay-a.example/ws/room-a?role=agent&secret=s",
		"wss://relay-b.example/ws/room-b?role=agent&secret=s")
	if common.RuntimeConfig.DoHServer != "" {
		t.Fatal("empty DoH must stay empty")
	}
}

func TestNextRelayEndpoint(t *testing.T) {
	rooms := []string{"wss://relay-a.example/ws/room-a?role=agent&secret=s",
		"wss://relay-b.example/ws/room-b?role=agent&secret=s"}
	origCC := def.CCAddress
	origURLs := common.RuntimeConfig.RelayURLs
	defer func() {
		def.CCAddress = origCC
		common.RuntimeConfig.RelayURLs = origURLs
	}()

	// ── multi-endpoint rotation ──
	common.RuntimeConfig.RelayURLs = rooms
	def.CCAddress = rooms[0]
	nextRelayEndpoint(rooms[0])
	if def.CCAddress != rooms[1] {
		t.Fatalf("expected failover to endpoint[1], got %s", maskURLSecret(def.CCAddress))
	}
	nextRelayEndpoint(rooms[1])
	if def.CCAddress != rooms[0] {
		t.Fatalf("expected wrap-around to endpoint[0], got %s", maskURLSecret(def.CCAddress))
	}

	// ── unknown URL: no rotation ──
	def.CCAddress = rooms[0]
	nextRelayEndpoint("wss://unknown.example/ws/x?role=agent&secret=s")
	if def.CCAddress != rooms[0] {
		t.Fatalf("unknown URL must not rotate, got %s", maskURLSecret(def.CCAddress))
	}

	// ── direct (non-relay) C2: never rotate ──
	def.CCAddress = "http://10.0.0.1:7000"
	nextRelayEndpoint("http://10.0.0.1:7000")
	if def.CCAddress != "http://10.0.0.1:7000" {
		t.Fatalf("direct mode must never rotate, got %s", maskURLSecret(def.CCAddress))
	}

	// ── CC-role URLs in agent config: not agent endpoints, no rotation ──
	common.RuntimeConfig.RelayURLs = []string{"wss://relay-a.example/ws/room-a?role=cc&secret=s",
		"wss://relay-b.example/ws/room-b?role=cc&secret=s"}
	def.CCAddress = "wss://relay-a.example/ws/room-a?role=agent&secret=s"
	nextRelayEndpoint(def.CCAddress)
	if !strings.HasPrefix(def.CCAddress, "wss://relay-a.example") {
		t.Fatalf("CC-role URL list must not drive agent rotation, got %s", maskURLSecret(def.CCAddress))
	}

	// ── single endpoint: nothing to rotate to ──
	common.RuntimeConfig.RelayURLs = rooms[:1]
	def.CCAddress = rooms[0]
	nextRelayEndpoint(rooms[0])
	if def.CCAddress != rooms[0] {
		t.Fatalf("single endpoint must not rotate, got %s", maskURLSecret(def.CCAddress))
	}
}
