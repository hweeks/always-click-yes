package ui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/gate"
	"github.com/hweeks/always-click-yes/internal/mcp"
	"github.com/hweeks/always-click-yes/internal/session"
	"github.com/hweeks/always-click-yes/internal/state"
)

// --- helpers ---

// actionClock is the fixed wall clock every model here is built against, so a
// gate deadline (and therefore a Frame) is a function of the test rather than of
// what second it ran in.
var actionClock = time.Unix(1_700_000_000, 0).UTC()

// actionModel is a ready model with a driver whose stdin the test can read back.
// PLAN, idle, with a session id — the state Ctrl+G arms from.
func actionModel(t *testing.T) (Model, *strings.Builder) {
	t.Helper()
	sent := &strings.Builder{}
	m := sizedModel(t)
	m.now = actionClock
	m.countdown = 30 * time.Second
	m.sessionID = "sess-abcdef01"
	m.drv = driver.NewWithWriter(driver.Options{}, nopCloser{sent})
	return m, sent
}

// bashPendingID is bashPending with a *named* tool_use id. The plain helper
// mints its own, which is all a test answering the head of the queue needs; the
// by-id tests have to name the id they are about to answer, and to know it is
// not the one on the other gate.
func bashPendingID(cmd, id string) (*gate.Pending, <-chan gate.Decision) {
	in := gate.PreToolUseInput{ToolName: "Bash", ToolUseID: id}
	in.ToolInput, _ = json.Marshal(map[string]string{"command": cmd})
	return gate.NewPending(in)
}

// twoGateModel raises two identifiable gates, oldest first.
func twoGateModel(t *testing.T) (Model, <-chan gate.Decision, <-chan gate.Decision) {
	t.Helper()
	m, _ := actionModel(t)
	m.processing = true

	first, firstCh := bashPendingID("go test ./...", "toolu_first")
	second, secondCh := bashPendingID("rm -rf /tmp/x", "toolu_second")
	m.enqueue(first)
	m.enqueue(second)
	if len(m.pending) != 2 {
		t.Fatalf("setup: want 2 pending gates, got %d", len(m.pending))
	}
	return m, firstCh, secondCh
}

// apply runs an action through the real message switch, so the routing, the
// queue flush and the redraw are all exercised — not just applyAction.
func apply(t *testing.T, m Model, a Action) (Model, ActionResult) {
	t.Helper()
	ack := make(chan ActionResult, 1)
	tm, _ := m.Update(ActionMsg{Action: a, Ack: ack})
	select {
	case res := <-ack:
		return tm.(Model), res
	default:
		t.Fatalf("%s was never acknowledged — a client would wait forever", a.Kind)
		return tm.(Model), ActionResult{}
	}
}

// undecided fails if a gate has been answered. Resolve is a synchronous send
// into a buffered channel, so a decision that was going to arrive is already here.
func undecided(t *testing.T, what string, ch <-chan gate.Decision) {
	t.Helper()
	select {
	case d := <-ch:
		t.Fatalf("%s was answered %+v; it should still be counting down", what, d)
	default:
	}
}

// decided reads the behavior a gate was answered with.
func decided(t *testing.T, what string, ch <-chan gate.Decision) string {
	t.Helper()
	select {
	case d := <-ch:
		return d.Behavior
	default:
		t.Fatalf("%s was never answered — claude is still blocked on the hook", what)
		return ""
	}
}

// askArgs is a two-question ask: the first multi-select, the second not. Two
// questions because advancing between them is the only part of the answer path
// that has state.
const askArgs = `{"questions":[
	{"header":"Storage","question":"Where should the cache live?","multiSelect":true,
	 "options":[{"label":"in memory"},{"label":"on disk"}]},
	{"header":"Eviction","question":"And the policy?",
	 "options":[{"label":"LRU"},{"label":"FIFO"}]}]}`

// askActionModel opens askArgs in the panel the way the ask socket would.
func askActionModel(t *testing.T) (Model, <-chan mcp.Answer) {
	t.Helper()
	m, _ := actionModel(t)
	p, reply := mcp.NewPending(mcp.Request{
		Tool: mcp.ToolAsk, ToolUseID: "tu_action", Args: json.RawMessage(askArgs),
	})
	m.openAsk(p)
	if m.ask == nil {
		t.Fatal("setup: the fixture did not open the ask panel")
	}
	return m, reply
}

