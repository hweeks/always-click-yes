package fleet

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
	"github.com/hweeks/always-click-yes/internal/state"
)

// --- fixtures ---

func testHost(name string, max int) config.FleetHost {
	return config.FleetHost{Name: name, MaxEngineers: new(max)}
}

func testFleetConfig(hosts ...config.FleetHost) config.FleetConfig {
	return config.FleetConfig{
		BaseBranch:        "main",
		DeadmanHours:      new(1.0),
		EngineerBudgetUSD: new(2.0),
		Hosts:             hosts,
	}
}

// mockEngine is one engineer's controllable half of a mockTransport: the
// test sends outbound wire messages on msgs, and inspects received for
// whatever the manager forwarded inbound (Answer/Cancel).
type mockEngine struct {
	id   string
	msgs chan any

	mu       sync.Mutex
	received []any
}

func (e *mockEngine) send(msg any) { e.msgs <- msg }

func (e *mockEngine) receivedCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.received)
}

func (e *mockEngine) receivedSnapshot() []any {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]any, len(e.received))
	copy(out, e.received)
	return out
}

// mockTransport is a Transport backed entirely by in-memory mockEngines —
// no process, no journal. Start assigns each call a wire id in call order;
// byTicket lets a test fetch the engine for a given LaunchReq.Ticket
// regardless of concurrent launch ordering.
type mockTransport struct {
	mu        sync.Mutex
	n         int
	engines   map[string]*mockEngine
	byTicket  map[string]*mockEngine
	startErrs map[int]error                // 1-based call index -> error to return instead of starting
	fromSeqs  map[string][]int64           // engineerID -> fromSeq per Attach call, in order
	specs     map[string]engineerwire.Spec // ticket -> the Spec Start was actually called with
}

func newMockTransport() *mockTransport {
	return &mockTransport{
		engines:  map[string]*mockEngine{},
		byTicket: map[string]*mockEngine{},
		fromSeqs: map[string][]int64{},
		specs:    map[string]engineerwire.Spec{},
	}
}

// specFor returns the Spec the manager actually handed Start for ticket.
func (mt *mockTransport) specFor(ticket string) engineerwire.Spec {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	return mt.specs[ticket]
}

// registerEngine seats an engine directly, bypassing Start — for tests that
// resume an engineer whose process (and wire id) predates this transport
// instance.
func (mt *mockTransport) registerEngine(id string) *mockEngine {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	eng := &mockEngine{id: id, msgs: make(chan any, 16)}
	mt.engines[id] = eng
	return eng
}

// fromSeqCalls is every fromSeq Attach was called with for engineerID, in order.
func (mt *mockTransport) fromSeqCalls(id string) []int64 {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	out := make([]int64, len(mt.fromSeqs[id]))
	copy(out, mt.fromSeqs[id])
	return out
}

// failNthStart makes the n'th Start call (1-based, across the whole
// transport's life) fail with err instead of creating an engine.
func (mt *mockTransport) failNthStart(n int, err error) {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	if mt.startErrs == nil {
		mt.startErrs = map[int]error{}
	}
	mt.startErrs[n] = err
}

func (mt *mockTransport) forHost(config.FleetHost) Transport { return mt }

func (mt *mockTransport) Start(_ context.Context, spec engineerwire.Spec) (StartAck, error) {
	mt.mu.Lock()
	mt.n++
	n := mt.n
	if err, ok := mt.startErrs[n]; ok {
		mt.mu.Unlock()
		return StartAck{}, err
	}
	id := fmt.Sprintf("w%d", n)
	eng := &mockEngine{id: id, msgs: make(chan any, 16)}
	mt.engines[id] = eng
	mt.byTicket[spec.Ticket] = eng
	mt.specs[spec.Ticket] = spec
	mt.mu.Unlock()
	return StartAck{EngineerID: id, PID: 1000 + n}, nil
}

