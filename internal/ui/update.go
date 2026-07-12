package ui

import (
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/gate"
)

const (
	headerHeight = 1
	footerHeight = 3 // input/gate panel occupies three lines
)

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		vpHeight := max(msg.Height-headerHeight-footerHeight, 3)
		if !m.ready {
			m.vp = viewport.New(msg.Width, vpHeight)
			m.ready = true
		} else {
			m.vp.Width = msg.Width
			m.vp.Height = vpHeight
		}
		m.input.Width = msg.Width - 4
		m.bar.Width = max(msg.Width-4, 10)
		m.rebuild()
		return m, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			if m.drv != nil {
				m.drv.Stop()
			}
			return m, tea.Quit
		}
		// When gates are pending, keys drive the countdown instead of the input.
		if len(m.pending) > 0 {
			m.handleGateKey(msg)
			m.rebuild()
			return m, nil
		}
		// Esc interjects: interrupt the in-flight turn to redirect.
		if msg.Type == tea.KeyEsc && m.interject() {
			m.rebuild()
			return m, nil
		}
		// Ctrl+G arms the run: switch from planning to auto-run.
		if msg.Type == tea.KeyCtrlG && m.phase == PhasePlan && m.sessionID != "" {
			m.appendEntry(entry{kind: eGood, body: "▶ arming — resuming session in auto-run…"})
			m.planReady = false
			m.status = "arming…"
			m.rebuild()
			return m, launchCmd(m.ctx, m.launcher, PhaseAutoRun, m.sessionID)
		}
		if msg.Type == tea.KeyEnter {
			m.sendInput()
			m.rebuild()
			return m, nil
		}

	case eventMsg:
		if msg.gen != m.gen {
			return m, nil // stale driver
		}
		m.ingest(msg.ev)
		m.onTurnEnd(msg.ev)
		m.rebuild()
		cmds = append(cmds, waitEvent(m.drv.Events(), m.gen))

	case streamClosedMsg:
		if msg.gen != m.gen {
			return m, nil // an old driver we deliberately swapped out
		}
		m.ended = true
		m.status = "session ended"
		m.appendEntry(entry{kind: eTurn, body: "──── session ended ────"})
		m.rebuild()
		return m, nil

	case driverReadyMsg:
		return m, m.onDriverReady(msg)

	case errMsg:
		alog.Printf("ui: error: %v", msg.err)
		m.appendEntry(entry{kind: eWarn, body: "error: " + msg.err.Error()})
		m.rebuild()
		return m, nil

	case gateMsg:
		m.enqueue(msg.p)
		m.rebuild()
		cmds = append(cmds, waitGate(m.gateReqs))

	case gateClosedMsg:
		// no more gates will arrive; nothing to re-arm
		return m, nil

	case tickMsg:
		m.now = time.Time(msg)
		m.expireDue()
		if len(m.pending) > 0 {
			m.rebuild()
		}
		return m, tickCmd()
	}

	// Route remaining messages to the sub-components.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	cmds = append(cmds, cmd)
	m.vp, cmd = m.vp.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

// handleGateKey processes controls while one or more permission gates are
// counting down.
func (m *Model) handleGateKey(msg tea.KeyMsg) {
	switch strings.ToLower(msg.String()) {
	case "s": // stop/veto the front gate
		name := m.pending[0].p.Input.ToolName
		m.resolveFront(
			gate.Decision{Behavior: gate.Deny, Reason: "vetoed by user"},
			entry{kind: eWarn, body: "✋ vetoed · ⚙ " + name})
	case "a": // approve the front gate immediately
		name := m.pending[0].p.Input.ToolName
		m.resolveFront(
			gate.Decision{Behavior: gate.Allow, Reason: "approved by user"},
			entry{kind: eGood, body: "✔ approved · ⚙ " + name})
	case "p": // pause / resume all countdowns
		m.togglePause()
	}
}
