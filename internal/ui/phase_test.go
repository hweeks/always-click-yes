package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/driver"
)

func result() driver.Event { return driver.Event{Type: driver.TypeResult} }

// autoRunModel returns an armed model, idle in AUTO-RUN.
func autoRunModel() Model {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.phase = PhaseAutoRun
	return m
}

// lastBody is the body of the newest transcript entry — after a nudge, the prompt
// that was just sent to the working session.
func lastBody(m *Model) string { return m.entries[len(m.entries)-1].body }

func TestParseVerdict(t *testing.T) {
	tests := []struct {
		text string
		want verdict
	}{
		{"All finished.\nSTATUS: DONE", verdictDone},
		{"status: done", verdictDone},
		{"STATUS:DONE", verdictDone},
		{"More to do.\nSTATUS: CONTINUE", verdictContinue},
		{"STATUS:CONTINUE", verdictContinue},
		{"I wrote the files.", verdictUnclear},
		{"", verdictUnclear},
		// DONE wins ties: a turn that continued working and then finished.
		{"STATUS: CONTINUE ... more work ... STATUS: DONE", verdictDone},
	}
	for _, tc := range tests {
		if got := parseVerdict(tc.text); got != tc.want {
			t.Errorf("parseVerdict(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

// A turn ending with the DONE sentinel completes the run — no second session, no
// extra turn.
func TestOnTurnEndDoneCompletes(t *testing.T) {
	m := autoRunModel()
	m.turnText = "Everything is finished.\nSTATUS: DONE"
	m.onTurnEnd(result())
	if m.phase != PhaseComplete {
		t.Fatalf("phase = %v, want COMPLETE", m.phase)
	}
	if m.processing {
		t.Fatal("a completed run has nothing in flight")
	}
}

// A CONTINUE turn is nudged onward in the same session and stays in auto-run.
func TestOnTurnEndContinueNudges(t *testing.T) {
	m := autoRunModel()
	m.turnText = "Two steps remain.\nSTATUS: CONTINUE"
	m.onTurnEnd(result())
	if m.phase != PhaseAutoRun {
		t.Fatalf("phase = %v, want AUTO-RUN", m.phase)
	}
	if !m.processing {
		t.Fatal("expected processing after a nudge")
	}
	if m.rounds != 1 {
		t.Fatalf("rounds = %d, want 1", m.rounds)
	}
	if m.preloaded {
		t.Fatal("should not preload the manual check on an auto nudge")
	}
	if lastBody(&m) != continuePrompt {
		t.Fatalf("nudge sent %q, want the continue prompt", lastBody(&m))
	}
}

// A turn with no sentinel gets the done-check question, asked of the session that
// did the work — not of an independent judge.
func TestOnTurnEndNoSentinelAsksTheSession(t *testing.T) {
	m := autoRunModel()
	m.turnText = "I wrote some files."
	m.onTurnEnd(result())
	if !m.processing {
		t.Fatal("expected the done-check to be sent automatically")
	}
	if lastBody(&m) != doneCheckPrompt {
		t.Fatalf("nudge sent %q, want the done-check prompt", lastBody(&m))
	}
}

// A nudge clears turnText, so the next verdict is read from the next turn alone —
// not from stale text left over.
func TestNudgeClearsTheTurnText(t *testing.T) {
	m := autoRunModel()
	m.turnText = "STATUS: CONTINUE"
	m.onTurnEnd(result())
	if m.turnText != "" {
		t.Fatalf("turnText = %q, want it cleared for the nudged turn", m.turnText)
	}
}

// Past the round cap the loop stops driving itself and hands the done-check to the
// user instead.
func TestNudgeCapHandsControlBack(t *testing.T) {
	m := autoRunModel()
	m.rounds = maxAutoRounds
	m.turnText = "STATUS: CONTINUE"
	m.onTurnEnd(result())
	if m.processing {
		t.Fatal("should not keep nudging past the round cap")
	}
	if !m.preloaded || m.input.Value() != doneCheckPrompt {
		t.Fatalf("expected the manual done-check preloaded, got %q", m.input.Value())
	}
	if !strings.Contains(m.transcript(), "auto-rounds") {
		t.Error("hitting the cap should be visible in the transcript")
	}
}

// After a manual interrupt, the turn end must not nudge or preload — the user is
// about to redirect.
func TestOnTurnEndInterrupted(t *testing.T) {
	m := autoRunModel()
	m.processing = true
	m.interrupted = true
	m.turnText = "STATUS: CONTINUE"
	m.onTurnEnd(result())
	if m.rounds != 0 || m.preloaded {
		t.Fatal("should neither nudge nor preload after a manual interrupt")
	}
	if m.interrupted {
		t.Fatal("interrupted flag should be cleared")
	}
}

// interject is a no-op when nothing is processing.
func TestInterjectGuards(t *testing.T) {
	m := autoRunModel()
	m.processing = false
	if m.interject() {
		t.Fatal("interject should return false when idle")
	}
}

// No action while gates are pending or outside auto-run.
func TestOnTurnEndGuards(t *testing.T) {
	m := autoRunModel()
	m.turnText = "STATUS: DONE"
	p, _ := bashPending("echo hi")
	m.enqueue(p)
	m.onTurnEnd(result())
	if m.phase == PhaseComplete {
		t.Error("should not judge completion while a gate is pending")
	}

	m2 := autoRunModel()
	m2.phase = PhasePlan
	m2.turnText = "STATUS: DONE"
	m2.onTurnEnd(result())
	if m2.phase == PhaseComplete || m2.rounds != 0 {
		t.Error("should not judge completion during planning")
	}
}

// Once complete, further turn ends are the user vetting the work — they must not
// re-enter the loop.
func TestOnTurnEndCompleteIsANormalChat(t *testing.T) {
	m := autoRunModel()
	m.turnText = "STATUS: DONE"
	m.onTurnEnd(result())
	if m.phase != PhaseComplete {
		t.Fatalf("phase = %v, want COMPLETE", m.phase)
	}

	m.turnText = "here is a summary of what I did" // a vetting reply, no sentinel
	m.onTurnEnd(result())
	if m.processing || m.rounds != 0 || m.preloaded {
		t.Fatal("a turn after COMPLETE must not be nudged — it is a normal chat")
	}
}
