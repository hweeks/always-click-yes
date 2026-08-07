package ui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/mcp"
)

func result() driver.Event { return driver.Event{Type: driver.TypeResult} }

// autoRunModel returns an armed model, idle in AUTO-RUN.
func autoRunModel() Model {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.phase = PhaseAutoRun
	return m
}

// lastBody is the body of the newest transcript entry.
func lastBody(m *Model) string { return m.entries[len(m.entries)-1].body }

func finishEvent(outcome, summary string) driver.ContentBlock {
	args, _ := json.Marshal(map[string]string{"outcome": outcome, "summary": summary})
	return driver.ContentBlock{
		Type: driver.BlockToolUse, Name: mcp.Qualified(mcp.ToolFinish), Input: args,
	}
}

// The run now ends because the session says so with a tool call, not because a
// magic string turned up in its prose.
func TestFinishToolEndsTheRun(t *testing.T) {
	m := autoRunModel()
	m.ingestToolUse(finishEvent("completed", "Added the ledger and verified it."))

	if m.phase != PhaseComplete {
		t.Fatalf("phase = %v, want COMPLETE", m.phase)
	}
	if !strings.Contains(lastBody(&m), "Added the ledger") {
		t.Errorf("the summary should reach the transcript, got %q", lastBody(&m))
	}
	if m.FinishOutcome() != "completed" {
		t.Errorf("FinishOutcome() = %q, want %q", m.FinishOutcome(), "completed")
	}
	if m.FinishSummary() != "Added the ledger and verified it." {
		t.Errorf("FinishSummary() = %q", m.FinishSummary())
	}
}

func TestFinishAbandonedIsStillTerminal(t *testing.T) {
	m := autoRunModel()
	m.ingestToolUse(finishEvent("abandoned", "The dependency is broken upstream."))

	if m.phase != PhaseComplete {
		t.Fatalf("phase = %v, want COMPLETE", m.phase)
	}
	if !strings.Contains(m.status, "abandoned") {
		t.Errorf("status = %q, want it to say the run was abandoned", m.status)
	}
}

// A Finish with missing or malformed arguments still ends the run. The session
// has said it is done; refusing to believe it over an absent field would leave
// the run wedged with nothing left to drive it.
func TestMalformedFinishStillEndsTheRun(t *testing.T) {
	for _, args := range []string{``, `{}`, `not json`, `{"outcome":""}`} {
		m := autoRunModel()
		m.ingestToolUse(driver.ContentBlock{
			Type:  driver.BlockToolUse,
			Name:  mcp.Qualified(mcp.ToolFinish),
			Input: json.RawMessage(args),
		})
		if m.phase != PhaseComplete {
			t.Errorf("args %q: phase = %v, want COMPLETE", args, m.phase)
		}
	}
}

// The nudge loop is gone. A turn that ends with work outstanding must simply
// stop: it used to cost up to ten more full-context turns asking "are you done
// yet?", which was the single most expensive habit acy had.
func TestIdleTurnDoesNotSendAnything(t *testing.T) {
	m := autoRunModel()
	// ingest clears processing on a result, then onTurnEnd runs — same order as
	// the real event loop (see update.go).
	m.processing = false
	m.turnText = "I have done some of it but not all"
	m.onTurnEnd(result())

	if m.processing {
		t.Fatal("an idle turn must not start another one — that was the nudge loop")
	}
	if m.phase != PhaseAutoRun {
		t.Errorf("phase = %v, want it to stay AUTO-RUN and wait for a human", m.phase)
	}
	if !strings.Contains(m.status, "idle") {
		t.Errorf("status = %q, want it to say the run is idle", m.status)
	}
}

// Prose that merely mentions the old sentinel must do nothing at all. The
// substring match this replaces would have ended the run on a commit message.
func TestSentinelTextIsJustText(t *testing.T) {
	for _, text := range []string{
		"STATUS: DONE",
		"the old code looked for STATUS: DONE in the reply",
		"STATUS:DONE",
	} {
		m := autoRunModel()
		m.processing = true
		m.turnText = text
		m.onTurnEnd(result())
		if m.phase == PhaseComplete {
			t.Errorf("%q ended the run; only the Finish tool may do that", text)
		}
	}
}

// While a task is running the parent is blocked on its report, so an idle turn
// is not idleness — it is waiting, and saying "nothing running" would be wrong.
func TestTurnEndWhileATaskRunsSaysWaiting(t *testing.T) {
	m := autoRunModel()
	m.dispatcher = &busyDispatcher{fakeDispatcher: newFakeDispatcher(nil)}
	m.processing = true
	m.onTurnEnd(result())

	if !strings.Contains(m.status, "waiting") {
		t.Errorf("status = %q, want it to say the run is waiting on a task", m.status)
	}
}

type busyDispatcher struct{ *fakeDispatcher }

func (b *busyDispatcher) Active() int { return 1 }

