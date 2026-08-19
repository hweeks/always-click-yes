package ui

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
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
	eQueued                // a message held back until the session goes idle
	eFlow                  // the ticket flow, redrawn as ascii + mermaid
)

// entry is one item in the transcript. It stays structured so it can be
// re-rendered at the current width on resize.
//
// raw/lang exist for the second front end and are invisible to this file. body
// is highlighted at ingest — deliberately, because rebuild() used to re-render
// every entry on each 120ms tick and re-lexing at that rate would burn CPU for
// no visual gain. It now memoizes across ticks by entry.seq (see
// rendercache.go) on top of that, but a resize still re-renders every entry, so
// the ingest-time highlighting still pays for itself. ANSI is a terminal's
// answer, not a webview's, though — so the unhighlighted source and its
// language travel alongside, and a non-terminal renderer highlights it its own
// way. See toolBodyParts.
type entry struct {
	seq    int // monotonic, assigned by appendEntry; never reused, never reset
	kind   ekind
	title  string // e.g. a tool name
	body   string
	raw    string // body without the syntax highlighting (plain text)
	lang   string // language hint for raw ("bash", "diff", "go", …; "" = none)
	styled bool   // body already carries ANSI (syntax highlighting); render it verbatim
	task   string // the delegated task this came from ("" = the parent itself)

	// agentTag is the lowercase badge label eClaude wears (Model.agentBadge,
	// stamped in by stamp()). Entries built directly by literal in tests never
	// go through stamp and leave this "" — renderEntry treats that the same as
	// "claude" so every pre-existing test keeps passing unchanged.
	agentTag string

	// html is this entry rendered for a browser, and is empty unless
	// Config.RenderHTML asked for it — which `acy run` never does, because the
	// terminal cannot display it and generating it would be work the run pays for
	// and nobody reads. Like body's highlighting it is produced once, at ingest;
	// see stamp.
	html string
}

// palette. lipgloss v2's Color is a constructor returning image/color.Color
// rather than a named type, so the palette and everything that takes a swatch
// is typed on the standard interface.
var (
	colDim    = lipgloss.Color("244")
	colYou    = lipgloss.Color("213")
	colClaude = lipgloss.Color("81")
	colTool   = lipgloss.Color("215")
	colGood   = lipgloss.Color("114")
	colErr    = lipgloss.Color("203")
	colPlan   = lipgloss.Color("221")
	colRun    = lipgloss.Color("208")
	colInk    = lipgloss.Color("235")
	colFlow   = lipgloss.Color("141")
)

// phaseColor is the accent the whole chrome takes in a given phase: the header
// bar, the composer border and the working indicator all follow it, so the mode
// is legible at a glance. AUTO-RUN gets its own hot orange rather than borrowing
// colClaude — the phase badge must not read as Claude's attribution color.
func phaseColor(p Phase) color.Color {
	switch p {
	case PhasePlan:
		return colPlan
	case PhaseAutoRun:
		return colRun
	case PhaseComplete:
		return colGood
	}
	return colDim
}

func badge(label string, bg color.Color) string {
	return lipgloss.NewStyle().Bold(true).Foreground(colInk).Background(bg).Padding(0, 1).Render(label)
}

// spinFrames are the glyphs the working indicator cycles through.
var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinGlyph returns the unstyled braille glyph for the given animation frame, so
// callers can style it to match the surface it sits on.
func spinGlyph(frame int) string {
	if frame < 0 {
		frame = -frame
	}
	return spinFrames[frame%len(spinFrames)]
}

// renderEntries renders the whole transcript to the given width. maxLines caps
// how many wrapped lines each expandable block (tool output, results, thinking)
// shows before a "… +N more lines" footer.
//
// cache memoizes each entry's render by entry.seq, across calls: rebuild() now
// also memoizes across ticks, on top of the per-entry work this cache saves, so
// an entry already rendered at this width/maxLines is looked up rather than
// re-rendered.
func renderEntries(entries []entry, width, maxLines int, cache map[int]string) string {
	if width < 20 {
		width = 20
	}
	if maxLines < 1 {
		maxLines = 10
	}
	parts := make([]string, len(entries))
	for i, e := range entries {
		r, ok := cache[e.seq]
		if !ok {
			r = renderEntry(e, width, maxLines)
			cache[e.seq] = r
		}
		parts[i] = r
	}
	return strings.Join(parts, "\n")
}

