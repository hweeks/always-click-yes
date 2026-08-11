package ui

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// View returns a tea.View rather than a string in v2: the terminal modes that
// used to be program options are now fields on the frame you hand back each
// render. Alt-screen is the only one acy sets, so it travels on the model (see
// Config.AltScreen) instead of on tea.NewProgram.
func (m Model) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = m.altScreen
	if !m.ready {
		v.SetContent("starting…")
		return v
	}

	body := m.vp.View()
	switch m.surface() {
	case SurfaceHelp:
		body = m.overlay(m.helpView())
	case SurfacePicker:
		body = m.overlay(m.pickerView())
	case SurfaceAsk:
		body = m.overlay(m.askView())
	case SurfaceQueue:
		body = m.overlay(m.queueEditView())
	}

	v.SetContent(strings.Join([]string{
		m.headerView(),
		body,
		m.footerView(),
	}, "\n"))
	return v
}

// footerView is the bottom region: a hint line under an overlay, otherwise the
// composer — with the countdown panel stacked above it while permissions are
// pending. layout() measures this to size the transcript, so it must be the only
// place the footer is built — a second copy of these conditions would drift and
// put the frame's height back out of sync with what's drawn.
func (m Model) footerView() string {
	hint := func(s string) string { return lipgloss.NewStyle().Foreground(colDim).Render(s) }
	switch m.surface() {
	case SurfaceHelp:
		return hint(helpFooterHint)
	case SurfacePicker:
		return hint(pickerFooterHint)
	case SurfaceAsk:
		keys := askFooterHint(m.ask.questions[m.ask.qIdx].multiSelect)
		if r := m.askRemaining(); !m.ask.deadline.IsZero() {
			keys += askAutoSkipNote(r)
		}
		return hint(keys)
	case SurfaceQueue:
		return hint(queueEditFooterHint)
	}
	// The gate stacks; it does not replace. In an armed run something is counting
	// down nearly all the time, so a panel that stood in for the composer left the
	// user with no text box to type into for most of the run. The queue panel
	// stacks between them, next to the box the messages were typed into.
	parts := make([]string, 0, 3)
	if len(m.pending) > 0 {
		parts = append(parts, m.gateView())
	}
	if q := m.queueView(); q != "" {
		parts = append(parts, q)
	}
	return strings.Join(append(parts, m.inputView()), "\n")
}

// queueMaxShown is how many held messages the footer panel lists before it just
// counts the rest — enough to recognise what is waiting, few enough that a long
// queue can't push the transcript off the screen.
const queueMaxShown = 3

// queueView is the dim panel listing messages waiting for the turn to end. It is
// the answer to "did that Enter do anything?", which used to be "no".
func (m Model) queueView() string {
	if len(m.queued) == 0 {
		return ""
	}
	dim := lipgloss.NewStyle().Foreground(colDim)
	lines := []string{dim.Render(queueSummary(len(m.queued)))}
	for i, q := range m.queued {
		if i == queueMaxShown {
			lines = append(lines, dim.Faint(true).Render(queueMoreNote(len(m.queued)-queueMaxShown)))
			break
		}
		lines = append(lines, dim.Render("   "+truncate(firstLine(q.text), max(m.width-6, 20))))
	}
	return strings.Join(lines, "\n")
}

// overlay pads content to the viewport height so the footer stays anchored while
// an overlay (help, picker) replaces the transcript region.
func (m Model) overlay(content string) string {
	return lipgloss.NewStyle().Width(m.vp.Width()).Height(m.vp.Height()).Render(content)
}

// helpView paints helpContent(): the sections, their titles and their rows. The
// text itself lives in present.go so the webview can render the same rows as a
// table instead of parsing ANSI back out of a string — this function knows only
// how a row looks, never what it says.
func (m Model) helpView() string {
	cmd := func(name, desc string) string {
		return lipgloss.NewStyle().Foreground(colTool).Render("  "+name) + "  " +
			lipgloss.NewStyle().Foreground(colDim).Render(desc)
	}
	lines := []string{lipgloss.NewStyle().Bold(true).Foreground(colClaude).Render(helpTitle)}
	for _, sec := range helpContent() {
		// The blank line goes *before* each section, which is what leaves one
		// under the title and none trailing the last section.
		lines = append(lines, "", lipgloss.NewStyle().Bold(true).Foreground(colDim).Render(sec.Title))
		for _, r := range sec.Rows {
			lines = append(lines, cmd(r.Keys, r.Description))
		}
	}
	return strings.Join(lines, "\n")
}