func TestOnTurnEndInterrupted(t *testing.T) {
	m := autoRunModel()
	m.processing = true
	m.interrupted = true
	m.onTurnEnd(result())

	if m.interrupted {
		t.Fatal("interrupted flag should be cleared")
	}
	if !strings.Contains(m.status, "interrupted") {
		t.Errorf("status = %q, want it to say the turn was interrupted", m.status)
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

// Esc must stop the children too. The parent is blocked on a task's report, so
// interrupting it alone would leave an orphan burning tokens on an answer that
// now has nowhere to go.
func TestInterjectCancelsRunningTasks(t *testing.T) {
	fake := newFakeDispatcher(nil)
	m := autoRunModel()
	m.dispatcher = &busyDispatcher{fakeDispatcher: fake}
	m.processing = true
	m.drv = nil // interject bails without a driver, so test cancelDispatches directly
	m.cancelDispatches("interrupted by the user")

	if len(fake.cancels) == 0 {
		t.Fatal("no task was cancelled; an orphaned child would keep spending")
	}
}

// No action while gates are pending, or outside auto-run.
func TestOnTurnEndGuards(t *testing.T) {
	m := autoRunModel()
	p, _ := bashPending("echo hi")
	m.enqueue(p)
	before := m.status
	m.onTurnEnd(result())
	if m.status != before {
		t.Error("a turn ending while a gate is pending should change nothing")
	}

	m2 := autoRunModel()
	m2.phase = PhasePlan
	m2.onTurnEnd(result())
	if strings.Contains(m2.status, "idle — no task") {
		t.Error("the idle notice belongs to AUTO-RUN, not planning")
	}
}

// Arming without a session must not half-happen: a phase flipped with no
// kickoff sent is a run that looks armed and does nothing.
func TestArmWithoutADriverIsRefused(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.sessionID = "s1"
	m.arm()
	if m.phase != PhasePlan {
		t.Errorf("phase = %v, want it to stay PLAN", m.phase)
	}
}

// Arming no longer launches a process. It flips the phase on the session already
// in front of the user, which is what makes the parent's context survive it.
func TestArmFlipsPhaseInPlace(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.sessionID = "s1"
	m.drv = driver.NewWithWriter(driver.Options{}, nopCloser{&strings.Builder{}})
	m.turnText = "here is the plan"
	gen := m.gen

	m.arm()

	if m.phase != PhaseAutoRun {
		t.Fatalf("phase = %v, want AUTO-RUN", m.phase)
	}
	if m.gen != gen {
		t.Errorf("gen moved from %d to %d; arming must not swap the driver", gen, m.gen)
	}
	if m.planBody != "here is the plan" {
		t.Errorf("planBody = %q, want the last assistant turn captured", m.planBody)
	}
}

// kickoffPrompt's "dispatch the work one task at a time" names the wrong tool
// once the session has a fleet: TestArmFlipsPhaseInPlace already proves a
// fleet-less run still gets that wording, so this covers the fork itself.
func TestKickoffPromptForFleet(t *testing.T) {
	if kickoffPromptFor(false) != kickoffPrompt {
		t.Errorf("kickoffPromptFor(false) should be the local-dispatch prompt")
	}
	if kickoffPromptFor(true) != archKickoffPrompt {
		t.Errorf("kickoffPromptFor(true) should be the fleet prompt")
	}
	if kickoffPromptFor(true) == kickoffPromptFor(false) {
		t.Error("the two kickoff prompts must not be identical")
	}
}

// Arming a session with a fleet wired sends the fleet kickoff, not the local
// one — the same fork proven in isolation by TestKickoffPromptForFleet, now
// through arm() itself.
func TestArmWithAFleetSendsTheFleetKickoff(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second, Fleet: newFakeFleetManager()})
	m.sessionID = "s1"
	m.drv = driver.NewWithWriter(driver.Options{}, nopCloser{&strings.Builder{}})

	m.arm()

	if lastBody(&m) != archKickoffPrompt {
		t.Errorf("arm() with a fleet sent %q, want the fleet kickoff prompt", lastBody(&m))
	}
}

func TestCapturePlanFallsBackToTheLastAssistantTurn(t *testing.T) {
	m := New(nil, Config{})
	m.turnText = "  the plan, as prose  "
	m.capturePlan()
	if m.planBody != "the plan, as prose" {
		t.Errorf("planBody = %q", m.planBody)
	}
}

func TestCapturePlanKeepsAnExplicitPlan(t *testing.T) {
	m := New(nil, Config{})
	m.planBody = "from PresentPlan"
	m.turnText = "a later chat message"
	m.capturePlan()
	if m.planBody != "from PresentPlan" {
		t.Errorf("planBody = %q, want the explicit plan kept", m.planBody)
	}
}

// Once complete, further turns are the user vetting the work.
func TestTurnAfterCompleteIsANormalChat(t *testing.T) {
	m := autoRunModel()
	m.ingestToolUse(finishEvent("completed", "done"))
	if m.phase != PhaseComplete {
		t.Fatalf("phase = %v, want COMPLETE", m.phase)
	}

	m.processing = false
	m.onTurnEnd(result())
	if m.processing {
		t.Error("a turn after COMPLETE must not be driven onward")
	}
}

// Finish is answered by acy itself, so a second call must not re-run the ending.
func TestFinishIsIdempotent(t *testing.T) {
	m := autoRunModel()
	m.ingestToolUse(finishEvent("completed", "first"))
	n := len(m.entries)
	m.ingestToolUse(finishEvent("completed", "second"))
	if len(m.entries) != n {
		t.Error("a second Finish should be ignored, not re-announced")
	}
}

// stackMode "ask" is the only case where the architect must be told to put
// the choice to the human before committing to a shape for the tickets —
// "chain" and "off" leave nothing to ask about.
func TestArchSystemPromptForAsksOnlyWhenStackModeIsAsk(t *testing.T) {
	askPrompt := ArchSystemPromptFor("ask")
	if !strings.Contains(askPrompt, mcp.Qualified(mcp.ToolAsk)) {
		t.Error(`ArchSystemPromptFor("ask") should mention mcp.Qualified(mcp.ToolAsk)`)
	}
	if !strings.Contains(askPrompt, "Before creating any tickets") {
		t.Error(`ArchSystemPromptFor("ask") should instruct asking before ticket creation`)
	}

	for _, mode := range []string{"chain", "off"} {
		prompt := ArchSystemPromptFor(mode)
		if strings.Contains(prompt, "Before creating any tickets") {
			t.Errorf("ArchSystemPromptFor(%q) should not contain the ask-before-tickets instruction", mode)
		}
	}
}
