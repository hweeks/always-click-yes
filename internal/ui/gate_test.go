package ui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/gate"
)

func bashPending(cmd string) (*gate.Pending, <-chan gate.Decision) {
	in := gate.PreToolUseInput{ToolName: "Bash"}
	in.ToolInput, _ = json.Marshal(map[string]string{"command": cmd})
	return gate.NewPending(in)
}

func namedPending(tool string) (*gate.Pending, <-chan gate.Decision) {
	in := gate.PreToolUseInput{ToolName: tool}
	in.ToolInput, _ = json.Marshal(map[string]string{})
	return gate.NewPending(in)
}

// The PreToolUse hook matches "*", so in auto-run the tools acy answers itself
// also raise a gate. They must be passed straight through and never queued: the
// ask panel outranks the gate panel in both key routing and rendering, so a
// queued countdown would tick invisibly behind an open question and then
// auto-approve a second, conflicting execution of a tool the user just answered.
func TestInterceptedToolsAreNeverGated(t *testing.T) {
	for _, tool := range []string{"AskUserQuestion", "ExitPlanMode", "mcp__srv__AskUserQuestion"} {
		m := New(nil, Config{Countdown: 30 * time.Second})
		m.now = time.Unix(1_000_000, 0)

		p, ch := namedPending(tool)
		m.enqueue(p)

		if len(m.pending) != 0 {
			t.Errorf("%s: was queued as a gate (%d pending); it must pass straight through", tool, len(m.pending))
		}
		select {
		case d := <-ch:
			if d.Behavior != gate.Allow {
				t.Errorf("%s: want allow, got %+v", tool, d)
			}
		case <-time.After(time.Second):
			t.Errorf("%s: no decision — claude is still blocked on the hook", tool)
		}
	}
}

// An ordinary tool must still be gated; otherwise the test above would pass even
// if enqueue let everything through.
func TestOrdinaryToolIsStillGated(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.now = time.Unix(1_000_000, 0)

	p, ch := bashPending("echo hi")
	m.enqueue(p)

	if len(m.pending) != 1 {
		t.Fatalf("want Bash queued for the countdown, got %d pending", len(m.pending))
	}
	select {
	case d := <-ch:
		t.Fatalf("Bash was resolved immediately (%+v); it should wait for the countdown", d)
	default:
	}
}

// baseToolName strips the mcp__<server>__ prefix so an MCP-provided tool matches
// the same name as its built-in counterpart. --plan-tools already accepts
// MCP-prefixed names, so the two sides have to agree.
func TestBaseToolName(t *testing.T) {
	cases := map[string]string{
		"AskUserQuestion":                "AskUserQuestion",
		"Bash":                           "Bash",
		"mcp__srv__AskUserQuestion":      "AskUserQuestion",
		"mcp__some_server__ExitPlanMode": "ExitPlanMode",
		"mcp__":                          "mcp__",
		"":                               "",
	}
	for in, want := range cases {
		if got := baseToolName(in); got != want {
			t.Errorf("baseToolName(%q) = %q, want %q", in, got, want)
		}
	}
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

// gatedModel is what an armed auto-run looks like for most of its life: a turn in
// flight and one tool counting down. The driver is real but writes to a buffer,
// so Interrupt() succeeds without a claude process.
func gatedModel(t *testing.T) (Model, <-chan gate.Decision) {
	t.Helper()
	m := sizedModel(t)
	m.now = time.Unix(1_000_000, 0)
	m.drv = driver.NewWithWriter(driver.Options{}, nopCloser{&strings.Builder{}})
	m.processing = true

	p, ch := bashPending("echo hi")
	m.enqueue(p)
	if len(m.pending) != 1 {
		t.Fatalf("setup: want 1 pending gate, got %d", len(m.pending))
	}
	return m, ch
}

func ctrlKey(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl} }

// A gate no longer swallows the keyboard. Every child tool call in an armed run
// raises one, so a blanket interception left the user unable to type for most of
// a run — including the letters the gate itself used to be bound to.
func TestTypingWhileGatedReachesTheComposer(t *testing.T) {
	const typed = "sap, and stop" // every old binding (s, a, p) is in here

	m, ch := gatedModel(t)
	for _, r := range typed {
		tm, _ := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = tm.(Model)
	}

	if got := m.input.Value(); got != typed {
		t.Errorf("composer value = %q, want %q", got, typed)
	}
	if len(m.pending) != 1 {
		t.Errorf("typing resolved the gate: %d pending, want 1", len(m.pending))
	}
	if m.paused {
		t.Error("typing paused the countdown")
	}
	select {
	case d := <-ch:
		t.Fatalf("typing answered the gate: %+v", d)
	default:
	}
}

// The three chords do what the bare letters used to, and the bare letters no
// longer do anything at all — they are text now.
func TestGateChords(t *testing.T) {
	cases := []struct {
		name     string
		key      tea.KeyPressMsg
		wantPend int
		wantDec  string // "" = the gate must still be waiting, undecided
		wantPaus bool
	}{
		{"ctrl+y allows now", ctrlKey('y'), 0, gate.Allow, false},
		{"ctrl+x vetoes", ctrlKey('x'), 0, gate.Deny, false},
		{"ctrl+r pauses", ctrlKey('r'), 1, "", true},
		{"a is not a binding", tea.KeyPressMsg{Code: 'a', Text: "a"}, 1, "", false},
		{"s is not a binding", tea.KeyPressMsg{Code: 's', Text: "s"}, 1, "", false},
		{"p is not a binding", tea.KeyPressMsg{Code: 'p', Text: "p"}, 1, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ch := gatedModel(t)
			tm, _ := m.Update(tc.key)
			m = tm.(Model)

			if len(m.pending) != tc.wantPend {
				t.Errorf("pending = %d, want %d", len(m.pending), tc.wantPend)
			}
			if m.paused != tc.wantPaus {
				t.Errorf("paused = %v, want %v", m.paused, tc.wantPaus)
			}
			// Resolve is synchronous into a buffered channel, so a decision that
			// was going to arrive is already here.
			select {
			case d := <-ch:
				if tc.wantDec == "" {
					t.Fatalf("gate was answered %+v; it should still be counting down", d)
				}
				if d.Behavior != tc.wantDec {
					t.Errorf("decision = %q, want %q", d.Behavior, tc.wantDec)
				}
			default:
				if tc.wantDec != "" {
					t.Fatal("no decision — claude is still blocked on the hook")
				}
			}
		})
	}
}

// Esc is the one key a pending gate still swallows. The PreToolUse hook that
// raised the countdown is blocked on the gate socket waiting for a decision, and
// interrupting the turn out from under it is an unanswered-hook deadlock path.
func TestEscDoesNotInterjectWhileGated(t *testing.T) {
	m, _ := gatedModel(t)

	tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = tm.(Model)
	if m.interrupted {
		t.Error("Esc interjected while a gate was pending")
	}
	if strings.Contains(m.transcript(), "interrupting") {
		t.Errorf("Esc announced an interrupt while gated:\n%s", m.transcript())
	}

	// The control: the same key with the queue empty still interjects, so the
	// guard above is the gate and not a broken Esc.
	m.pending = nil
	tm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = tm.(Model)
	if !m.interrupted {
		t.Fatal("Esc did not interject with no gate pending")
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
