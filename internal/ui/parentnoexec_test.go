package ui

import (
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/gate"
)

// TestParentNoExecDefaultsFalse: every existing claude path must be
// bit-for-bit unchanged, which starts with the zero value of Config leaving
// the field off.
func TestParentNoExecDefaultsFalse(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	if m.parentNoExec {
		t.Fatal("ParentNoExec must default to false")
	}
}

// TestParentNoExecDeniesParentBashOutright is the codex-only enforcement this
// milestone adds: with no --tools registry to lean on, ParentNoExec is what
// stands in for claude's structural "Bash isn't even in the registry"
// guarantee. The deny must be immediate, never a countdown.
func TestParentNoExecDeniesParentBashOutright(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second, ParentNoExec: true})
	m.now = time.Now()

	p, decisions := pendingFrom("Bash", "parent-sess")
	m.enqueue(p)

	if len(m.pending) != 0 {
		t.Fatalf("parent Bash was queued for a countdown (%d pending) under ParentNoExec; it must be denied outright", len(m.pending))
	}
	select {
	case d := <-decisions:
		if d.Behavior != gate.Deny {
			t.Errorf("behavior = %v, want deny", d.Behavior)
		}
	default:
		t.Fatal("no decision — ParentNoExec did not resolve the parent Bash request")
	}
}

// TestParentNoExecStillAllowsParentRead: ParentNoExec must not disturb the
// existing read-only bypass — a Read from the supervising session still
// passes straight through.
func TestParentNoExecStillAllowsParentRead(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second, ParentNoExec: true})
	m.now = time.Now()

	p, decisions := pendingFrom("Read", "parent-sess")
	m.enqueue(p)

	if len(m.pending) != 0 {
		t.Fatalf("parent Read was queued (%d pending) under ParentNoExec; read-only tools must still pass through", len(m.pending))
	}
	select {
	case d := <-decisions:
		if d.Behavior != gate.Allow {
			t.Errorf("behavior = %v, want allow", d.Behavior)
		}
	default:
		t.Fatal("no decision — a parent Read should pass straight through even with ParentNoExec set")
	}
}

// TestParentNoExecNeverFiresForAChild is the guarantee that actually matters:
// a dispatched child is *meant* to write, and ParentNoExec exists solely to
// close the gap for the supervising session — it must never touch a child's
// own Bash call, which still owes its ordinary countdown.
func TestParentNoExecNeverFiresForAChild(t *testing.T) {
	m := New(nil, Config{
		Countdown:    30 * time.Second,
		ParentNoExec: true,
		Dispatcher:   newFakeDispatcher(map[string]string{"child-sess": "t1"}),
	})
	m.now = time.Now()

	p, decisions := pendingFrom("Bash", "child-sess")
	m.enqueue(p)

	select {
	case d := <-decisions:
		t.Fatalf("a child's Bash was resolved immediately (%+v); ParentNoExec must never fire for a child", d)
	default:
	}
	if len(m.pending) != 1 {
		t.Fatalf("want the child's Bash queued for its ordinary countdown, got %d pending", len(m.pending))
	}
}