// pickerView renders the /resume session list with the selected row highlighted,
// windowed to fit the viewport height.
func (m Model) pickerView() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(colPlan).Render(
		"↩ resume a session · [PHASE] marks the runs acy supervised")
	maxVisible := max(m.vp.Height()-3, 3)
	start := 0
	if m.pickIdx >= maxVisible {
		start = m.pickIdx - maxVisible + 1
	}
	end := min(start+maxVisible, len(m.sessionList))

	rows := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		s := m.sessionList[i]
		summary := s.Summary
		if summary == "" {
			summary = "(no summary)"
		}
		// Sessions acy supervised carry their state; the rest are just claude
		// sessions, and show only what claude knows about them.
		if s.Label != "" {
			summary = "[" + s.Label + "] " + summary
		}
		line := fmt.Sprintf("%s  %s  %s", short(s.ID), s.modTime.Format("Jan 02 15:04"), summary)
		line = truncate(line, max(m.vp.Width()-2, 20))
		if i == m.pickIdx {
			rows = append(rows, lipgloss.NewStyle().Bold(true).Foreground(colInk).Background(colPlan).Render("▸ "+line))
		} else {
			rows = append(rows, lipgloss.NewStyle().Foreground(colDim).Render("  "+line))
		}
	}
	return title + "\n\n" + strings.Join(rows, "\n")
}

// queueEditView renders the /queue edit overlay: every held message with the
// cursor row highlighted, windowed to fit the viewport height — the same shape
// pickerView uses for the /resume list.
func (m Model) queueEditView() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(colPlan).Render(
		"✎ edit the queue · Enter pulls a message into the composer · Ctrl+X drops it")
	maxVisible := max(m.vp.Height()-3, 3)
	start := 0
	if m.queueCursor >= maxVisible {
		start = m.queueCursor - maxVisible + 1
	}
	end := min(start+maxVisible, len(m.queued))

	rows := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		line := truncate(firstLine(m.queued[i].text), max(m.vp.Width()-2, 20))
		if i == m.queueCursor {
			rows = append(rows, lipgloss.NewStyle().Bold(true).Foreground(colInk).Background(colPlan).Render("▸ "+line))
		} else {
			rows = append(rows, lipgloss.NewStyle().Foreground(colDim).Render("  "+line))
		}
	}
	return title + "\n\n" + strings.Join(rows, "\n")
}

// askView renders the current AskUserQuestion: its prompt and options, with the
// cursor row highlighted and selected rows marked.
func (m Model) askView() string {
	a := m.ask
	q := a.questions[a.qIdx]

	header := q.header
	if header == "" {
		header = "question"
	}
	title := lipgloss.NewStyle().Bold(true).Foreground(colYou).Render("❓ "+header) +
		lipgloss.NewStyle().Foreground(colDim).Render(askProgressNote(a.qIdx, len(a.questions)))
	prompt := lipgloss.NewStyle().Foreground(colClaude).Render(q.question)

	rows := make([]string, 0, len(q.options))
	for i, o := range q.options {
		mark := " "
		if q.multiSelect && q.selected[i] {
			mark = "✔"
		}
		line := o.label
		if o.description != "" {
			line += " — " + o.description
		}
		line = truncate(line, max(m.vp.Width()-6, 20))
		if i == q.cursor {
			rows = append(rows, lipgloss.NewStyle().Bold(true).Foreground(colInk).Background(colYou).Render(fmt.Sprintf("▸ %s %s", mark, line)))
		} else {
			rows = append(rows, lipgloss.NewStyle().Foreground(colDim).Render(fmt.Sprintf("  %s %s", mark, line)))
		}
	}
	return title + "\n" + prompt + "\n\n" + strings.Join(rows, "\n")
}

// headerView is the top status bar: a full-width strip in the phase's color, so
// the mode is legible from across the room. A dark chip carries the phase name;
// the live status, session and cost sit right-aligned on the same strip.
func (m Model) headerView() string {
	col := phaseColor(m.phase)
	bar := lipgloss.NewStyle().Background(col).Foreground(colInk)

	chip := lipgloss.NewStyle().Bold(true).Foreground(col).Background(colInk).
		Render(" " + m.phase.String() + " ")
	left := chip
	// The branch badge sits beside the phase chip, on the left, where it can
	// never be lost to truncation — unlike the right-hand meta strip, which is
	// deliberately truncated from the tail on a narrow terminal.
	if m.branch != "" {
		left += lipgloss.NewStyle().Foreground(colInk).Background(colDim).
			Render(" " + m.branch + " ")
	}
	left += bar.Bold(true).Render(" always-click-yes ")

	meta := []string{m.status}
	// Right after the status, because it *is* status: it says the next thing that
	// will happen when this turn ends. The meta strip is truncated from the tail.
	if n := len(m.queued); n > 0 {
		meta = append(meta, fmt.Sprintf("%d queued", n))
	}
	if m.sessionID != "" {
		meta = append(meta, "session "+short(m.sessionID))
	}
	meta = append(meta, fmt.Sprintf("$%.4f", m.grandTotalCost()))
	if t := m.tokenSummary(); t != "" {
		meta = append(meta, t)
	}
	if b := m.billing(); b != "" {
		meta = append(meta, b)
	}
	rightText := strings.Join(meta, " · ")
	if m.processing {
		rightText = spinGlyph(m.spinFrame) + " " + rightText
	}
	// Everything must fit the single header row; shrink the meta before it wraps.
	if avail := m.width - lipgloss.Width(left) - 2; avail >= 0 {
		rightText = truncate(rightText, avail)
	} else {
		rightText = ""
	}
	right := bar.Render(" " + rightText + " ")

	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(right), 0)
	return left + bar.Render(strings.Repeat(" ", gap)) + right
}

