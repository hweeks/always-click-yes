package ui

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/codex"
	"github.com/hweeks/always-click-yes/internal/gate"
)

// TestCodexBridgeMergeGuardDeniesGhPrMergeWithNoCountdown is the test that
// proves internal/codex's "Bash" tool name / "command" field choice is
// load-bearing rather than cosmetic: a codex command-execution approval
// whose command is a `gh pr merge` must reach mergeGuardVerdict — through the
// exact translation codex.Bridge uses for a live driver — and come back
// denied without ever being queued for a countdown.
//
// This calls codex.BuildPreToolUseInput directly rather than driving a live
// *codex.Driver end to end: that function is the one piece of the bridge
// callable without a process, and it is the same code forward() calls for a
// real approval request (see internal/codex/gate.go). The wire-level half of
// the bridge — that Attach/forward actually delivers this same
// PreToolUseInput shape off a real Driver's Approvals() stream, and that
// resolving it writes "accept"/"decline" back — is covered by
// internal/codex's own Bridge tests; inventing a hand-rolled
// gate.PreToolUseInput here instead would prove nothing about the naming
// choice actually shipping in the bridge.
func TestCodexBridgeMergeGuardDeniesGhPrMergeWithNoCountdown(t *testing.T) {
	req := codex.ApprovalRequest{
		ID:     0,
		Method: "item/commandExecution/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread-1","itemId":"item-1","cwd":"/work","command":"gh pr merge 3"}`),
	}
	in, ok := codex.BuildPreToolUseInput(req)
	if !ok {
		t.Fatal("BuildPreToolUseInput rejected a well-formed commandExecution approval")
	}
	if in.ToolName != "Bash" {
		t.Fatalf("ToolName = %q, want Bash — mergeGuardVerdict only ever inspects a tool literally named Bash", in.ToolName)
	}

	p, decisions := gate.NewPending(in)

	m := New(nil, Config{Countdown: 30 * time.Second})
	m.now = time.Now()
	m.enqueue(p)

	if len(m.pending) != 0 {
		t.Fatalf("gh pr merge 3 arriving via codex was queued for a countdown (%d pending); the merge guard must deny it outright", len(m.pending))
	}
	select {
	case d := <-decisions:
		if d.Behavior != gate.Deny {
			t.Fatalf("decision = %+v, want deny", d)
		}
	case <-time.After(time.Second):
		t.Fatal("no decision — enqueue never resolved the pending")
	}
}
