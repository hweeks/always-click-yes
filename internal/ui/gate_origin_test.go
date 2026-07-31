package ui

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/gate"
	"github.com/hweeks/always-click-yes/internal/mcp"
	"github.com/hweeks/always-click-yes/internal/orchestrator"
	"github.com/hweeks/always-click-yes/internal/state"
)

// fakeDispatcher answers only the question the gate asks of it: whose session is
// this? Everything else is inert.
type fakeDispatcher struct {
	sessions map[string]string // claude session id -> task id
	events   chan orchestrator.Event
	cancels  []string
}

func newFakeDispatcher(sessions map[string]string) *fakeDispatcher {
	return &fakeDispatcher{sessions: sessions, events: make(chan orchestrator.Event, 8)}
}

func (f *fakeDispatcher) Dispatch(context.Context, *mcp.Pending) (orchestrator.Status, error) {
	return orchestrator.Status{}, nil
}
func (f *fakeDispatcher) Events() <-chan orchestrator.Event { return f.events }
func (f *fakeDispatcher) TaskFor(sessionID string) (string, bool) {
	id, ok := f.sessions[sessionID]
	return id, ok
}
func (f *fakeDispatcher) Statuses() []orchestrator.Status      { return nil }
func (f *fakeDispatcher) Ledger() []state.Task                 { return nil }
func (f *fakeDispatcher) Totals() (state.Tokens, float64, int) { return state.Tokens{}, 0, 0 }
func (f *fakeDispatcher) Cancel(taskID, reason string)         { f.cancels = append(f.cancels, taskID) }
func (f *fakeDispatcher) CancelAll(reason string)              { f.cancels = append(f.cancels, "*") }
func (f *fakeDispatcher) Active() int                          { return 0 }
func (f *fakeDispatcher) RetryCooldown() bool                  { return false }

func pendingFrom(tool, sessionID string) (*gate.Pending, <-chan gate.Decision) {
	in := gate.PreToolUseInput{ToolName: tool, SessionID: sessionID}
	in.ToolInput, _ = json.Marshal(map[string]string{})
	return gate.NewPending(in)
}

// The regression this whole milestone has to not introduce.
//
// The bypass used to key on `m.phase == PhasePlan`, which was safe only because
// the plan registry has no Write or Edit in it. A dispatched child carries the
// FULL registry and shares this same socket, so keying on phase would wave
// through every edit a child made, with no countdown, in the phase where nobody
// is watching. Origin is the only thing that distinguishes them.
func TestChildToolsAlwaysCountDown(t *testing.T) {
	for _, tool := range []string{"Write", "Edit", "Read", "Grep", "NotebookEdit"} {
		t.Run(tool, func(t *testing.T) {
			for _, phase := range []Phase{PhasePlan, PhaseAutoRun, PhaseComplete} {
				m := New(nil, Config{
					Countdown:  30 * time.Second,
					Dispatcher: newFakeDispatcher(map[string]string{"child-sess": "t3"}),
				})
				m.phase = phase
				m.now = time.Now()

				p, decisions := pendingFrom(tool, "child-sess")
				m.enqueue(p)

				select {
				case d := <-decisions:
					t.Fatalf("phase %v: a child's %s was resolved immediately (%+v); "+
						"a child is unwatched, so it always owes a countdown", phase, tool, d)
				default:
				}
				if len(m.pending) != 1 {
					t.Fatalf("phase %v: want the child's %s queued, got %d pending",
						phase, tool, len(m.pending))
				}
				if m.pending[0].task != "t3" {
					t.Errorf("gate item task = %q, want t3 — the panel has to say who is asking",
						m.pending[0].task)
				}
			}
		})
	}
}

