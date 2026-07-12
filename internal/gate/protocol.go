// Package gate implements the permission-approval bridge between claude's
// PreToolUse hook and the supervisor TUI. The hook (a short-lived process claude
// spawns per gated tool) connects to a unix socket, forwards the tool request,
// and blocks for an allow/deny decision that the TUI produces after a countdown.
package gate

import "encoding/json"

// Decision behaviors.
const (
	Allow = "allow"
	Deny  = "deny"
)

// PreToolUseInput is the JSON claude writes to a PreToolUse hook on stdin.
// (Fields captured live from claude 2.1.207; extra fields are ignored.)
type PreToolUseInput struct {
	SessionID      string          `json:"session_id"`
	Cwd            string          `json:"cwd"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolUseID      string          `json:"tool_use_id"`
	PermissionMode string          `json:"permission_mode"`
}

// Decision is the supervisor's answer sent back over the socket.
type Decision struct {
	Behavior string `json:"behavior"` // Allow or Deny
	Reason   string `json:"reason,omitempty"`
}

// HookOutput is what the hook prints on stdout for claude to consume.
type HookOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"` // allow|deny|ask
		PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	} `json:"hookSpecificOutput"`
}

// NewHookOutput builds the claude-facing hook output for a decision.
func NewHookOutput(d Decision) HookOutput {
	var out HookOutput
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	out.HookSpecificOutput.PermissionDecision = d.Behavior
	out.HookSpecificOutput.PermissionDecisionReason = d.Reason
	return out
}
