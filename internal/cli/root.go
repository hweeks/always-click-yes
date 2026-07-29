// Package cli wires the Cobra command tree for always-click-yes.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/hweeks/always-click-yes/internal/version"
)

// Root returns the root command. With no subcommand it runs the supervisor TUI.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:   "always-click-yes",
		Short: "Babysit a long-running Claude Code task and auto-approve its prompts",
		Long: "always-click-yes supervises a Claude Code session: you plan a task,\n" +
			"arm it, and it auto-approves each permission prompt after a short,\n" +
			"interruptible countdown — then checks whether the plan is complete.",
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("{{.Name}} {{.Version}}\n")

	runCmd := newRunCmd()
	root.AddCommand(runCmd)
	// serve is a documented sibling of run, not a hidden command: it is how a
	// webview client (and anything else that is not a terminal) supervises a run.
	root.AddCommand(newServeCmd())
	root.AddCommand(newHookCmd())
	root.AddCommand(newMCPCmd())

	// Default to `run` when invoked with no subcommand.
	root.RunE = runCmd.RunE
	root.Flags().AddFlagSet(runCmd.Flags())

	return root
}
