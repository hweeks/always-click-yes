package ui

import (
	"image/color"
	"strconv"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
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
	seen := map[color.Color]Phase{}
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

// No ekind may let a pathological body (long unbroken prose, a single
// unbroken long token like a URL, or a long line buried mid-body) overflow
// the requested width — that's what sends the viewport scrolling sideways.
func TestRenderEntryNeverExceedsWidth(t *testing.T) {
	const width = 60
	kinds := []struct {
		name  string
		entry entry
	}{
		{"eMeta", entry{kind: eMeta}},
		{"eYou", entry{kind: eYou}},
		{"eClaude", entry{kind: eClaude}},
		{"eThinking", entry{kind: eThinking}},
		{"eTool", entry{kind: eTool, title: "Bash"}},
		{"eToolOK", entry{kind: eToolOK, title: "Bash"}},
		{"eToolErr", entry{kind: eToolErr, title: "Bash"}},
		{"ePlan", entry{kind: ePlan, title: "plan"}},
		{"eTurn", entry{kind: eTurn}},
		{"eComplete", entry{kind: eComplete}},
		{"eGood", entry{kind: eGood}},
		{"eWarn", entry{kind: eWarn}},
		{"eQueued", entry{kind: eQueued}},
	}
	bodies := map[string]string{
		"long prose":       strings.Repeat("word ", 80),
		"unbroken token":   "https://example.com/" + strings.Repeat("a", 280),
		"long middle line": "first line\n" + strings.Repeat("y", 200) + "\nlast line",
	}
	for _, k := range kinds {
		for bname, body := range bodies {
			e := k.entry
			e.body = body
			out := renderEntry(e, width, 10)
			for _, line := range strings.Split(out, "\n") {
				if w := lipgloss.Width(line); w > width {
					t.Errorf("%s/%s: line is %d cols, exceeds width %d:\n%q", k.name, bname, w, width, line)
				}
			}
		}
	}
}

// eGood, eWarn, eMeta and eComplete now render inside a rounded border like
// the other attributed entries. eTurn stays unboxed — it's a horizontal
// rule, and a border around a rule would read as nonsense — but must still
// wrap a long body to width.
func TestRenderEntryNoticesAreBoxed(t *testing.T) {
	const width = 60
	for _, kind := range []ekind{eGood, eWarn, eMeta, eComplete} {
		out := renderEntry(entry{kind: kind, body: "short notice"}, width, 10)
		if !strings.Contains(out, "╭") || !strings.Contains(out, "╰") {
			t.Errorf("kind %d: expected a rounded border, got:\n%s", kind, out)
		}
	}

	out := renderEntry(entry{kind: eTurn, body: strings.Repeat("y", 200)}, width, 10)
	if strings.Contains(out, "╭") {
		t.Errorf("eTurn should not be boxed, got:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > width {
			t.Errorf("eTurn: line is %d cols, exceeds width %d", w, width)
		}
	}
}
