package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
	"github.com/hweeks/always-click-yes/internal/fleet"
	"github.com/hweeks/always-click-yes/internal/gate"
	"github.com/hweeks/always-click-yes/internal/gitops"
	"github.com/hweeks/always-click-yes/internal/mcp"
	"github.com/hweeks/always-click-yes/internal/state"
)

// fakeFleetManager answers only what fleet.go's tool handlers need, mirroring
// fakeDispatcher in gate_origin_test.go.
type fakeFleetManager struct {
	mu sync.Mutex

	launched  []fleet.LaunchReq
	launchRet fleet.EngineerStatus
	launchErr error

	events chan fleet.Event

	answers   []fleetAnswerCall
	answerErr error

	statuses    []fleet.EngineerStatus
	active      int
	used, total int

	cancels []string

	ledger      []state.Engineer
	resumed     []state.Engineer
	resumeCalls int
	resumeErr   error
}

type fleetAnswerCall struct{ engineerID, questionID, text string }

func newFakeFleetManager() *fakeFleetManager {
	return &fakeFleetManager{events: make(chan fleet.Event, 8)}
}

func (f *fakeFleetManager) Launch(_ context.Context, req fleet.LaunchReq) (fleet.EngineerStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launched = append(f.launched, req)
	if f.launchErr != nil {
		return fleet.EngineerStatus{}, f.launchErr
	}
	return f.launchRet, nil
}
func (f *fakeFleetManager) Events() <-chan fleet.Event { return f.events }
func (f *fakeFleetManager) Answer(engineerID, questionID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, fleetAnswerCall{engineerID, questionID, text})
	return f.answerErr
}
func (f *fakeFleetManager) Statuses() []fleet.EngineerStatus { return f.statuses }
func (f *fakeFleetManager) Active() int                      { return f.active }
func (f *fakeFleetManager) Capacity() (int, int)             { return f.used, f.total }
func (f *fakeFleetManager) CancelAll(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, reason)
}
func (f *fakeFleetManager) Ledger() []state.Engineer { return f.ledger }
func (f *fakeFleetManager) Resume(_ context.Context, entries []state.Engineer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCalls++
	f.resumed = entries
	return f.resumeErr
}

// fleetPending builds an in-flight fleet tool call the way the ask bridge
// would, mirroring pendingFor in ask_test.go.
func fleetPending(tool, args string) (*mcp.Pending, <-chan mcp.Answer) {
	return mcp.NewPending(mcp.Request{Tool: tool, ToolUseID: "tu_fleet", Args: json.RawMessage(args)})
}

// --- LaunchEngineer ---

func TestLaunchEngineerRefusedWithoutFleet(t *testing.T) {
	m := &Model{phase: PhaseAutoRun}
	p, reply := fleetPending(mcp.ToolLaunchEngineer, `{}`)
	m.startLaunchEngineer(p)
	if got := answer(t, reply); got != mcp.FleetUnavailable {
		t.Errorf("answer = %q, want mcp.FleetUnavailable", got)
	}
}

func TestLaunchEngineerRefusedWhenNotArmed(t *testing.T) {
	m := &Model{phase: PhasePlan, fleet: newFakeFleetManager()}
	p, reply := fleetPending(mcp.ToolLaunchEngineer, `{"ticket":"T1","title":"x","brief":"y","success":"z"}`)
	m.startLaunchEngineer(p)
	if got := answer(t, reply); got != mcp.LaunchNotArmed {
		t.Errorf("answer = %q, want mcp.LaunchNotArmed", got)
	}
}

// Strict parsing: a missing required field names itself rather than launching
// on half a brief.
func TestLaunchEngineerMissingFieldsRefusesWithoutLaunching(t *testing.T) {
	fake := newFakeFleetManager()
	m := &Model{phase: PhaseAutoRun, fleet: fake}
	p, reply := fleetPending(mcp.ToolLaunchEngineer, `{"ticket":"T1"}`)
	m.startLaunchEngineer(p)

	got := answer(t, reply)
	for _, want := range []string{"title", "brief", "success"} {
		if !strings.Contains(got, want) {
			t.Errorf("answer = %q, want it to name the missing field %q", got, want)
		}
	}
	if len(fake.launched) != 0 {
		t.Error("nothing should have been launched with missing fields")
	}
}