func (mt *mockTransport) Attach(ctx context.Context, engineerID string, fromSeq int64, in io.Reader, onMsg func(any)) error {
	mt.mu.Lock()
	eng := mt.engines[engineerID]
	mt.fromSeqs[engineerID] = append(mt.fromSeqs[engineerID], fromSeq)
	mt.mu.Unlock()
	if eng == nil {
		return fmt.Errorf("mock: unknown engineer %q", engineerID)
	}

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		dec := engineerwire.NewDecoder(in)
		for {
			msg, err := dec.Decode()
			if err != nil {
				return
			}
			eng.mu.Lock()
			eng.received = append(eng.received, msg)
			eng.mu.Unlock()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-eng.msgs:
			if !ok {
				return fmt.Errorf("mock: engine %q closed", engineerID)
			}
			onMsg(msg)
			if _, isResult := msg.(engineerwire.Result); isResult {
				return nil
			}
		}
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// drainEvent reads the next Event off m.Events(), failing the test if none
// arrives within timeout.
func drainEvent(t *testing.T, m *Manager, timeout time.Duration) Event {
	t.Helper()
	select {
	case ev, ok := <-m.Events():
		if !ok {
			t.Fatal("Events channel closed unexpectedly")
		}
		return ev
	case <-time.After(timeout):
		t.Fatal("timed out waiting for an event")
	}
	panic("unreachable")
}

// --- capacity / host picking ---

func TestManagerLaunchLeastLoadedPick(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 2), testHost("b", 2))
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	st1, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"})
	if err != nil {
		t.Fatalf("Launch 1: %v", err)
	}
	if st1.Host != "a" {
		t.Errorf("first launch host = %q, want %q (first host, both empty)", st1.Host, "a")
	}

	st2, err := m.Launch(context.Background(), LaunchReq{Ticket: "T2"})
	if err != nil {
		t.Fatalf("Launch 2: %v", err)
	}
	if st2.Host != "b" {
		t.Errorf("second launch host = %q, want %q (least loaded after a=1,b=0)", st2.Host, "b")
	}

	st3, err := m.Launch(context.Background(), LaunchReq{Ticket: "T3"})
	if err != nil {
		t.Fatalf("Launch 3: %v", err)
	}
	if st3.Host != "a" {
		t.Errorf("third launch host = %q, want %q (a=1,b=1 ties, config order picks a)", st3.Host, "a")
	}
}

func TestManagerLaunchHostPin(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 1), testHost("b", 1))
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	st, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1", Host: "b"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if st.Host != "b" {
		t.Errorf("Host = %q, want %q", st.Host, "b")
	}

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T2", Host: "nope"}); err == nil {
		t.Error("Launch with unknown host pin: want error, got nil")
	} else if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %q, want it to name the unknown host", err.Error())
	}

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T3", Host: "b"}); err == nil {
		t.Error("Launch pinned to a full host: want error, got nil")
	} else if !strings.Contains(err.Error(), "b") {
		t.Errorf("error = %q, want it to name the full host", err.Error())
	}
}

func TestManagerNoCapacityNamesPerHostUsage(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("alpha", 1), testHost("beta", 1))
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"}); err != nil {
		t.Fatalf("Launch 1: %v", err)
	}
	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T2"}); err != nil {
		t.Fatalf("Launch 2: %v", err)
	}

	_, err := m.Launch(context.Background(), LaunchReq{Ticket: "T3"})
	if err == nil {
		t.Fatal("Launch beyond total capacity: want error, got nil")
	}
	for _, want := range []string{"alpha 1/1", "beta 1/1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestManagerParallelLaunchesRespectCapacity(t *testing.T) {
	mt := newMockTransport()
	const perHost = 3
	cfg := testFleetConfig(testHost("a", perHost), testHost("b", perHost))
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	const attempts = 10 // > 2*perHost, so some must fail with "no capacity"
	var wg sync.WaitGroup
	results := make([]error, attempts)
	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := m.Launch(context.Background(), LaunchReq{Ticket: fmt.Sprintf("T%d", i)})
			results[i] = err
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		} else if !strings.Contains(err.Error(), "no capacity") {
			t.Errorf("unexpected launch error: %v", err)
		}
	}
	if succeeded != 2*perHost {
		t.Errorf("succeeded = %d, want %d (exactly total capacity)", succeeded, 2*perHost)
	}
	if used, total := m.Capacity(); used != 2*perHost || total != 2*perHost {
		t.Errorf("Capacity() = (%d, %d), want (%d, %d)", used, total, 2*perHost, 2*perHost)
	}
	if got := m.Active(); got != 2*perHost {
		t.Errorf("Active() = %d, want %d", got, 2*perHost)
	}
}

// --- no queue: Launch fails outright when full, and a freed slot admits ---
// --- the next Launch call rather than anything being queued ---

func TestManagerNoQueueLaunchFailsWhenFullThenSlotFrees(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 1))
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"}); err != nil {
		t.Fatalf("Launch 1: %v", err)
	}
	drainEvent(t, m, time.Second) // KindStarted

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T2"}); err == nil {
		t.Fatal("Launch while full: want error, got nil")
	}
	if got := m.Active(); got != 1 {
		t.Errorf("Active() while full and rejected = %d, want 1 (rejection must not queue)", got)
	}

	eng := mt.byTicket["T1"]
	eng.send(engineerwire.Result{Outcome: "completed", Summary: "done"})
	drainEvent(t, m, time.Second) // KindResult

	waitFor(t, time.Second, func() bool { return m.Active() == 0 })

	st2, err := m.Launch(context.Background(), LaunchReq{Ticket: "T2"})
	if err != nil {
		t.Fatalf("Launch after slot freed: %v", err)
	}
	if st2.Host != "a" {
		t.Errorf("Host = %q, want %q", st2.Host, "a")
	}
}

// --- Start failure ---