// openPicker opens the /resume picker on m the way startResume does: rows built
// from a session list, cursor on the first.
func openPicker(t *testing.T, m *Model) {
	t.Helper()
	m.sessionList = pickRows([]session.Info{
		{ID: "aaaabbbbcccc", ModTime: actionClock, Summary: "port the parser"},
		{ID: "ddddeeeeffff", ModTime: actionClock, Summary: "a plain claude chat"},
	}, nil)
	m.pickIdx = 0
	m.picking = true
	if len(m.sessionList) != 2 {
		t.Fatalf("setup: want 2 picker rows, got %d", len(m.sessionList))
	}
}

// --- the acknowledgement contract ---

// A hung client must not be able to wedge the model loop. The event loop is one
// goroutine: a blocking send here would freeze the terminal, every countdown and
// the driver reader behind a socket nobody is reading.
func TestAckNeverBlocks(t *testing.T) {
	m, _ := actionModel(t)
	blocked := make(chan ActionResult) // unbuffered, and nobody is receiving

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Update(ActionMsg{Action: Submit("hello"), Ack: blocked})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("applyAction blocked on an unread ack channel — the whole run would hang")
	}
}

// The TUI raises actions with no channel at all, which must be as ordinary as
// any other call rather than a nil-channel panic.
func TestNilAckIsFine(t *testing.T) {
	m, sent := actionModel(t)
	m.Update(ActionMsg{Action: Submit("no ack for me")})
	if !strings.Contains(sent.String(), "no ack for me") {
		t.Errorf("the message never reached the driver:\n%s", sent.String())
	}
}

// --- happy paths, one per action ---

