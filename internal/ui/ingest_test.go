package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/mcp"
)

// TestIngest feeds a captured turn through the model's ingest logic and checks
// that header state and transcript lines come out right — no TTY required.
func TestIngest(t *testing.T) {
	m := New(nil, Config{})

	lines := []string{
		`{"type":"system","subtype":"init","session_id":"abcdef1234","model":"claude-sonnet-5","permissionMode":"default"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"echo hi","description":"say hi"}}]}}`,
		`{"type":"user","message":{"content":[{"tool_use_id":"t1","type":"tool_result","content":"hi","is_error":false}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}`,
		`{"type":"result","subtype":"success","stop_reason":"end_turn","terminal_reason":"completed","total_cost_usd":0.5}`,
	}
	for _, l := range lines {
		ev, err := driver.Decode([]byte(l))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		m.ingest(ev)
	}

	if m.sessionID != "abcdef1234" || m.model != "claude-sonnet-5" || m.mode != "default" {
		t.Errorf("header state wrong: %+v", m)
	}
	if m.totalCost() != 0.5 {
		t.Errorf("totalCost() = %v, want 0.5", m.totalCost())
	}
	if m.status != "idle" {
		t.Errorf("status = %q, want idle", m.status)
	}

	joined := m.transcript()
	for _, want := range []string{"session abcdef12", "Bash", "echo hi", "hi", "done", "turn complete", "$0.5000"} {
		if !strings.Contains(joined, want) {
			t.Errorf("transcript missing %q\n---\n%s", want, joined)
		}
	}
}

// TestIngestPlan verifies ExitPlanMode is rendered as a plan and flips planReady.
func TestIngestPlan(t *testing.T) {
	m := New(nil, Config{})
	ev, err := driver.Decode([]byte(
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t9","name":"ExitPlanMode","input":{"plan":"1. do X\n2. do Y"}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	m.ingest(ev)
	if !m.planReady {
		t.Fatal("planReady should be set after ExitPlanMode")
	}
	found := false
	for _, e := range m.entries {
		if e.kind == ePlan && strings.Contains(e.body, "do X") && strings.Contains(e.body, "do Y") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a plan entry with the full plan text; entries=%+v", m.entries)
	}
}

// TestIngestCodexMcpPresentPlanShowsThePlanBox proves the other half of the
// codex plan-box fix end to end, at the boundary that actually matters: a
// driver.Event carrying the exact tool_use shape the codex driver's
// emitMcpToolCall now produces (internal/codex/translate.go) — a qualified
// "mcp__acy__PresentPlan" name, not the bare "ExitPlanMode" claude's driver
// uses — drives ui.Model to the plan box the same way TestIngestPlan already
// proves for claude. Before internal/codex/translate.go grew its "mcpToolCall"
// case, no event with this shape was ever emitted by that driver at all: this
// test only confirms ui.Model's side was never the problem, which is also why
// it is a synthesized event rather than a replay of the codex wire fixture —
// that replay lives in internal/codex/driver_test.go's
// TestMcpToolCallItemEmitsQualifiedToolUseAndResult.
func TestIngestCodexMcpPresentPlanShowsThePlanBox(t *testing.T) {
	m := New(nil, Config{})
	line := fmt.Sprintf(
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t9","name":%q,"input":{"plan":"1. do X\n2. do Y"}}]}}`,
		mcp.Qualified(mcp.ToolPlan))
	ev, err := driver.Decode([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	m.ingest(ev)

	if !m.planReady {
		t.Fatal("planReady should be set after mcp__acy__PresentPlan")
	}
	if !strings.Contains(m.planBody, "do X") || !strings.Contains(m.planBody, "do Y") {
		t.Errorf("planBody = %q, want the full plan text", m.planBody)
	}
}

// TestIngestCodexMcpFinishEndsTheRun is TestIngestCodexMcpPresentPlanShowsThePlanBox's
// counterpart for Finish, the other tool internal/cli/mcp.go answers locally
// (see its own comment on why: "the supervisor sees the tool_use and moves
// the phase itself"). Same shape of proof: a qualified "mcp__acy__Finish"
// tool_use, decoded exactly as codex's driver would emit it, must still end
// the run.
func TestIngestCodexMcpFinishEndsTheRun(t *testing.T) {
	m := autoRunModel()
	line := fmt.Sprintf(
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t10","name":%q,"input":{"outcome":"completed","summary":"Added the codex mcpToolCall translation and verified the plan box appears."}}]}}`,
		mcp.Qualified(mcp.ToolFinish))
	ev, err := driver.Decode([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	m.ingest(ev)

	if m.phase != PhaseComplete {
		t.Fatalf("phase = %v, want COMPLETE", m.phase)
	}
	if m.FinishOutcome() != "completed" {
		t.Errorf("FinishOutcome() = %q, want %q", m.FinishOutcome(), "completed")
	}
	if !strings.Contains(m.FinishSummary(), "codex mcpToolCall translation") {
		t.Errorf("FinishSummary() = %q", m.FinishSummary())
	}
}

// TestIngestForeignMcpServerFinishDoesNotEndTheRun is the negative case the
// two tests above don't cover: a codex thread's MCP servers are not limited
// to acy's own (`codex mcp add` writes into ~/.codex/config.toml, merged
// with acy's inline config rather than replaced by it — see
// internal/codex/translate.go's emitMcpToolCall comment), so a *different*
// server can expose a tool that happens to share a name with one of acy's
// own reserved ones. A tool_use named "mcp__some-other-server__Finish" must
// not be mistaken for acy's own Finish: baseToolName alone would strip it
// down to the bare "Finish" regardless of server, which is exactly why
// ingestToolUse dispatches on acyDispatchName instead — this proves that
// distinction holds by driving a real Model rather than just asserting on
// the block's Name string, which "the run silently ended" could pass even
// if the underlying check were dropped.
func TestIngestForeignMcpServerFinishDoesNotEndTheRun(t *testing.T) {
	m := autoRunModel()
	const foreignName = "mcp__some-other-server__Finish"
	line := fmt.Sprintf(
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t11","name":%q,"input":{"outcome":"completed","summary":"a foreign server's tool, not acy's Finish"}}]}}`,
		foreignName)
	ev, err := driver.Decode([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	m.ingest(ev)

	if m.phase == PhaseComplete {
		t.Fatalf("phase = COMPLETE; a %q tool_use from a non-acy MCP server must not end the run", foreignName)
	}
}