func TestManagerStartFailureMarksFailed(t *testing.T) {
	mt := newMockTransport()
	mt.failNthStart(1, fmt.Errorf("boom"))
	cfg := testFleetConfig(testHost("a", 1))
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	_, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"})
	if err == nil {
		t.Fatal("Launch with a failing Start: want error, got nil")
	}

	ev := drainEvent(t, m, time.Second)
	if ev.Kind != KindFailed {
		t.Errorf("event kind = %v, want KindFailed", ev.Kind)
	}

	sts := m.Statuses()
	if len(sts) != 1 || sts[0].State != StateFailed {
		t.Fatalf("Statuses() = %+v, want one entry in state %q", sts, StateFailed)
	}
	if used, _ := m.Capacity(); used != 0 {
		t.Errorf("Capacity used = %d, want 0 (a failed start must free its slot)", used)
	}
}

// --- events, question/answer routing, result ---

func TestManagerEventRoutingAndResultFreesSlot(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 1))
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	st, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1", Title: "fix it"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	id := st.EngineerID

	started := drainEvent(t, m, time.Second)
	if started.Kind != KindStarted || started.EngineerID != id || started.Ticket != "T1" || started.Host != "a" {
		t.Fatalf("started event = %+v", started)
	}

	eng := mt.byTicket["T1"]
	eng.send(engineerwire.Hello{EngineerID: eng.id, ProtocolVersion: engineerwire.ProtocolVersion})
	eng.send(engineerwire.Event{Kind: engineerwire.EventPhase, Text: "working"})

	progress := drainEvent(t, m, time.Second)
	if progress.Kind != KindProgress || progress.EngineerID != id {
		t.Fatalf("progress event = %+v", progress)
	}
	if progress.Progress == nil || progress.Progress.Text != "working" {
		t.Fatalf("progress payload = %+v", progress.Progress)
	}

	eng.send(engineerwire.Question{QuestionID: "q1", Questions: []engineerwire.AskQuestion{{Question: "ok?"}}})
	question := drainEvent(t, m, time.Second)
	if question.Kind != KindQuestion || question.Question == nil || question.Question.QuestionID != "q1" {
		t.Fatalf("question event = %+v", question)
	}

	if err := m.Answer(id, "q1", "yes please"); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	waitFor(t, time.Second, func() bool {
		for _, msg := range eng.receivedSnapshot() {
			if a, ok := msg.(engineerwire.Answer); ok && a.QuestionID == "q1" && a.Text == "yes please" {
				return true
			}
		}
		return false
	})

	eng.send(engineerwire.Result{
		Outcome: "completed", Summary: "shipped it", PRURL: "https://example/pr/1",
		CostUSD: 1.25, Tokens: state.Tokens{Input: 10, Output: 20},
	})
	result := drainEvent(t, m, time.Second)
	if result.Kind != KindResult || result.Result == nil || result.Result.Outcome != "completed" {
		t.Fatalf("result event = %+v", result)
	}

	waitFor(t, time.Second, func() bool { return m.Active() == 0 })
	sts := m.Statuses()
	if len(sts) != 1 {
		t.Fatalf("Statuses() = %+v, want 1 entry", sts)
	}
	got := sts[0]
	if got.State != StateDone || got.Outcome != "completed" || got.PRURL != "https://example/pr/1" || got.CostUSD != 1.25 {
		t.Errorf("final status = %+v", got)
	}
	if used, _ := m.Capacity(); used != 0 {
		t.Errorf("Capacity used = %d, want 0 after result", used)
	}
}

// Two engineers running at once: an Answer sent to one must never reach the
// other.
func TestManagerAnswerRoutesToTheRightEngineer(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 2))
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"}); err != nil {
		t.Fatalf("Launch 1: %v", err)
	}
	st2, err := m.Launch(context.Background(), LaunchReq{Ticket: "T2"})
	if err != nil {
		t.Fatalf("Launch 2: %v", err)
	}
	drainEvent(t, m, time.Second)
	drainEvent(t, m, time.Second)

	if err := m.Answer(st2.EngineerID, "q1", "for engineer two"); err != nil {
		t.Fatalf("Answer: %v", err)
	}

	eng1 := mt.byTicket["T1"]
	eng2 := mt.byTicket["T2"]
	waitFor(t, time.Second, func() bool { return eng2.receivedCount() > 0 })

	if eng1.receivedCount() != 0 {
		t.Errorf("engineer 1 received %d inbound messages, want 0", eng1.receivedCount())
	}
	got := eng2.receivedSnapshot()
	if len(got) != 1 {
		t.Fatalf("engineer 2 received %+v, want exactly 1 message", got)
	}
	a, ok := got[0].(engineerwire.Answer)
	if !ok || a.Text != "for engineer two" {
		t.Errorf("engineer 2 received %+v, want the Answer for it", got[0])
	}
}

// --- cancel ---