// inputView renders the framed message box plus a context-sensitive hint line.
// The box has a top and bottom rule (no side borders) in the phase's color, and
// while a turn is in flight the large working indicator sits above it.
func (m Model) inputView() string {
	// What to say comes from present.go, which both front ends share; how it
	// looks stays here. The two styling cases below deliberately test the model
	// rather than the hint kind — "complete and not processing" is a wider state
	// than the complete hint, and narrowing it would repaint a frame.
	hint := m.hint().Text
	hintStyle := lipgloss.NewStyle().Foreground(colDim)
	switch {
	case m.planReady && m.phase == PhasePlan:
		hintStyle = lipgloss.NewStyle().Bold(true).Foreground(colPlan)
	case m.phase == PhaseComplete && !m.processing:
		hintStyle = lipgloss.NewStyle().Foreground(colGood)
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder(), true, false).
		BorderForeground(phaseColor(m.phase)).
		Width(max(m.width-2, 20)).
		Render(m.input.View())

	out := box
	// Directly under the box, because it is a statement about what is in the box:
	// the paths are sitting there as editable text, and this line is the
	// confirmation that acy read the drag as files rather than as a stray string.
	if note := attachNote(m.attached, max(m.width-2, 20)); note != "" {
		out += "\n" + lipgloss.NewStyle().Foreground(colDim).Render(note)
	}
	out += "\n" + hintStyle.Render(hint)
	if m.processing {
		out = m.workingView() + "\n" + out
	}
	return out
}

// workingView is the large in-flight indicator: a phase-colored WORKING chip with
// the elapsed time, and an animated sweep bar filling the rest of the row. It
// animates off spinFrame, which the 120ms tick already advances.
func (m Model) workingView() string {
	col := phaseColor(m.phase)
	label := lipgloss.NewStyle().Bold(true).Foreground(colInk).Background(col).Padding(0, 1).
		Render(spinGlyph(m.spinFrame) + " WORKING")

	elapsed := ""
	if !m.turnStart.IsZero() {
		if d := m.now.Sub(m.turnStart); d > 0 {
			elapsed = fmt.Sprintf(" %ds", int(d.Seconds()))
		}
	}
	et := lipgloss.NewStyle().Bold(true).Foreground(col).Render(elapsed)

	barWidth := m.width - lipgloss.Width(label) - lipgloss.Width(et) - 2
	return label + et + " " + sweep(barWidth, m.spinFrame, col)
}

// sweep draws a width-cell track with a bright segment bouncing across it — the
// indeterminate progress bar for "a turn is running, no ETA".
func sweep(width, frame int, col color.Color) string {
	if width < 8 {
		return ""
	}
	span := max(width/5, 4)
	period := max(width-span, 1)
	pos := frame % (2 * period)
	if pos >= period {
		pos = 2*period - pos
	}
	track := lipgloss.NewStyle().Foreground(colDim).Faint(true)
	lit := lipgloss.NewStyle().Bold(true).Foreground(col)
	return track.Render(strings.Repeat("╌", pos)) +
		lit.Render(strings.Repeat("━", span)) +
		track.Render(strings.Repeat("╌", width-span-pos))
}

// gateView renders the permission countdown panel shown while gates are pending.
func (m Model) gateView() string {
	front := m.pending[0]
	rem := m.frontRemaining()
	frac := 0.0
	if m.countdown > 0 {
		frac = float64(rem) / float64(m.countdown)
	}
	secs := int(rem/time.Second) + 1
	if rem <= 0 {
		secs = 0
	}

	state := lipgloss.NewStyle().Bold(true).Foreground(colTool).Render(gateStateLabel(secs, false))
	if m.paused {
		state = lipgloss.NewStyle().Bold(true).Foreground(colErr).Render(gateStateLabel(secs, true))
	}

	// Name the task when a delegated child raised this. Approving an edit reads
	// very differently depending on whether you asked for it or a task you
	// dispatched ten minutes ago did.
	desc := ""
	if front.task != "" {
		desc = lipgloss.NewStyle().Foreground(colPlan).Render("["+front.task+"] ") + " "
	}
	desc += badge("⚙ "+front.p.Input.ToolName, colTool) + " " +
		lipgloss.NewStyle().Foreground(colDim).Render(firstLine(toolArgs(front.p.Input.ToolInput)))
	if note := gateQueuedNote(len(m.pending) - 1); note != "" {
		desc += lipgloss.NewStyle().Foreground(colDim).Render(note)
	}

	// Kept short enough to survive an 80-column panel on one line: it sits right
	// above the composer now, and a wrapped hint reads as part of the message box.
	keys := lipgloss.NewStyle().Foreground(colDim).Render(
		"^Y allow  ^X stop  ^R pause  ^C quit  ·  or just keep typing")

	panel := strings.Join([]string{
		m.bar.ViewAs(frac),
		state + "   " + desc,
		keys,
	}, "\n")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder(), true, false, false, false).
		BorderForeground(colTool).
		Width(max(m.width-2, 20)).
		Render(panel)
}
