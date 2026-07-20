package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func (m Model) View() string {
	if !m.ready {
		return "starting…"
	}

	body := m.vp.View()
	switch {
	case m.showHelp:
		body = m.overlay(m.helpView())
	case m.picking:
		body = m.overlay(m.pickerView())
	case m.ask != nil:
		body = m.overlay(m.askView())
	}

	return strings.Join([]string{
		m.headerView(),
		body,
		m.footerView(),
	}, "\n")
}

// footerView is the bottom region: a hint line under an overlay, the countdown
// panel while permissions are pending, and otherwise the composer. layout()
// measures this to size the transcript, so it must be the only place the footer
// is built — a second copy of these conditions would drift and put the frame's
// height back out of sync with what's drawn.
func (m Model) footerView() string {
	hint := func(s string) string { return lipgloss.NewStyle().Foreground(colDim).Render(s) }
	switch {
	case m.showHelp:
		return hint("press any key to close")
	case m.picking:
		return hint("↑/↓ move · Enter resume · Esc cancel")
	case m.ask != nil:
		keys := "↑/↓ move · Enter confirm · Esc skip"
		if m.ask.questions[m.ask.qIdx].multiSelect {
			keys = "↑/↓ move · Space toggle · Enter confirm · Esc skip"
		}
		// In AUTO-RUN the question is on a clock, and a countdown nobody can see is
		// how the gate bug happened. Say it out loud.
		if r := m.askRemaining(); !m.ask.deadline.IsZero() {
			keys += fmt.Sprintf(" · auto-skip in %ds", int(r.Seconds()+0.5))
		}
		return hint(keys)
	case len(m.pending) > 0:
		return m.gateView()
	}
	return m.inputView()
}

// overlay pads content to the viewport height so the footer stays anchored while
// an overlay (help, picker) replaces the transcript region.
func (m Model) overlay(content string) string {
	return lipgloss.NewStyle().Width(m.vp.Width).Height(m.vp.Height).Render(content)
}

// helpView lists the slash commands and key bindings.
func (m Model) helpView() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(colClaude).Render("always-click-yes · help")
	cmd := func(name, desc string) string {
		return lipgloss.NewStyle().Foreground(colTool).Render("  "+name) + "  " +
			lipgloss.NewStyle().Foreground(colDim).Render(desc)
	}
	lines := []string{
		title,
		"",
		lipgloss.NewStyle().Bold(true).Foreground(colDim).Render("commands"),
		cmd("/help", "show this help"),
		cmd("/resume [id]", "restore a prior run — transcript, phase and cost (picker if no id)"),
		cmd("/model <name>", "set the model for the next launched/resumed session"),
		cmd("/clear", "clear the transcript view"),
		cmd("/log", "show the debug-log path"),
		cmd("/quit", "quit (same as Ctrl+C)"),
		"",
		lipgloss.NewStyle().Bold(true).Foreground(colDim).Render("keys"),
		cmd("Enter", "send the message"),
		cmd("Ctrl+J", "newline without sending"),
		cmd("Ctrl+G", "arm the plan (start auto-run)"),
		cmd("Esc", "interject / interrupt the current turn"),
		cmd("↑/↓ PgUp/PgDn", "scroll the transcript"),
		cmd("Ctrl+C", "quit"),
		"",
		lipgloss.NewStyle().Bold(true).Foreground(colDim).Render("while a gate is counting down"),
		cmd("a / s / p", "allow now / stop (veto) / pause-resume"),
		"",
		lipgloss.NewStyle().Bold(true).Foreground(colDim).Render("while Claude is asking a question"),
		cmd("↑/↓ j/k", "move between options"),
		cmd("Space", "toggle a choice (multi-select only)"),
		cmd("Enter", "confirm and go to the next question"),
		cmd("Esc", "skip the questions"),
	}
	return strings.Join(lines, "\n")
}

// pickerView renders the /resume session list with the selected row highlighted,
// windowed to fit the viewport height.
func (m Model) pickerView() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(colPlan).Render(
		"↩ resume a session · [PHASE] marks the runs acy supervised")
	maxVisible := max(m.vp.Height-3, 3)
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
		snap, ok := m.sessionSnaps[s.ID]
		if label := snapLabel(snap, ok); label != "" {
			summary = "[" + label + "] " + summary
		}
		line := fmt.Sprintf("%s  %s  %s", short(s.ID), s.ModTime.Format("Jan 02 15:04"), summary)
		line = truncate(line, max(m.vp.Width-2, 20))
		if i == m.pickIdx {
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
	progress := ""
	if len(a.questions) > 1 {
		progress = fmt.Sprintf("  (%d/%d)", a.qIdx+1, len(a.questions))
	}
	title := lipgloss.NewStyle().Bold(true).Foreground(colYou).Render("❓ "+header) +
		lipgloss.NewStyle().Foreground(colDim).Render(progress)
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
		line = truncate(line, max(m.vp.Width-6, 20))
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
	left := chip + bar.Bold(true).Render(" always-click-yes ")

	meta := []string{m.status}
	if m.sessionID != "" {
		meta = append(meta, "session "+short(m.sessionID))
	}
	meta = append(meta, fmt.Sprintf("$%.4f", m.totalCost()))
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
	var hint string
	switch {
	case m.processing:
		hint = "working… · Esc to interject · Ctrl+C to quit"
	case m.planReady && m.phase == PhasePlan:
		hint = "📋 plan ready above · Ctrl+G to arm & run · or keep chatting to refine"
	case m.phase == PhasePlan:
		hint = "Enter to send · Ctrl+G to arm (start auto-run) · Ctrl+C to quit"
	case m.phase == PhaseComplete:
		hint = "plan complete · Enter to send a follow-up · Ctrl+C to quit"
	default:
		hint = "Enter to send · Ctrl+C to quit"
	}
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

	out := box + "\n" + hintStyle.Render(hint)
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
func sweep(width, frame int, col lipgloss.Color) string {
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

	state := lipgloss.NewStyle().Bold(true).Foreground(colTool).Render(fmt.Sprintf("⏳ auto-approve in %2ds", secs))
	if m.paused {
		state = lipgloss.NewStyle().Bold(true).Foreground(colErr).Render("⏸  PAUSED         ")
	}

	desc := badge("⚙ "+front.p.Input.ToolName, colTool) + " " +
		lipgloss.NewStyle().Foreground(colDim).Render(firstLine(toolArgs(front.p.Input.ToolInput)))
	if n := len(m.pending) - 1; n > 0 {
		desc += lipgloss.NewStyle().Foreground(colDim).Render(fmt.Sprintf("  (+%d queued)", n))
	}

	keys := lipgloss.NewStyle().Foreground(colDim).Render("[s]top  [a]llow  [p]ause  ^C quit")

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
