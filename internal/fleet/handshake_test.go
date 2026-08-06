package fleet

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
	"github.com/hweeks/always-click-yes/internal/state"
)

// A Hello reporting a protocol major version other than the architect's own
// gets the engineer cancelled, with a failed event naming both sides'
// protocol versions and both binaries' acy versions — docs/engineer-protocol.md's
// "refuses to attach" promise, enforced where the Hello is actually seen.
func TestManagerHelloProtocolMismatchCancelsEngineer(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 1))
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	st, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	drainEvent(t, m, timeout) // started

	eng := mt.byTicket["T1"]
	const engineerMajor = engineerwire.ProtocolVersion + 1
	eng.send(engineerwire.Hello{EngineerID: eng.id, ProtocolVersion: engineerMajor, ACYVersion: "9.9.9-engineer"})

	ev := drainEvent(t, m, timeout)
	if ev.Kind != KindFailed || ev.EngineerID != st.EngineerID {
		t.Fatalf("event = %+v, want KindFailed for %s", ev, st.EngineerID)
	}
	for _, want := range []string{
		strconv.Itoa(engineerMajor),
		strconv.Itoa(engineerwire.ProtocolVersion),
		"9.9.9-engineer",
	} {
		if !strings.Contains(ev.Err.Error(), want) {
			t.Errorf("refusal %q missing %q", ev.Err.Error(), want)
		}
	}

	waitFor(t, timeout, func() bool { return m.Active() == 0 })
	sts := m.Statuses()
	if len(sts) != 1 || sts[0].State != StateCancelled {
		t.Fatalf("Statuses() = %+v, want one cancelled entry", sts)
	}
	if used, _ := m.Capacity(); used != 0 {
		t.Errorf("Capacity used = %d, want 0 (a rejected handshake must free its slot)", used)
	}
}

// A Hello whose protocol version matches is recorded on EngineerStatus —
// what FleetStatus reads to show version skew across a fleet — and the
// engineer keeps running.
func TestManagerHelloProtocolMatchRecordsVersions(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 1))
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	st, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	drainEvent(t, m, timeout) // started

	eng := mt.byTicket["T1"]
	eng.send(engineerwire.Hello{EngineerID: eng.id, ProtocolVersion: engineerwire.ProtocolVersion, ACYVersion: "1.2.3"})

	waitFor(t, timeout, func() bool {
		for _, s := range m.Statuses() {
			if s.EngineerID == st.EngineerID {
				return s.ACYVersion == "1.2.3"
			}
		}
		return false
	})

	sts := m.Statuses()
	if len(sts) != 1 || sts[0].State != StateRunning {
		t.Fatalf("Statuses() = %+v, want a still-running engineer", sts)
	}
	if sts[0].ProtocolVersion != engineerwire.ProtocolVersion || sts[0].ACYVersion != "1.2.3" {
		t.Errorf("status = %+v, want protocol %d and acy version %q recorded", sts[0], engineerwire.ProtocolVersion, "1.2.3")
	}

	// Clean finish, so the test doesn't leave a running engineer behind.
	eng.send(engineerwire.Result{Outcome: "completed"})
	drainEvent(t, m, timeout)
	waitFor(t, timeout, func() bool { return m.Active() == 0 })
}

// An engineer resumed from a mid-journal seq (LastSeq >= 1) never has its
// handshake re-checked, even if its transport hands the reattach a Hello
// reporting a mismatched protocol version — the real journal would never
// replay seq 1 to a follower starting past it, so this Hello can only reach
// handleMsg here because the fake transport does not filter by seq the way
// a real journal does; resumedMidJournal is what makes the manager itself
// ignore it, matching what a real journal would do anyway.
func TestManagerResumeMidJournalSkipsHandshakeCheck(t *testing.T) {
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
	waitFor(t, timeout, func() bool { return len(mt.fromSeqCalls("w-resumed")) > 0 })

	// A mismatched Hello must not cancel the engineer nor emit any event.
	eng.send(engineerwire.Hello{EngineerID: "w-resumed", ProtocolVersion: engineerwire.ProtocolVersion + 1, ACYVersion: "0.0.1"})

	select {
	case ev := <-m.Events():
		t.Fatalf("a mid-journal resumed engineer's Hello produced an event: %+v, want none", ev)
	case <-time.After(150 * time.Millisecond):
	}

	sts := m.Statuses()
	if len(sts) != 1 || sts[0].State != StateRunning {
		t.Fatalf("Statuses() = %+v, want the engineer still running", sts)
	}
	if used, _ := m.Capacity(); used != 1 {
		t.Errorf("Capacity used = %d, want 1 (the mismatched hello must not free the slot)", used)
	}

	// Clean finish.
	eng.send(engineerwire.Result{Seq: 11, Outcome: "completed"})
	drainEvent(t, m, timeout)
	waitFor(t, timeout, func() bool { return m.Active() == 0 })
}