func TestManagerCancelMidRun(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 1))
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	st, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	drainEvent(t, m, time.Second) // started

	if err := m.Cancel(st.EngineerID, "operator stop"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	sts := m.Statuses()
	if len(sts) != 1 || sts[0].State != StateCancelled {
		t.Fatalf("Statuses() = %+v, want state %q", sts, StateCancelled)
	}
	if got := m.Active(); got != 0 {
		t.Errorf("Active() = %d, want 0 (cancel must free the slot)", got)
	}
	if used, _ := m.Capacity(); used != 0 {
		t.Errorf("Capacity used = %d, want 0", used)
	}

	ev := drainEvent(t, m, time.Second)
	if ev.Kind != KindFailed || ev.EngineerID != st.EngineerID {
		t.Fatalf("cancel event = %+v", ev)
	}

	eng := mt.byTicket["T1"]
	waitFor(t, time.Second, func() bool {
		for _, msg := range eng.receivedSnapshot() {
			if c, ok := msg.(engineerwire.Cancel); ok && c.Reason == "operator stop" {
				return true
			}
		}
		return false
	})

	// Idempotent: cancelling an already-terminal engineer is a no-op, not
	// an error, and does not double-free the slot or emit again.
	if err := m.Cancel(st.EngineerID, "again"); err != nil {
		t.Errorf("second Cancel: %v, want nil (idempotent)", err)
	}
	select {
	case ev := <-m.Events():
		t.Fatalf("second Cancel emitted an event: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}

	// The freed slot admits a new launch.
	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T2"}); err != nil {
		t.Fatalf("Launch after cancel freed the slot: %v", err)
	}
}

func TestManagerCancelUnknownEngineer(t *testing.T) {
	mt := newMockTransport()
	m := NewManager(testFleetConfig(testHost("a", 1)), mt.forHost)
	t.Cleanup(m.Close)

	if err := m.Cancel("does-not-exist", "reason"); err == nil {
		t.Error("Cancel on an unknown engineer: want error, got nil")
	}
}

func TestManagerAnswerUnknownEngineer(t *testing.T) {
	mt := newMockTransport()
	m := NewManager(testFleetConfig(testHost("a", 1)), mt.forHost)
	t.Cleanup(m.Close)

	if err := m.Answer("does-not-exist", "q1", "text"); err == nil {
		t.Error("Answer on an unknown engineer: want error, got nil")
	}
}

func TestManagerCancelAll(t *testing.T) {
	mt := newMockTransport()
	m := NewManager(testFleetConfig(testHost("a", 2)), mt.forHost)
	t.Cleanup(m.Close)

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"}); err != nil {
		t.Fatalf("Launch 1: %v", err)
	}
	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T2"}); err != nil {
		t.Fatalf("Launch 2: %v", err)
	}
	drainEvent(t, m, time.Second)
	drainEvent(t, m, time.Second)

	m.CancelAll("shutting down")

	waitFor(t, time.Second, func() bool { return m.Active() == 0 })
	for _, st := range m.Statuses() {
		if st.State != StateCancelled {
			t.Errorf("engineer %s state = %q, want %q", st.EngineerID, st.State, StateCancelled)
		}
	}
}

// --- close ---

func TestManagerCloseIdempotent(t *testing.T) {
	mt := newMockTransport()
	m := NewManager(testFleetConfig(testHost("a", 2)), mt.forHost)

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	drainEvent(t, m, time.Second)

	m.Close()
	m.Close() // must not panic or block

	if got := m.Active(); got != 0 {
		t.Errorf("Active() after Close = %d, want 0", got)
	}

	// Close's own CancelAll may have buffered a terminal event for T1 ahead
	// of the close; drain whatever is already queued before expecting the
	// channel itself to report closed.
	deadline := time.After(time.Second)
drain:
	for {
		select {
		case _, ok := <-m.Events():
			if !ok {
				break drain
			}
		case <-deadline:
			t.Fatal("Events() did not close within a second of Close()")
		}
	}

	// Close must refuse further launches rather than starting an orphan.
	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T2"}); err == nil {
		t.Error("Launch after Close: want error, got nil")
	}
}

func TestManagerLaunchEmptyTicketRejected(t *testing.T) {
	mt := newMockTransport()
	m := NewManager(testFleetConfig(testHost("a", 1)), mt.forHost)
	t.Cleanup(m.Close)

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "   "}); err == nil {
		t.Error("Launch with a blank ticket: want error, got nil")
	}
}

// --- PR watcher integration ---

