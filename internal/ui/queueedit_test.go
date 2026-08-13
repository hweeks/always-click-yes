package ui

import (
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// Ids survive an edit and a removal untouched: a client that edited message 2
// a moment ago must still be talking about message 2 after message 1 leaves,
// not about whatever slid into its old slot.
func TestQueueIDsStableAcrossEditAndRemoval(t *testing.T) {
	m, _ := busyModel(t)
	m = typeAndSend(t, m, "first")
	m = typeAndSend(t, m, "second")
	m = typeAndSend(t, m, "third")
	if len(m.queued) != 3 {
		t.Fatalf("setup: queued = %v, want 3 messages", m.queued)
	}
	ids := []int{m.queued[0].id, m.queued[1].id, m.queued[2].id}

	if res := m.queueEditAction(ids[1], "second, revised"); !res.Accepted {
		t.Fatalf("edit was rejected: %s", res.Reason)
	}
	if got := []int{m.queued[0].id, m.queued[1].id, m.queued[2].id}; got[0] != ids[0] || got[1] != ids[1] || got[2] != ids[2] {
		t.Errorf("ids changed across an edit: got %v, want %v", got, ids)
	}
	if m.queued[1].text != "second, revised" {
		t.Errorf("text = %q, want the edit applied", m.queued[1].text)
	}

	if res := m.queueRemoveAction(ids[0]); !res.Accepted {
		t.Fatalf("remove was rejected: %s", res.Reason)
	}
	if len(m.queued) != 2 {
		t.Fatalf("queued = %v, want 2 left after removing one", m.queued)
	}
	if m.queued[0].id != ids[1] || m.queued[1].id != ids[2] {
		t.Errorf("ids shifted across a removal: got [%d %d], want [%d %d]",
			m.queued[0].id, m.queued[1].id, ids[1], ids[2])
	}
	if m.queued[0].text != "second, revised" {
		t.Errorf("the edit did not survive the removal: %q", m.queued[0].text)
	}
}

// An id that names nothing is the normal case, not an error: the queue can
// flush out from under a client between it reading a frame and its request
// landing. The only wrong answer is to guess and act on the front of the queue
// instead — so both actions must refuse, and change nothing.
func TestQueueEditAndRemoveUnknownIDRejected(t *testing.T) {
	m, _ := busyModel(t)
	m = typeAndSend(t, m, "only message")
	id := m.queued[0].id

	if res := m.queueEditAction(id+999, "new text"); res.Accepted {
		t.Errorf("editing an unknown id was accepted: %+v", res)
	}
	if len(m.queued) != 1 || m.queued[0].id != id || m.queued[0].text != "only message" {
		t.Errorf("queue changed after a rejected edit: %v", m.queued)
	}

	if res := m.queueRemoveAction(id + 999); res.Accepted {
		t.Errorf("removing an unknown id was accepted: %+v", res)
	}
	if len(m.queued) != 1 || m.queued[0].id != id || m.queued[0].text != "only message" {
		t.Errorf("queue changed after a rejected remove: %v", m.queued)
	}
}

// The whole queue still leaves as ONE turn after an edit, blank-line-joined,
// in order — editing a message must not turn the flush into N sends, and the
// text that goes out has to be the edited one, not the original draft.
func TestFlushAfterEditSendsEditedTextInOneTurn(t *testing.T) {
	m, sent := busyModel(t)
	m = typeAndSend(t, m, "first thought")
	m = typeAndSend(t, m, "second thought")
	m = typeAndSend(t, m, "third thought")

	editID := m.queued[1].id
	if res := m.queueEditAction(editID, "second thought, revised"); !res.Accepted {
		t.Fatalf("edit was rejected: %s", res.Reason)
	}

	tm, _ := m.Update(resultMsg(m))
	m = tm.(Model)

	out := sent.String()
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("stdin has %d lines, want exactly 1 — one turn, not one per message:\n%s", n, out)
	}
	// Backticked, deliberately: the messages are joined by a real blank line
	// before the driver writes them, and stream-json then escapes that newline
	// inside the JSON string — so what should appear on the wire is the two-
	// character sequence \n\n, not an actual line break.
	if !strings.Contains(out, `first thought\n\nsecond thought, revised\n\nthird thought`) {
		t.Errorf("messages should be joined by a blank line, in order, with the edit applied; stdin got:\n%s", out)
	}
	if len(m.queued) != 0 {
		t.Errorf("queued = %v, want it emptied by the flush", m.queued)
	}
}

// /queue edit opens the overlay; Esc takes it down without eating the very
// next keystroke, the same contract the picker and the ask panel keep.
func TestQueueEditOpensOverlayEscClosesWithoutSwallowingNextKey(t *testing.T) {
	m, _ := busyModel(t)
	m = typeAndSend(t, m, "hold this thought")

	m.runCommand("queue", "edit")
	if !m.queueOpen {
		t.Fatalf("setup: /queue edit did not open the overlay")
	}
	if m.queueCursor != 0 {
		t.Errorf("queueCursor = %d, want 0 on open", m.queueCursor)
	}

	tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = tm.(Model)
	if m.queueOpen {
		t.Fatal("Esc did not close the overlay")
	}

	tm, _ = m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m = tm.(Model)
	if m.input.Value() != "x" {
		t.Errorf("composer = %q, want the keystroke right after Esc to reach it", m.input.Value())
	}
}

