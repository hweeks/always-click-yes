package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// ekind identifies how a transcript entry is styled.
type ekind int

const (
	eMeta     ekind = iota // dim informational line
	eYou                   // a message you sent
	eClaude                // Claude's prose
	eThinking              // Claude's thinking (dimmed, condensed)
	eTool                  // a tool call
	eToolOK                // a tool result (success)
	eToolErr               // a tool result (error)
	ePlan                  // a proposed plan (ExitPlanMode) — rendered as a box
	eTurn                  // a turn separator
	eComplete              // the completion banner
	eGood                  // a positive notice (approved, etc.)
	eWarn                  // a warning/negative notice (vetoed, interrupted)
)

// entry is one item in the transcript. It stays structured so it can be
// re-rendered at the current width on resize.
type entry struct {
	kind  ekind
	title string // e.g. a tool name
	body  string
}

// palette
var (
	colDim    = lipgloss.Color("244")
	colYou    = lipgloss.Color("213")
	colClaude = lipgloss.Color("81")
	colTool   = lipgloss.Color("215")
	colGood   = lipgloss.Color("114")
	colErr    = lipgloss.Color("203")
	colPlan   = lipgloss.Color("221")
	colInk    = lipgloss.Color("235")
)

func badge(label string, bg lipgloss.Color) string {
	return lipgloss.NewStyle().Bold(true).Foreground(colInk).Background(bg).Padding(0, 1).Render(label)
}

// spinner glyphs and the colors they cycle through, giving the "working…"
// indicator both motion and a gentle color pulse.
var (
	spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	spinColors = []lipgloss.Color{colClaude, colTool, colPlan}
)

// spinner returns a single animated, colored braille glyph for the given frame.
func spinner(frame int) string {
	if frame < 0 {
		frame = -frame
	}
	glyph := spinFrames[frame%len(spinFrames)]
	col := spinColors[(frame/2)%len(spinColors)]
	return lipgloss.NewStyle().Bold(true).Foreground(col).Render(glyph)
}

// renderEntries renders the whole transcript to the given width. maxLines caps
// how many wrapped lines each expandable block (tool output, results, thinking)
// shows before a "… +N more lines" footer.
func renderEntries(entries []entry, width, maxLines int) string {
	if width < 20 {
		width = 20
	}
	if maxLines < 1 {
		maxLines = 10
	}
	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(renderEntry(e, width, maxLines))
	}
	return b.String()
}

func renderEntry(e entry, width, maxLines int) string {
	wrap := func(s string, indent int) string {
		return lipgloss.NewStyle().Width(width - indent).Render(s)
	}
	switch e.kind {
	case eMeta:
		return lipgloss.NewStyle().Foreground(colDim).Render(e.body)

	case eYou:
		return badge("you", colYou) + "\n" + wrap(e.body, 0)

	case eClaude:
		return badge("claude", colClaude) + "\n" + renderMarkdown(e.body, width)

	case eThinking:
		label := lipgloss.NewStyle().Foreground(colDim).Italic(true).Render("∴ thinking")
		body := clampBlock(e.body, width, maxLines, colDim, lipgloss.NewStyle().Foreground(colDim).Italic(true))
		if body == "" {
			return label
		}
		return label + "\n" + body

	case eTool:
		head := badge("⚙ "+e.title, colTool)
		body := clampBlock(e.body, width, maxLines, colTool, lipgloss.NewStyle().Foreground(colDim))
		if body == "" {
			return head
		}
		return head + "\n" + body

	case eToolOK:
		body := clampBlock(e.body, width, maxLines, colGood, lipgloss.NewStyle().Foreground(colDim))
		if body == "" {
			return lipgloss.NewStyle().Foreground(colGood).Render("  ↳ (ok)")
		}
		return body

	case eToolErr:
		body := clampBlock(e.body, width, maxLines, colErr, lipgloss.NewStyle().Foreground(colErr))
		if body == "" {
			return lipgloss.NewStyle().Foreground(colErr).Render("  ✗ (error)")
		}
		return body

	case ePlan:
		return renderPlan(e, width)

	case eTurn:
		return lipgloss.NewStyle().Foreground(colDim).Faint(true).Render(e.body)

	case eComplete:
		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).BorderForeground(colGood).
			Foreground(colGood).Bold(true).Padding(0, 1)
		return box.Render(e.body)

	case eGood:
		return lipgloss.NewStyle().Foreground(colGood).Render(e.body)

	case eWarn:
		return lipgloss.NewStyle().Foreground(colErr).Render(e.body)
	}
	return e.body
}

// renderPlan draws the proposed plan in a bordered box with a prominent arm hint.
func renderPlan(e entry, width int) string {
	boxWidth := max(width-2, 20)
	title := lipgloss.NewStyle().Bold(true).Foreground(colPlan).Render("📋 PROPOSED PLAN")
	body := renderMarkdown(strings.TrimSpace(e.body), max(boxWidth-4, 10))
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(colPlan).
		Padding(0, 1).Width(boxWidth).
		Render(title + "\n\n" + body)
	hint := lipgloss.NewStyle().Bold(true).Foreground(colInk).Background(colPlan).Padding(0, 1).
		Render("▶ Press Ctrl+G to arm & auto-run this plan")
	return box + "\n" + hint
}

// clampBlock renders body as a width-wrapped block behind a colored left gutter
// bar, capped at maxLines visual lines with a dim "… +N more lines" footer when
// truncated. gutter colors the bar; style colors the text. Returns "" for empty
// bodies so callers can omit the block entirely.
func clampBlock(body string, width, maxLines int, gutter lipgloss.Color, style lipgloss.Style) string {
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return ""
	}
	// The gutter bar (1 col) + PaddingLeft (1 col) consume two columns.
	inner := max(width-2, 10)
	wrapped := lipgloss.NewStyle().Width(inner).Render(body)
	lines := strings.Split(wrapped, "\n")
	hidden := 0
	if len(lines) > maxLines {
		hidden = len(lines) - maxLines
		lines = lines[:maxLines]
	}
	content := style.Render(strings.Join(lines, "\n"))
	if hidden > 0 {
		noun := "lines"
		if hidden == 1 {
			noun = "line"
		}
		more := lipgloss.NewStyle().Foreground(colDim).Faint(true).
			Render(fmt.Sprintf("… +%d more %s", hidden, noun))
		content += "\n" + more
	}
	bar := lipgloss.NewStyle().
		Border(lipgloss.Border{Left: "▎"}, false, false, false, true).
		BorderForeground(gutter).
		PaddingLeft(1)
	return bar.Render(content)
}
