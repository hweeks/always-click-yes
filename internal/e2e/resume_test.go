package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/ui"
)

// kickoffPromptText is the opening words of the prompt arming sends (ui.kickoffPrompt,
// which is unexported). A resumed run must never send it a second time.
const kickoffPromptText = "The plan is approved"

// The feature, end to end and for real: arm a run, kill it mid-flight the way a
// closed terminal would, then bring it back with --continue and watch it finish
// the job on its own.
//
// This is the test that would have caught every bug worth catching in the resume
// path, because it is the only one that exercises the two halves together —
// claude's transcript and acy's snapshot — against a claude that is really there.
func TestE2EResumeAnArmedRunAfterACrash(t *testing.T) {
	dir := scratchProject(t)

	// --- the first life: plan, arm, and get killed mid-run ---
	first := newHarness(t, options{Cwd: dir})

	first.typeAndSend("Create two files in the current directory: one.txt containing the word one, " +
		"and two.txt containing the word two. Keep the plan to those two steps.")
	first.waitFor("the plan session to be idle", planTimeout, func(m ui.Model) bool {
		return m.SessionID() != "" && m.Status() == "idle"
	})

	var sessionID string
	first.read(func(m ui.Model) { sessionID = m.SessionID() })

	first.key(keyCtrlG)
	first.waitFor("the run to arm", 30*time.Second, func(m ui.Model) bool {
		return m.Phase() == ui.PhaseAutoRun
	})

	// Let it get properly underway — at least one tool through the gate — so that
	// what we kill is a run with work behind it and work ahead of it.
	first.waitFor("the armed run to reach a tool", workTimeout, func(m ui.Model) bool {
		return m.PendingGates() > 0 || strings.Contains(m.Transcript(), "Write")
	})
	time.Sleep(5 * time.Second) // give the first approval time to land

	first.crash() // the terminal dies. Nothing is tidied. This is the real failure mode.

	// What acy persisted is now the only thing that knows this run existed.
	snap, ok := snapshotFor(t, sessionID)
	if !ok {
		t.Fatal("no snapshot survived the crash — nothing could be resumed")
	}
	if snap.Phase != "AUTO-RUN" {
		t.Errorf("snapshot phase = %q, want AUTO-RUN — a resume would come back disarmed", snap.Phase)
	}
	if strings.TrimSpace(snap.PlanBody) == "" {
		t.Error("the snapshot has no plan — a resume would restore a run with no record of what was approved")
	}
	if snap.CostSettled <= 0 {
		t.Error("the snapshot has no cost — the tally would restart at zero")
	}

	// --- the second life: --continue ---
	second := newHarness(t, options{Cwd: dir, Continue: true})

	// The transcript comes back, both sides of it. This is the assertion that the
	// replay is reading claude's own record rather than starting from nothing: the
	// prompt below was typed at the *first* harness, and no live event ever carries
	// it (claude never echoes an injected user turn).
	second.waitFor("the transcript to be restored", 60*time.Second, func(m ui.Model) bool {
		return strings.Contains(m.Transcript(), "Create two files")
	})

	second.read(func(m ui.Model) {
		if m.Phase() != ui.PhaseAutoRun {
			t.Errorf("phase = %s, want AUTO-RUN — the run came back disarmed", m.Phase())
		}
		if m.SessionID() != sessionID {
			t.Errorf("session = %q, want the one we crashed (%q)", m.SessionID(), sessionID)
		}
		if m.TotalCost() < snap.CostSettled {
			t.Errorf("cost = %.4f, want at least the %.4f already spent", m.TotalCost(), snap.CostSettled)
		}

		// A resumed run must not be told "the plan is approved, begin implementing it":
		// it is already half done, and that prompt would start it over.
		//
		// The count is the assertion, not the presence: the *original* kickoff is part
		// of the history being replayed, so it is supposed to be on screen exactly
		// once. A second copy is the bug — that would be acy sending it again.
		if n := strings.Count(m.Transcript(), kickoffPromptText); n != 1 {
			t.Errorf("the kickoff prompt appears %d times, want exactly 1 (the replayed original); "+
				"a second copy means the resumed run was re-kicked-off instead of rejoining the completion loop", n)
		}
	})

	// Rejoining the loop is the whole mechanism: a resumed auto-run is an idle
	// auto-run, so acy asks the session itself whether it is done — the nudge that
	// spends the first auto-round — or, if the replayed turn already said DONE,
	// completes on the spot.
	second.waitFor("the resumed run to pick itself back up", 3*time.Minute, func(m ui.Model) bool {
		return m.Dispatches() > 0 || m.Phase() == ui.PhaseComplete
	})

	// And now the payoff: nobody touches the keyboard, and it finishes.
	second.waitFor("the resumed run to complete", workTimeout, func(m ui.Model) bool {
		return m.Phase() == ui.PhaseComplete
	})

	for _, f := range []string{"one.txt", "two.txt"} {
		if _, err := readFileIn(t, dir, f); err != nil {
			t.Errorf("the resumed run reported COMPLETE without writing %s: %v", f, err)
		}
	}
}