func TestLaunchEngineerLaunches(t *testing.T) {
	fake := newFakeFleetManager()
	fake.launchRet = fleet.EngineerStatus{EngineerID: "e1", Host: "mbp1", Branch: "agent/e1-t1", Ticket: "T1"}
	fake.used, fake.total = 1, 4
	m := &Model{phase: PhaseAutoRun, fleet: fake}
	p, reply := fleetPending(mcp.ToolLaunchEngineer,
		`{"ticket":"T1","title":"add x","brief":"do the thing","success":"tests pass","host":"mbp1","budget_usd":5}`)
	m.startLaunchEngineer(p)

	if len(fake.launched) != 1 {
		t.Fatalf("want 1 launch, got %d", len(fake.launched))
	}
	req := fake.launched[0]
	if req.Ticket != "T1" || req.Title != "add x" || req.Brief != "do the thing" ||
		req.Success != "tests pass" || req.Host != "mbp1" || req.BudgetUSD != 5 {
		t.Errorf("launch request = %+v", req)
	}

	got := answer(t, reply)
	for _, want := range []string{"e1", "mbp1", "agent/e1-t1"} {
		if !strings.Contains(got, want) {
			t.Errorf("answer = %q, missing %q", got, want)
		}
	}
	if len(m.entries) != 1 {
		t.Fatalf("want 1 transcript entry, got %d", len(m.entries))
	}
}

// A full fleet (or a full/unknown pinned host) is a normal answer, not a
// failure: the text has to include per-host usage so the architect knows to
// Await rather than retry blind.
func TestLaunchEngineerFullFleetIsNotAFailure(t *testing.T) {
	fake := newFakeFleetManager()
	fake.launchErr = errors.New(`fleet: no capacity: mbp1 4/4`)
	fake.used, fake.total = 4, 4
	m := &Model{phase: PhaseAutoRun, fleet: fake}
	p, reply := fleetPending(mcp.ToolLaunchEngineer, `{"ticket":"T2","title":"y","brief":"z","success":"w"}`)
	m.startLaunchEngineer(p)

	got := answer(t, reply)
	if !strings.Contains(got, "4/4") {
		t.Errorf("answer = %q, want fleet capacity so the model knows what happened", got)
	}
	if !strings.Contains(got, "Await") {
		t.Errorf("answer = %q, want it to point the model at Await instead of retrying blind", got)
	}
}

// --- Await ---

func TestAwaitRefusedWithoutFleet(t *testing.T) {
	m := &Model{phase: PhaseAutoRun}
	p, reply := fleetPending(mcp.ToolAwait, `{}`)
	m.startAwait(p)
	if got := answer(t, reply); got != mcp.FleetUnavailable {
		t.Errorf("answer = %q, want mcp.FleetUnavailable", got)
	}
}

func TestAwaitRefusedWhenNotArmed(t *testing.T) {
	m := &Model{phase: PhasePlan, fleet: newFakeFleetManager()}
	p, reply := fleetPending(mcp.ToolAwait, `{}`)
	m.startAwait(p)
	if got := answer(t, reply); got != mcp.LaunchNotArmed {
		t.Errorf("answer = %q, want mcp.LaunchNotArmed", got)
	}
}

func TestAwaitNothingRunning(t *testing.T) {
	fake := newFakeFleetManager()
	m := &Model{phase: PhaseAutoRun, fleet: fake}
	p, reply := fleetPending(mcp.ToolAwait, `{}`)
	m.startAwait(p)

	if got := answer(t, reply); got != mcp.AwaitNothingRunning {
		t.Errorf("answer = %q, want mcp.AwaitNothingRunning", got)
	}
	if m.fleetAwait != nil {
		t.Error("nothing should be held when there is nothing that could ever produce an event")
	}
}

// A buffered event resolves Await immediately, oldest first.
func TestAwaitResolvesFromBuffer(t *testing.T) {
	fake := newFakeFleetManager()
	fake.active = 1
	m := &Model{phase: PhaseAutoRun, fleet: fake}
	m.fleetBuf = []fleet.Event{
		{EngineerID: "e1", Kind: fleet.KindStarted, Ticket: "T1", Host: "h1"},
		{EngineerID: "e2", Kind: fleet.KindStarted, Ticket: "T2", Host: "h2"},
	}

	p, reply := fleetPending(mcp.ToolAwait, `{}`)
	m.startAwait(p)

	got := answer(t, reply)
	if !strings.Contains(got, "e1") {
		t.Errorf("answer = %q, want the OLDEST buffered event (e1), not e2", got)
	}
	if len(m.fleetBuf) != 1 || m.fleetBuf[0].EngineerID != "e2" {
		t.Fatalf("buffer after Await = %+v, want just e2 left", m.fleetBuf)
	}
}

