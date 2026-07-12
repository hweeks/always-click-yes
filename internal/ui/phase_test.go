package ui

import (
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/driver"
)

func TestParseVerdict(t *testing.T) {
	cases := map[string]verdict{
		"all good. STATUS: DONE": verdictDone,
		"status: done":           verdictDone,
		"Not yet. STATUS: CONTINUE and I'll keep going": verdictContinue,
		"STATUS:CONTINUE":                     verdictContinue,
		"no sentinel here":                    verdictNone,
		"STATUS: DONE beats STATUS: CONTINUE": verdictDone,
	}
	for in, want := range cases {
		if got := parseVerdict(in); got != want {
			t.Errorf("parseVerdict(%q) = %v, want %v", in, got, want)
		}
	}
}

func result() driver.Event { return driver.Event{Type: driver.TypeResult} }

func autoRunModel() Model {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.phase = PhaseAutoRun
	return m
}

// idle with no pending verdict preloads the done-check.
func TestOnTurnEndPreloads(t *testing.T) {
	m := autoRunModel()
	m.onTurnEnd(result())
	if !m.preloaded {
		t.Fatal("expected done-check to be preloaded")
	}
	if m.input.Value() != doneCheckPrompt {
		t.Fatalf("input not preloaded with done-check: %q", m.input.Value())
	}
}

// a verdict turn saying DONE moves to Complete.
func TestOnTurnEndDone(t *testing.T) {
	m := autoRunModel()
	m.awaitingVerdict = true
	m.turnText = "Everything is finished. STATUS: DONE"
	m.onTurnEnd(result())
	if m.phase != PhaseComplete {
		t.Fatalf("phase = %v, want COMPLETE", m.phase)
	}
	if m.preloaded {
		t.Fatal("should not preload after completion")
	}
}

// a verdict turn saying CONTINUE re-asks (preloads again, stays in auto-run).
func TestOnTurnEndContinue(t *testing.T) {
	m := autoRunModel()
	m.awaitingVerdict = true
	m.turnText = "More to do. STATUS: CONTINUE"
	m.onTurnEnd(result())
	if m.phase != PhaseAutoRun {
		t.Fatalf("phase = %v, want AUTO-RUN", m.phase)
	}
	if !m.preloaded {
		t.Fatal("expected re-ask (preloaded) after CONTINUE")
	}
	if m.awaitingVerdict {
		t.Fatal("awaitingVerdict should be cleared")
	}
}

// after a manual interrupt, the turn end must NOT preload the done-check.
func TestOnTurnEndInterrupted(t *testing.T) {
	m := autoRunModel()
	m.processing = true
	m.interrupted = true
	m.onTurnEnd(result())
	if m.preloaded {
		t.Fatal("should not preload done-check after a manual interrupt")
	}
	if m.interrupted {
		t.Fatal("interrupted flag should be cleared")
	}
	if m.phase != PhaseAutoRun {
		t.Fatalf("phase = %v, want AUTO-RUN", m.phase)
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

// no action while gates are pending or outside auto-run.
func TestOnTurnEndGuards(t *testing.T) {
	m := autoRunModel()
	p, _ := bashPending("echo hi")
	m.enqueue(p)
	m.onTurnEnd(result())
	if m.preloaded {
		t.Error("should not preload while a gate is pending")
	}

	m2 := New(nil, Config{})
	m2.phase = PhasePlan
	m2.onTurnEnd(result())
	if m2.preloaded {
		t.Error("should not preload during planning")
	}
}
