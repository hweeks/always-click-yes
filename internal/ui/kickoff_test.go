package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/judge"
)

// nopCloser lets a driver's injected stdin be inspected without a claude process.
type nopCloser struct{ b *strings.Builder }

func (w nopCloser) Write(p []byte) (int, error) { return w.b.Write(p) }
func (w nopCloser) Close() error                { return nil }

// armedModel returns a model about to receive a driver, with everything the
// auto-run branch of onDriverReady reads already in place. The returned builder
// captures every byte acy writes to the session's stdin — which is how these tests
// tell "sent the kickoff" from "sent nothing".
func armedModel(t *testing.T, j JudgeFunc) (*Model, *driver.Driver, *strings.Builder) {
	t.Helper()
	var sent strings.Builder
	m := New(nil, Config{Countdown: 30 * time.Second, Judge: j})
	drv := driver.NewWithWriter(driver.Options{}, nopCloser{&sent})
	return &m, drv, &sent
}

// Arming is the launch that starts the work, and the kickoff prompt is what starts
// it. This pins today's behavior so the resume path below can't quietly break it.
func TestOnDriverReadyArmingSendsTheKickoff(t *testing.T) {
	m, drv, sent := armedModel(t, fakeJudge(judge.VerdictDone, "", nil))

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
// send nothing and instead rejoin the loop where every turn already ends — at the
// judge.
func TestOnDriverReadyResumedAutoRunVerifiesInsteadOfKickingOff(t *testing.T) {
	m, drv, sent := armedModel(t, fakeJudge(judge.VerdictContinue, "", nil))
	m.planBody = "the approved plan"

	cmd := m.onDriverReady(driverReadyMsg{drv: drv, phase: PhaseAutoRun, kickoff: false})

	if sent.String() != "" {
		t.Fatalf("a resumed run must not be prompted; stdin got:\n%s", sent.String())
	}
	if !m.verifying {
		t.Error("a resumed auto-run should hand off to the judge")
	}
	if cmd == nil {
		t.Error("expected the judge command to be dispatched")
	}
	if m.processing {
		t.Error("nothing was sent, so nothing is in flight")
	}
}

// With no judge wired, a resumed auto-run falls back to the manual done-check —
// the same fallback a live idle turn already uses.
func TestOnDriverReadyResumedAutoRunWithoutJudgePreloads(t *testing.T) {
	m, drv, sent := armedModel(t, nil)

	m.onDriverReady(driverReadyMsg{drv: drv, phase: PhaseAutoRun, kickoff: false})

	if sent.String() != "" {
		t.Fatalf("a resumed run must not be prompted; stdin got:\n%s", sent.String())
	}
	if !m.preloaded {
		t.Error("expected the manual done-check to be preloaded")
	}
}

// The plan is what the judge grades against. ExitPlanMode never fires in `claude -p`,
// so without this fallback planBody is empty and the judge is asked whether nothing
// has been completed.
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