// The common case: nothing buffered, an engineer running — Await holds the
// Pending, exactly like a gate holds, and ingestFleet resolves it when the
// next event lands.
func TestAwaitHoldsThenResolvesOnEvent(t *testing.T) {
	fake := newFakeFleetManager()
	fake.active = 1
	m := &Model{phase: PhaseAutoRun, fleet: fake}
	p, reply := fleetPending(mcp.ToolAwait, `{}`)
	m.startAwait(p)

	if m.fleetAwait == nil {
		t.Fatal("Await with an engineer running and nothing buffered should hold, not resolve")
	}
	select {
	case a := <-reply:
		t.Fatalf("Await resolved before any event arrived: %q", a.Text)
	default:
	}

	m.ingestFleet(fleet.Event{
		EngineerID: "e1", Kind: fleet.KindResult,
		Result: &engineerwire.Result{Outcome: "completed", Summary: "done", PRURL: "https://example.com/pr/1", CostUSD: 1.5},
	})

	if m.fleetAwait != nil {
		t.Error("the held Await should be cleared once an event resolves it")
	}
	got := answer(t, reply)
	for _, want := range []string{"e1", "completed", "https://example.com/pr/1"} {
		if !strings.Contains(got, want) {
			t.Errorf("Await's answer = %q, missing %q", got, want)
		}
	}
}

// Events buffered with no Await pending must drain in the order they arrived.
func TestFleetEventBufferOrdering(t *testing.T) {
	fake := newFakeFleetManager()
	fake.active = 1
	m := &Model{phase: PhaseAutoRun, fleet: fake}

	for _, id := range []string{"e1", "e2", "e3"} {
		m.ingestFleet(fleet.Event{EngineerID: id, Kind: fleet.KindStarted, Ticket: "T-" + id})
	}
	if len(m.fleetBuf) != 3 {
		t.Fatalf("want 3 buffered events, got %d", len(m.fleetBuf))
	}
	for _, want := range []string{"e1", "e2", "e3"} {
		p, reply := fleetPending(mcp.ToolAwait, `{}`)
		m.startAwait(p)
		if got := answer(t, reply); !strings.Contains(got, want) {
			t.Fatalf("buffer order broken: answer = %q, want %q next", got, want)
		}
	}
}

// A question or a result must never be dropped to make room, even under a
// flood of progress events past the buffer's cap — only progress is
// disposable.
func TestFleetBufferNeverDropsQuestionsOrResults(t *testing.T) {
	fake := newFakeFleetManager()
	fake.active = 1
	m := &Model{phase: PhaseAutoRun, fleet: fake}

	m.ingestFleet(fleet.Event{
		EngineerID: "e1", Kind: fleet.KindQuestion,
		Question: &engineerwire.Question{QuestionID: "q1", Questions: []engineerwire.AskQuestion{
			{Question: "which?", Header: "H", Options: []engineerwire.AskOption{{Label: "a"}}},
		}},
	})
	for range fleetBufferCap + 10 {
		m.ingestFleet(fleet.Event{
			EngineerID: "e1", Kind: fleet.KindProgress,
			Progress: &engineerwire.Event{Kind: engineerwire.EventLog, Text: "tick"},
		})
	}

	var sawQuestion bool
	for _, ev := range m.fleetBuf {
		if ev.Kind == fleet.KindQuestion {
			sawQuestion = true
		}
	}
	if !sawQuestion {
		t.Fatal("the buffered question was dropped; a lost question corrupts the architect's view of the fleet")
	}
	if len(m.fleetBuf) > fleetBufferCap {
		t.Errorf("buffer grew to %d, want it capped at %d", len(m.fleetBuf), fleetBufferCap)
	}
}

// --- AnswerEngineer ---

func TestAnswerEngineerRefusedWithoutFleet(t *testing.T) {
	m := &Model{}
	p, reply := fleetPending(mcp.ToolAnswerEngineer, `{"engineer_id":"e1","question_id":"q1","answer":"yes"}`)
	m.startAnswerEngineer(p)
	if got := answer(t, reply); got != mcp.FleetUnavailable {
		t.Errorf("answer = %q, want mcp.FleetUnavailable", got)
	}
}

func TestAnswerEngineerMissingFields(t *testing.T) {
	fake := newFakeFleetManager()
	m := &Model{fleet: fake}
	p, reply := fleetPending(mcp.ToolAnswerEngineer, `{"engineer_id":"e1"}`)
	m.startAnswerEngineer(p)

	got := answer(t, reply)
	for _, want := range []string{"question_id", "answer"} {
		if !strings.Contains(got, want) {
			t.Errorf("answer = %q, want it to name the missing field %q", got, want)
		}
	}
	if len(fake.answers) != 0 {
		t.Error("nothing should have been answered with missing fields")
	}
}

