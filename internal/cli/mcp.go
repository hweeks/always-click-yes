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
	var socket, role string
	cmd := &cobra.Command{
		Use:    "mcp",
		Short:  "Internal: MCP server exposing acy's UI tools (spawned by claude)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			r := mcp.ParseRole(role)
			return mcp.Serve(os.Stdin, os.Stdout, r, func(name string, args json.RawMessage, toolUseID string) (string, error) {
				switch name {
				case mcp.ToolAsk, mcp.ToolDispatch:
					// Both block on the supervisor: an ask until a human answers,
					// a dispatch until a whole child process has run its task.
					// mcp.Ask's read is deliberately unbounded, which is already
					// what a twenty-minute task needs.
					a, err := mcp.Ask(socket, mcp.Request{
						Tool: name, ToolUseID: toolUseID, Role: r, Args: args,
					})
					if err != nil {
						// Fail open, unlike the gate's fail-closed deny: claude's turn is
						// blocked on this reply, so an unreachable supervisor must yield a
						// useless answer rather than a hung session.
						if name == mcp.ToolDispatch {
							return mcp.DispatchUnavailable, nil
						}
						return mcp.SupervisorGone, nil
					}
					return a.Text, nil

				case mcp.ToolPlan:
					// Answered locally. The supervisor reads the plan out of the ordinary
					// tool_use event, so there is nothing to wait for here.
					return mcp.PlanRecorded, nil

				case mcp.ToolFinish:
					// Also local, for the same reason: the supervisor sees the tool_use
					// and moves the phase itself.
					return mcp.FinishRecorded, nil
				}
				return "", fmt.Errorf("unknown tool: %s", name)
			})
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "supervisor ask socket path")
	cmd.Flags().StringVar(&role, "role", string(mcp.RoleParent), "which side of the run this serves: parent or child")
	return cmd
}
