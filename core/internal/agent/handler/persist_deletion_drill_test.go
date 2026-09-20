package handler

// persist_deletion_drill_test.go — REAL-system drill (gated): proves what
// happens to the persistence auto-start when the on-disk copy is deleted.
// Uses a throwaway cover name and cleans everything up.
//
// Expected: the unit file survives (enabled), but ExecStart loops 203/EXEC
// (binary missing) — no process ever runs. Deleting the copy therefore
// kills the persistence EFFECT even though the unit still fires.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jm33-m0/emp3r0r/core/lib/util"
)

func TestPersistDeletionKillsAutostart(t *testing.T) {
	if os.Getenv("PERSIST_REAL_DRILL") == "" {
		t.Skip("set PERSIST_REAL_DRILL=1 to run against the real user session")
	}
	const name = "persist-drill-probe"
	home, err := persistHome()
	if err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(home, ".local", "bin", name)
	t.Cleanup(func() {
		_ = persistSystemdRemove(binPath)
		_ = os.Remove(binPath)
	})

	// 1. drop a harmless fake image + install systemd
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(binPath, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	util.BackdateFile(binPath, 20, 180)
	if _, err := persistSystemdInstall(binPath, []string{binPath}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if ok, _ := persistSystemdStatus(binPath); !ok {
		t.Fatal("precondition: unit should be installed")
	}

	// 2. while a client would be running from memfd, delete the disk copy
	if err := os.Remove(binPath); err != nil {
		t.Fatal(err)
	}

	// 3. does auto-start still work? Type=simple reports "active" the instant
	// the fork succeeds; the exec failure (203/EXEC = binary missing) lands
	// asynchronously. The honest signal: ExecMainStatus=203 and the unit
	// never reaches a stable running state.
	_, _ = systemctlUser("restart", name+".service")
	execFailed := false
	for i := 0; i < 10; i++ { // RestartSec is 20-90s; poll while it loops
		out, err := systemctlUser("show", name+".service", "-p", "ExecMainStatus", "--value")
		status := strings.TrimSpace(out)
		if err == nil && status == "203" {
			execFailed = true
			break
		}
		t.Logf("poll %d: ExecMainStatus=%q (err=%v)", i, status, err)
		time.Sleep(3 * time.Second)
	}
	if !execFailed {
		t.Fatal("no 203/EXEC observed — the unit seems able to run without its on-disk binary")
	}
	t.Log("deletion confirmed to kill autostart: ExecStart loops 203/EXEC, no process ever runs")
	if ok, _ := persistSystemdStatus(binPath); ok {
		t.Log("note: unit file still present (enabled but broken) — status reports it")
	}
}