func TestAnswerEngineerDelivers(t *testing.T) {
	fake := newFakeFleetManager()
	m := &Model{fleet: fake}
	p, reply := fleetPending(mcp.ToolAnswerEngineer, `{"engineer_id":"e1","question_id":"q1","answer":"sqlite"}`)
	m.startAnswerEngineer(p)

	if len(fake.answers) != 1 || fake.answers[0] != (fleetAnswerCall{"e1", "q1", "sqlite"}) {
		t.Fatalf("answer not delivered: %+v", fake.answers)
	}
	if got := answer(t, reply); !strings.Contains(got, "e1") {
		t.Errorf("answer = %q, want it to confirm delivery", got)
	}
}

func TestAnswerEngineerSurfacesError(t *testing.T) {
	fake := newFakeFleetManager()
	fake.answerErr = errors.New(`fleet: unknown engineer "e9"`)
	m := &Model{fleet: fake}
	p, reply := fleetPending(mcp.ToolAnswerEngineer, `{"engineer_id":"e9","question_id":"q1","answer":"x"}`)
	m.startAnswerEngineer(p)
	if got := answer(t, reply); !strings.Contains(got, "unknown engineer") {
		t.Errorf("answer = %q, want the fleet's error surfaced", got)
	}
}

// --- FleetStatus ---

func TestFleetStatusRefusedWithoutFleet(t *testing.T) {
	m := &Model{}
	p, reply := fleetPending(mcp.ToolFleetStatus, `{}`)
	m.startFleetStatus(p)
	if got := answer(t, reply); got != mcp.FleetUnavailable {
		t.Errorf("answer = %q, want mcp.FleetUnavailable", got)
	}
}

func TestFleetStatusRendersTable(t *testing.T) {
	fake := newFakeFleetManager()
	fake.statuses = []fleet.EngineerStatus{
		{EngineerID: "e1", Ticket: "T1", Host: "h1", State: fleet.StateRunning, CostUSD: 0.5},
		{EngineerID: "e2", Ticket: "T2", Host: "h2", State: fleet.StateDone, Outcome: "completed", PRURL: "https://example.com/pr/2", CostUSD: 1.2},
	}
	fake.used, fake.total = 2, 4
	m := &Model{fleet: fake}
	p, reply := fleetPending(mcp.ToolFleetStatus, `{}`)
	m.startFleetStatus(p)

	got := answer(t, reply)
	for _, want := range []string{"e1", "T1", "h1", fleet.StateRunning, "e2", "completed", "https://example.com/pr/2", "2/4"} {
		if !strings.Contains(got, want) {
			t.Errorf("fleet status = %q, missing %q", got, want)
		}
	}
}

// --- /fleet ---

func TestFleetSlashCommandRendersReport(t *testing.T) {
	fake := newFakeFleetManager()
	fake.statuses = []fleet.EngineerStatus{{EngineerID: "e1", Ticket: "T1", Host: "h1", State: fleet.StateRunning}}
	fake.used, fake.total = 1, 4
	m := New(nil, Config{Fleet: fake})
	before := len(m.entries)

	if cmd := m.runCommand("fleet", ""); cmd != nil {
		t.Error("runCommand(fleet) should not return a tea.Cmd")
	}
	if len(m.entries) != before+1 {
		t.Fatalf("entries = %d, want %d (one report appended)", len(m.entries), before+1)
	}
	got := m.entries[len(m.entries)-1].body
	if !strings.Contains(got, "e1") || !strings.Contains(got, "1/4") {
		t.Errorf("fleet report = %q", got)
	}
}

func TestFleetSlashCommandWithoutFleet(t *testing.T) {
	m := New(nil, Config{})
	m.runCommand("fleet", "")
	got := m.entries[len(m.entries)-1].body
	if !strings.Contains(got, "no fleet") {
		t.Errorf("fleet report = %q, want it to say no fleet is configured", got)
	}
}

// --- routing ---