func TestActionHappyPaths(t *testing.T) {
	cases := []struct {
		name string
		// setup prepares a model for the action; nil means the plain PLAN model.
		setup func(t *testing.T, m *Model)
		act   Action
		// check asserts on what the action did.
		check func(t *testing.T, m *Model, sent string, res ActionResult)
	}{
		{
			name: "submit sends when idle",
			act:  Submit("port the parser"),
			check: func(t *testing.T, m *Model, sent string, res ActionResult) {
				if !strings.Contains(sent, "port the parser") {
					t.Errorf("nothing reached the driver:\n%s", sent)
				}
				if !m.processing {
					t.Error("a send starts a turn")
				}
				if res.Reason != "sent" {
					t.Errorf("reason = %q, want it to say the message was sent", res.Reason)
				}
			},
		},
		{
			name:  "submit queues when busy",
			setup: func(_ *testing.T, m *Model) { m.processing = true },
			act:   Submit("and add a test"),
			check: func(t *testing.T, m *Model, sent string, res ActionResult) {
				if len(m.queued) != 1 || m.queued[0].text != "and add a test" {
					t.Errorf("queued = %v, want the message held", m.queued)
				}
				if sent != "" {
					t.Errorf("the driver was written to mid-turn:\n%s", sent)
				}
				if !strings.Contains(res.Reason, "queued") {
					t.Errorf("reason = %q, want it to say the message was queued", res.Reason)
				}
			},
		},
		{
			// The point of routing Submit through parseCommand: /tokens, /tasks and
			// the rest have to work identically from either front end.
			name: "submit routes a slash command",
			act:  Submit("/tokens"),
			check: func(t *testing.T, m *Model, sent string, _ ActionResult) {
				if sent != "" {
					t.Errorf("/tokens was forwarded to claude:\n%s", sent)
				}
				if !strings.Contains(lastBody(m), "context") {
					t.Errorf("the token report never reached the transcript: %q", lastBody(m))
				}
			},
		},
		{
			name: "arm flips the phase in place",
			act:  Arm(),
			check: func(t *testing.T, m *Model, sent string, _ ActionResult) {
				if m.phase != PhaseAutoRun {
					t.Fatalf("phase = %v, want AUTO-RUN", m.phase)
				}
				if !strings.Contains(sent, "The plan is approved") {
					t.Errorf("the kickoff prompt never went out:\n%s", sent)
				}
			},
		},
		{
			name:  "interject interrupts the turn",
			setup: func(_ *testing.T, m *Model) { m.processing = true },
			act:   Interject(),
			check: func(t *testing.T, m *Model, _ string, _ ActionResult) {
				if !m.interrupted {
					t.Error("the turn was not interrupted")
				}
				if !strings.Contains(m.transcript(), "interrupting") {
					t.Errorf("nothing announced the interrupt:\n%s", m.transcript())
				}
			},
		},
		{
			name: "set model",
			act:  SetModel("claude-opus-5"),
			check: func(t *testing.T, m *Model, _ string, _ ActionResult) {
				if m.nextModel != "claude-opus-5" {
					t.Errorf("nextModel = %q", m.nextModel)
				}
			},
		},
		{
			name: "clear empties the transcript but not the counter",
			act:  Clear(),
			check: func(t *testing.T, m *Model, _ string, _ ActionResult) {
				// One entry: the "(transcript cleared)" notice /clear leaves behind.
				if len(m.entries) != 1 {
					t.Errorf("entries = %d, want just the cleared notice", len(m.entries))
				}
				if m.entries[0].seq <= 1 {
					t.Errorf("seq = %d, want the counter to carry on past the cleared entries", m.entries[0].seq)
				}
			},
		},
		{
			name:  "done ends the run",
			setup: func(_ *testing.T, m *Model) { m.phase = PhaseAutoRun },
			act:   Done("all three tasks landed"),
			check: func(t *testing.T, m *Model, _ string, _ ActionResult) {
				if m.phase != PhaseComplete {
					t.Fatalf("phase = %v, want COMPLETE", m.phase)
				}
				if !strings.Contains(lastBody(m), "all three tasks landed") {
					t.Errorf("the summary never reached the transcript: %q", lastBody(m))
				}
			},
		},
		{
			// Without this a panel user is stuck in a modal: they can pick a row, but
			// Esc is a key the webview has no way to send.
			name:  "picker close dismisses the picker",
			setup: func(t *testing.T, m *Model) { openPicker(t, m) },
			act:   PickerClose(),
			check: func(t *testing.T, m *Model, _ string, res ActionResult) {
				if m.picking {
					t.Error("the picker is still open")
				}
				// Against the reason rather than a literal: the wording exists once,
				// in action.go, and the entry and the reason are that same string.
				if !strings.Contains(lastBody(m), res.Reason) {
					t.Errorf("the cancel never reached the transcript: %q", lastBody(m))
				}
			},
		},
		{
			name: "queue clear drops held messages",
			setup: func(_ *testing.T, m *Model) {
				m.processing = true
				m.queued = []queuedMsg{{id: 1, text: "one"}, {id: 2, text: "two"}}
			},
			act: QueueClear(),
			check: func(t *testing.T, m *Model, _ string, res ActionResult) {
				if len(m.queued) != 0 {
					t.Errorf("queued = %v, want it emptied", m.queued)
				}
				if !strings.Contains(res.Reason, "2 queued messages") {
					t.Errorf("reason = %q, want it to count what was dropped", res.Reason)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, sent := actionModel(t)
			if tc.setup != nil {
				tc.setup(t, &m)
			}
			m, res := apply(t, m, tc.act)
			if !res.Accepted {
				t.Fatalf("%s was rejected: %s", tc.act.Kind, res.Reason)
			}
			tc.check(t, &m, sent.String(), res)
		})
	}
}

// Quit is separate because what it does is return a command, not change state.
func TestQuitAction(t *testing.T) {
	m, _ := actionModel(t)
	ack := make(chan ActionResult, 1)
	_, cmd := m.Update(ActionMsg{Action: Quit(), Ack: ack})
	if !(<-ack).Accepted {
		t.Fatal("Quit was rejected")
	}
	if cmd == nil {
		t.Fatal("Quit returned no command — nothing would actually quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("Quit's command produced %T, want tea.QuitMsg", cmd())
	}
}

// Resume hands back a command rather than restoring inline: reading a
// transcript off disk is not something the update loop may block on.
func TestResumeAction(t *testing.T) {
	m, _ := actionModel(t)
	m.loadState = func(string) (state.Snapshot, bool, error) {
		return state.Snapshot{Phase: "PLAN"}, true, nil
	}
	m.replay = func(string) ([]driver.Event, error) { return nil, nil }

	ack := make(chan ActionResult, 1)
	_, cmd := m.Update(ActionMsg{Action: Resume("sess-9"), Ack: ack})
	if res := <-ack; !res.Accepted {
		t.Fatalf("Resume was rejected: %s", res.Reason)
	}
	if cmd == nil {
		t.Fatal("Resume returned no command — nothing would load the session")
	}
	msg, ok := cmd().(resumeMsg)
	if !ok {
		t.Fatalf("Resume's command produced %T, want resumeMsg", cmd())
	}
	if msg.id != "sess-9" {
		t.Errorf("resuming %q, want sess-9", msg.id)
	}
}

// A refused pickerClose says nothing in the transcript. The cancel entry is the
// one sentence a user reads as "the picker is gone", and printing it for a
// picker that was never open narrates something that did not happen.
func TestPickerCloseWithNoPickerNarratesNothing(t *testing.T) {
	m, _ := actionModel(t)
	before := m.transcript()

	m, res := apply(t, m, PickerClose())
	if res.Accepted {
		t.Fatalf("pickerClose was accepted with no picker open: %+v", res)
	}
	if m.picking {
		t.Error("a refusal opened the picker")
	}
	if got := m.transcript(); got != before {
		t.Errorf("a refused pickerClose wrote to the transcript:\n%s", got)
	}
}

// Esc in the picker goes *through* the action rather than around it. Inlining
// `m.picking = false` here again would pass every other test in the file and
// leave two copies of the cancel wording to drift apart; this is the test that
// notices.
func TestPickerEscRaisesTheAction(t *testing.T) {
	byKey, _ := actionModel(t)
	openPicker(t, &byKey)
	byAction, _ := actionModel(t)
	openPicker(t, &byAction)

	if cmd := byKey.handlePickKey(tea.KeyPressMsg{Code: tea.KeyEscape}); cmd != nil {
		t.Error("Esc in the picker returned a command — cancelling launches nothing")
	}
	byAction, res := apply(t, byAction, PickerClose())
	if !res.Accepted {
		t.Fatalf("PickerClose was rejected: %s", res.Reason)
	}

	if byKey.picking {
		t.Error("Esc left the picker open")
	}
	if got, want := frameJSON(t, byKey), frameJSON(t, byAction); got != want {
		t.Errorf("Esc and PickerClose diverged.\n--- key ---\n%s\n--- action ---\n%s", got, want)
	}

	// And Esc again, with the picker already closed. Routed through the action it
	// inherits the refusal and says nothing; an inlined `m.picking = false` plus
	// an appendEntry would print a second cancel for a picker that was not open.
	// That is the difference this test exists to notice.
	before := byKey.transcript()
	if cmd := byKey.handlePickKey(tea.KeyPressMsg{Code: tea.KeyEscape}); cmd != nil {
		t.Error("Esc on a closed picker returned a command")
	}
	if got := byKey.transcript(); got != before {
		t.Errorf("Esc narrated a cancel with no picker open:\n%s", got)
	}
}

// --- gates, by id ---

func TestGateAllowAnswersOnlyTheNamedGate(t *testing.T) {
	m, firstCh, secondCh := twoGateModel(t)

	// The *second* gate, which is not the one on screen. Naming it must answer it
	// and leave the head of the queue exactly where it was.
	m, res := apply(t, m, GateAllow("toolu_second"))
	if !res.Accepted {
		t.Fatalf("GateAllow was rejected: %s", res.Reason)
	}
	if b := decided(t, "toolu_second", secondCh); b != gate.Allow {
		t.Errorf("decision = %q, want allow", b)
	}
	undecided(t, "toolu_first", firstCh)

	if len(m.pending) != 1 || m.pending[0].p.Input.ToolUseID != "toolu_first" {
		t.Fatalf("pending = %+v, want only toolu_first left", m.pending)
	}
	if !strings.Contains(m.transcript(), "✔ approved") {
		t.Errorf("nothing said the tool was approved:\n%s", m.transcript())
	}
}

func TestGateDenyAnswersOnlyTheNamedGate(t *testing.T) {
	m, firstCh, secondCh := twoGateModel(t)

	m, res := apply(t, m, GateDeny("toolu_first"))
	if !res.Accepted {
		t.Fatalf("GateDeny was rejected: %s", res.Reason)
	}
	if b := decided(t, "toolu_first", firstCh); b != gate.Deny {
		t.Errorf("decision = %q, want deny", b)
	}
	undecided(t, "toolu_second", secondCh)

	if len(m.pending) != 1 || m.pending[0].p.Input.ToolUseID != "toolu_second" {
		t.Fatalf("pending = %+v, want only toolu_second left", m.pending)
	}
	if !strings.Contains(m.transcript(), "✋ vetoed") {
		t.Errorf("nothing said the tool was vetoed:\n%s", m.transcript())
	}
}

// The refusal this whole design exists for.
//
// Gates auto-approve on their own countdown, so a client can perfectly
// reasonably hold an id that stopped existing a moment ago. Answering "the front
// one instead" would approve a tool nobody looked at — which is the single thing
// a permission gate exists to prevent.
func TestStaleToolUseIDAnswersNothing(t *testing.T) {
	for _, id := range []string{"toolu_gone", "", "toolu_FIRST"} {
		t.Run("id="+id, func(t *testing.T) {
			m, firstCh, secondCh := twoGateModel(t)

			m, res := apply(t, m, GateAllow(id))
			if res.Accepted {
				t.Fatalf("GateAllow(%q) was accepted; that id names no pending gate", id)
			}
			if !strings.Contains(res.Reason, "no gate is pending") {
				t.Errorf("reason = %q, want it to say the id names nothing", res.Reason)
			}
			undecided(t, "toolu_first", firstCh)
			undecided(t, "toolu_second", secondCh)
			if len(m.pending) != 2 {
				t.Errorf("pending = %d, want both gates untouched", len(m.pending))
			}
			if strings.Contains(m.transcript(), "approved") {
				t.Errorf("a stale id wrote an approval into the transcript:\n%s", m.transcript())
			}
		})
	}
}

// The same id twice: the second call has nothing to answer, and must say so
// rather than falling through to whatever is now at the head.
func TestAnsweringAResolvedGateTwiceIsRefused(t *testing.T) {
	m, _, secondCh := twoGateModel(t)

	m, res := apply(t, m, GateAllow("toolu_first"))
	if !res.Accepted {
		t.Fatalf("the first answer was rejected: %s", res.Reason)
	}
	m, res = apply(t, m, GateAllow("toolu_first"))
	if res.Accepted {
		t.Fatal("answering an already-resolved gate was accepted")
	}
	undecided(t, "toolu_second", secondCh)
	if len(m.pending) != 1 {
		t.Errorf("pending = %d, want the second gate still waiting", len(m.pending))
	}
}

// GatePause says what it wants rather than toggling, so two clients that both
// think they are pausing cannot between them resume.
func TestGatePauseIsExplicitAndIdempotent(t *testing.T) {
	m, _, _ := twoGateModel(t)

	m, res := apply(t, m, GatePause(true))
	if !res.Accepted || !m.paused {
		t.Fatalf("GatePause(true): accepted=%v paused=%v (%s)", res.Accepted, m.paused, res.Reason)
	}
	// Frozen remainders, not a stale deadline — the same thing ^R produces.
	if m.pending[0].remaining <= 0 {
		t.Errorf("remaining = %v, want the countdown frozen with time left", m.pending[0].remaining)
	}

	m, res = apply(t, m, GatePause(true))
	if !res.Accepted || !m.paused {
		t.Errorf("pausing twice should be a no-op that still succeeds: %+v", res)
	}
	if !strings.Contains(res.Reason, "already") {
		t.Errorf("reason = %q, want it to say nothing changed", res.Reason)
	}

	m, res = apply(t, m, GatePause(false))
	if !res.Accepted || m.paused {
		t.Fatalf("GatePause(false): accepted=%v paused=%v (%s)", res.Accepted, m.paused, res.Reason)
	}
}

// --- ask ---

func TestAskAnswerWalksTheQuestions(t *testing.T) {
	m, reply := askActionModel(t)

	m, res := apply(t, m, AskAnswer(0, []int{0, 1}))
	if !res.Accepted {
		t.Fatalf("answering the first question was rejected: %s", res.Reason)
	}
	if m.ask == nil || m.ask.qIdx != 1 {
		t.Fatalf("want the panel advanced to question 1, got %+v", m.ask)
	}
	select {
	case a := <-reply:
		t.Fatalf("the ask was answered after one of two questions: %q", a.Text)
	default:
	}

	m, res = apply(t, m, AskAnswer(1, []int{0}))
	if !res.Accepted {
		t.Fatalf("answering the last question was rejected: %s", res.Reason)
	}
	if m.ask != nil {
		t.Error("the panel should be closed once the last question is answered")
	}
	got := answer(t, reply)
	for _, want := range []string{"Storage: in memory, on disk", "Eviction: LRU"} {
		if !strings.Contains(got, want) {
			t.Errorf("answer %q is missing %q", got, want)
		}
	}
	if !m.processing {
		t.Error("an answered question unblocks the turn, so a turn is in flight")
	}
}

func TestAskSkipAnswersNeutrally(t *testing.T) {
	m, reply := askActionModel(t)

	m, res := apply(t, m, AskSkip())
	if !res.Accepted {
		t.Fatalf("AskSkip was rejected: %s", res.Reason)
	}
	if m.ask != nil {
		t.Error("the panel should be closed by a skip")
	}
	if got := answer(t, reply); !strings.Contains(got, "best judgment") {
		t.Errorf("skip answer = %q, want claude told to proceed", got)
	}
}

// --- refusals ---

func TestActionRefusals(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(t *testing.T, m *Model)
		act        Action
		wantReason string // substring
		// wantEntry is a transcript substring the TUI says today for this refusal.
		wantEntry string
	}{
		{
			name:       "arm with no driver",
			setup:      func(_ *testing.T, m *Model) { m.drv = nil },
			act:        Arm(),
			wantReason: "no session is running",
			// arm() has always said this; the action must not invent a second wording.
			wantEntry: "cannot arm yet — no session is running",
		},
		{
			name:       "arm with no session id",
			setup:      func(_ *testing.T, m *Model) { m.sessionID = "" },
			act:        Arm(),
			wantReason: "no session id yet",
		},
		{
			name:       "arm an already-armed run",
			setup:      func(_ *testing.T, m *Model) { m.phase = PhaseAutoRun },
			act:        Arm(),
			wantReason: "not in PLAN",
		},
		{
			name:       "submit on an ended session",
			setup:      func(_ *testing.T, m *Model) { m.ended = true },
			act:        Submit("hello?"),
			wantReason: "the session has ended",
		},
		{
			name:       "submit with no driver",
			setup:      func(_ *testing.T, m *Model) { m.drv = nil },
			act:        Submit("into the void"),
			wantReason: "no session is running",
		},
		{
			name:       "submit nothing",
			act:        Submit("   \n  "),
			wantReason: "nothing to send",
		},
		{
			name:       "interject with nothing in flight",
			act:        Interject(),
			wantReason: "nothing is in flight",
		},
		{
			name:       "interject with no driver",
			setup:      func(_ *testing.T, m *Model) { m.drv = nil; m.processing = true },
			act:        Interject(),
			wantReason: "no session is running",
		},
		{
			// The deadlock the terminal's Esc guard exists for, restated where an
			// HTTP caller can actually reach it: the PreToolUse hook that raised the
			// gate is blocked on the socket waiting for a decision.
			name: "interject while a gate is pending",
			setup: func(t *testing.T, m *Model) {
				m.processing = true
				p, _ := bashPendingID("echo hi", "toolu_x")
				m.enqueue(p)
			},
			act:        Interject(),
			wantReason: "answer the pending gate first",
		},
		{
			name:       "gate allow with no gates at all",
			act:        GateAllow("toolu_nope"),
			wantReason: "no gate is pending",
		},
		{
			name:       "ask answer with no question open",
			act:        AskAnswer(0, []int{0}),
			wantReason: "no question is open",
		},
		{
			name:       "ask skip with no question open",
			act:        AskSkip(),
			wantReason: "no question is open",
		},
		{
			name:       "resume with an empty id",
			act:        Resume("  "),
			wantReason: "no session id to resume",
		},
		{
			name:       "picker close with no picker open",
			act:        PickerClose(),
			wantReason: "the resume picker is not open",
		},
		{
			name:       "set model with no name",
			act:        SetModel(""),
			wantReason: "no model name",
		},
		{
			name:       "done on a finished run",
			setup:      func(_ *testing.T, m *Model) { m.phase = PhaseComplete },
			act:        Done(""),
			wantReason: "already finished",
			wantEntry:  "this run is already finished",
		},
		{
			name:       "an action kind nobody implements",
			act:        Action{Kind: "teleport"},
			wantReason: "unknown action teleport",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, sent := actionModel(t)
			if tc.setup != nil {
				tc.setup(t, &m)
			}
			m, res := apply(t, m, tc.act)

			if res.Accepted {
				t.Fatalf("%s should have been refused, got %+v", tc.act.Kind, res)
			}
			if !strings.Contains(res.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", res.Reason, tc.wantReason)
			}
			if tc.wantEntry != "" && !strings.Contains(m.transcript(), tc.wantEntry) {
				t.Errorf("the transcript should still say %q:\n%s", tc.wantEntry, m.transcript())
			}
			if sent.String() != "" {
				t.Errorf("a refused action wrote to the driver:\n%s", sent.String())
			}
		})
	}
}

