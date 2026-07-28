package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/ui"
)

// Timeouts are generous: every one of them is waiting on a real model doing real
// work, and a flaky failure here would be worse than a slow pass.
const (
	planTimeout = 3 * time.Minute
	workTimeout = 8 * time.Minute
)

// The whole premise of the tool, end to end: plan a task, arm it, and have it
// approve its own tools, do the work, and decide for itself that it is finished —
// with a file on disk at the end as the only evidence that matters.
func TestE2EPlanArmAutoApproveComplete(t *testing.T) {
	dir := scratchProject(t)
	h := newHarness(t, options{Cwd: dir})

	h.typeAndSend("Create a file called hello.txt in the current directory containing exactly the word: world. " +
		"That is the entire task. Keep the plan to one step.")
	h.waitFor("the plan session to be idle", planTimeout, func(m ui.Model) bool {
		return m.SessionID() != "" && m.Status() == "idle"
	})

	// Plan mode cannot write, so nothing exists yet — that is the point of planning.
	if _, err := readFileIn(t, dir, "hello.txt"); err == nil {
		t.Fatal("plan mode wrote a file; it is supposed to be incapable of that")
	}

	h.key(keyCtrlG) // arm

	// Arming is the moment the plan has to be captured: it is what the snapshot
	// persists and a resume restores, and ExitPlanMode never fires in `claude -p`,
	// so it can only have come from the last assistant turn.
	h.waitFor("the run to arm", 30*time.Second, func(m ui.Model) bool {
		return m.Phase() == ui.PhaseAutoRun
	})
	h.read(func(m ui.Model) {
		if strings.TrimSpace(m.PlanBody()) == "" {
			t.Fatal("armed with an empty plan — nothing would survive into a resume")
		}
	})

	// Every gated tool now auto-approves after the countdown, with no key pressed.
	h.waitFor("the run to complete", workTimeout, func(m ui.Model) bool {
		return m.Phase() == ui.PhaseComplete
	})

	got, err := readFileIn(t, dir, "hello.txt")
	if err != nil {
		t.Fatalf("the run reported COMPLETE but wrote no file: %v", err)
	}
	if !strings.Contains(strings.ToLower(got), "world") {
		t.Errorf("hello.txt = %q, want it to contain \"world\"", got)
	}

	h.read(func(m ui.Model) {
		if m.TotalCost() <= 0 {
			t.Error("a completed run should have a cost")
		}
		// The whole architecture in one assertion: the supervising session
		// cannot write, so the only way that file exists is that it delegated
		// the work to a child process.
		if m.Dispatches() == 0 {
			t.Error("the file appeared but nothing was dispatched — " +
				"the parent should have no way to write it itself")
		}
		if m.ChildTokens().Volume() == 0 {
			t.Error("no child token usage recorded; the ledger cannot prove where the work happened")
		}
		// The point of delegating: the parent pays for a short report, not for
		// everything the child read on its way to writing the file.
		if pt, ct := m.ParentTokens(), m.ChildTokens(); pt.CacheRead > ct.CacheRead*4 {
			t.Errorf("parent cache reads (%d) dwarf the child's (%d); "+
				"the parent is carrying work it was supposed to delegate", pt.CacheRead, ct.CacheRead)
		}
		// Token accounting is the instrument this project is being measured
		// with, so a live run has to prove it actually reads the wire — a
		// decoder that silently reports zero would make every later
		// before/after comparison meaningless.
		tok := m.ParentTokens()
		if tok.Output <= 0 {
			t.Error("a completed run should have recorded output tokens")
		}
		if tok.CacheRead <= 0 && tok.CacheCreate <= 0 {
			t.Error("a completed run should have recorded cache usage")
		}
		if m.LastContext() <= 0 {
			t.Error("the last turn should report the context it carried")
		}
	})
}

// The veto key. It is the most important key on the board and it is the one you
// will almost never press — so it is exactly the one that has to be tested.
func TestE2EVetoBlocksATool(t *testing.T) {
	dir := scratchProject(t)
	// A long countdown: the test needs time to press the key before the gate
	// auto-approves, which is the whole hazard the veto exists for.
	h := newHarness(t, options{Cwd: dir, Countdown: 30 * time.Second})

	h.typeAndSend("Create a file called forbidden.txt containing the word: nope. Keep the plan to one step.")
	h.waitFor("the plan session to be idle", planTimeout, func(m ui.Model) bool {
		return m.SessionID() != "" && m.Status() == "idle"
	})

	h.key(keyCtrlG)
	h.waitFor("a tool to reach the gate", workTimeout, func(m ui.Model) bool {
		return m.PendingGates() > 0
	})

	h.key(keyCtrlX) // veto

	h.waitFor("the gate to clear", 30*time.Second, func(m ui.Model) bool {
		return m.PendingGates() == 0
	})
	h.read(func(m ui.Model) {
		if !strings.Contains(m.Transcript(), "denied") && !strings.Contains(m.Transcript(), "vetoed") {
			t.Errorf("a veto should be visible in the transcript:\n%s", m.Transcript())
		}
	})

	// The tool acy vetoed must not have run. claude may well try another way — the
	// claim here is narrow and true: the vetoed call did not execute.
	if _, err := readFileIn(t, dir, "forbidden.txt"); err == nil {
		t.Error("the vetoed tool wrote its file anyway — the gate did not hold")
	}
}
