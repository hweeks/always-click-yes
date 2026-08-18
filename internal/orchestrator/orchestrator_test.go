package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/mcp"
)

// fakeChild is a scripted claude process. Nothing here launches anything: the
// point of the Child interface is that the whole lifecycle is testable offline.
type fakeChild struct {
	events chan driver.Event
	sent   []string

	mu      sync.Mutex
	stopped bool
}

func newFakeChild() *fakeChild {
	return &fakeChild{events: make(chan driver.Event, 16)}
}

func (f *fakeChild) Events() <-chan driver.Event { return f.events }

func (f *fakeChild) Send(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, s)
	return nil
}

func (f *fakeChild) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.stopped {
		f.stopped = true
		close(f.events)
	}
}

func (f *fakeChild) wasStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

func (f *fakeChild) prompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return ""
	}
	return f.sent[0]
}

// resultWith builds the terminal event of a child turn.
func resultWith(structured string, cost float64, cacheRead int) driver.Event {
	ev := driver.Event{
		Type: driver.TypeResult, Subtype: "success", StopReason: "end_turn",
		TotalCostUSD: cost,
		Usage:        &driver.Usage{OutputTokens: 500, CacheReadInputTokens: cacheRead},
	}
	if structured != "" {
		ev.StructuredOutput = json.RawMessage(structured)
	}
	return ev
}

const goodReport = `{"outcome":"completed","summary":"Added the ledger.",
	"changed":[{"path":"internal/state/state.go","action":"modified"}],
	"verified":[{"check":"go test ./...","result":"pass"}]}`

// dispatchCall builds a blocked tools/call the way the mcp bridge would.
func dispatchCall(t *testing.T, args string) (*mcp.Pending, <-chan mcp.Answer) {
	t.Helper()
	return mcp.NewPending(mcp.Request{
		Tool:      "Dispatch",
		ToolUseID: "tu-1",
		Args:      json.RawMessage(args),
	})
}

func waitAnswer(t *testing.T, ch <-chan mcp.Answer) mcp.Answer {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(3 * time.Second):
		t.Fatal("the blocked call was never resolved — a real parent would hang here forever")
		return mcp.Answer{}
	}
}