// The ask socket carries several different tools now; this proves each of
// the four fleet ones actually reaches its own handler through the real
// askMsg switch, not just through calling the handler directly.
func TestAskMsgRoutesFleetTools(t *testing.T) {
	fake := newFakeFleetManager()
	fake.launchRet = fleet.EngineerStatus{EngineerID: "e1", Host: "h1", Branch: "b1"}
	fake.statuses = []fleet.EngineerStatus{{EngineerID: "e1"}}

	m := New(nil, Config{Fleet: fake})
	m.phase = PhaseAutoRun

	p, reply := fleetPending(mcp.ToolLaunchEngineer, `{"ticket":"T1","title":"x","brief":"y","success":"z"}`)
	next, _ := m.Update(askMsg{p})
	m = next.(Model)
	if len(fake.launched) != 1 {
		t.Fatal("askMsg did not route LaunchEngineer to startLaunchEngineer")
	}
	answer(t, reply)

	p, reply = fleetPending(mcp.ToolFleetStatus, `{}`)
	next, _ = m.Update(askMsg{p})
	m = next.(Model)
	if got := answer(t, reply); !strings.Contains(got, "e1") {
		t.Fatalf("askMsg did not route FleetStatus: %q", got)
	}

	p, reply = fleetPending(mcp.ToolAnswerEngineer, `{"engineer_id":"e1","question_id":"q1","answer":"ok"}`)
	next, _ = m.Update(askMsg{p})
	m = next.(Model)
	answer(t, reply)
	if len(fake.answers) != 1 {
		t.Fatal("askMsg did not route AnswerEngineer to startAnswerEngineer")
	}

	// Await, with nothing running: the immediate AwaitNothingRunning refusal
	// proves this landed in startAwait rather than falling through to openAsk.
	p, reply = fleetPending(mcp.ToolAwait, `{}`)
	next, _ = m.Update(askMsg{p})
	m = next.(Model)
	if got := answer(t, reply); got != mcp.AwaitNothingRunning {
		t.Fatalf("askMsg did not route Await: %q", got)
	}
}

// The PreToolUse hook matches "*", so without this the fleet tools would raise
// a countdown that ticks invisibly behind the ask-socket answer acy already
// gave — mirrors TestDispatchIsIntercepted.
func TestFleetToolsAreIntercepted(t *testing.T) {
	for _, tool := range []string{mcp.ToolLaunchEngineer, mcp.ToolAwait, mcp.ToolAnswerEngineer, mcp.ToolFleetStatus} {
		t.Run(tool, func(t *testing.T) {
			m := New(nil, Config{Countdown: 30 * time.Second})
			m.now = time.Now()

			p, decisions := pendingFrom(mcp.Qualified(tool), "parent-sess")
			m.enqueue(p)

			select {
			case d := <-decisions:
				if d.Behavior != gate.Allow {
					t.Errorf("behavior = %v, want allow", d.Behavior)
				}
			default:
				t.Fatalf("%s raised a countdown; acy answers it itself over the ask socket", tool)
			}
			if len(m.pending) != 0 {
				t.Errorf("%d gates queued for %s, want 0", len(m.pending), tool)
			}
		})
	}
}

// --- pr events ---

// A pr event renders in the transcript and, held behind an Await, resolves
// it with text carrying the URL and head — mirroring TestAwaitHoldsThenResolvesOnEvent
// for the new Kind.
func TestAwaitReturnsPREvent(t *testing.T) {
	fake := newFakeFleetManager()
	fake.active = 1
	m := &Model{phase: PhaseAutoRun, fleet: fake}
	p, reply := fleetPending(mcp.ToolAwait, `{}`)
	m.startAwait(p)

	if m.fleetAwait == nil {
		t.Fatal("Await with an engineer running and nothing buffered should hold, not resolve")
	}

	m.ingestFleet(fleet.Event{
		Kind: fleet.KindPR,
		PR:   &fleet.PREvent{URL: "https://example.com/pr/9", Head: "acy/t9-fix", Number: 9, State: "merged"},
	})

	if m.fleetAwait != nil {
		t.Error("the held Await should be cleared once the pr event resolves it")
	}
	got := answer(t, reply)
	for _, want := range []string{"merged", "https://example.com/pr/9", "acy/t9-fix"} {
		if !strings.Contains(got, want) {
			t.Errorf("Await's answer = %q, missing %q", got, want)
		}
	}

	if len(m.entries) != 1 {
		t.Fatalf("want 1 transcript entry for the pr event, got %d", len(m.entries))
	}
	body := m.entries[0].body
	for _, want := range []string{"PR merged", "https://example.com/pr/9", "acy/t9-fix"} {
		if !strings.Contains(body, want) {
			t.Errorf("transcript entry = %q, missing %q", body, want)
		}
	}
}

// --- stack events ---

