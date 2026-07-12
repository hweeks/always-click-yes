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

	footer := m.inputView()
	if len(m.pending) > 0 {
		footer = m.gateView()
	}

	return strings.Join([]string{
		m.headerView(),
		m.vp.View(),
		footer,
	}, "\n")
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
	right := lipgloss.NewStyle().Foreground(colDim).Render(strings.Join(meta, " · "))

	return left + "  " + right
}

// inputView renders the message box plus a context-sensitive hint line.
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
	if m.planReady && m.phase == PhasePlan {
		hintStyle = lipgloss.NewStyle().Bold(true).Foreground(colPlan)
	}
	return strings.Join([]string{m.input.View(), "", hintStyle.Render(hint)}, "\n")
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

	return strings.Join([]string{
		m.bar.ViewAs(frac),
		state + "   " + desc,
		keys,
	}, "\n")
}