// The ask panel is answered by index for the same reason a gate is answered by
// id: the question a client last saw may already be gone.
func TestAskAnswerRefusals(t *testing.T) {
	cases := []struct {
		name       string
		act        Action
		wantReason string
	}{
		{"a question that is not the one being asked", AskAnswer(1, []int{0}), "not the one being asked"},
		{"a question index off the end", AskAnswer(7, []int{0}), "not the one being asked"},
		{"an option index off the end", AskAnswer(0, []int{2}), "out of range"},
		{"a negative option index", AskAnswer(0, []int{-1}), "out of range"},
		{"no option at all", AskAnswer(0, nil), "no option chosen"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, reply := askActionModel(t)
			m, res := apply(t, m, tc.act)

			if res.Accepted {
				t.Fatalf("%v should have been refused", tc.act)
			}
			if !strings.Contains(res.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", res.Reason, tc.wantReason)
			}
			if m.ask == nil || m.ask.qIdx != 0 {
				t.Errorf("a refused answer moved the panel: %+v", m.ask)
			}
			select {
			case a := <-reply:
				t.Fatalf("a refused answer was delivered to claude: %q", a.Text)
			default:
			}
		})
	}
}

// Several options on a single-select question is a client bug, not a silent
// "take the first one" — the answer text would name choices the model never
// offered as a set.
func TestAskAnswerRefusesMultipleOnSingleSelect(t *testing.T) {
	m, _ := askActionModel(t)
	m, _ = apply(t, m, AskAnswer(0, []int{0, 1})) // question 0 is multi-select
	if m.ask == nil || m.ask.qIdx != 1 {
		t.Fatalf("setup: want the panel on question 1, got %+v", m.ask)
	}

	_, res := apply(t, m, AskAnswer(1, []int{0, 1}))
	if res.Accepted {
		t.Fatal("a single-select question accepted two options")
	}
	if !strings.Contains(res.Reason, "single option") {
		t.Errorf("reason = %q", res.Reason)
	}
}