// The happy path, and the single most important guarantee in the package: the
// blocked call gets resolved with the child's report.
func TestDispatchRunsAChildAndResolvesTheCall(t *testing.T) {
	child := newFakeChild()
	o := New(func(context.Context, Task) (Child, error) { return child, nil }, 1)
	defer o.Close()

	p, answers := dispatchCall(t, `{"title":"add the ledger","instruction":"Add a token ledger.",
		"context":["internal/state/state.go"],"success":"go test ./... passes"}`)
	st, err := o.Dispatch(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Task.ID != "t1" {
		t.Errorf("task id = %q, want t1", st.Task.ID)
	}
	if st.Task.SessionID == "" {
		t.Error("a child must get a pre-assigned session id, or its gates cannot be attributed")
	}

	child.events <- resultWith(goodReport, 0.42, 12_000)

	a := waitAnswer(t, answers)
	for _, want := range []string{"t1", "add the ledger", "COMPLETED", "Added the ledger.", "internal/state/state.go"} {
		if !strings.Contains(a.Text, want) {
			t.Errorf("answer missing %q:\n%s", want, a.Text)
		}
	}
	if !child.wasStopped() {
		t.Error("the child process was left running after it reported")
	}

	// The prompt has to stand alone — the child never saw the conversation.
	got := child.prompt()
	for _, want := range []string{"Add a token ledger.", "internal/state/state.go", "go test ./... passes"} {
		if !strings.Contains(got, want) {
			t.Errorf("task prompt missing %q:\n%s", want, got)
		}
	}
}

func TestDispatchRecordsCostAndTokens(t *testing.T) {
	child := newFakeChild()
	o := New(func(context.Context, Task) (Child, error) { return child, nil }, 1)
	defer o.Close()

	p, answers := dispatchCall(t, `{"title":"x","instruction":"do x"}`)
	if _, err := o.Dispatch(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	child.events <- resultWith(goodReport, 1.25, 300_000)
	waitAnswer(t, answers)

	tok, cost, n := o.Totals()
	if n != 1 {
		t.Errorf("dispatch count = %d, want 1", n)
	}
	if cost != 1.25 {
		t.Errorf("cost = %v, want 1.25", cost)
	}
	// The whole point: this volume was spent over there, not in the parent.
	if tok.CacheRead != 300_000 {
		t.Errorf("child cache reads = %d, want 300000", tok.CacheRead)
	}
}

// A child that ignores the schema, crashes or runs out of budget must still
// produce an honest answer. A silent "completed" is the one failure the parent
// cannot detect for itself.
func TestChildWithoutAReportDegradesHonestly(t *testing.T) {
	cases := []struct {
		name  string
		ev    driver.Event
		wants []string
	}{
		{"no structured output", resultWith("", 0.1, 10), []string{"BLOCKED", "no structured output"}},
		{"malformed", resultWith(`{"nope":true}`, 0.1, 10), []string{"BLOCKED", "no structured output"}},
		{"interrupted", func() driver.Event {
			ev := resultWith("", 0.1, 10)
			ev.TerminalReason = "aborted_streaming"
			return ev
		}(), []string{"FAILED", "interrupted"}},
		{"errored", func() driver.Event {
			ev := resultWith("", 0.1, 10)
			ev.IsError = true
			ev.Result = "budget exceeded"
			return ev
		}(), []string{"FAILED", "spend ceiling"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			child := newFakeChild()
			o := New(func(context.Context, Task) (Child, error) { return child, nil }, 1)
			defer o.Close()

			p, answers := dispatchCall(t, `{"title":"x","instruction":"do x"}`)
			if _, err := o.Dispatch(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			child.events <- c.ev

			a := waitAnswer(t, answers)
			for _, want := range c.wants {
				if !strings.Contains(a.Text, want) {
					t.Errorf("answer missing %q:\n%s", want, a.Text)
				}
			}
		})
	}
}

func TestLimitsCapEachTaskToTheRunRemainder(t *testing.T) {
	first, second := newFakeChild(), newFakeChild()
	children := []*fakeChild{first, second}
	o := NewWithLimits(func(context.Context, Task) (Child, error) {
		c := children[0]
		children = children[1:]
		return c, nil
	}, 1, Limits{DefaultTaskBudgetUSD: 2, RunBudgetUSD: 3})
	defer o.Close()

	p1, a1 := dispatchCall(t, `{"title":"one","instruction":"one"}`)
	st1, err := o.Dispatch(t.Context(), p1)
	if err != nil || st1.Task.BudgetUSD != 2 {
		t.Fatalf("first budget = $%.2f, err=%v", st1.Task.BudgetUSD, err)
	}
	first.events <- resultWith(goodReport, 2.25, 10)
	waitAnswer(t, a1)

	p2, a2 := dispatchCall(t, `{"title":"two","instruction":"two"}`)
	st2, err := o.Dispatch(t.Context(), p2)
	if err != nil || st2.Task.BudgetUSD != 0.75 {
		t.Fatalf("second budget = $%.2f, err=%v", st2.Task.BudgetUSD, err)
	}
	second.events <- resultWith(goodReport, 0.75, 10)
	waitAnswer(t, a2)

	p3, a3 := dispatchCall(t, `{"title":"three","instruction":"three"}`)
	if _, err := o.Dispatch(t.Context(), p3); err == nil {
		t.Fatal("dispatch beyond run budget succeeded")
	}
	if got := waitAnswer(t, a3).Text; !strings.Contains(got, "not started") || !strings.Contains(got, "do not retry") {
		t.Errorf("budget rejection = %q", got)
	}
}

func TestRateLimitIsAFailureNotADegradedSuccess(t *testing.T) {
	child := newFakeChild()
	o := New(func(context.Context, Task) (Child, error) { return child, nil }, 1)
	defer o.Close()
	p, answers := dispatchCall(t, `{"title":"x","instruction":"x"}`)
	if _, err := o.Dispatch(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	ev := resultWith("", 0.5, 10)
	ev.IsError, ev.APIErrorStatus, ev.Result = true, 429, "You've hit your session limit"
	child.events <- ev
	if got := waitAnswer(t, answers).Text; !strings.Contains(got, "FAILED") || !strings.Contains(got, "rate limit") {
		t.Errorf("rate-limit answer = %q", got)
	}
	if got := o.Statuses()[0].State; got != StateFailed {
		t.Errorf("state = %q, want failed", got)
	}
}

func TestRunBudgetIncludesResumedChildSpend(t *testing.T) {
	child := newFakeChild()
	o := NewWithLimits(func(context.Context, Task) (Child, error) { return child, nil }, 1,
		Limits{DefaultTaskBudgetUSD: 2, RunBudgetUSD: 3})
	defer o.Close()
	o.SeedSpent(2.5)
	p, answers := dispatchCall(t, `{"title":"last","instruction":"last"}`)
	st, err := o.Dispatch(t.Context(), p)
	if err != nil || st.Task.BudgetUSD != 0.5 {
		t.Fatalf("resumed budget = $%.2f, err=%v", st.Task.BudgetUSD, err)
	}
	child.events <- resultWith(goodReport, 0.5, 10)
	waitAnswer(t, answers)
}

// The child dying without a result event at all.
func TestChildStreamClosingIsReported(t *testing.T) {
	child := newFakeChild()
	o := New(func(context.Context, Task) (Child, error) { return child, nil }, 1)
	defer o.Close()

	p, answers := dispatchCall(t, `{"title":"x","instruction":"do x"}`)
	if _, err := o.Dispatch(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	child.Stop() // the process died

	a := waitAnswer(t, answers)
	if !strings.Contains(a.Text, "FAILED") {
		t.Errorf("a dead child should report a failure, got:\n%s", a.Text)
	}
}

// Cancel must resolve the blocked call, not merely kill the process. The `acy
// mcp` process waiting on the socket belongs to the parent's process group, so
// killing the child does not release it.
func TestCancelResolvesTheBlockedCall(t *testing.T) {
	child := newFakeChild()
	o := New(func(context.Context, Task) (Child, error) { return child, nil }, 1)
	defer o.Close()

	p, answers := dispatchCall(t, `{"title":"slow","instruction":"take forever"}`)
	st, err := o.Dispatch(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, o, st.Task.ID, StateRunning)

	o.CancelAll("interrupted by the user")

	a := waitAnswer(t, answers)
	if !strings.Contains(a.Text, "cancelled") {
		t.Errorf("want a cancellation notice, got:\n%s", a.Text)
	}
	if !child.wasStopped() {
		t.Error("cancelling left the child process running")
	}
}

// If the parent itself dies, the child is pointless: its report has nowhere to
// go. Keeping it alive would burn tokens on work nobody will ever read.
func TestAbandonedCallKillsTheChild(t *testing.T) {
	child := newFakeChild()
	o := New(func(context.Context, Task) (Child, error) { return child, nil }, 1)
	defer o.Close()

	p, _ := dispatchCall(t, `{"title":"x","instruction":"do x"}`)
	st, err := o.Dispatch(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, o, st.Task.ID, StateRunning)

	p.Abandon() // the mcp child disconnected: the parent is gone

	waitForState(t, o, st.Task.ID, StateCancelled)
	if !child.wasStopped() {
		t.Error("the child outlived the caller that was waiting for it")
	}
}

// Gate attribution. This is what lets the countdown tell a child's Edit from
// the parent's Read, now that both arrive on one socket.
func TestTaskForAttributesBySessionID(t *testing.T) {
	child := newFakeChild()
	o := New(func(context.Context, Task) (Child, error) { return child, nil }, 1)
	defer o.Close()

	p, _ := dispatchCall(t, `{"title":"x","instruction":"do x"}`)
	st, err := o.Dispatch(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}

	id, ok := o.TaskFor(st.Task.SessionID)
	if !ok || id != st.Task.ID {
		t.Errorf("TaskFor(child session) = %q,%v; want %q,true", id, ok, st.Task.ID)
	}
	if _, ok := o.TaskFor("some-other-session"); ok {
		t.Error("the parent's own session must not be attributed to a task")
	}
	if _, ok := o.TaskFor(""); ok {
		t.Error("an empty session id must never match")
	}
}

// fakeCodexChild is a fakeChild that also reports its own, server-assigned
// session id — standing in for *codex.Driver, whose thread id (unlike
// claude's caller-chosen --session-id) is only known once Start returns.
type fakeCodexChild struct {
	*fakeChild
	sessionID string
}

func (f *fakeCodexChild) SessionID() string { return f.sessionID }

// TestRunRekeysGateAttributionToACodexChildsRealSessionID proves the fix
// this exact scenario needs: a codex child never adopts the session id
// orchestrator pre-assigned (codex has no --session-id equivalent — the
// server assigns thread/start's own id), so without re-keying, TaskFor would
// never recognize the child's own approval requests as coming from a child
// at all. On a codex run that misattribution is not cosmetic: it is read as
// "the parent," and with ParentNoExec set, every one of the child's tool
// calls would be denied outright instead of counted down.
func TestRunRekeysGateAttributionToACodexChildsRealSessionID(t *testing.T) {
	const realSessionID = "codex-thread-abc"
	var mu sync.Mutex
	var spawnedPreAssignedID string

	o := New(func(_ context.Context, t Task) (Child, error) {
		mu.Lock()
		spawnedPreAssignedID = t.SessionID
		mu.Unlock()
		return &fakeCodexChild{fakeChild: newFakeChild(), sessionID: realSessionID}, nil
	}, 1)
	defer o.Close()

	p, answers := dispatchCall(t, `{"title":"x","instruction":"do x"}`)
	st, err := o.Dispatch(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	preAssignedID := st.Task.SessionID
	if preAssignedID == "" {
		t.Fatal("Dispatch must still hand the child a pre-assigned session id")
	}

	// state flips to "running" synchronously inside pump(), before the run()
	// goroutine it just started has even called spawn — so wait on the
	// rekey's own effect instead of the task's state.
	waitForTaskFor(t, o, realSessionID, st.Task.ID)

	mu.Lock()
	got := spawnedPreAssignedID
	mu.Unlock()
	if got != preAssignedID {
		t.Fatalf("spawn was called with SessionID %q, want the pre-assigned %q", got, preAssignedID)
	}
	if id, ok := o.TaskFor(realSessionID); !ok || id != st.Task.ID {
		t.Errorf("TaskFor(%q) = %q,%v; want %q,true — the child's real session id must be attributed", realSessionID, id, ok, st.Task.ID)
	}
	if _, ok := o.TaskFor(preAssignedID); ok {
		t.Errorf("TaskFor(%q) still matches after rekeying — the stale pre-assigned id must be forgotten", preAssignedID)
	}

	o.CancelAll("test done")
	waitAnswer(t, answers)
}

// With limit 1 the second dispatch waits rather than running concurrently.
func TestSecondDispatchQueuesBehindTheFirst(t *testing.T) {
	var mu sync.Mutex
	children := []*fakeChild{newFakeChild(), newFakeChild()}
	var spawned int

	o := New(func(context.Context, Task) (Child, error) {
		mu.Lock()
		defer mu.Unlock()
		c := children[spawned]
		spawned++
		return c, nil
	}, 1)
	defer o.Close()

	p1, ans1 := dispatchCall(t, `{"title":"first","instruction":"one"}`)
	st1, err := o.Dispatch(t.Context(), p1)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, o, st1.Task.ID, StateRunning)

	p2, ans2 := dispatchCall(t, `{"title":"second","instruction":"two"}`)
	st2, err := o.Dispatch(t.Context(), p2)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, o, st2.Task.ID, StateQueued)

	// Give a would-be second spawn every chance to happen before ruling it out.
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	got := spawned
	mu.Unlock()
	if got != 1 {
		t.Fatalf("spawned %d children at once, want 1 — two children in one working tree corrupt each other", got)
	}
	if n := o.Active(); n != 2 {
		t.Errorf("Active() = %d, want 2 (one running, one queued)", n)
	}

	children[0].events <- resultWith(goodReport, 0.1, 10)
	waitAnswer(t, ans1)

	// Only now does the second start.
	children[1].events <- resultWith(goodReport, 0.2, 20)
	waitAnswer(t, ans2)

	if _, _, n := o.Totals(); n != 2 {
		t.Errorf("ledger has %d tasks, want 2", n)
	}
}

func TestDispatchRejectsUnusableArguments(t *testing.T) {
	o := New(func(context.Context, Task) (Child, error) {
		t.Fatal("nothing should be spawned for a bad dispatch")
		return nil, nil
	}, 1)
	defer o.Close()

	for _, args := range []string{``, `not json`, `{}`, `{"title":"x"}`, `{"instruction":"   "}`} {
		p, answers := dispatchCall(t, args)
		if _, err := o.Dispatch(t.Context(), p); err == nil {
			t.Errorf("args %q should have been rejected", args)
		}
		// Even a rejected call must be answered, or the parent hangs.
		a := waitAnswer(t, answers)
		if !strings.Contains(a.Text, "could not be read") {
			t.Errorf("args %q: want an explanation, got %q", args, a.Text)
		}
	}
}

// A missing title is filled from the instruction rather than rejected: the
// title is for the human watching, and losing a whole task over it would be a
// poor trade.
func TestMissingTitleIsDerivedFromTheInstruction(t *testing.T) {
	spec, err := parseDispatch(json.RawMessage(`{"instruction":"Add the ledger\nand test it"}`))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Title != "Add the ledger" {
		t.Errorf("title = %q, want %q", spec.Title, "Add the ledger")
	}
}

func TestNewUUIDLooksLikeAV4(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := newUUID()
		if len(id) != 36 {
			t.Fatalf("uuid %q is %d chars, want 36", id, len(id))
		}
		if id[14] != '4' {
			t.Errorf("uuid %q is not version 4", id)
		}
		if seen[id] {
			t.Fatalf("uuid %q repeated", id)
		}
		seen[id] = true
	}
}

// Close must not deadlock or leak, with work in flight.
func TestCloseCancelsEverythingInFlight(t *testing.T) {
	child := newFakeChild()
	o := New(func(context.Context, Task) (Child, error) { return child, nil }, 1)

	p, answers := dispatchCall(t, `{"title":"x","instruction":"do x"}`)
	st, err := o.Dispatch(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, o, st.Task.ID, StateRunning)

	done := make(chan struct{})
	go func() { o.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close deadlocked")
	}
	waitAnswer(t, answers)
	o.Close() // twice must be safe
}

func waitForState(t *testing.T, o *Orchestrator, id, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range o.Statuses() {
			if s.Task.ID == id && s.State == want {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("task %s never reached state %q", id, want)
}

// waitForTaskFor polls TaskFor(sessionID) until it attributes to wantTaskID
// or the deadline passes. Used where the effect under test — rekeying —
// happens inside the run goroutine sometime after spawn returns, with no
// state transition of its own to poll on.
func waitForTaskFor(t *testing.T, o *Orchestrator, sessionID, wantTaskID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if id, ok := o.TaskFor(sessionID); ok && id == wantTaskID {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("TaskFor(%q) never attributed to %q", sessionID, wantTaskID)
}