// /queue edit on an empty queue refuses rather than opening on nothing.
func TestQueueEditRefusesOnEmptyQueue(t *testing.T) {
	m := sizedModel(t)
	m.runCommand("queue", "edit")
	if m.queueOpen {
		t.Error("the overlay opened on an empty queue")
	}
	if !strings.Contains(lastBody(&m), "nothing queued to edit") {
		t.Errorf("no refusal reached the transcript: %q", lastBody(&m))
	}
}

// An Ask arriving while the queue editor is open must not leave the screen
// showing one panel while keys route to another. This is the PR #47 review
// finding: view.go's render switch kept the queue overlay on screen while
// update.go's key routing tested m.ask first, so Enter — apparently picking a
// queued message — actually answered a question the user could not see.
// openAsk now closes the queue editor outright, and both switches read
// m.surface() (present.go), so there is exactly one decision instead of two
// that can disagree.
func TestAskArrivingWhileQueueEditorOpenClosesTheQueueEditor(t *testing.T) {
	m, _ := busyModel(t)
	m = typeAndSend(t, m, "hold this thought")
	m.runCommand("queue", "edit")
	if !m.queueOpen {
		t.Fatalf("setup: /queue edit did not open the overlay")
	}
	queuedBefore := append([]queuedMsg(nil), m.queued...)

	p, reply := pendingFor(`{"questions":[{"question":"proceed?","options":[{"label":"yes"},{"label":"no"}]}]}`)
	m.openAsk(p)

	if m.queueOpen {
		t.Fatal("the queue editor stayed open once an Ask arrived — it must close, not sit underneath")
	}
	if m.ask == nil {
		t.Fatal("openAsk did not open the ask panel")
	}

	// The rendered surface and the one routing keys must be the same surface.
	if got := m.surface(); got != SurfaceAsk {
		t.Fatalf("surface() = %q, want %q", got, SurfaceAsk)
	}
	rendered := stripAnsi(m.footerView())
	if strings.Contains(rendered, queueEditFooterHint) {
		t.Errorf("footer still shows the queue-edit hint while an Ask is open:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Esc skip") {
		t.Errorf("footer does not show the ask hint:\n%s", rendered)
	}

	// Enter must answer the visible Ask, not silently manipulate the queue
	// that used to render underneath it.
	tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m2 := tm.(Model)

	if !reflect.DeepEqual(m2.queued, queuedBefore) {
		t.Errorf("Enter changed the queue: got %v, want unchanged %v", m2.queued, queuedBefore)
	}
	if m2.ask != nil {
		t.Error("Enter on a single-question ask should have submitted it")
	}
	if got := answer(t, reply); !strings.Contains(got, "yes") {
		t.Errorf("answer = %q, want the option Enter actually selected, not a default/no-op answer", got)
	}

	// Closing the Ask must return control somewhere sane — the plain composer,
	// not a queue editor resurrected from whatever the queue looked like
	// before the question interrupted it.
	if m2.queueOpen {
		t.Error("closing the Ask reopened the queue editor")
	}
	if got := m2.surface(); got != SurfaceNone {
		t.Errorf("surface() after the Ask closes = %q, want %q (composer)", got, SurfaceNone)
	}
}

// Frame.Queue is the wire shape a client edits or drops a message by: an id
// alongside the text, never a bare string a client would have to guess a
// position for. Empty marshals as [], never null.
func TestFrameQueueMarshalsWithIDs(t *testing.T) {
	m := New(nil, Config{})
	if got := mustMarshal(t, m.Frame().Queue); got != "[]" {
		t.Errorf("empty queue marshaled as %s, want []", got)
	}

	m.queued = []queuedMsg{{id: 5, text: "alpha"}, {id: 9, text: "beta"}}
	want := `[{"id":5,"text":"alpha"},{"id":9,"text":"beta"}]`
	if got := mustMarshal(t, m.Frame().Queue); got != want {
		t.Errorf("queue marshaled as %s, want %s", got, want)
	}
}

// If the stream closes with an edited message still held, the report the user
// can copy out of must carry the edit — not the draft they typed before
// changing their mind.
func TestReportUnsentQueuePrintsEditedText(t *testing.T) {
	m, _ := busyModel(t)
	m = typeAndSend(t, m, "the original draft")
	id := m.queued[0].id
	if res := m.queueEditAction(id, "the revised draft"); !res.Accepted {
		t.Fatalf("edit was rejected: %s", res.Reason)
	}

	tm, _ := m.Update(streamClosedMsg{gen: m.gen})
	m = tm.(Model)

	if !strings.Contains(lastBody(&m), "the revised draft") {
		t.Errorf("the edited text never reached the unsent-queue report: %q", lastBody(&m))
	}
	if strings.Contains(lastBody(&m), "the original draft") {
		t.Errorf("the stale pre-edit text leaked into the unsent-queue report: %q", lastBody(&m))
	}
	if len(m.queued) != 0 {
		t.Errorf("queued = %v, want it cleared once reported", m.queued)
	}
}
