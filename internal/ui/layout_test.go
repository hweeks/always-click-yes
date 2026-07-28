package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// typeInto returns the model after the given text has been typed into the
// composer, routed through Update so the real layout pass runs.
//
// One key press per rune: v2's KeyPressMsg carries a single Code, so there is no
// longer a "here are twelve runes at once" key event to stand in for typing —
// and one-at-a-time is what these tests are about anyway.
func typeInto(m Model, text string) Model {
	for _, r := range text {
		tm, _ := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = tm.(Model)
	}
	return m
}

// The frame must be exactly as tall as the terminal no matter how long the
// message is. This is the flip: the composer used to wrap to a second line while
// the layout still budgeted a fixed four-line footer, so the frame grew to
// height+1, the terminal scrolled, and the box appeared to flicker between one
// and two lines.
func TestFrameHeightIsStableAsComposerGrows(t *testing.T) {
	const height = 30
	m := New(nil, Config{})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: height})
	m = tm.(Model)

	// Walk the composer across the wrap boundary a character at a time.
	for i := range 200 {
		m = typeInto(m, "x")
		if got := lipgloss.Height(m.View().Content); got != height {
			t.Fatalf("after %d chars: frame is %d lines, want %d", i+1, got, height)
		}
	}
}

// Overflowing the composer must grow it, not scroll it sideways: the text stays
// visible and the transcript gives up the rows.
func TestComposerGrowsAndTranscriptShrinks(t *testing.T) {
	m := New(nil, Config{})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	m = tm.(Model)

	oneRow, oneVP := m.input.Height(), m.vp.Height()
	if oneRow != 1 {
		t.Fatalf("empty composer is %d rows, want 1", oneRow)
	}

	m = typeInto(m, strings.Repeat("word ", 30)) // ~150 cols into a 40-col box
	if m.input.Height() <= oneRow {
		t.Errorf("composer did not grow: %d rows", m.input.Height())
	}
	if m.vp.Height() >= oneVP {
		t.Errorf("transcript did not shrink: %d rows, was %d", m.vp.Height(), oneVP)
	}
	if !strings.Contains(m.input.View(), "word") {
		t.Error("composer text is not visible")
	}
}

// Every word typed must still be on screen. The textarea scrolls its own view to
// chase the cursor and only scrolls down, so a composer that is still one row
// tall when the message first wraps will scroll past the opening row and never
// come back — silently hiding the top of what you typed.
func TestComposerShowsTheWholeMessageAsItWraps(t *testing.T) {
	m := New(nil, Config{})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 72, Height: 20})
	m = tm.(Model)

	// One rune at a time, the way a person types — the width-crossing keystroke
	// is exactly the one that used to break it.
	const msg = "the quick brown fox jumps over the lazy dog and keeps on running far past the edge of the box"
	for _, r := range msg {
		m = typeInto(m, string(r))
	}

	view := m.input.View()
	for word := range strings.FieldsSeq(msg) {
		if !strings.Contains(view, word) {
			t.Fatalf("%q is missing from the composer; it rendered:\n%s", word, view)
		}
	}
}

// The working indicator adds a footer line while a turn is in flight; the frame
// must still be exactly terminal-height, in and out of the working state.
func TestFrameHeightIsStableWhileWorking(t *testing.T) {
	const height = 30
	m := New(nil, Config{})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: height})
	m = tm.(Model)

	m.processing = true
	m.layout()
	if got := lipgloss.Height(m.View().Content); got != height {
		t.Fatalf("working frame is %d lines, want %d", got, height)
	}

	m.processing = false
	m.layout()
	if got := lipgloss.Height(m.View().Content); got != height {
		t.Fatalf("idle frame is %d lines, want %d", got, height)
	}
}

// Growth is capped, so a very long message can't squeeze the transcript away.
func TestComposerGrowthIsCapped(t *testing.T) {
	m := New(nil, Config{})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	m = tm.(Model)

	m = typeInto(m, strings.Repeat("word ", 500))
	if m.input.Height() != maxInputRows {
		t.Errorf("composer is %d rows, want the %d-row cap", m.input.Height(), maxInputRows)
	}
	if lipgloss.Height(m.View().Content) != 30 {
		t.Errorf("frame is %d lines, want 30", lipgloss.Height(m.View().Content))
	}
}

// Sending clears the composer, which must hand the rows back to the transcript.
func TestComposerShrinksAfterSend(t *testing.T) {
	m := New(nil, Config{})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	m = tm.(Model)
	tall := m
	tall = typeInto(tall, strings.Repeat("word ", 30))

	// No driver is wired, so Enter can't actually send; reset directly and
	// re-layout the way Update does.
	tall.input.Reset()
	tall.layout()

	if tall.input.Height() != 1 {
		t.Errorf("composer is %d rows after clearing, want 1", tall.input.Height())
	}
	if tall.vp.Height() != m.vp.Height() {
		t.Errorf("transcript is %d rows, want its original %d back", tall.vp.Height(), m.vp.Height())
	}
}