// Launch refuses at the cap, names the cap/count/URLs, and recovers once a
// live Refresh (triggered by the refusal path itself) sees the PR merge —
// all without a second real gh call inside the 10s rate-limit window.
func TestManagerLaunchRefusedAtPRCapThenRecoversAfterMerge(t *testing.T) {
	mt := newMockTransport()
	gh := &fakeGHRunner{}
	gh.queue(`[{"url":"https://example/pr/1","state":"OPEN","headRefName":"acy/a","number":1}]`, nil)
	clock := newFakeClock(time.Unix(1000, 0))
	watcher := NewPRWatcher("/repo", gh.run, time.Minute, clock.Now)
	if err := watcher.poll(context.Background()); err != nil {
		t.Fatalf("bootstrap poll: %v", err)
	}
	if got := watcher.OpenCount(); got != 1 {
		t.Fatalf("OpenCount() after bootstrap poll = %d, want 1", got)
	}

	m := NewManager(testFleetConfig(testHost("a", 1)), mt.forHost, WithPRWatcher(watcher, 1))
	t.Cleanup(m.Close)

	_, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"})
	if err == nil {
		t.Fatal("Launch at the PR cap: want error, got nil")
	}
	for _, want := range []string{"1/1", "https://example/pr/1", "Await"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q missing %q", err.Error(), want)
		}
	}

	// Still inside the 10s refresh window: refuses again with no new gh call.
	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1b"}); err == nil {
		t.Fatal("second Launch inside the refresh window: want error, got nil")
	}
	if got := gh.callCount(); got != 1 {
		t.Errorf("gh calls = %d, want 1 (Refresh rate-limited)", got)
	}

	// The PR merges; move past the rate-limit window so the next Launch's
	// cheap re-check actually re-polls and sees it.
	clock.Advance(11 * time.Second)
	gh.queue(`[{"url":"https://example/pr/1","state":"MERGED","headRefName":"acy/a","number":1}]`, nil)

	st, err := m.Launch(context.Background(), LaunchReq{Ticket: "T2"})
	if err != nil {
		t.Fatalf("Launch after the PR merged: %v", err)
	}
	if st.Host != "a" {
		t.Errorf("Host = %q, want %q", st.Host, "a")
	}
	if got := watcher.OpenCount(); got != 0 {
		t.Errorf("OpenCount() after the merge = %d, want 0", got)
	}

	// The merge diff must also have reached the manager's own Events()
	// stream as a KindPR event, alongside the launch's own KindStarted —
	// order between the two is not guaranteed, since one is relayed by the
	// forwarding goroutine and the other emitted directly by Launch.
	var sawMerged, sawStarted bool
	for range 2 {
		ev := drainEvent(t, m, time.Second)
		switch ev.Kind {
		case KindPR:
			if ev.PR != nil && ev.PR.State == "merged" && ev.PR.Head == "acy/a" {
				sawMerged = true
			}
		case KindStarted:
			sawStarted = true
		}
	}
	if !sawMerged {
		t.Error("want a KindPR merged event forwarded onto Events()")
	}
	if !sawStarted {
		t.Error("want the KindStarted event from the successful launch")
	}
}

// A refusal at the cap names how many further PRs are mid-stack and
// uncapped, so the architect doesn't mistake the cap for less headroom than
// it actually has.
func TestManagerLaunchRefusedAtPRCapNamesStackedCount(t *testing.T) {
	mt := newMockTransport()
	gh := &fakeGHRunner{}
	gh.queue(`[
		{"url":"https://example/pr/1","state":"OPEN","headRefName":"acy/a","baseRefName":"main","number":1},
		{"url":"https://example/pr/2","state":"OPEN","headRefName":"acy/b","baseRefName":"acy/a","number":2}
	]`, nil)
	watcher := NewPRWatcher("/repo", gh.run, time.Minute, nil)
	if err := watcher.poll(context.Background()); err != nil {
		t.Fatalf("bootstrap poll: %v", err)
	}

	m := NewManager(testFleetConfig(testHost("a", 1)), mt.forHost, WithPRWatcher(watcher, 1))
	t.Cleanup(m.Close)

	_, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"})
	if err == nil {
		t.Fatal("Launch at the PR cap: want error, got nil")
	}
	for _, want := range []string{"1/1", "1 more are mid-stack and uncapped", "https://example/pr/1", "Await"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q missing %q", err.Error(), want)
		}
	}
}

// prCap <= 0 means uncapped: Launch never consults the watcher at all.
func TestManagerLaunchUncappedIgnoresWatcher(t *testing.T) {
	mt := newMockTransport()
	gh := &fakeGHRunner{}
	gh.queue(`[{"url":"https://example/pr/1","state":"OPEN","headRefName":"acy/a","number":1}]`, nil)
	watcher := NewPRWatcher("/repo", gh.run, time.Minute, nil)
	if err := watcher.poll(context.Background()); err != nil {
		t.Fatalf("bootstrap poll: %v", err)
	}

	m := NewManager(testFleetConfig(testHost("a", 2)), mt.forHost, WithPRWatcher(watcher, 0))
	t.Cleanup(m.Close)

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"}); err != nil {
		t.Fatalf("Launch with prCap disabled: %v", err)
	}
	if got := gh.callCount(); got != 1 {
		t.Errorf("gh calls = %d, want 1 (only the bootstrap poll, no cap check)", got)
	}
}

