package ui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/orchestrator"
	"github.com/hweeks/always-click-yes/internal/state"
)

// busyModel is the state that used to eat every keystroke: a turn in flight on a
// driver whose stdin a test can read back.
func busyModel(t *testing.T) (Model, *strings.Builder) {
	t.Helper()
	sent := &strings.Builder{}
	m := sizedModel(t)
	m.drv = driver.NewWithWriter(driver.Options{}, nopCloser{sent})
	m.phase = PhaseAutoRun
	m.processing = true
	return m, sent
}

// typeAndSend puts text in the composer and presses Enter, through Update, so the
// key routing is exercised rather than sendInput being called directly.
func typeAndSend(t *testing.T, m Model, text string) Model {
	t.Helper()
	m.input.SetValue(text)
	tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	return tm.(Model)
}

// resultMsg is the idle signal, delivered the way the event loop delivers it.
func resultMsg(m Model) tea.Msg {
	return eventMsg{ev: driver.Event{Type: driver.TypeResult}, gen: m.gen}
}

// Enter during a turn queues rather than dropping. The old sendInput refused on
// m.processing and said nothing, so in an armed run — which is nearly always
// processing — the key was simply dead.
func TestEnterWhileProcessingQueues(t *testing.T) {
	m, sent := busyModel(t)

	m = typeAndSend(t, m, "also update the README")

	if len(m.queued) != 1 || m.queued[0].text != "also update the README" {
		t.Fatalf("queued = %v, want the message held", m.queued)
	}
	if sent.String() != "" {
		t.Errorf("the driver was written to while a turn was in flight:\n%s", sent.String())
	}
	if m.input.Value() != "" {
		t.Errorf("composer = %q, want it cleared once the message is queued", m.input.Value())
	}
	if !strings.Contains(m.transcript(), "also update the README") {
		t.Errorf("nothing said the message was queued:\n%s", m.transcript())
	}
}

// The whole queue leaves as ONE user turn. Each turn re-bills the entire
// accumulated context, so N sends would pay for the conversation N times to
// deliver text the model reads in one pass regardless.
func TestResultEventFlushesTheQueueAsOneTurn(t *testing.T) {
	m, sent := busyModel(t)
	m = typeAndSend(t, m, "first thought")
	m = typeAndSend(t, m, "second thought")

	tm, _ := m.Update(resultMsg(m))
	m = tm.(Model)

	out := sent.String()
	if !strings.Contains(out, "first thought") || !strings.Contains(out, "second thought") {
		t.Fatalf("both queued messages should have been sent; stdin got:\n%s", out)
	}
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("stdin has %d lines, want exactly 1 — one turn, not one per message:\n%s", n, out)
	}
	if !strings.Contains(out, `first thought\n\nsecond thought`) {
		t.Errorf("messages should be joined by a blank line; stdin got:\n%s", out)
	}
	if len(m.queued) != 0 {
		t.Errorf("queued = %v, want it emptied by the flush", m.queued)
	}
	if !m.processing {
		t.Error("a flush starts a turn")
	}
}

// A turn ending while a delegated task still runs is not idleness — the parent is
// blocked on that task's report and will carry on by itself. Sending then would
// interleave the user's message with a report the parent is waiting for.
func TestNoFlushWhileADispatchIsActive(t *testing.T) {
	m, sent := busyModel(t)
	m.dispatcher = &busyDispatcher{fakeDispatcher: newFakeDispatcher(nil)}
	m = typeAndSend(t, m, "one more thing")

	tm, _ := m.Update(resultMsg(m))
	m = tm.(Model)

	if sent.String() != "" {
		t.Errorf("flushed while a task was still running; stdin got:\n%s", sent.String())
	}
	if len(m.queued) != 1 {
		t.Errorf("queued = %v, want the message still held", m.queued)
	}
}

// settlingDispatcher is a dispatcher whose task count a test can move, so a run
// can go from "a task is running" to "the last child has reported".
type settlingDispatcher struct {
	*fakeDispatcher
	active int
}

func (s *settlingDispatcher) Active() int { return s.active }