// A finished run is still resumable — you just come back to a chat, with the cost
// it already incurred intact rather than reset to zero.
func TestE2EResumeACompletedRunLandsInPlan(t *testing.T) {
	dir := scratchProject(t)

	first := newHarness(t, options{Cwd: dir})
	first.typeAndSend("Create a file called done.txt containing the word: done. Keep the plan to one step.")
	first.waitFor("the plan session to be idle", planTimeout, func(m ui.Model) bool {
		return m.SessionID() != "" && m.Status() == "idle"
	})

	var sessionID string
	first.read(func(m ui.Model) { sessionID = m.SessionID() })

	first.key(keyCtrlG)
	first.waitFor("the run to complete", workTimeout, func(m ui.Model) bool {
		return m.Phase() == ui.PhaseComplete
	})

	var spent float64
	first.read(func(m ui.Model) { spent = m.TotalCost() })
	first.crash()

	second := newHarness(t, options{Cwd: dir, Resume: sessionID})
	second.waitFor("the completed run to be restored", 60*time.Second, func(m ui.Model) bool {
		return strings.Contains(m.Transcript(), "already completed")
	})

	second.read(func(m ui.Model) {
		if m.Phase() != ui.PhasePlan {
			t.Errorf("phase = %s, want PLAN — a finished run has nothing left to auto-run", m.Phase())
		}
		if m.TotalCost() < spent {
			t.Errorf("cost = %.4f, want the %.4f already spent to carry over", m.TotalCost(), spent)
		}
	})
}

// A session acy never supervised — a bare `claude` run, say — has no snapshot. It
// must still resume: you get the conversation back, in a plan session, with nothing
// to restore. This is the path that used to be all acy could do.
func TestE2EResumeASessionWithNoSnapshot(t *testing.T) {
	dir := scratchProject(t)

	first := newHarness(t, options{Cwd: dir})
	first.typeAndSend("Say the word pineapple and nothing else.")
	first.waitFor("the session to answer", planTimeout, func(m ui.Model) bool {
		return m.SessionID() != "" && m.Status() == "idle"
	})

	var sessionID string
	first.read(func(m ui.Model) { sessionID = m.SessionID() })
	first.crash()

	// Throw away everything acy knew about that session, keeping only claude's own
	// transcript — which is exactly the state of a session acy never drove.
	deleteSnapshot(t, sessionID)

	second := newHarness(t, options{Cwd: dir, Resume: sessionID})
	second.waitFor("the conversation to come back", 60*time.Second, func(m ui.Model) bool {
		return strings.Contains(strings.ToLower(m.Transcript()), "pineapple")
	})
	second.read(func(m ui.Model) {
		if m.Phase() != ui.PhasePlan {
			t.Errorf("phase = %s, want PLAN", m.Phase())
		}
	})
}