// --- the regression guard for the whole premise ---

// Ctrl+Y, Ctrl+X and Ctrl+R now raise Actions rather than doing the work
// themselves. This is what says they still do the same thing: the chord and the
// equivalent action, from the same starting state, must leave the model
// identical — the transcript, the queue, the remaining gates, the paused flag,
// all of it.
//
// Frame is the comparison rather than reflect.DeepEqual on the Model, and that
// is not a dodge: Model holds channels, funcs and a textarea with internal
// pointers, none of which compare meaningfully, while Frame is by construction
// every piece of state either front end can observe. A difference that Frame
// cannot see is a difference nothing can see.
func TestChordsAndActionsLeaveIdenticalState(t *testing.T) {
	cases := []struct {
		name  string
		key   tea.KeyPressMsg
		act   func(m Model) Action
		wantD string // the behavior the front gate should be answered with, "" = untouched
	}{
		{"ctrl+y", ctrlKey('y'), func(m Model) Action {
			return GateAllow(m.pending[0].p.Input.ToolUseID)
		}, gate.Allow},
		{"ctrl+x", ctrlKey('x'), func(m Model) Action {
			return GateDeny(m.pending[0].p.Input.ToolUseID)
		}, gate.Deny},
		{"ctrl+r", ctrlKey('r'), func(m Model) Action {
			return GatePause(!m.paused)
		}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			byKey, keyCh, _ := twoGateModel(t)
			byAction, actCh, _ := twoGateModel(t)

			// A held message, so the queue flush each path performs is part of what
			// is being compared rather than a difference hiding behind an empty queue.
			byKey.queued = []queuedMsg{{id: 1, text: "then run the linter"}}
			byAction.queued = []queuedMsg{{id: 1, text: "then run the linter"}}

			tm, _ := byKey.Update(tc.key)
			byKey = tm.(Model)

			tm, _ = byAction.Update(ActionMsg{Action: tc.act(byAction)})
			byAction = tm.(Model)

			if got, want := frameJSON(t, byKey), frameJSON(t, byAction); got != want {
				t.Errorf("the chord and the action diverged.\n--- chord ---\n%s\n--- action ---\n%s", got, want)
			}

			// And the gate itself: identical model state would be worth nothing if
			// neither path had actually answered the hook.
			if tc.wantD == "" {
				undecided(t, "the front gate (chord)", keyCh)
				undecided(t, "the front gate (action)", actCh)
				return
			}
			if b := decided(t, "the front gate (chord)", keyCh); b != tc.wantD {
				t.Errorf("chord decision = %q, want %q", b, tc.wantD)
			}
			if b := decided(t, "the front gate (action)", actCh); b != tc.wantD {
				t.Errorf("action decision = %q, want %q", b, tc.wantD)
			}
		})
	}
}

