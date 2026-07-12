package ui

import (
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

// renderEntries renders the whole transcript to the given width.
func renderEntries(entries []entry, width int) string {
	if width < 20 {
		width = 20
	}
	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(renderEntry(e, width))
	}
	return b.String()
}

func renderEntry(e entry, width int) string {
	wrap := func(s string, indent int) string {
		return lipgloss.NewStyle().Width(width - indent).Render(s)
	}
	switch e.kind {
	case eMeta:
		return lipgloss.NewStyle().Foreground(colDim).Render(e.body)

	case eYou:
		return badge("you", colYou) + "\n" + wrap(e.body, 0)

	case eClaude:
		return badge("claude", colClaude) + "\n" + wrap(e.body, 0)

	case eThinking:
		return lipgloss.NewStyle().Foreground(colDim).Italic(true).Render("  ∴ " + firstLine(e.body))

	case eTool:
		head := badge("⚙ "+e.title, colTool)
		body := lipgloss.NewStyle().Foreground(colDim).Render(wrap(e.body, 2))
		return head + "\n" + indent(body, 2)

	case eToolOK:
		return lipgloss.NewStyle().Foreground(colGood).Render("  ↳ " + firstLine(e.body))

	case eToolErr:
		return lipgloss.NewStyle().Foreground(colErr).Render("  ✗ " + firstLine(e.body))

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
	boxWidth := width - 2
	if boxWidth < 20 {
		boxWidth = 20
	}
	title := lipgloss.NewStyle().Bold(true).Foreground(colPlan).Render("📋 PROPOSED PLAN")
	body := strings.TrimSpace(e.body)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(colPlan).
		Padding(0, 1).Width(boxWidth).
		Render(title + "\n\n" + body)
	hint := lipgloss.NewStyle().Bold(true).Foreground(colInk).Background(colPlan).Padding(0, 1).
		Render("▶ Press Ctrl+G to arm & auto-run this plan")
	return box + "\n" + hint
}

// indent left-pads every line of s by n spaces.
func indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}