// The child, not the parent, can be the last thing to finish. Esc with a task
// running cancels the dispatches and interrupts the parent, so the parent's
// aborted turn reports while the child is still shutting down — and that flush is
// refused, correctly, for an active dispatch. Nothing else drives the parent
// afterwards, so the child's own completion has to release the queue or it never
// leaves.
func TestChildCompletionFlushesTheQueueAfterTheParentWentIdle(t *testing.T) {
	m, sent := busyModel(t)
	disp := &settlingDispatcher{fakeDispatcher: newFakeDispatcher(nil), active: 1}
	m.dispatcher = disp
	m = typeAndSend(t, m, "actually, do it the other way")

	// The parent reports first, with the child still cancelling: nothing may go out.
	tm, _ := m.Update(resultMsg(m))
	m = tm.(Model)
	if strings.Contains(sent.String(), "the other way") {
		t.Fatalf("flushed while the task was still shutting down; stdin got:\n%s", sent.String())
	}
	if len(m.queued) != 1 {
		t.Fatalf("queued = %v, want the message still held", m.queued)
	}

	// The orchestrator drops a task from `running` before it emits the terminal
	// event, so by the time the UI sees this the run really is idle.
	disp.active = 0
	tm, _ = m.Update(childMsg{ev: orchestrator.Event{
		TaskID: "t1", Title: "the task", Kind: orchestrator.KindFinished,
	}})
	m = tm.(Model)

	if !strings.Contains(sent.String(), "actually, do it the other way") {
		t.Fatalf("the queue was stranded — no driver event follows the last child; stdin got:\n%s", sent.String())
	}
	if len(m.queued) != 0 {
		t.Errorf("queued = %v, want it emptied by the flush", m.queued)
	}
	if !m.processing {
		t.Error("a flush starts a turn")
	}
}

// A gate counting down is a turn that has not finished either: the hook that
// raised it is blocked, so the queue waits with it.
func TestNoFlushWhileAGateIsPending(t *testing.T) {
	m, sent := busyModel(t)
	m = typeAndSend(t, m, "hold on")
	p, _ := bashPending("echo hi")
	m.enqueue(p)

	tm, _ := m.Update(resultMsg(m))
	m = tm.(Model)

	if sent.String() != "" {
		t.Errorf("flushed with a gate still pending; stdin got:\n%s", sent.String())
	}
	if len(m.queued) != 1 {
		t.Errorf("queued = %v, want the message still held", m.queued)
	}
}

// gatedBusyModel is a busy model holding one queued message behind one gate with
// a countdown a test can step past by hand.
func gatedBusyModel(t *testing.T, text string) (Model, *strings.Builder) {
	t.Helper()
	m, sent := busyModel(t)
	// Both before enqueue: the deadline is m.now.Add(m.countdown), read once.
	m.countdown = 30 * time.Second
	m.now = time.Unix(1_000_000, 0)
	m = typeAndSend(t, m, text)

	p, _ := bashPending("echo hi")
	m.enqueue(p)
	if len(m.pending) != 1 {
		t.Fatalf("setup: want 1 pending gate, got %d", len(m.pending))
	}

	// The turn reports while the gate still counts down: that refusal is correct,
	// and it is what leaves the gate as the only thing holding the queue.
	tm, _ := m.Update(resultMsg(m))
	m = tm.(Model)
	if sent.String() != "" {
		t.Fatalf("setup: flushed with a gate still pending; stdin got:\n%s", sent.String())
	}
	if len(m.queued) != 1 {
		t.Fatalf("setup: queued = %v, want the message still held", m.queued)
	}
	return m, sent
}

// A gate can be the last thing holding the queue. Its countdown expiring is not a
// driver event, so nothing else is coming: the tick that auto-approves it has to
// release the queue itself, or the message is stranded with an empty composer and
// no key that frees it.
func TestGateExpiringOnItsCountdownFlushesTheQueue(t *testing.T) {
	m, sent := gatedBusyModel(t, "and add a test for it")

	tm, _ := m.Update(tickMsg(m.now.Add(m.countdown + time.Second)))
	m = tm.(Model)

	if len(m.pending) != 0 {
		t.Fatalf("setup: the gate should have auto-approved, %d still pending", len(m.pending))
	}
	if !strings.Contains(sent.String(), "and add a test for it") {
		t.Fatalf("the queue was stranded by the expiring gate; stdin got:\n%s", sent.String())
	}
	if len(m.queued) != 0 {
		t.Errorf("queued = %v, want it emptied by the flush", m.queued)
	}
	if !strings.Contains(m.transcript(), "and add a test for it") {
		t.Errorf("the flushed message never reached the transcript:\n%s", m.transcript())
	}
	// And on screen. The tick only rebuilds the viewport for a live gate, which is
	// precisely what just went away, so a flush that sends has to force the redraw
	// itself — otherwise the message shows as queued and nothing else until
	// something unrelated happens to redraw. Twice is the tell: the ⏳ entry from
	// when it was held, and the "you" entry from when it went out.
	view := stripAnsi(m.View().Content)
	if n := strings.Count(view, "and add a test for it"); n < 2 {
		t.Errorf("the sent message was not redrawn (shown %d×, want the queued and the sent copy):\n%s", n, view)
	}
}

