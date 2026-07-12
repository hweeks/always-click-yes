package ui

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/gate"
)

func bashPending(cmd string) (*gate.Pending, <-chan gate.Decision) {
	in := gate.PreToolUseInput{ToolName: "Bash"}
	in.ToolInput, _ = json.Marshal(map[string]string{"command": cmd})
	return gate.NewPending(in)
}

// TestCountdownAutoApprove: a gate auto-approves once its deadline passes.
func TestCountdownAutoApprove(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	base := time.Unix(1_000_000, 0)
	m.now = base

	p, ch := bashPending("echo hi")
	m.enqueue(p)
	if len(m.pending) != 1 {
		t.Fatalf("want 1 pending, got %d", len(m.pending))
	}

	// 10s in: still pending, no decision yet.
	m.now = base.Add(10 * time.Second)
	m.expireDue()
	if len(m.pending) != 1 {
		t.Fatalf("gate expired too early")
	}
	select {
	case d := <-ch:
		t.Fatalf("unexpected early decision %+v", d)
	default:
	}

	// 31s in: auto-approved.
	m.now = base.Add(31 * time.Second)
	m.expireDue()
	if len(m.pending) != 0 {
		t.Fatalf("gate not expired")
	}
	select {
	case d := <-ch:
		if d.Behavior != gate.Allow {
			t.Fatalf("want allow, got %+v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("no auto-approve decision")
	}
}

// TestVetoFront: pressing stop denies the head gate.
func TestVetoFront(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.now = time.Unix(1_000_000, 0)

	p, ch := bashPending("rm -rf x")
	m.enqueue(p)
	m.resolveFront(gate.Decision{Behavior: gate.Deny, Reason: "vetoed by user"}, entry{kind: eWarn, body: "vetoed"})

	if len(m.pending) != 0 {
		t.Fatalf("front not removed")
	}
	d := <-ch
	if d.Behavior != gate.Deny {
		t.Fatalf("want deny, got %+v", d)
	}
}

// TestPauseFreezesCountdown: while paused, a gate past its original deadline does
// not auto-approve; resuming restores the remaining time.
func TestPauseFreezesCountdown(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	base := time.Unix(1_000_000, 0)
	m.now = base

	p, ch := bashPending("echo hi")
	m.enqueue(p) // deadline = base+30s, 30s remaining

	// Pause at 20s in -> 10s remaining frozen.
	m.now = base.Add(20 * time.Second)
	m.togglePause()

	// Jump far past the original deadline; must NOT expire while paused.
	m.now = base.Add(5 * time.Minute)
	m.expireDue()
	if len(m.pending) != 1 {
		t.Fatalf("gate expired while paused")
	}
	select {
	case <-ch:
		t.Fatal("decision delivered while paused")
	default:
	}

	// Resume: deadline = now + 10s remaining. Not yet due.
	m.togglePause()
	m.expireDue()
	if len(m.pending) != 1 {
		t.Fatalf("gate expired immediately after resume")
	}
	// 11s later -> due.
	m.now = m.now.Add(11 * time.Second)
	m.expireDue()
	if len(m.pending) != 0 {
		t.Fatalf("gate did not expire after resume window")
	}
	if (<-ch).Behavior != gate.Allow {
		t.Fatal("want allow after resume")
	}
}