// The parent can only look, so making it count down for a Read would train the
// user to stop reading the countdown — which is the failure that matters.
func TestParentReadOnlyToolsPassStraightThrough(t *testing.T) {
	for _, tool := range []string{"Read", "Grep", "Glob", "WebSearch"} {
		m := New(nil, Config{
			Countdown:  30 * time.Second,
			Dispatcher: newFakeDispatcher(map[string]string{"child-sess": "t1"}),
		})
		m.phase = PhaseAutoRun
		m.now = time.Now()

		p, decisions := pendingFrom(tool, "parent-sess")
		m.enqueue(p)

		select {
		case d := <-decisions:
			if d.Behavior != gate.Allow {
				t.Errorf("%s: behavior = %v, want allow", tool, d.Behavior)
			}
		default:
			t.Errorf("%s from the parent should not raise a countdown", tool)
		}
		if len(m.pending) != 0 {
			t.Errorf("%s: %d gates queued, want 0", tool, len(m.pending))
		}
	}
}

// Bash survives a read-only registry — `bash -c 'rm -rf'` is not a read — so it
// keeps its countdown even for the parent.
func TestParentBashStillCountsDown(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.phase = PhasePlan
	m.now = time.Now()

	p, decisions := bashPending("rm -rf /")
	m.enqueue(p)

	select {
	case d := <-decisions:
		t.Fatalf("Bash was resolved immediately (%+v); it is the one mutation vector left", d)
	default:
	}
	if len(m.pending) != 1 {
		t.Fatalf("want Bash queued, got %d pending", len(m.pending))
	}
}

// A tool nobody has thought about must count down rather than sail through: the
// bypass is an allowlist for exactly this reason.
func TestUnknownParentToolCountsDown(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.phase = PhaseAutoRun
	m.now = time.Now()

	p, decisions := pendingFrom("SomeNewToolNobodyAnticipated", "parent-sess")
	m.enqueue(p)

	select {
	case d := <-decisions:
		t.Fatalf("an unrecognised tool was waved through (%+v)", d)
	default:
	}
	if len(m.pending) != 1 {
		t.Fatalf("want the unknown tool queued, got %d pending", len(m.pending))
	}
}

// Dispatch is answered by acy itself over the ask socket, so a countdown for it
// would tick invisibly and then "approve" a task that had already run.
func TestDispatchIsIntercepted(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.now = time.Now()

	p, decisions := pendingFrom(mcp.Qualified(mcp.ToolDispatch), "parent-sess")
	m.enqueue(p)

	select {
	case d := <-decisions:
		if d.Behavior != gate.Allow {
			t.Errorf("behavior = %v, want allow", d.Behavior)
		}
	default:
		t.Fatal("Dispatch raised a countdown; acy answers it itself")
	}
	if len(m.pending) != 0 {
		t.Errorf("%d gates queued for Dispatch, want 0", len(m.pending))
	}
}

// Without a dispatcher there are no children, so nothing can be attributed to
// one — and the parent rules must still apply rather than everything queuing.
func TestGateWorksWithNoDispatcher(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.phase = PhaseAutoRun
	m.now = time.Now()

	p, decisions := pendingFrom("Read", "parent-sess")
	m.enqueue(p)

	select {
	case <-decisions:
	default:
		t.Fatal("a parent Read should pass through even with delegation disabled")
	}
}

// StructuredOutput is how a --json-schema child hands back its report. The hook
// matches "*", so it raised a countdown like any other tool — measured at two
// per child, 30s each, guarding the model's own answer. Vetoing it could only
// destroy the report and make a finished task look failed.
func TestAnswerToolsAreNeverGated(t *testing.T) {
	for _, from := range []string{"child-sess", "parent-sess"} {
		m := New(nil, Config{
			Countdown:  30 * time.Second,
			Dispatcher: newFakeDispatcher(map[string]string{"child-sess": "t1"}),
		})
		m.phase = PhaseAutoRun
		m.now = time.Now()

		p, decisions := pendingFrom("StructuredOutput", from)
		m.enqueue(p)

		select {
		case d := <-decisions:
			if d.Behavior != gate.Allow {
				t.Errorf("from %s: behavior = %v, want allow", from, d.Behavior)
			}
		default:
			t.Errorf("from %s: StructuredOutput raised a countdown; it returns a result, it does not act", from)
		}
		if len(m.pending) != 0 {
			t.Errorf("from %s: %d gates queued, want 0", from, len(m.pending))
		}
	}
}