// Same stranding, answered by hand: ^Y resolves the front gate outside any event
// path at all, so it has to retry the flush too.
func TestApprovingTheLastGateFlushesTheQueue(t *testing.T) {
	m, sent := gatedBusyModel(t, "then run the linter")

	tm, _ := m.Update(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	m = tm.(Model)

	if len(m.pending) != 0 {
		t.Fatalf("setup: ctrl+y should have resolved the gate, %d still pending", len(m.pending))
	}
	if !strings.Contains(sent.String(), "then run the linter") {
		t.Fatalf("the queue was stranded by the approved gate; stdin got:\n%s", sent.String())
	}
	if len(m.queued) != 0 {
		t.Errorf("queued = %v, want it emptied by the flush", m.queued)
	}
	if !m.processing {
		t.Error("a flush starts a turn")
	}
}

// Queue, then Esc: the interject path needs no code of its own. Esc aborts the
// turn, the aborted turn's result lands, and the queued text goes out as the
// redirect.
func TestQueueThenInterjectSendsOnTheAbortedTurn(t *testing.T) {
	m, sent := busyModel(t)
	m = typeAndSend(t, m, "stop, do it the other way")

	tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = tm.(Model)
	if !m.interrupted {
		t.Fatal("setup: Esc did not interject")
	}
	// The interrupt itself goes down the same stdin, so look for the message.
	if strings.Contains(sent.String(), "do it the other way") {
		t.Errorf("the redirect must wait for the aborted turn to report; stdin got:\n%s", sent.String())
	}

	tm, _ = m.Update(resultMsg(m))
	m = tm.(Model)

	if !strings.Contains(sent.String(), "do it the other way") {
		t.Fatalf("the queued redirect never went out; stdin got:\n%s", sent.String())
	}
	if len(m.queued) != 0 {
		t.Errorf("queued = %v, want it emptied", m.queued)
	}
}

// An idle session sends immediately: queueing is for a busy one, not a new
// mailbox in front of every message.
func TestIdleSendIsStillImmediate(t *testing.T) {
	m, sent := busyModel(t)
	m.processing = false

	m = typeAndSend(t, m, "go on then")

	if len(m.queued) != 0 {
		t.Errorf("queued = %v, want an idle send to go straight out", m.queued)
	}
	if !strings.Contains(sent.String(), "go on then") {
		t.Errorf("nothing reached the driver; stdin got:\n%s", sent.String())
	}
}

// With no session to send to there is nothing to queue for either.
func TestNothingIsQueuedWithoutADriver(t *testing.T) {
	m := sizedModel(t)
	m.processing = true
	m = typeAndSend(t, m, "into the void")
	if len(m.queued) != 0 {
		t.Errorf("queued = %v, want nothing queued with no driver", m.queued)
	}
}

func TestQueueClearEmptiesIt(t *testing.T) {
	m, _ := busyModel(t)
	m = typeAndSend(t, m, "one")
	m = typeAndSend(t, m, "two")

	m.runCommand("queue", "clear")

	if len(m.queued) != 0 {
		t.Fatalf("queued = %v, want it emptied by /queue clear", m.queued)
	}
	if !strings.Contains(lastBody(&m), "cleared") {
		t.Errorf("clearing should be confirmed, got %q", lastBody(&m))
	}
}

// /queue reads the messages back in full — it is the "what did I type" command.
func TestQueueListsWhatIsWaiting(t *testing.T) {
	m, _ := busyModel(t)
	if m.runCommand("queue", ""); !strings.Contains(lastBody(&m), "nothing queued") {
		t.Errorf("an empty queue should say so, got %q", lastBody(&m))
	}

	m = typeAndSend(t, m, "check the migration too")
	m.runCommand("queue", "")
	if !strings.Contains(lastBody(&m), "check the migration too") {
		t.Errorf("/queue should list the message, got %q", lastBody(&m))
	}
}

// Nothing is lost silently. If the stream closes with messages queued they are
// printed back so the user can copy them out.
func TestClosingStreamReportsUnsentMessages(t *testing.T) {
	m, _ := busyModel(t)
	m = typeAndSend(t, m, "the thing I typed as it died")

	tm, _ := m.Update(streamClosedMsg{gen: m.gen})
	m = tm.(Model)

	if !m.ended {
		t.Fatal("setup: the stream closing should end the session")
	}
	if !strings.Contains(m.transcript(), "the thing I typed as it died") {
		t.Errorf("the unsent message was swallowed:\n%s", m.transcript())
	}
	if len(m.queued) != 0 {
		t.Errorf("queued = %v, want it cleared once reported", m.queued)
	}
}

// An ended session refuses rather than queueing for a turn that will never come.
func TestSendAfterTheSessionEndedDoesNotQueue(t *testing.T) {
	m, _ := busyModel(t)
	m.ended = true
	m = typeAndSend(t, m, "hello?")
	if len(m.queued) != 0 {
		t.Errorf("queued = %v, want nothing queued after the session ended", m.queued)
	}
}

// The count is visible in both places the run's state is shown, and the panel
// lists the first few without letting a long queue take over the screen.
func TestFooterAndHeaderShowTheQueue(t *testing.T) {
	m, _ := busyModel(t)
	for _, s := range []string{"alpha", "beta", "gamma", "delta", "epsilon"} {
		m = typeAndSend(t, m, s)
	}

	head := stripAnsi(m.headerView())
	if !strings.Contains(head, "5 queued") {
		t.Errorf("header does not show the queue count:\n%s", head)
	}

	foot := stripAnsi(m.footerView())
	if !strings.Contains(foot, "5 queued · sends when this turn ends") {
		t.Errorf("footer is missing the queue panel:\n%s", foot)
	}
	for _, want := range []string{"alpha", "beta", "gamma", "(+2 more)"} {
		if !strings.Contains(foot, want) {
			t.Errorf("footer does not contain %q:\n%s", want, foot)
		}
	}
	if strings.Contains(foot, "delta") {
		t.Errorf("footer should count the tail, not list it:\n%s", foot)
	}
	// footerView is what layout() measures, so the panel has to be part of it
	// rather than drawn by a second path.
	if !strings.Contains(foot, stripAnsi(m.inputView())) {
		t.Errorf("footer lost the composer under the queue panel:\n%s", foot)
	}
}

// The hint must not advertise Esc while a gate is up: gate keys fall through to
// the composer now, and Esc is deliberately suppressed there (the blocked hook
// would deadlock).
func TestComposerHintIsGateAware(t *testing.T) {
	m, _ := gatedModel(t)
	if got := stripAnsi(m.inputView()); strings.Contains(got, "Esc to interject") {
		t.Errorf("the hint offers Esc while a gate is pending:\n%s", got)
	}

	m.pending = nil
	if got := stripAnsi(m.inputView()); !strings.Contains(got, "Esc to interject") {
		t.Errorf("with no gate pending the hint should offer Esc:\n%s", got)
	}
}

// The queue is transient by design: a message surviving a crash into a different
// phase is worse than one that was lost, so it must never reach the snapshot.
func TestQueueIsNotPersisted(t *testing.T) {
	var saved []string
	m := New(nil, Config{
		Countdown: 30 * time.Second,
		SaveState: func(s state.Snapshot) error {
			b, err := json.Marshal(s)
			if err != nil {
				return err
			}
			saved = append(saved, string(b))
			return nil
		},
	})
	m.sessionID = "sess-1"
	m.queued = []queuedMsg{{id: 1, text: "a secret plan"}}
	m.persist()

	if len(saved) != 1 {
		t.Fatalf("setup: %d snapshots written, want 1", len(saved))
	}
	for _, s := range saved {
		if strings.Contains(s, "a secret plan") {
			t.Errorf("the queue reached the snapshot:\n%s", s)
		}
	}
}
