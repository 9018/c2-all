package server

import (
	"os"
	"testing"

	"github.com/jm33-m0/emp3r0r/core/internal/live"
)

// TestRepointOldWorkerToNewRelay sets the MIGRATE_URL on the old account so
// agents re-dial the new relay. Used to recover the manual migration run.
func TestRepointOldWorkerToNewRelay(t *testing.T) {
	live.EmpWorkSpace = os.Getenv("EMP3R0R_WORKSPACE")
	if os.Getenv("CF_REPOINT_E2E") == "" {
		t.Skip("set CF_REPOINT_E2E=1")
	}
	cfg, err := loadCFAccounts()
	if err != nil {
		t.Fatal(err)
	}
	oldID := os.Getenv("OLD_ACCOUNT")
	newRelay := os.Getenv("NEW_RELAY")
	if oldID == "" || newRelay == "" {
		t.Fatal("need OLD_ACCOUNT and NEW_RELAY")
	}
	old := cfg.findByID(oldID)
	if old == nil {
		t.Fatalf("old account %s not in fleet", oldID)
	}
	secret := cfg.SharedSecret
	if secret == "" {
		t.Fatal("no shared secret")
	}
	migrateURL := newRelay + "/ws/prod-room-a?role=agent&secret=" + secret
	if err := pointWorkerAtMigration(old, cfg.WorkerName, secret, migrateURL); err != nil {
		t.Fatalf("pointWorkerAtMigration: %v", err)
	}
	t.Logf("old worker repointed to %s", newRelay)
}
