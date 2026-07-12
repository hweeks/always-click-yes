package cli

import (
	"encoding/json"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/hweeks/always-click-yes/internal/gate"
)

// newHookCmd is the hidden subcommand that claude spawns as the PreToolUse hook.
// It reads the tool-call JSON on stdin, forwards it to the supervisor's gate
// socket, blocks for an allow/deny decision, and prints claude's hook output.
func newHookCmd() *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:    "hook",
		Short:  "Internal: PreToolUse hook handler (spawned by claude)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, _ := io.ReadAll(os.Stdin)

			d, err := gate.Ask(socket, raw)
			if err != nil {
				// Fail closed: if the supervisor is unreachable, deny rather than
				// run an unapproved tool.
				d = gate.Decision{Behavior: gate.Deny, Reason: "supervisor unavailable: " + err.Error()}
			}
			return json.NewEncoder(os.Stdout).Encode(gate.NewHookOutput(d))
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "supervisor gate socket path")
	return cmd
}
