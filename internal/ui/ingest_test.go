package ui

import (
	"strings"
	"testing"

	"github.com/hweeks/always-click-yes/internal/driver"
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
	if m.cost != 0.5 {
		t.Errorf("cost = %v, want 0.5", m.cost)
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