// A pr event must never be dropped, even behind a full Events() buffer that
// has already dropped a progress frame — the same guarantee KindResult gets.
func TestManagerPREventsNeverDropped(t *testing.T) {
	mt := newMockTransport()
	watcher := NewPRWatcher("/repo", (&fakeGHRunner{}).run, time.Minute, nil)
	m := NewManager(testFleetConfig(testHost("a", 1)), mt.forHost, WithPRWatcher(watcher, 0), WithEventsBuffer(1))
	t.Cleanup(m.Close)

	m.emit(Event{Kind: KindProgress, EngineerID: "e0"})
	m.emit(Event{Kind: KindProgress, EngineerID: "e0-dropped"}) // buffer full, dropped

	watcher.events <- PREvent{URL: "https://example/pr/9", Head: "acy/x", Number: 9, State: "merged"}

	first := drainEvent(t, m, time.Second)
	if first.Kind != KindProgress || first.EngineerID != "e0" {
		t.Fatalf("first event = %+v, want the occupying progress event", first)
	}

	second := drainEvent(t, m, time.Second)
	if second.Kind != KindPR || second.PR == nil || second.PR.URL != "https://example/pr/9" {
		t.Fatalf("second event = %+v, want the pr event that was blocked behind the full buffer", second)
	}
}

// Close must stop the forwarding goroutine rather than hang waiting on it —
// this is the deadlock WithPRWatcher's forwarder has to avoid: it cannot
// wait on baseCtx, since baseCancel only runs after Close's own wg.Wait.
func TestManagerCloseStopsPRForwarder(t *testing.T) {
	mt := newMockTransport()
	watcher := NewPRWatcher("/repo", (&fakeGHRunner{}).run, time.Minute, nil)
	m := NewManager(testFleetConfig(testHost("a", 1)), mt.forHost, WithPRWatcher(watcher, 0))

	done := make(chan struct{})
	go func() {
		m.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return; the pr forwarding goroutine likely deadlocked")
	}
}

// buildSpec carries the fleet's verify config through to the engineer's
// Spec, dereferencing VerifyTimeoutSeconds the same way DeadmanHours already
// is a few lines above it.
func TestBuildSpecCarriesVerifyConfig(t *testing.T) {
	cfg := testFleetConfig(testHost("a", 1))
	cfg.VerifyCommands = []string{"go build ./...", "go test ./..."}
	cfg.VerifyTimeoutSeconds = new(120)

	spec := buildSpec(cfg, LaunchReq{Ticket: "T1"}, "T1", "eng/t1", cfg.BaseBranch, "", 1.0)

	if got, want := spec.VerifyCommands, cfg.VerifyCommands; len(got) != len(want) {
		t.Fatalf("VerifyCommands = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("VerifyCommands[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	}
	if spec.VerifyTimeoutSeconds != 120 {
		t.Errorf("VerifyTimeoutSeconds = %d, want 120", spec.VerifyTimeoutSeconds)
	}
}

// A FleetConfig a test constructs directly, rather than through
// config.FleetConfig's resolve, may leave VerifyTimeoutSeconds nil —
// buildSpec must not panic dereferencing it, and should fall back to 0.
func TestBuildSpecVerifyTimeoutNilDefaultsToZero(t *testing.T) {
	cfg := testFleetConfig(testHost("a", 1))
	cfg.VerifyTimeoutSeconds = nil

	spec := buildSpec(cfg, LaunchReq{Ticket: "T1"}, "T1", "eng/t1", cfg.BaseBranch, "", 1.0)

	if spec.VerifyTimeoutSeconds != 0 {
		t.Errorf("VerifyTimeoutSeconds = %d, want 0", spec.VerifyTimeoutSeconds)
	}
}

// --- stacked launches ---

// finishEngineer sends a completed Result with prURL on ticket's mock
// engine and waits for it to reach StateDone — the shared setup every
// stacking test needs before a child can stack on that ticket.
func finishEngineer(t *testing.T, m *Manager, mt *mockTransport, ticket, prURL string) {
	t.Helper()
	mt.byTicket[ticket].send(engineerwire.Result{Outcome: "completed", PRURL: prURL})
	waitFor(t, time.Second, func() bool {
		for _, st := range m.Statuses() {
			if st.Ticket == ticket {
				return st.State == StateDone
			}
		}
		return false
	})
}

// A stacked launch's Spec targets the parent's own branch as its base and
// the fleet's real trunk as StackTrunk, and the returned status records
// which chain it joined and what it sits on top of.
func TestManagerLaunchStackedCarriesParentBranchAndTrunk(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 2))
	cfg.StackMode = "chain"
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	parentSt, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"})
	if err != nil {
		t.Fatalf("Launch parent: %v", err)
	}
	drainEvent(t, m, time.Second) // KindStarted
	finishEngineer(t, m, mt, "T1", "https://example/pr/1")
	drainEvent(t, m, time.Second) // KindResult

	childSt, err := m.Launch(context.Background(), LaunchReq{Ticket: "T2", StackOn: "T1"})
	if err != nil {
		t.Fatalf("Launch child: %v", err)
	}

	spec := mt.specFor("T2")
	if spec.BaseBranch != parentSt.Branch {
		t.Errorf("child BaseBranch = %q, want parent branch %q", spec.BaseBranch, parentSt.Branch)
	}
	if spec.StackTrunk != cfg.BaseBranch {
		t.Errorf("child StackTrunk = %q, want %q", spec.StackTrunk, cfg.BaseBranch)
	}
	if childSt.StackBase != parentSt.Branch {
		t.Errorf("child StackBase = %q, want parent branch %q", childSt.StackBase, parentSt.Branch)
	}
	if childSt.StackID == "" {
		t.Error("child StackID is empty, want a chain id")
	}
}

