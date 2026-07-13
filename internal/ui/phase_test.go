package ui

import (
	"context"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/judge"
)

func result() driver.Event { return driver.Event{Type: driver.TypeResult} }

// autoRunModel returns an armed model with no judge wired (manual-verify path).
func autoRunModel() Model {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.phase = PhaseAutoRun
	return m
}

// autoRunWithJudge wires an injected judge so the automatic path is exercised.
func autoRunWithJudge(j JudgeFunc) Model {
	m := New(nil, Config{Countdown: 30 * time.Second, Judge: j})
	m.phase = PhaseAutoRun
	return m
}

func fakeJudge(v judge.Verdict, rationale string, err error) JudgeFunc {
	return func(context.Context, string, string) (judge.Result, error) {
		return judge.Result{Verdict: v, Text: rationale}, err
	}
}

// A fresh idle turn with a judge wired dispatches a verification command.
func TestOnTurnEndDispatchesJudge(t *testing.T) {
	m := autoRunWithJudge(fakeJudge(judge.VerdictDone, "", nil))
	cmd := m.onTurnEnd(result())
	if cmd == nil {
		t.Fatal("expected a judge command to be dispatched")
	}
	if !m.verifying {
		t.Fatal("expected verifying to be set")
	}
	if m.preloaded {
		t.Fatal("should not preload the manual done-check when a judge is wired")
	}
}

// Without a judge, a fresh idle turn falls back to the manual done-check.
func TestOnTurnEndNoJudgePreloads(t *testing.T) {
	m := autoRunModel()
	if cmd := m.onTurnEnd(result()); cmd != nil {
		t.Fatal("no judge means no command")
	}
	if !m.preloaded || m.input.Value() != doneCheckPrompt {
		t.Fatalf("expected manual done-check preloaded, got %q", m.input.Value())
	}
}

// A DONE verdict moves to COMPLETE.
func TestOnVerdictDone(t *testing.T) {
	m := autoRunWithJudge(fakeJudge(judge.VerdictDone, "", nil))
	m.verifying = true
	m.onVerdict(verdictMsg{gen: m.gen, v: judge.VerdictDone})
	if m.phase != PhaseComplete {
		t.Fatalf("phase = %v, want COMPLETE", m.phase)
	}
	if m.verifying {
		t.Fatal("verifying should be cleared")
	}
}

// A CONTINUE verdict nudges the working session and stays in auto-run.
func TestOnVerdictContinue(t *testing.T) {
	m := autoRunWithJudge(fakeJudge(judge.VerdictContinue, "step 2 remains", nil))
	m.verifying = true
	m.onVerdict(verdictMsg{gen: m.gen, v: judge.VerdictContinue, rationale: "step 2 remains"})
	if m.phase != PhaseAutoRun {
		t.Fatalf("phase = %v, want AUTO-RUN", m.phase)
	}
	if !m.processing {
		t.Fatal("expected processing after a CONTINUE nudge")
	}
	if m.rounds != 1 {
		t.Fatalf("rounds = %d, want 1", m.rounds)
	}
	if m.preloaded {
		t.Fatal("should not preload the manual check on an auto CONTINUE")
	}
}

// CONTINUE past the round cap hands control back to the user (manual verify).
func TestOnVerdictContinueCap(t *testing.T) {
	m := autoRunWithJudge(fakeJudge(judge.VerdictContinue, "", nil))
	m.rounds = maxAutoRounds
	m.onVerdict(verdictMsg{gen: m.gen, v: judge.VerdictContinue})
	if m.processing {
		t.Fatal("should not keep nudging past the round cap")
	}
	if !m.preloaded {
		t.Fatal("expected manual fallback preloaded past the cap")
	}
}

// A judge error falls back to the manual done-check.
func TestOnVerdictError(t *testing.T) {
	m := autoRunWithJudge(fakeJudge(judge.VerdictUnclear, "", context.DeadlineExceeded))
	m.verifying = true
	m.onVerdict(verdictMsg{gen: m.gen, err: context.DeadlineExceeded})
	if m.verifying {
		t.Fatal("verifying should be cleared on error")
	}
	if !m.preloaded {
		t.Fatal("expected manual fallback on judge error")
	}
}

// An unclear verdict falls back to the manual done-check.
func TestOnVerdictUnclear(t *testing.T) {
	m := autoRunWithJudge(fakeJudge(judge.VerdictUnclear, "", nil))
	m.onVerdict(verdictMsg{gen: m.gen, v: judge.VerdictUnclear})
	if !m.preloaded {
		t.Fatal("expected manual fallback on an unclear verdict")
	}
}

// A verdict from a stale generation is ignored.
func TestOnVerdictStaleGen(t *testing.T) {
	m := autoRunWithJudge(fakeJudge(judge.VerdictDone, "", nil))
	m.gen = 5
	m.verifying = true
	m.onVerdict(verdictMsg{gen: 1, v: judge.VerdictDone})
	if m.phase == PhaseComplete {
		t.Fatal("a stale verdict must not complete the run")
	}
	if !m.verifying {
		t.Fatal("stale verdict should leave state untouched")
	}
}

// The manual fallback path: a same-session DONE reply completes the run.
func TestOnTurnEndManualDone(t *testing.T) {
	m := autoRunModel()
	m.awaitingVerdict = true
	m.turnText = "Everything is finished. STATUS: DONE"
	m.onTurnEnd(result())
	if m.phase != PhaseComplete {
		t.Fatalf("phase = %v, want COMPLETE", m.phase)
	}
}

// The manual fallback path: CONTINUE re-preloads and stays in auto-run.
func TestOnTurnEndManualContinue(t *testing.T) {
	m := autoRunModel()
	m.awaitingVerdict = true
	m.turnText = "More to do. STATUS: CONTINUE"
	m.onTurnEnd(result())
	if m.phase != PhaseAutoRun {
		t.Fatalf("phase = %v, want AUTO-RUN", m.phase)
	}
	if !m.preloaded {
		t.Fatal("expected re-ask (preloaded) after manual CONTINUE")
	}
	if m.awaitingVerdict {
		t.Fatal("awaitingVerdict should be cleared")
	}
}

// After a manual interrupt, the turn end must not verify or preload.
func TestOnTurnEndInterrupted(t *testing.T) {
	m := autoRunWithJudge(fakeJudge(judge.VerdictDone, "", nil))
	m.processing = true
	m.interrupted = true
	if cmd := m.onTurnEnd(result()); cmd != nil {
		t.Fatal("should not verify after a manual interrupt")
	}
	if m.preloaded || m.verifying {
		t.Fatal("should neither preload nor verify after a manual interrupt")
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
	m := autoRunWithJudge(fakeJudge(judge.VerdictDone, "", nil))
	p, _ := bashPending("echo hi")
	m.enqueue(p)
	if cmd := m.onTurnEnd(result()); cmd != nil {
		t.Error("should not verify while a gate is pending")
	}
	if m.verifying {
		t.Error("should not verify while a gate is pending")
	}

	m2 := autoRunWithJudge(fakeJudge(judge.VerdictDone, "", nil))
	m2.phase = PhasePlan
	if cmd := m2.onTurnEnd(result()); cmd != nil {
		t.Error("should not verify during planning")
	}
}
