package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

	out := m.View()
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
	if out := m.View(); !strings.Contains(out, "commands") {
		t.Errorf("expected help overlay to list commands, got:\n%s", out)
	}
}

func TestViewPickerOverlay(t *testing.T) {
	m := sizedModel(t)
	m.picking = true
	m.sessionList = []session.Info{{ID: "deadbeefcafe", Summary: "do a thing"}}
	if out := m.View(); !strings.Contains(out, "deadbeef") || !strings.Contains(out, "do a thing") {
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
	out := m.View()
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
	if strings.Contains(m.View(), "WORKING") {
		t.Error("idle view should not show the working indicator")
	}

	m.processing = true
	m.turnStart = time.Now().Add(-42 * time.Second)
	m.now = time.Now()
	out := m.View()
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