// A successful link renders and Awaits with the full chain named, mirroring
// TestAwaitReturnsPREvent for the new Kind.
func TestAwaitReturnsStackLinkEvent(t *testing.T) {
	fake := newFakeFleetManager()
	fake.active = 1
	m := &Model{phase: PhaseAutoRun, fleet: fake}
	p, reply := fleetPending(mcp.ToolAwait, `{}`)
	m.startAwait(p)

	m.ingestFleet(fleet.Event{
		Kind:  fleet.KindStack,
		Stack: &fleet.StackEvent{Op: "link", Branches: []string{"acy/t1-base", "acy/t2-child"}},
	})

	if m.fleetAwait != nil {
		t.Error("the held Await should be cleared once the stack event resolves it")
	}
	got := answer(t, reply)
	for _, want := range []string{"acy/t1-base", "acy/t2-child", "->"} {
		if !strings.Contains(got, want) {
			t.Errorf("Await's answer = %q, missing %q", got, want)
		}
	}

	if len(m.entries) != 1 {
		t.Fatalf("want 1 transcript entry for the stack event, got %d", len(m.entries))
	}
	body := m.entries[0].body
	for _, want := range []string{"acy/t1-base", "acy/t2-child", "->"} {
		if !strings.Contains(body, want) {
			t.Errorf("transcript entry = %q, missing %q", body, want)
		}
	}
}

// A successful sync renders and Awaits as a short, unremarkable line.
func TestAwaitReturnsStackSyncEvent(t *testing.T) {
	fake := newFakeFleetManager()
	fake.active = 1
	m := &Model{phase: PhaseAutoRun, fleet: fake}
	p, reply := fleetPending(mcp.ToolAwait, `{}`)
	m.startAwait(p)

	m.ingestFleet(fleet.Event{
		Kind:  fleet.KindStack,
		Stack: &fleet.StackEvent{Op: "sync"},
	})

	got := answer(t, reply)
	if !strings.Contains(got, "sync") {
		t.Errorf("Await's answer = %q, want it to mention the sync", got)
	}
	if len(m.entries) != 1 {
		t.Fatalf("want 1 transcript entry for the stack event, got %d", len(m.entries))
	}
	if !strings.Contains(m.entries[0].body, "sync") {
		t.Errorf("transcript entry = %q, want it to mention the sync", m.entries[0].body)
	}
}

// A conflict must name the branch and tell the architect plainly this needs a
// human and will not be retried automatically — the one case where Await's
// text has to stop the model from just trying again.
func TestAwaitReturnsStackConflictEvent(t *testing.T) {
	fake := newFakeFleetManager()
	fake.active = 1
	m := &Model{phase: PhaseAutoRun, fleet: fake}
	p, reply := fleetPending(mcp.ToolAwait, `{}`)
	m.startAwait(p)

	conflictErr := fmt.Errorf("gh-stack: %w", gitops.ErrStackConflict)
	m.ingestFleet(fleet.Event{
		Kind:  fleet.KindStack,
		Stack: &fleet.StackEvent{Op: "sync", Branch: "acy/t2-child", Err: conflictErr},
	})

	got := answer(t, reply)
	for _, want := range []string{"acy/t2-child", "human"} {
		if !strings.Contains(got, want) {
			t.Errorf("Await's answer = %q, missing %q", got, want)
		}
	}
	if !strings.Contains(strings.ToLower(got), "not retry") && !strings.Contains(strings.ToLower(got), "will not auto-resolve") {
		t.Errorf("Await's answer = %q, want it to say this will not be retried automatically", got)
	}

	if len(m.entries) != 1 {
		t.Fatalf("want 1 transcript entry for the stack event, got %d", len(m.entries))
	}
	ent := m.entries[0]
	if ent.kind != eWarn {
		t.Errorf("transcript entry kind = %v, want eWarn for a conflict", ent.kind)
	}
	if !strings.Contains(ent.body, "acy/t2-child") {
		t.Errorf("transcript entry = %q, want it to name the conflicting branch", ent.body)
	}
}

// --- abandonment ---

// An Await left holding when the session ends must still be answered, or the
// mcp child (if somehow still alive) would hold claude blocked forever.
func TestFleetAwaitAbandonedWhenStreamCloses(t *testing.T) {
	fake := newFakeFleetManager()
	fake.active = 1
	m := sizedModel(t)
	m.fleet = fake
	m.phase = PhaseAutoRun
	m.now = time.Now()

	p, reply := fleetPending(mcp.ToolAwait, `{}`)
	m.startAwait(p)
	if m.fleetAwait == nil {
		t.Fatal("setup: Await should be held")
	}

	next, _ := m.Update(streamClosedMsg{gen: m.gen})
	if next.(Model).fleetAwait != nil {
		t.Error("the held Await survived the session ending")
	}
	if got := answer(t, reply); got != mcp.SupervisorGone {
		t.Errorf("abandoned Await answer = %q, want mcp.SupervisorGone", got)
	}
}

// --- cancellation on quit ---

