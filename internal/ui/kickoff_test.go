package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/driver"
)

// nopCloser lets a driver's injected stdin be inspected without a claude process.
type nopCloser struct{ b *strings.Builder }

func (w nopCloser) Write(p []byte) (int, error) { return w.b.Write(p) }
func (w nopCloser) Close() error                { return nil }

// armedModel returns a model about to receive a driver, with everything the
// auto-run branch of onDriverReady reads already in place. The returned builder
// captures every byte acy writes to the session's stdin — which is how these tests
// tell "sent the kickoff" from "sent nothing".
func armedModel(t *testing.T) (*Model, *driver.Driver, *strings.Builder) {
	t.Helper()
	var sent strings.Builder
	m := New(nil, Config{Countdown: 30 * time.Second})
	drv := driver.NewWithWriter(driver.Options{}, nopCloser{&sent})
	return &m, drv, &sent
}

// Arming is the launch that starts the work, and the kickoff prompt is what starts
// it. This pins today's behavior so the resume path below can't quietly break it.
func TestOnDriverReadyArmingSendsTheKickoff(t *testing.T) {
	m, drv, sent := armedModel(t)

	m.onDriverReady(driverReadyMsg{drv: drv, phase: PhaseAutoRun, kickoff: true})

	if !strings.Contains(sent.String(), "The plan is approved") {
		t.Fatalf("arming should send the kickoff prompt; stdin got:\n%s", sent.String())
	}
	if !m.processing {
		t.Error("arming should leave the run working")
	}
}

// The heart of resume: a restored auto-run is already underway. Re-sending the
// kickoff would tell a half-finished session to start the plan over, so it must
// instead rejoin the loop where every turn already ends — by asking the session
// itself whether it is done.
func TestOnDriverReadyResumedAutoRunAsksInsteadOfKickingOff(t *testing.T) {
	m, drv, sent := armedModel(t)
	m.planBody = "the approved plan"
	m.turnText = "I was halfway through step two." // what the replay recovered

	m.onDriverReady(driverReadyMsg{drv: drv, phase: PhaseAutoRun, kickoff: false})

	if strings.Contains(sent.String(), "The plan is approved") {
		t.Fatalf("a resumed run must not be re-kicked-off; stdin got:\n%s", sent.String())
	}
	if !strings.Contains(sent.String(), "Have we completed every step") {
		t.Fatalf("a resumed run should be asked whether it is done; stdin got:\n%s", sent.String())
	}
	if !m.processing {
		t.Error("the done-check is a real turn; the run should be working")
	}
	if m.rounds != 1 {
		t.Errorf("rounds = %d, want 1 — the resume nudge spends an auto-round", m.rounds)
	}
}

// If the replayed final turn already carries the DONE sentinel, the run finished
// before acy died — complete it, and send the session nothing at all.
func TestOnDriverReadyResumedAutoRunHonorsAReplayedDone(t *testing.T) {
	m, drv, sent := armedModel(t)
	m.turnText = "Both files are written.\nSTATUS: DONE"

	m.onDriverReady(driverReadyMsg{drv: drv, phase: PhaseAutoRun, kickoff: false})

	if m.phase != PhaseComplete {
		t.Fatalf("phase = %v, want COMPLETE — the replayed sentinel was not read", m.phase)
	}
	if sent.String() != "" {
		t.Fatalf("a finished run must not be prompted; stdin got:\n%s", sent.String())
	}
}

// The plan is the record of what the user approved: the snapshot persists it and a
// resume shows it. ExitPlanMode never fires in `claude -p`, so without this
// fallback planBody would be empty.
func TestCapturePlanFallsBackToTheLastAssistantTurn(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.turnText = "1. do the thing\n2. do the other thing\n"

	m.capturePlan()

	if !strings.Contains(m.planBody, "do the thing") {
		t.Fatalf("planBody = %q, want the last assistant turn", m.planBody)
	}
}

// A plan that really did arrive via ExitPlanMode wins: it is the plan the user was
// shown in a box and approved, and the last assistant turn may be chatter after it.
func TestCapturePlanKeepsAnExplicitPlan(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.planBody = "the real plan"
	m.turnText = "sure, sounds good"

	m.capturePlan()

	if m.planBody != "the real plan" {
		t.Fatalf("planBody = %q, want the explicit plan preserved", m.planBody)
	}
}
