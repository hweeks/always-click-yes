package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/ui"
)

// TestE2ECodexPlanArmAutoApproveComplete proves the second backend through
// the whole product, rather than merely at the app-server wire boundary:
//
//	Codex parent → acy's MCP server → Codex child → in-band Codex approval
//	→ acy's countdown → structured child report → parent Finish.
//
// The ordinary Claude test above already establishes the product behavior.
// This one establishes that the exact same behavior is reachable through the
// different transport Codex uses: there is no hook process, tool approvals
// arrive as app-server server requests, and a child's JSON report is its final
// agent message rather than a Claude result field. Keep the task deliberately
// tiny: the point is the wiring, not spending a subscription on an elaborate
// implementation.
//
//	ACY_LIVE=1 go test ./internal/e2e -run TestE2ECodexPlanArmAutoApproveComplete -v
func TestE2ECodexPlanArmAutoApproveComplete(t *testing.T) {
	dir := scratchProject(t)
	h := newHarness(t, options{
		Cwd:       dir,
		Agent:     "codex",
		Countdown: 100 * time.Millisecond,
	})

	h.typeAndSend("Create a file named codex-e2e.txt in the current directory containing exactly: codex works. " +
		"That is the entire task. Keep the plan to one step, then delegate the implementation when armed.")
	h.waitFor("the codex planning session to become idle", planTimeout, func(m ui.Model) bool {
		return m.SessionID() != "" && m.Status() == "idle"
	})

	// The parent is read-only even though Codex cannot omit its shell tool from
	// the registry. The only way this file can appear is a dispatched child.
	if _, err := readFileIn(t, dir, "codex-e2e.txt"); err == nil {
		t.Fatal("the codex planning session wrote a file; the supervising session must be read-only")
	}

	h.key(keyCtrlG)
	h.waitFor("the codex run to arm", 30*time.Second, func(m ui.Model) bool {
		return m.Phase() == ui.PhaseAutoRun
	})
	h.waitFor("the codex run to complete", workTimeout, func(m ui.Model) bool {
		return m.Phase() == ui.PhaseComplete
	})

	got, err := readFileIn(t, dir, "codex-e2e.txt")
	if err != nil {
		t.Fatalf("the codex-backed run completed but wrote no file: %v", err)
	}
	if got != "codex works" {
		t.Errorf("codex-e2e.txt = %q, want %q", got, "codex works")
	}
	h.read(func(m ui.Model) {
		if m.Dispatches() == 0 {
			t.Error("the file appeared but no Codex child was dispatched")
		}
		if m.ChildTokens().Volume() == 0 {
			t.Error("no child token usage was recorded from the Codex child")
		}
		if !strings.Contains(strings.ToLower(m.Transcript()), "codex") {
			t.Error("the completed Codex run left no usable transcript")
		}
	})
}