// Engineers are durable remote work; only an explicit /quit tears them down
// (mirroring cancelDispatches, which Esc/interject already reaches).
func TestQuitCancelsRunningEngineers(t *testing.T) {
	fake := newFakeFleetManager()
	fake.active = 1
	m := New(nil, Config{Fleet: fake})

	if cmd := m.runCommand("quit", ""); cmd == nil {
		t.Fatal("runCommand(quit) should return tea.Quit")
	}
	if len(fake.cancels) == 0 {
		t.Fatal("quitting should cancel every running engineer")
	}
}

func TestQuitDoesNotCancelWhenFleetIsIdle(t *testing.T) {
	fake := newFakeFleetManager() // active = 0
	m := New(nil, Config{Fleet: fake})
	m.runCommand("quit", "")
	if len(fake.cancels) != 0 {
		t.Error("quitting an idle fleet should not append a spurious cancel notice")
	}
}

// --- Frame ---

func TestFrameCarriesEngineersAndFleetSummary(t *testing.T) {
	fake := newFakeFleetManager()
	fake.statuses = []fleet.EngineerStatus{
		{EngineerID: "e1", Ticket: "T1", Title: "add x", Host: "h1", Branch: "agent/e1", State: fleet.StateRunning, CostUSD: 0.25},
	}
	fake.active, fake.used, fake.total = 1, 1, 4

	m := &Model{fleet: fake}
	m.syncFleet()

	fr := m.Frame()
	if len(fr.Engineers) != 1 {
		t.Fatalf("want 1 engineer in the frame, got %d", len(fr.Engineers))
	}
	e := fr.Engineers[0]
	if e.ID != "e1" || e.Ticket != "T1" || e.Title != "add x" || e.Host != "h1" ||
		e.Branch != "agent/e1" || e.State != fleet.StateRunning || e.CostUSD != 0.25 {
		t.Errorf("frame engineer = %+v", e)
	}
	if fr.Fleet.Active != 1 || fr.Fleet.CapacityUsed != 1 || fr.Fleet.CapacityTotal != 4 {
		t.Errorf("frame fleet summary = %+v", fr.Fleet)
	}
}

func TestFrameEngineersEmptyWithoutFleet(t *testing.T) {
	m := &Model{}
	fr := m.Frame()
	if fr.Engineers == nil {
		t.Error("Engineers must be [], never null, so a client never has to handle the two differently")
	}
	if len(fr.Engineers) != 0 {
		t.Errorf("want no engineers with no fleet wired, got %d", len(fr.Engineers))
	}
}

// --- fleetEventText / fleetEntry: KindResult, with and without Verification ---

func baseResultEvent(checks []engineerwire.VerifyCheck) fleet.Event {
	return fleet.Event{
		EngineerID: "e1",
		Kind:       fleet.KindResult,
		Result: &engineerwire.Result{
			Outcome:      "completed",
			Summary:      "done",
			PRURL:        "https://example.com/pr/1",
			CostUSD:      1.5,
			Verification: checks,
		},
	}
}

