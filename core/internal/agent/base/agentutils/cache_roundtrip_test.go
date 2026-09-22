package agentutils

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"

	"github.com/jm33-m0/emp3r0r/core/internal/agent/base/common"
	"github.com/jm33-m0/emp3r0r/core/internal/def"
)

func withTempCache(t *testing.T) {
	t.Helper()
	common.RuntimeConfig = &def.Config{
		Password:        "test-password-roundtrip",
		AgentUUID:       "11111111-2222-3333-4444-555555555555",
		AgentUUIDParent: "11111111-2222-3333-4444-555555555555",
		MultiHost:       true,
	}
	// keyCachePath derives from HOME
	t.Setenv("HOME", t.TempDir())
}

// TestFirstRunMintPersistsPerHostUUID: first run on a host mints a per-host
// UUID and the cache carries it — NOT the build UUID.
func TestFirstRunMintPersistsPerHostUUID(t *testing.T) {
	withTempCache(t)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	hostUUID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	// ApplyHostIdentity mint branch
	if err := saveCachedIdentity(key, hostUUID); err != nil {
		t.Fatalf("save: %v", err)
	}

	gotKey, gotUUID, err := loadCachedIdentity()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if gotUUID != hostUUID {
		t.Fatalf("minted UUID lost: got %q want %q", gotUUID, hostUUID)
	}
	if gotKey.PublicKey.X.Cmp(key.PublicKey.X) != 0 {
		t.Fatalf("key mismatch")
	}

	// key-only writer (single-host path) must PRESERVE the uuid
	key2, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err := saveCachedAgentKey(key2); err != nil {
		t.Fatalf("save2: %v", err)
	}
	_, gotUUID2, err := loadCachedIdentity()
	if err != nil {
		t.Fatalf("load2: %v", err)
	}
	if gotUUID2 != hostUUID {
		t.Fatalf("key-only writer clobbered the UUID: got %q want %q", gotUUID2, hostUUID)
	}
}

// TestCacheRoundTripAcrossRestart: reboot path restores key AND uuid.
func TestCacheRoundTripAcrossRestart(t *testing.T) {
	withTempCache(t)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	if err := saveCachedIdentity(key, common.RuntimeConfig.AgentUUID); err != nil {
		t.Fatalf("save: %v", err)
	}
	gotKey, gotUUID, err := loadCachedIdentity()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if gotUUID != common.RuntimeConfig.AgentUUID {
		t.Fatalf("UUID lost: got %q want %q", gotUUID, common.RuntimeConfig.AgentUUID)
	}
	if gotKey.PublicKey.X.Cmp(key.PublicKey.X) != 0 {
		t.Fatalf("key mismatch")
	}
}