func renderEntry(e entry, width, maxLines int) string {
	// entryBox border + padding eat four columns; wrap the content inside them.
	// Unboxed kinds wrap to the bare width instead, since there is no border or
	// padding around them to eat columns out of the terminal's actual width.
	inner := max(width-6, 10)
	tag := taskTag(e)
	switch e.kind {
	case eMeta:
		body := lipgloss.NewStyle().Foreground(colDim).Width(inner).Render(e.body)
		return entryBox(body, colDim, width)

	case eYou:
		body := lipgloss.NewStyle().Width(inner).Render(e.body)
		return badge("you", colYou) + "\n" + entryBox(body, colYou, width)

	case eClaude:
		label := e.agentTag
		if label == "" {
			label = "claude"
		}
		return badge(tag+label, colClaude) + "\n" + entryBox(renderMarkdown(e.body, inner), colClaude, width)

	case eThinking:
		label := lipgloss.NewStyle().Foreground(colDim).Italic(true).Render("∴ thinking")
		body := clampBlock(e.body, width, maxLines, colDim, lipgloss.NewStyle().Foreground(colDim).Italic(true))
		if body == "" {
			return label
		}
		return label + "\n" + body

	case eTool:
		head := badge(tag+"⚙ "+e.title, colTool)
		style := lipgloss.NewStyle().Foreground(colDim)
		if e.styled {
			style = lipgloss.NewStyle() // the body is already ANSI-highlighted code
		}
		body := clampLines(e.body, inner, maxLines, style)
		if body == "" {
			return head
		}
		return head + "\n" + entryBox(body, colTool, width)

	case eToolOK:
		body := clampLines(e.body, inner, maxLines, lipgloss.NewStyle().Foreground(colDim))
		if body == "" {
			return lipgloss.NewStyle().Foreground(colGood).Render("  " + tag + "↳ (ok)")
		}
		return entryBox(body, colGood, width)

	case eToolErr:
		body := clampLines(e.body, inner, maxLines, lipgloss.NewStyle().Foreground(colErr))
		if body == "" {
			return lipgloss.NewStyle().Foreground(colErr).Render("  " + tag + "✗ (error)")
		}
		return entryBox(body, colErr, width)

	case ePlan:
		return renderPlan(e, width)

	case eTurn:
		// Unboxed: a border around a horizontal rule would read as nonsense.
		return lipgloss.NewStyle().Foreground(colDim).Faint(true).Width(width).Render(e.body)

	case eComplete:
		body := lipgloss.NewStyle().Foreground(colGood).Bold(true).Width(inner).Render(e.body)
		return entryBox(body, colGood, width)

	case eGood:
		body := lipgloss.NewStyle().Foreground(colGood).Width(inner).Render(e.body)
		return entryBox(body, colGood, width)

	case eWarn:
		body := lipgloss.NewStyle().Foreground(colErr).Width(inner).Render(e.body)
		return entryBox(body, colErr, width)

	case eQueued:
		// Deliberately the same shape as eYou, dimmed: it is your message, it just
		// has not gone anywhere yet. The badge is what tells the two apart when you
		// scroll back and find the same text twice — once queued, once sent.
		body := lipgloss.NewStyle().Foreground(colDim).Width(inner).Render(e.body)
		return badge("⏳ queued", colDim) + "\n" + entryBox(body, colDim, width)

	case eFlow:
		// Plain/preformatted, not renderMarkdown: the body is ascii lanes followed
		// by a fenced mermaid block, and running that through the markdown
		// renderer would reflow the ascii art and syntax-highlight mermaid as a
		// foreign language neither glamour nor chroma actually knows.
		head := badge("⛓ flow", colFlow)
		body := clampLines(e.body, inner, maxLines, lipgloss.NewStyle())
		if body == "" {
			return head
		}
		return head + "\n" + entryBox(body, colFlow, width)
	}
	return lipgloss.NewStyle().Width(width).Render(e.body)
}

// taskTag marks an entry as belonging to a delegated task rather than to the
// conversation you are having. Without it a child's tool calls read as though
// the parent were making them, which is exactly the confusion the whole design
// is trying to remove.
func taskTag(e entry) string {
	if e.task == "" {
		return ""
	}
	return e.task + " · "
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

// entryBox frames a transcript entry's body in a rounded border matching its
// attribution color. width is the full transcript width; the box renders two
// columns narrower so the border stays inside it.
func entryBox(content string, col color.Color, width int) string {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(col).
		Padding(0, 1).Width(max(width-2, 20)).
		Render(content)
}

// clampLines wraps body to width, caps it at maxLines visual lines with a dim
// "… +N more lines" footer when truncated, and applies style to the kept text.
// Returns "" for empty bodies so callers can omit the block entirely.
func clampLines(body string, width, maxLines int, style lipgloss.Style) string {
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return ""
	}
	wrapped := lipgloss.NewStyle().Width(width).Render(body)
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
	return content
}

// clampBlock is clampLines behind a colored left gutter bar, for the entries
// that stay light (thinking) instead of taking a full entryBox border.
func clampBlock(body string, width, maxLines int, gutter color.Color, style lipgloss.Style) string {
	// The gutter bar (1 col) + PaddingLeft (1 col) consume two columns.
	content := clampLines(body, max(width-2, 10), maxLines, style)
	if content == "" {
		return ""
	}
	bar := lipgloss.NewStyle().
		Border(lipgloss.Border{Left: "▎"}, false, false, false, true).
		BorderForeground(gutter).
		PaddingLeft(1)
	return bar.Render(content)
}
