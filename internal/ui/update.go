package ui

import (
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/gate"
)

// transcriptKeyMap restricts the viewport to deliberate scroll keys. The bubbles
// default binds j/k/d/u/f/b and space to scrolling, which would fire while the
// user is typing a message (the input box and viewport both receive key events).
// Keeping only the arrows and PgUp/PgDn frees every letter for typing.
func transcriptKeyMap() viewport.KeyMap {
	km := viewport.DefaultKeyMap()
	km.Up = key.NewBinding(key.WithKeys("up"))
	km.Down = key.NewBinding(key.WithKeys("down"))
	km.PageUp = key.NewBinding(key.WithKeys("pgup"))
	km.PageDown = key.NewBinding(key.WithKeys("pgdown"))
	km.HalfPageUp = key.NewBinding()
	km.HalfPageDown = key.NewBinding()
	return km
}

const (
	headerHeight = 1
	maxInputRows = 8 // the composer grows to this many rows, then scrolls internally
)

// Update runs the message switch, then re-lays-out the frame. The composer grows
// with its content, so the footer's height is not a constant — layout has to run
// after anything that can change it (a keystroke, a send that clears the box, a
// preloaded prompt, a gate arriving), which is to say: after everything.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Give the composer its full height *before* it handles the message. The
	// textarea scrolls its own view to keep the cursor visible and only ever
	// scrolls down: left one row tall, the keystroke that first wraps the message
	// would scroll it past row one, and growing it afterwards never scrolls back —
	// the top of the message would just vanish. Sized up front, it never scrolls
	// until the message genuinely outgrows the cap, and layout shrinks it back to
	// fit the content.
	m.input.SetHeight(maxInputRows)

	m, cmd := m.update(msg)
	m.layout()
	return m, cmd
}

// layout sizes the composer to its content and gives the transcript whatever is
// left. Deriving the viewport's height from the footer as *actually rendered* is
// what keeps header + body + footer exactly `height` lines tall: the old fixed
// footerHeight was a lie the moment the input wrapped, and the extra line pushed
// the frame past the bottom of the screen, which is what made the box appear to
// flip between one and two lines.
func (m *Model) layout() {
	if !m.ready {
		return
	}
	m.input.SetWidth(max(m.width-2, 20))
	m.input.SetHeight(clamp(wrappedRows(m.input.Value(), m.input.Width()), 1, maxInputRows))

	vpHeight := max(m.height-headerHeight-lipgloss.Height(m.footerView()), 3)
	if m.vp.Height == vpHeight {
		return
	}
	atBottom := m.vp.AtBottom()
	m.vp.Height = vpHeight
	if atBottom {
		m.vp.GotoBottom() // stay pinned to the newest output as the composer grows
	}
}

// wrappedRows is how many rows a value occupies once soft-wrapped to width.
// textarea.LineCount counts logical lines only (it is len(value) split on "\n"),
// so it can't answer this; measuring the wrapped render can.
func wrappedRows(value string, width int) int {
	if width < 1 || value == "" {
		return 1
	}
	return lipgloss.Height(lipgloss.NewStyle().Width(width).Render(value))
}

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }

func (m Model) update(msg tea.Msg) (Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if !m.ready {
			m.vp = viewport.New(msg.Width, max(msg.Height-headerHeight-4, 3))
			m.vp.KeyMap = transcriptKeyMap()
			m.ready = true
		} else {
			m.vp.Width = msg.Width
		}
		m.bar.Width = max(msg.Width-4, 10)
		m.layout() // the viewport must be sized before rebuild re-renders into it
		m.rebuild()
		return m, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			if m.drv != nil {
				m.drv.Stop()
			}
			return m, tea.Quit
		}
		// The help overlay is dismissed by any key.
		if m.showHelp {
			m.showHelp = false
			m.rebuild()
			return m, nil
		}
		// The resume picker captures navigation keys until dismissed.
		if m.picking {
			cmd := m.handlePickKey(msg)
			m.rebuild()
			return m, cmd
		}
		// A pending AskUserQuestion captures navigation keys until answered.
		if m.ask != nil {
			cmd := m.handleAskKey(msg)
			m.rebuild()
			return m, cmd
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
		// Ctrl+G arms the run: switch from planning to auto-run. The driver check is
		// not redundant — a resume knows the session id before its process exists, and
		// arming into that gap would launch a second claude for the same session.
		if msg.Type == tea.KeyCtrlG && m.phase == PhasePlan && m.sessionID != "" && m.drv != nil {
			m.capturePlan()
			m.appendEntry(entry{kind: eGood, body: "▶ arming — resuming session in auto-run…"})
			m.planReady = false
			m.status = "arming…"
			m.persist()
			m.rebuild()
			return m, launchCmd(m.ctx, m.launcher, LaunchSpec{
				Phase:    PhaseAutoRun,
				ResumeID: m.sessionID,
				Model:    m.nextModel,
				Kickoff:  true, // arming is the one launch that starts the work
			})
		}
		if msg.Type == tea.KeyEnter {
			cmd := m.handleEnter()
			m.rebuild()
			return m, cmd
		}

	case eventMsg:
		if msg.gen != m.gen {
			return m, nil // stale driver
		}
		m.ingest(msg.ev)
		// init tells us the session id, result moves the cost: both are the state a
		// crash would otherwise lose.
		if msg.ev.IsInit() || msg.ev.IsTurnEnd() {
			m.persist()
		}
		if c := m.onTurnEnd(msg.ev); c != nil {
			cmds = append(cmds, c)
		}
		m.rebuild()
		cmds = append(cmds, waitEvent(m.drv.Events(), m.gen))

	case resumeMsg:
		cmds = append(cmds, m.applyResume(msg))
		m.rebuild()

	case verdictMsg:
		m.onVerdict(msg)
		m.rebuild()
		return m, nil

	case streamClosedMsg:
		if msg.gen != m.gen {
			return m, nil // an old driver we deliberately swapped out
		}
		m.ended = true
		m.status = "session ended"
		// Nothing is left to answer, and an open panel would swallow every key.
		m.ask = nil
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
		m.spinFrame++ // animates the footer/header spinner; View() re-renders each tick
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