// Four ways a stacked launch is refused, each of which must launch nothing:
// no new ledger entry, and no Start call recorded on the mock transport.
func TestManagerLaunchStackRefusals(t *testing.T) {
	t.Run("stackMode off disables stacking entirely", func(t *testing.T) {
		mt := newMockTransport()
		cfg := testFleetConfig(testHost("a", 1))
		cfg.StackMode = "off"
		m := NewManager(cfg, mt.forHost)
		t.Cleanup(m.Close)

		_, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1", StackOn: "whatever"})
		if err == nil {
			t.Fatal("Launch with stackMode off: want error, got nil")
		}
		for _, want := range []string{"disabled", "stackMode"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err.Error(), want)
			}
		}
		if len(m.Statuses()) != 0 {
			t.Errorf("Statuses() = %+v, want none launched", m.Statuses())
		}
		if _, ok := mt.byTicket["T1"]; ok {
			t.Error("an engineer was started for T1, want none")
		}
	})

	t.Run("StackOn names a ticket that was never launched", func(t *testing.T) {
		mt := newMockTransport()
		cfg := testFleetConfig(testHost("a", 1))
		cfg.StackMode = "chain"
		m := NewManager(cfg, mt.forHost)
		t.Cleanup(m.Close)

		_, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1", StackOn: "ghost"})
		if err == nil {
			t.Fatal("Launch stacked on an unknown ticket: want error, got nil")
		}
		if !strings.Contains(err.Error(), "ghost") {
			t.Errorf("error %q does not name the unknown ticket", err.Error())
		}
		if len(m.Statuses()) != 0 {
			t.Errorf("Statuses() = %+v, want none launched", m.Statuses())
		}
		if _, ok := mt.byTicket["T1"]; ok {
			t.Error("an engineer was started for T1, want none")
		}
	})

	t.Run("StackOn names a parent that has not finished", func(t *testing.T) {
		mt := newMockTransport()
		cfg := testFleetConfig(testHost("a", 1))
		cfg.StackMode = "chain"
		m := NewManager(cfg, mt.forHost)
		t.Cleanup(m.Close)

		if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "P"}); err != nil {
			t.Fatalf("Launch parent: %v", err)
		}
		drainEvent(t, m, time.Second) // KindStarted

		_, err := m.Launch(context.Background(), LaunchReq{Ticket: "C", StackOn: "P"})
		if err == nil {
			t.Fatal("Launch stacked on an unfinished parent: want error, got nil")
		}
		if !strings.Contains(err.Error(), "no open PR yet") && !strings.Contains(err.Error(), "Await") {
			t.Errorf("error %q does not indicate the parent has no PR yet", err.Error())
		}
		if got := len(m.Statuses()); got != 1 {
			t.Errorf("Statuses() has %d entries, want 1 (only the parent)", got)
		}
		if _, ok := mt.byTicket["C"]; ok {
			t.Error("an engineer was started for C, want none")
		}
	})

	t.Run("StackOn names a parent that already has a child", func(t *testing.T) {
		mt := newMockTransport()
		cfg := testFleetConfig(testHost("a", 1))
		cfg.StackMode = "chain"
		m := NewManager(cfg, mt.forHost)
		t.Cleanup(m.Close)

		if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "P"}); err != nil {
			t.Fatalf("Launch parent: %v", err)
		}
		drainEvent(t, m, time.Second) // KindStarted
		finishEngineer(t, m, mt, "P", "https://example/pr/p")
		drainEvent(t, m, time.Second) // KindResult

		if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "C1", StackOn: "P"}); err != nil {
			t.Fatalf("Launch first child: %v", err)
		}
		drainEvent(t, m, time.Second) // KindStarted

		_, err := m.Launch(context.Background(), LaunchReq{Ticket: "C2", StackOn: "P"})
		if err == nil {
			t.Fatal("Launch a second child on the same parent: want error, got nil")
		}
		if !strings.Contains(err.Error(), "C1") {
			t.Errorf("error %q does not name the ticket that already claimed the parent", err.Error())
		}
		if got := len(m.Statuses()); got != 2 {
			t.Errorf("Statuses() has %d entries, want 2 (parent + first child only)", got)
		}
		if _, ok := mt.byTicket["C2"]; ok {
			t.Error("an engineer was started for C2, want none")
		}
	})
}