// frameJSON is the model's whole observable state as a comparable string.
func frameJSON(t *testing.T, m Model) string {
	t.Helper()
	b, err := json.MarshalIndent(m.Frame(), "", "  ")
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	return string(b)
}

// Enter and a Submit action are the same code with different callers, so a
// message typed into the composer and one that arrived over the seam have to
// land the same way.
func TestEnterAndSubmitLeaveIdenticalState(t *testing.T) {
	byKey, keySent := actionModel(t)
	byAction, actSent := actionModel(t)

	byKey = typeAndSend(t, byKey, "port the parser")
	tm, _ := byAction.Update(ActionMsg{Action: Submit("port the parser")})
	byAction = tm.(Model)

	if keySent.String() != actSent.String() {
		t.Errorf("the driver saw different bytes.\n--- key ---\n%s\n--- action ---\n%s",
			keySent.String(), actSent.String())
	}
	// turnStart comes off the wall clock, so blank it before comparing: it is the
	// one field neither path can make deterministic.
	byKey.turnStart, byAction.turnStart = time.Time{}, time.Time{}
	if got, want := frameJSON(t, byKey), frameJSON(t, byAction); got != want {
		t.Errorf("Enter and Submit diverged.\n--- key ---\n%s\n--- action ---\n%s", got, want)
	}
}

// sendInput now only reads the composer and asks submitText to do the work, so
// the refusals have to keep leaving the typed text where the user can see it.
func TestRefusedSendKeepsTheComposer(t *testing.T) {
	m, _ := actionModel(t)
	m.ended = true
	m = typeAndSend(t, m, "hello?")

	if m.input.Value() != "hello?" {
		t.Errorf("composer = %q, want the message left in the box for a send that never happened", m.input.Value())
	}
}
