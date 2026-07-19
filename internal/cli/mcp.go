package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hweeks/always-click-yes/internal/mcp"
)

// newMCPCmd is the hidden subcommand claude spawns as an MCP server (via the
// generated --mcp-config). It speaks JSON-RPC on stdin/stdout and relays
// AskUserQuestion to the supervisor's ask socket, where the TUI's picker answers
// it. The binary is its own MCP server, as it is already its own PreToolUse hook.
//
// It deliberately does not log: alog.Open truncates, and this process shares the
// supervisor's log path. claude also swallows an MCP server's stderr, so there is
// nowhere useful to write anyway — the supervisor logs both sides of the bridge.
func newMCPCmd() *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:    "mcp",
		Short:  "Internal: MCP server exposing acy's UI tools (spawned by claude)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return mcp.Serve(os.Stdin, os.Stdout, func(name string, args json.RawMessage, toolUseID string) (string, error) {
				switch name {
				case mcp.ToolAsk:
					a, err := mcp.Ask(socket, mcp.Request{Tool: name, ToolUseID: toolUseID, Args: args})
					if err != nil {
						// Fail open, unlike the gate's fail-closed deny: claude's turn is
						// blocked on this reply, so an unreachable supervisor must yield a
						// useless answer rather than a hung session.
						return mcp.SupervisorGone, nil
					}
					return a.Text, nil

				case mcp.ToolPlan:
					// Answered locally. The supervisor reads the plan out of the ordinary
					// tool_use event, so there is nothing to wait for here.
					return mcp.PlanRecorded, nil
				}
				return "", fmt.Errorf("unknown tool: %s", name)
			})
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "supervisor ask socket path")
	return cmd
}