// Stacking being available (StackMode != "off") must not change the default
// behavior of a launch that leaves StackOn empty.
func TestManagerLaunchUnstackedUnaffectedByStackMode(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 1))
	cfg.StackMode = "chain"
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	st, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	spec := mt.specFor("T1")
	if spec.BaseBranch != cfg.BaseBranch {
		t.Errorf("BaseBranch = %q, want fleet default %q", spec.BaseBranch, cfg.BaseBranch)
	}
	if spec.StackTrunk != "" {
		t.Errorf("StackTrunk = %q, want empty", spec.StackTrunk)
	}
	if st.StackID != "" || st.StackBase != "" {
		t.Errorf("StackID/StackBase = %q/%q, want both empty", st.StackID, st.StackBase)
	}
}

// Chain reports a three-deep stack's branches bottom-to-top, and every
// engineer in it shares the same stack id.
func TestManagerChainThreeDeepBottomToTop(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 1))
	cfg.StackMode = "chain"
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	stA, err := m.Launch(context.Background(), LaunchReq{Ticket: "A"})
	if err != nil {
		t.Fatalf("Launch A: %v", err)
	}
	drainEvent(t, m, time.Second) // KindStarted
	finishEngineer(t, m, mt, "A", "https://example/pr/a")
	drainEvent(t, m, time.Second) // KindResult

	stB, err := m.Launch(context.Background(), LaunchReq{Ticket: "B", StackOn: "A"})
	if err != nil {
		t.Fatalf("Launch B: %v", err)
	}
	drainEvent(t, m, time.Second) // KindStarted
	finishEngineer(t, m, mt, "B", "https://example/pr/b")
	drainEvent(t, m, time.Second) // KindResult

	stC, err := m.Launch(context.Background(), LaunchReq{Ticket: "C", StackOn: "B"})
	if err != nil {
		t.Fatalf("Launch C: %v", err)
	}
	drainEvent(t, m, time.Second) // KindStarted, C is left running

	var stackIDForA string
	for _, st := range m.Statuses() {
		if st.Ticket == "A" {
			stackIDForA = st.StackID
		}
	}
	if stackIDForA == "" || stackIDForA != stB.StackID || stB.StackID != stC.StackID {
		t.Fatalf("stack ids: A=%q B=%q C=%q, want all equal and non-empty", stackIDForA, stB.StackID, stC.StackID)
	}

	got := m.Chain(stackIDForA)
	want := []string{stA.Branch, stB.Branch, stC.Branch}
	if len(got) != len(want) {
		t.Fatalf("Chain() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Chain()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// A Ledger()/Resume() round trip must restore a chain well enough for a
// fresh manager to accept a new child stacked on the still-unfinished tip it
// resumed.
func TestManagerLedgerResumeAcceptsNewChildAfterward(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 2))
	cfg.StackMode = "chain"
	m := NewManager(cfg, mt.forHost)

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "A"}); err != nil {
		t.Fatalf("Launch A: %v", err)
	}
	drainEvent(t, m, time.Second) // KindStarted
	finishEngineer(t, m, mt, "A", "https://example/pr/a")
	drainEvent(t, m, time.Second) // KindResult

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "B", StackOn: "A"}); err != nil {
		t.Fatalf("Launch B: %v", err)
	}
	drainEvent(t, m, time.Second) // KindStarted, B is left running

	snapshot := m.Ledger()
	m.Close()

	var branchA, branchB, wireIDB string
	for _, e := range snapshot {
		switch e.Ticket {
		case "A":
			branchA = e.Branch
		case "B":
			branchB = e.Branch
			wireIDB = e.WireID
		}
	}
	if wireIDB == "" {
		t.Fatalf("snapshot %+v missing B's wire id", snapshot)
	}

	mt2 := newMockTransport()
	engB := mt2.registerEngine(wireIDB)
	m2 := NewManager(cfg, mt2.forHost)
	t.Cleanup(m2.Close)

	if err := m2.Resume(context.Background(), snapshot); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	engB.send(engineerwire.Result{Outcome: "completed", PRURL: "https://example/pr/b"})
	waitFor(t, time.Second, func() bool {
		for _, st := range m2.Statuses() {
			if st.Ticket == "B" {
				return st.State == StateDone
			}
		}
		return false
	})

	stC, err := m2.Launch(context.Background(), LaunchReq{Ticket: "C", StackOn: "B"})
	if err != nil {
		t.Fatalf("Launch C stacked on the resumed chain: %v", err)
	}

	var stackID string
	for _, st := range m2.Statuses() {
		if st.Ticket == "B" {
			stackID = st.StackID
		}
	}
	if stackID == "" {
		t.Fatal("B has no StackID after resume")
	}

	got := m2.Chain(stackID)
	want := []string{branchA, branchB, stC.Branch}
	if len(got) != len(want) {
		t.Fatalf("Chain() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Chain()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
