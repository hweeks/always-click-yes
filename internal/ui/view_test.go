package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/hweeks/always-click-yes/internal/session"
)

// sizedModel returns a ready model at a fixed terminal size, with no driver.
func sizedModel(t *testing.T) Model {
	t.Helper()
	m := New(nil, Config{})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return tm.(Model)
}

func TestViewRendersToolBlocks(t *testing.T) {
	m := sizedModel(t)
	m.entries = append(m.entries,
		entry{kind: eTool, title: "Bash", body: "go build ./...\necho done"},
		entry{kind: eToolOK, body: strings.TrimRight(strings.Repeat("output line\n", 30), "\n")},
	)
	m.rebuild()

	out := m.View().Content
	if !strings.Contains(out, "Bash") {
		t.Error("expected the Bash tool badge in the view")
	}
	if !strings.Contains(out, "more lines") {
		t.Error("expected a truncation footer for the 30-line tool result")
	}
}

func TestViewHelpOverlay(t *testing.T) {
	m := sizedModel(t)
	m.runCommand("help", "")
	if !m.showHelp {
		t.Fatal("expected showHelp after /help")
	}
	if out := m.View().Content; !strings.Contains(out, "commands") {
		t.Errorf("expected help overlay to list commands, got:\n%s", out)
	}
}

func TestViewPickerOverlay(t *testing.T) {
	m := sizedModel(t)
	m.picking = true
	m.sessionList = []session.Info{{ID: "deadbeefcafe", Summary: "do a thing"}}
	if out := m.View().Content; !strings.Contains(out, "deadbeef") || !strings.Contains(out, "do a thing") {
		t.Errorf("expected picker to show the session, got:\n%s", out)
	}
}

func TestViewAskOverlay(t *testing.T) {
	m := sizedModel(t)
	m.ask = &askState{questions: []askQuestion{{
		header:   "Color",
		question: "Which color?",
		options:  []askOption{{label: "red"}, {label: "blue"}},
		selected: map[int]bool{},
	}}}
	out := m.View().Content
	if !strings.Contains(out, "Which color?") || !strings.Contains(out, "red") {
		t.Errorf("expected ask overlay to show the question and options, got:\n%s", out)
	}
}

// The header is a single full-width strip naming the phase, whatever the phase.
func TestHeaderIsFullWidthPhaseBar(t *testing.T) {
	m := sizedModel(t)
	for _, p := range []Phase{PhasePlan, PhaseAutoRun, PhaseComplete} {
		m.phase = p
		h := m.headerView()
		if lipgloss.Height(h) != 1 {
			t.Errorf("%s: header is %d lines, want 1", p, lipgloss.Height(h))
		}
		if got := lipgloss.Width(h); got != m.width {
			t.Errorf("%s: header is %d cols, want the full %d", p, got, m.width)
		}
		if !strings.Contains(h, p.String()) {
			t.Errorf("%s: header does not name the phase:\n%s", p, h)
		}
	}
}

// The large working indicator shows while a turn is in flight — WORKING label,
// sweep bar, elapsed time — and disappears when idle.
func TestWorkingIndicator(t *testing.T) {
	m := sizedModel(t)
	if strings.Contains(m.View().Content, "WORKING") {
		t.Error("idle view should not show the working indicator")
	}

	m.processing = true
	m.turnStart = time.Now().Add(-42 * time.Second)
	m.now = time.Now()
	out := m.View().Content
	if !strings.Contains(out, "WORKING") {
		t.Fatal("processing view should show the WORKING indicator")
	}
	if !strings.Contains(out, "42s") {
		t.Error("expected the elapsed time on the indicator")
	}
	if !strings.Contains(out, "━") {
		t.Error("expected the sweep bar on the indicator")
	}
}

// A pending gate stacks its countdown panel above the composer instead of
// standing in for it — otherwise there is no text box on screen for the keys that
// now fall through (see TestTypingWhileGatedReachesTheComposer).
func TestFooterStacksGateAboveComposer(t *testing.T) {
	m, _ := gatedModel(t)
	panel, composer := m.gateView(), m.inputView()
	foot := m.footerView()

	if !strings.Contains(foot, panel) {
		t.Errorf("footer is missing the countdown panel:\n%s", foot)
	}
	if !strings.Contains(foot, composer) {
		t.Errorf("footer is missing the composer:\n%s", foot)
	}
	// Named separately so a panel or composer that renders to nothing can't
	// satisfy the Contains checks above.
	for _, want := range []string{"auto-approve in", "^Y allow", m.input.Placeholder} {
		if !strings.Contains(stripAnsi(foot), want) {
			t.Errorf("footer does not contain %q:\n%s", want, foot)
		}
	}
	if got, want := lipgloss.Height(foot), lipgloss.Height(panel)+lipgloss.Height(composer); got != want {
		t.Errorf("footer is %d lines, want %d — layout() sizes the viewport from this", got, want)
	}
}

// TestTranscriptKeyMap guards the scroll-bug fix: typing letters must not be
// bound to viewport scrolling.
func TestTranscriptKeyMap(t *testing.T) {
	m := sizedModel(t)
	for _, k := range m.vp.KeyMap.Down.Keys() {
		if k == "j" || k == "d" {
			t.Errorf("viewport Down should not bind %q (would scroll while typing)", k)
		}
	}
	for _, k := range m.vp.KeyMap.PageDown.Keys() {
		if k == " " || k == "f" {
			t.Errorf("viewport PageDown should not bind %q (would scroll while typing)", k)
		}
	}
}
