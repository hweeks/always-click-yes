package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
	"github.com/hweeks/always-click-yes/internal/state"
)

// A resumed non-terminal engineer re-follows from LastSeq+1, occupies its
// host slot, and behaves like any other engineer once it reports.
func TestManagerResumeRunningReattachesFromLastSeq(t *testing.T) {
	orig := backoffSleep
	backoffSleep = noopBackoff
	t.Cleanup(func() { backoffSleep = orig })

	mt := newMockTransport()
	eng := mt.registerEngine("w-resumed")
	cfg := testFleetConfig(testHost("a", 1))
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	entries := []state.Engineer{{
		EngineerID: "e5", WireID: "w-resumed", Ticket: "T1", Host: "a",
		State: StateRunning, LastSeq: 10,
	}}
	if err := m.Resume(context.Background(), entries); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if used, _ := m.Capacity(); used != 1 {
		t.Errorf("Capacity used = %d, want 1 (a resumed running engineer occupies its host slot)", used)
	}
	if got := m.Active(); got != 1 {
		t.Errorf("Active() = %d, want 1", got)
	}

	waitFor(t, time.Second, func() bool { return len(mt.fromSeqCalls("w-resumed")) > 0 })
	if calls := mt.fromSeqCalls("w-resumed"); calls[0] != 11 {
		t.Errorf("first Attach fromSeq = %d, want 11 (LastSeq 10 + 1)", calls[0])
	}

	eng.send(engineerwire.Result{Seq: 11, Outcome: "completed", Summary: "done"})
	ev := drainEvent(t, m, time.Second)
	if ev.Kind != KindResult || ev.EngineerID != "e5" {
		t.Fatalf("event = %+v, want a KindResult for e5", ev)
	}
	waitFor(t, time.Second, func() bool { return m.Active() == 0 })
}

// A Result that landed in the journal while the architect was dead is
// replayed the moment Resume reattaches, with no reconnect in between —
// the engineer resolves itself.
func TestManagerResumeMissedResultResolvesEngineer(t *testing.T) {
	orig := backoffSleep
	backoffSleep = noopBackoff
	t.Cleanup(func() { backoffSleep = orig })

	mt := newMockTransport()
	eng := mt.registerEngine("w-missed")
	eng.send(engineerwire.Result{
		Seq: 4, Outcome: "completed", Summary: "shipped while you were away", PRURL: "https://example/pr/9",
	})

	cfg := testFleetConfig(testHost("a", 1))
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	entries := []state.Engineer{{
		EngineerID: "e7", WireID: "w-missed", Ticket: "T2", Host: "a",
		State: StateRunning, LastSeq: 3,
	}}
	if err := m.Resume(context.Background(), entries); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	ev := drainEvent(t, m, time.Second)
	if ev.Kind != KindResult || ev.Result == nil || ev.Result.Outcome != "completed" {
		t.Fatalf("event = %+v, want the missed Result", ev)
	}
	waitFor(t, time.Second, func() bool { return m.Active() == 0 })

	sts := m.Statuses()
	if len(sts) != 1 || sts[0].State != StateDone || sts[0].PRURL != "https://example/pr/9" {
		t.Fatalf("Statuses() = %+v, want a done engineer with the pr url", sts)
	}
	if used, _ := m.Capacity(); used != 0 {
		t.Errorf("Capacity used = %d, want 0 (a resolved engineer frees its slot)", used)
	}
}

// An engineer whose reattach cannot even replay one message — the daemon is
// gone, or its journal is — is declared failed rather than retried forever
// silently.
func TestManagerResumeDeadAttachEmitsFailedEvent(t *testing.T) {
	orig := backoffSleep
	backoffSleep = noopBackoff
	t.Cleanup(func() { backoffSleep = orig })

	mt := newMockTransport() // "w-gone" is never registered: Attach always errors
	cfg := testFleetConfig(testHost("a", 1))
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	entries := []state.Engineer{{
		EngineerID: "e9", WireID: "w-gone", Ticket: "T3", Host: "a",
		State: StateRunning, LastSeq: 2,
	}}
	if err := m.Resume(context.Background(), entries); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	ev := drainEvent(t, m, time.Second)
	if ev.Kind != KindFailed || ev.EngineerID != "e9" {
		t.Fatalf("event = %+v, want KindFailed for e9", ev)
	}
	waitFor(t, time.Second, func() bool { return m.Active() == 0 })

	sts := m.Statuses()
	if len(sts) != 1 || sts[0].State != StateFailed {
		t.Fatalf("Statuses() = %+v, want one failed entry", sts)
	}
	if used, _ := m.Capacity(); used != 0 {
		t.Errorf("Capacity used = %d, want 0 (a dead reattach must free its slot)", used)
	}
}

// Terminal entries are re-recorded as-is: no Attach, no slot, just history.
func TestManagerResumeTerminalEntriesLandInLedger(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 2))
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	entries := []state.Engineer{
		{EngineerID: "e1", Ticket: "T1", Host: "a", State: StateDone, Outcome: "completed", PRURL: "https://example/pr/1", CostUSD: 2.5},
		{EngineerID: "e2", Ticket: "T2", Host: "a", State: StateFailed, Outcome: "boom"},
		{EngineerID: "e3", Ticket: "T3", Host: "a", State: StateCancelled, Outcome: "operator stop"},
	}
	if err := m.Resume(context.Background(), entries); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	sts := m.Statuses()
	if len(sts) != 3 {
		t.Fatalf("Statuses() = %+v, want 3 terminal entries", sts)
	}
	for i, want := range []string{StateDone, StateFailed, StateCancelled} {
		if sts[i].State != want {
			t.Errorf("entry %d state = %q, want %q", i, sts[i].State, want)
		}
	}
	if sts[0].PRURL != "https://example/pr/1" || sts[0].CostUSD != 2.5 {
		t.Errorf("entry 0 = %+v, lost fields on restore", sts[0])
	}
	if used, _ := m.Capacity(); used != 0 {
		t.Errorf("Capacity used = %d, want 0 (terminal entries hold no slot)", used)
	}
	if got := m.Active(); got != 0 {
		t.Errorf("Active() = %d, want 0", got)
	}

	select {
	case ev := <-m.Events():
		t.Fatalf("unexpected event for a terminal-only resume: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// Resume must run before any engineer is launched: calling it afterward is
// refused rather than risking an id collision or a duplicated ledger entry.
func TestManagerResumeAfterLaunchErrors(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 2))
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	err := m.Resume(context.Background(), []state.Engineer{
		{EngineerID: "e9", Ticket: "T9", Host: "a", State: StateRunning},
	})
	if err == nil {
		t.Fatal("Resume after Launch: want error, got nil")
	}
}
