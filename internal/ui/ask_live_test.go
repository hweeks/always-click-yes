package ui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/hweeks/always-click-yes/internal/driver"
)

// TestLiveAskUserQuestion is the only test that proves acy's AskUserQuestion
// wiring matches what claude actually sends and actually accepts. Every other ask
// test is checked against testdata/ask_tool_use.json — a payload we captured, not
// one claude produced on the spot — so they can only prove acy agrees with
// itself. This one proves acy agrees with claude, on both halves of the exchange:
//
//   - the input schema: parseAsk must accept the real tool_use input;
//   - the output shape: claude must accept the plain-string tool_result acy sends
//     back (ask.go submitAsk) and carry on with the turn rather than erroring.
//
// The second half is the one that was never verified. The turn is blocked on this
// tool result, so if the shape is wrong the session hangs — and no offline test
// can catch it.
//
// KNOWN TO FAIL against claude 2.1.207, and the failure is the point: `claude -p`
// does not offer AskUserQuestion at all. Its system/init event advertises a fixed
// 30-tool registry containing neither AskUserQuestion nor ExitPlanMode, in every
// --permission-mode, with and without --allowedTools. They appear to be
// interactive-TUI-only tools. Since acy always spawns with -p, claude can never
// emit the tool_use that opens the ask panel.
//
// So this test is a live probe of that constraint, not a regression test: when it
// starts passing, headless AskUserQuestion has become real (or acy has moved the
// question onto an MCP tool, which claude *does* put in the registry) and the
// captured fixture is worth refreshing.
//
//	ACY_LIVE=1 go test ./internal/ui/ -run TestLiveAskUserQuestion -v
//
// Add ACY_UPDATE_FIXTURE=1 to rewrite testdata/ask_tool_use.json from the line
// claude emitted, refreshing the offline tests against the real schema.
func TestLiveAskUserQuestion(t *testing.T) {
	if os.Getenv("ACY_LIVE") == "" {
		t.Skip("set ACY_LIVE=1 to run the live AskUserQuestion test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Plan mode with AskUserQuestion pre-approved: exactly how acy launches the
	// planning phase (internal/cli/run.go). No hooks, so nothing else can run.
	drv := driver.New(driver.Options{
		PermissionMode: "plan",
		AllowedTools:   []string{"AskUserQuestion"},
	})
	if err := drv.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer drv.Stop()

	err := drv.Send("Use the AskUserQuestion tool right now to ask me a single " +
		"multiple-choice question: which colour do I prefer, red or blue? " +
		"Ask it with the tool. Do not answer it yourself and do not plan anything.")
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	m := &Model{drv: drv}
	var asked, answered, textAfter, completed bool
	var terminal string

	for ev := range drv.Events() {
		switch {
		case ev.Type == driver.TypeAssistant:
			for _, b := range ev.Message.Blocks() {
				switch {
				case b.Type == driver.BlockToolUse && baseToolName(b.Name) == "AskUserQuestion":
					asked = true
					t.Logf("claude sent AskUserQuestion input:\n%s", b.Input)

					// Half one: does parseAsk accept the real input schema?
					if _, ok := parseAsk(b.Input); !ok {
						t.Fatalf("parseAsk rejected a REAL AskUserQuestion input — "+
							"the schema has drifted from ask.go:\n%s", b.Input)
					}
					if os.Getenv("ACY_UPDATE_FIXTURE") != "" {
						writeFixture(t, ev.Raw)
					}

					// Answer it through the production path: open the panel, press
					// Enter, let submitAsk write the tool_result.
					m.ingestToolUse(b)
					if m.ask == nil {
						t.Fatal("ingestToolUse did not open the panel for a real AskUserQuestion")
					}
					for m.ask != nil {
						m.handleAskKey(tea.KeyPressMsg{Code: tea.KeyEnter})
					}
					answered = true

				case b.Type == driver.BlockText && b.Text != "":
					if answered {
						// Half two: claude accepted our tool_result and kept talking.
						textAfter = true
						t.Logf("claude continued after the answer: %q", b.Text)
					}
				}
			}

		case ev.IsTurnEnd():
			terminal = ev.TerminalReason
			completed = ev.TerminalReason == "completed"
			t.Logf("result: terminal_reason=%s stop=%s cost=$%.4f", ev.TerminalReason, ev.StopReason, ev.TotalCostUSD)
			drv.Stop()
		}
	}

	if !asked {
		t.Fatal("claude never called AskUserQuestion, so the wire shape is still unverified. " +
			"Expected against 2.1.207: `claude -p` advertises a 30-tool registry that excludes " +
			"AskUserQuestion entirely, so it cannot be called no matter what --allowedTools says. " +
			"Check the tools list on the system/init event before assuming acy is at fault.")
	}
	if !answered {
		t.Fatal("never answered the question")
	}
	// This is the assertion the whole test exists for. If acy's tool_result shape
	// is wrong, claude does not simply complete the turn.
	if !completed {
		t.Errorf("claude did not complete the turn after acy's tool_result "+
			"(terminal_reason=%q) — the tool_result shape sent by submitAsk is not what claude expects", terminal)
	}
	if !textAfter {
		t.Errorf("claude produced no text after the answer; the tool_result may not have unblocked the turn")
	}
}

// writeFixture records the exact stream-json line claude emitted, so the offline
// tests are checked against a captured payload instead of an invented one.
func writeFixture(t *testing.T, raw []byte) {
	t.Helper()
	p := filepath.Join("testdata", "ask_tool_use.json")
	if err := os.WriteFile(p, append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("update fixture: %v", err)
	}
	t.Logf("wrote %s — the offline ask tests now run against this captured payload", p)
}
