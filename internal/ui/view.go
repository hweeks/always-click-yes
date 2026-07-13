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
	footer := m.inputView()
	switch {
	case m.showHelp:
		body = m.overlay(m.helpView())
		footer = lipgloss.NewStyle().Foreground(colDim).Render("press any key to close")
	case m.picking:
		body = m.overlay(m.pickerView())
		footer = lipgloss.NewStyle().Foreground(colDim).Render("↑/↓ move · Enter resume · Esc cancel")
	case m.ask != nil:
		body = m.overlay(m.askView())
		hint := "↑/↓ move · Enter confirm · Esc skip"
		if m.ask.questions[m.ask.qIdx].multiSelect {
			hint = "↑/↓ move · Space toggle · Enter confirm · Esc skip"
		}
		footer = lipgloss.NewStyle().Foreground(colDim).Render(hint)
	case len(m.pending) > 0:
		footer = m.gateView()
	}

	return strings.Join([]string{
		m.headerView(),
		body,
		footer,
	}, "\n")
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
		cmd("/resume [id]", "resume a prior session (picker if no id)"),
		cmd("/model <name>", "set the model for the next launched/resumed session"),
		cmd("/clear", "clear the transcript view"),
		cmd("/log", "show the debug-log path"),
		cmd("/quit", "quit (same as Ctrl+C)"),
		"",
		lipgloss.NewStyle().Bold(true).Foreground(colDim).Render("keys"),
		cmd("Ctrl+G", "arm the plan (start auto-run)"),
		cmd("Esc", "interject / interrupt the current turn"),
		cmd("↑/↓ PgUp/PgDn", "scroll the transcript"),
		cmd("Ctrl+C", "quit"),
		"",
		lipgloss.NewStyle().Bold(true).Foreground(colDim).Render("while a gate is counting down"),
		cmd("a / s / p", "allow now / stop (veto) / pause-resume"),
	}
	return strings.Join(lines, "\n")
}

// pickerView renders the /resume session list with the selected row highlighted,
// windowed to fit the viewport height.
func (m Model) pickerView() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(colPlan).Render("↩ resume a session")
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

// headerView is the top status bar: phase badge, name, live status, and cost.
func (m Model) headerView() string {
	phaseColors := map[Phase]lipgloss.Color{
		PhasePlan:     colPlan,
		PhaseAutoRun:  colClaude,
		PhaseComplete: colGood,
	}
	col, ok := phaseColors[m.phase]
	if !ok {
		col = colDim
	}
	left := badge(m.phase.String(), col) + " " +
		lipgloss.NewStyle().Bold(true).Foreground(colClaude).Render("always-click-yes")

	meta := []string{m.status}
	if m.sessionID != "" {
		meta = append(meta, "session "+short(m.sessionID))
	}
	meta = append(meta, fmt.Sprintf("$%.4f", m.cost))
	right := ""
	if m.processing || m.verifying {
		right = spinner(m.spinFrame) + " "
	}
	right += lipgloss.NewStyle().Foreground(colDim).Render(strings.Join(meta, " · "))

	return left + "  " + right
}

// inputView renders the framed message box plus a context-sensitive hint line.
// The box has a top and bottom rule (no side borders) that brightens while a
// turn is in flight.
func (m Model) inputView() string {
	var hint, spin string
	switch {
	case m.processing:
		spin = spinner(m.spinFrame) + " "
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
	if m.planReady && m.phase == PhasePlan {
		hintStyle = lipgloss.NewStyle().Bold(true).Foreground(colPlan)
	}

	borderColor := colDim
	if m.processing || m.verifying {
		borderColor = colClaude
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder(), true, false).
		BorderForeground(borderColor).
		Width(max(m.width-2, 20)).
		Render(m.input.View())

	return box + "\n" + spin + hintStyle.Render(hint)
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
	// A top rule keeps the gate panel the same height (4 lines) as the framed
	// input box, so the footer doesn't jump when a gate appears.
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder(), true, false, false, false).
		BorderForeground(colTool).
		Width(max(m.width-2, 20)).
		Render(panel)
}
