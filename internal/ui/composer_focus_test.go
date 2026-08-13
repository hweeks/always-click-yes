package ui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/hweeks/always-click-yes/internal/session"
)

// Every overlay that replaces the composer as the thing the keyboard is
// pointed at must blur it, and every one of them must give it back on close —
// otherwise the cursor keeps blinking behind a panel that is not reading
// keystrokes, or goes dark under a composer that is.

func TestComposerBlursForHelpAndRefocusesOnClose(t *testing.T) {
	m := sizedModel(t)
	if !m.input.Focused() {
		t.Fatal("setup: composer should start focused")
	}

	m = typeAndSend(t, m, "/help")
	if !m.showHelp {
		t.Fatal("setup: /help did not open the overlay")
	}
	if m.input.Focused() {
		t.Error("composer should be blurred while /help is open")
	}

	tm, _ := m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m = tm.(Model)
	if m.showHelp {
		t.Fatal("any key should close /help")
	}
	if !m.input.Focused() {
		t.Error("composer should refocus once /help closes")
	}
}

func TestComposerBlursForPickerAndRefocusesOnClose(t *testing.T) {
	m := sizedModel(t)
	m.sessionLister = func() ([]session.Info, error) {
		return []session.Info{{ID: "sess-1", ModTime: time.Now(), Summary: "a prior run"}}, nil
	}

	m = typeAndSend(t, m, "/resume")
	if !m.picking {
		t.Fatal("setup: /resume did not open the picker")
	}
	if m.input.Focused() {
		t.Error("composer should be blurred while the /resume picker is open")
	}

	tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = tm.(Model)
	if m.picking {
		t.Fatal("Esc should close the picker")
	}
	if !m.input.Focused() {
		t.Error("composer should refocus once the picker closes")
	}
}

func TestComposerBlursForAskAndRefocusesOnClose(t *testing.T) {
	m := sizedModel(t)
	p, reply := pendingFor(string(fixtureArgs(t)))

	tm, _ := m.Update(askMsg{p})
	m = tm.(Model)
	if m.ask == nil {
		t.Fatal("setup: askMsg did not open the ask panel")
	}
	if m.input.Focused() {
		t.Error("composer should be blurred while an ask panel is open")
	}

	tm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = tm.(Model)
	if m.ask != nil {
		t.Fatal("Esc should close the ask panel")
	}
	if !m.input.Focused() {
		t.Error("composer should refocus once the ask panel closes")
	}

	answer(t, reply) // the skipped question must still be answered, or claude's turn hangs
}

func TestComposerBlursForQueueEditAndRefocusesOnClose(t *testing.T) {
	m, _ := busyModel(t)
	m = typeAndSend(t, m, "hold this for later")
	if len(m.queued) != 1 {
		t.Fatalf("setup: want 1 queued message, got %d", len(m.queued))
	}

	m = typeAndSend(t, m, "/queue edit")
	if !m.queueOpen {
		t.Fatal("setup: /queue edit did not open the overlay")
	}
	if m.input.Focused() {
		t.Error("composer should be blurred while the /queue edit overlay is open")
	}

	tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = tm.(Model)
	if m.queueOpen {
		t.Fatal("Esc should close the /queue edit overlay")
	}
	if !m.input.Focused() {
		t.Error("composer should refocus once the /queue edit overlay closes")
	}
}

// A pending gate must NOT blur the composer: the gate panel stacks above it
// rather than replacing it, and typing must still work there.
func TestPendingGateDoesNotBlurComposer(t *testing.T) {
	m, _ := gatedModel(t)
	if !m.input.Focused() {
		t.Fatal("setup: composer should be focused before the gate arrived")
	}

	tm, _ := m.Update(tea.KeyPressMsg{Code: 'z', Text: "z"})
	m = tm.(Model)
	if !m.input.Focused() {
		t.Error("a pending gate should not blur the composer")
	}
}

// Blur needs no cmd of its own: it just stops the textarea from accepting the
// next blink tick (see textarea.Model.Update's `if !m.focus` branch), which is
// what ends the recursive blink chain. syncComposerFocus returning a non-nil
// cmd on that transition would mean it invented a timer nobody asked for.
func TestSyncComposerFocusBlurNeedsNoCmd(t *testing.T) {
	m := sizedModel(t)
	if !m.input.Focused() {
		t.Fatal("setup: composer should start focused")
	}

	m.showHelp = true
	if cmd := m.syncComposerFocus(); cmd != nil {
		t.Error("blurring the composer should return a nil cmd")
	}
	if m.input.Focused() {
		t.Error("composer should be blurred once showHelp is set")
	}
}
