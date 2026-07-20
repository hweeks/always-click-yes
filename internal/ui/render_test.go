package ui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestClampBlockTruncates(t *testing.T) {
	var lines []string
	for i := range 20 {
		lines = append(lines, "line"+strconv.Itoa(i))
	}
	body := strings.Join(lines, "\n")
	out := clampBlock(body, 80, 5, colGood, lipgloss.NewStyle().Foreground(colDim))

	if !strings.Contains(out, "+15 more lines") {
		t.Errorf("expected a '+15 more lines' footer, got:\n%s", out)
	}
	if !strings.Contains(out, "line4") {
		t.Error("expected kept line 'line4' in output")
	}
	if strings.Contains(out, "line5") {
		t.Error("did not expect truncated 'line5' in output")
	}
}

func TestClampBlockEmpty(t *testing.T) {
	if out := clampBlock("   \n  ", 80, 10, colDim, lipgloss.NewStyle()); out != "" {
		t.Errorf("expected empty output for blank body, got %q", out)
	}
}

func TestClampBlockSingularFooter(t *testing.T) {
	out := clampBlock("a\nb\nc", 80, 2, colDim, lipgloss.NewStyle())
	if !strings.Contains(out, "+1 more line") || strings.Contains(out, "more lines") {
		t.Errorf("expected singular '+1 more line', got:\n%s", out)
	}
}

// Each phase must have its own accent, and AUTO-RUN in particular must not
// borrow Claude's attribution color — that collision is what this UI pass fixed.
func TestPhaseColorDistinct(t *testing.T) {
	seen := map[lipgloss.Color]Phase{}
	for _, p := range []Phase{PhasePlan, PhaseAutoRun, PhaseComplete} {
		c := phaseColor(p)
		if prev, dup := seen[c]; dup {
			t.Errorf("phase %s shares color %v with %s", p, c, prev)
		}
		seen[c] = p
	}
	if phaseColor(PhaseAutoRun) == colClaude {
		t.Error("AUTO-RUN must not use Claude's attribution color")
	}
	if phaseColor(Phase(99)) != colDim {
		t.Error("unknown phases should fall back to dim")
	}
}

// Transcript entries with bodies are boxed: border runes present, and the
// rendered block never exceeds the requested width.
func TestRenderEntryBordered(t *testing.T) {
	const width = 60
	for _, e := range []entry{
		{kind: eYou, body: "hello there"},
		{kind: eClaude, body: "some **prose**"},
		{kind: eTool, title: "Bash", body: "echo hi"},
		{kind: eToolOK, body: "it worked"},
		{kind: eToolErr, body: "it broke"},
	} {
		out := renderEntry(e, width, 10)
		if !strings.Contains(out, "╭") || !strings.Contains(out, "╰") {
			t.Errorf("kind %d: expected a rounded border, got:\n%s", e.kind, out)
		}
		for line := range strings.SplitSeq(out, "\n") {
			if w := lipgloss.Width(line); w > width {
				t.Errorf("kind %d: line is %d cols, exceeds width %d", e.kind, w, width)
			}
		}
	}
}

// Empty tool results keep their compact one-liners instead of an empty box.
func TestRenderEntryEmptyResultsStayCompact(t *testing.T) {
	if out := renderEntry(entry{kind: eToolOK}, 60, 10); strings.Contains(out, "╭") {
		t.Errorf("empty ok result should not be boxed, got:\n%s", out)
	}
	if out := renderEntry(entry{kind: eToolErr}, 60, 10); strings.Contains(out, "╭") {
		t.Errorf("empty error result should not be boxed, got:\n%s", out)
	}
}

// A styled (pre-highlighted) tool body must survive rendering with its ANSI
// intact rather than being re-styled dim.
func TestRenderEntryKeepsHighlighting(t *testing.T) {
	body := highlight("echo hi", "bash")
	out := renderEntry(entry{kind: eTool, title: "Bash", body: body, styled: true}, 60, 10)
	if !strings.Contains(stripAnsi(out), "echo hi") {
		t.Fatalf("tool body text missing, got:\n%s", out)
	}
	if !strings.Contains(out, "\x1b[38;5;") {
		t.Error("expected the chroma 256-color codes to survive rendering")
	}
}