func TestFleetEventTextResultVerification(t *testing.T) {
	tests := []struct {
		name   string
		checks []engineerwire.VerifyCheck
		want   string
	}{
		{
			name:   "no verification is byte-identical to the pre-verification rendering",
			checks: nil,
			want:   "engineer_id e1 completed — done\npr_url https://example.com/pr/1\ncost_usd 1.5000",
		},
		{
			name: "all passed",
			checks: []engineerwire.VerifyCheck{
				{Name: "go build ./...", Status: engineerwire.VerifyPassed},
				{Name: "go test -race ./...", Status: engineerwire.VerifyPassed},
			},
			want: "engineer_id e1 completed — done\npr_url https://example.com/pr/1\ncost_usd 1.5000" +
				"\nverification 2 passed",
		},
		{
			name: "one failed",
			checks: []engineerwire.VerifyCheck{
				{Name: "go build ./...", Status: engineerwire.VerifyPassed},
				{Name: "golangci-lint run ./...", Status: engineerwire.VerifyFailed, ExitCode: 1},
			},
			want: "engineer_id e1 completed — done\npr_url https://example.com/pr/1\ncost_usd 1.5000" +
				"\nverification 1 passed, 1 failed" +
				"\nverification FAILED: golangci-lint run ./... (exit 1) (see journal for output)",
		},
		{
			name: "skipped and timeout counted and named, no failure line",
			checks: []engineerwire.VerifyCheck{
				{Name: "some-tool", Status: engineerwire.VerifySkipped},
				{Name: "slow-tool", Status: engineerwire.VerifyTimeout},
			},
			want: "engineer_id e1 completed — done\npr_url https://example.com/pr/1\ncost_usd 1.5000" +
				"\nverification 1 skipped, 1 timeout",
		},
		{
			name: "more than 3 failures caps the named list and reports the rest",
			checks: []engineerwire.VerifyCheck{
				{Name: "c1", Status: engineerwire.VerifyFailed, ExitCode: 1},
				{Name: "c2", Status: engineerwire.VerifyFailed, ExitCode: 2},
				{Name: "c3", Status: engineerwire.VerifyFailed, ExitCode: 3},
				{Name: "c4", Status: engineerwire.VerifyFailed, ExitCode: 4},
				{Name: "c5", Status: engineerwire.VerifyFailed, ExitCode: 5},
			},
			want: "engineer_id e1 completed — done\npr_url https://example.com/pr/1\ncost_usd 1.5000" +
				"\nverification 5 failed" +
				"\nverification FAILED: c1 (exit 1)" +
				"\nverification FAILED: c2 (exit 2)" +
				"\nverification FAILED: c3 (exit 3)" +
				"\nverification ...and 2 more (see journal for output)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fleetEventText(baseResultEvent(tt.checks))
			if got != tt.want {
				t.Errorf("fleetEventText =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestFleetEntryResultVerification(t *testing.T) {
	tests := []struct {
		name     string
		checks   []engineerwire.VerifyCheck
		wantBody string
		wantKind ekind
	}{
		{
			name:     "no verification is byte-identical to the pre-verification rendering",
			checks:   nil,
			wantBody: "■ e1 finished — completed\ndone\nPR: https://example.com/pr/1\n$1.5000",
			wantKind: eGood,
		},
		{
			name: "all passed stays eGood",
			checks: []engineerwire.VerifyCheck{
				{Name: "go build ./...", Status: engineerwire.VerifyPassed},
				{Name: "go test -race ./...", Status: engineerwire.VerifyPassed},
			},
			wantBody: "■ e1 finished — completed\ndone\nPR: https://example.com/pr/1\n$1.5000" +
				"\nverification: 2 passed",
			wantKind: eGood,
		},
		{
			name: "one failed forces eWarn even though Outcome is completed",
			checks: []engineerwire.VerifyCheck{
				{Name: "go build ./...", Status: engineerwire.VerifyPassed},
				{Name: "golangci-lint run ./...", Status: engineerwire.VerifyFailed, ExitCode: 1},
			},
			wantBody: "■ e1 finished — completed\ndone\nPR: https://example.com/pr/1\n$1.5000" +
				"\nverification: 1 passed, 1 failed" +
				"\n  FAILED: golangci-lint run ./... (exit 1) (see journal for output)",
			wantKind: eWarn,
		},
		{
			name: "skipped and timeout counted and named, stays eGood",
			checks: []engineerwire.VerifyCheck{
				{Name: "some-tool", Status: engineerwire.VerifySkipped},
				{Name: "slow-tool", Status: engineerwire.VerifyTimeout},
			},
			wantBody: "■ e1 finished — completed\ndone\nPR: https://example.com/pr/1\n$1.5000" +
				"\nverification: 1 skipped, 1 timeout",
			wantKind: eGood,
		},
		{
			name: "more than 3 failures caps the named list and reports the rest",
			checks: []engineerwire.VerifyCheck{
				{Name: "c1", Status: engineerwire.VerifyFailed, ExitCode: 1},
				{Name: "c2", Status: engineerwire.VerifyFailed, ExitCode: 2},
				{Name: "c3", Status: engineerwire.VerifyFailed, ExitCode: 3},
				{Name: "c4", Status: engineerwire.VerifyFailed, ExitCode: 4},
				{Name: "c5", Status: engineerwire.VerifyFailed, ExitCode: 5},
			},
			wantBody: "■ e1 finished — completed\ndone\nPR: https://example.com/pr/1\n$1.5000" +
				"\nverification: 5 failed" +
				"\n  FAILED: c1 (exit 1)" +
				"\n  FAILED: c2 (exit 2)" +
				"\n  FAILED: c3 (exit 3)" +
				"\n  ...and 2 more (see journal for output)",
			wantKind: eWarn,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fleetEntry(baseResultEvent(tt.checks))
			if got.body != tt.wantBody {
				t.Errorf("fleetEntry body =\n%q\nwant\n%q", got.body, tt.wantBody)
			}
			if got.kind != tt.wantKind {
				t.Errorf("fleetEntry kind = %v, want %v", got.kind, tt.wantKind)
			}
		})
	}
}
