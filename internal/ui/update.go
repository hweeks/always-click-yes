package ui

import (
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/mcp"
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

// sendKeys are the key spellings that submit the composer: unmodified Enter, and
// only that. The modified variants insert a newline instead — they are bound on
// the textarea's KeyMap.InsertNewline in New, and reach it by falling out of the
// key switch below to the sub-component routing.
var sendKeys = key.NewBinding(key.WithKeys("enter"))

func isEnter(msg tea.KeyPressMsg) bool { return key.Matches(msg, sendKeys) }

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
	m.input.SetHeight(clamp(max(
		wrappedRows(m.input.Value(), m.input.Width()),
		composerCursorRows(&m.input),
	), 1, maxInputRows))

	vpHeight := max(m.height-headerHeight-lipgloss.Height(m.footerView()), 3)
	if m.vp.Height() == vpHeight {
		return
	}
	atBottom := m.vp.AtBottom()
	m.vp.SetHeight(vpHeight)
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

// composerCursorRows is how many rows the composer must show for the cursor to
// be on screen — every wrapped row above it, plus its own.
//
// It exists because bubbles v2 made SetHeight reposition the textarea's internal
// view to keep the cursor visible, and that reposition only ever scrolls *down*.
// A box sized to the text alone is one row short whenever the cursor sits just
// past a line that exactly fills the width, so the shrink at the end of a
// keystroke would scroll the text you just typed off the top and never bring it
// back. Sizing to the cursor means the shrink never has to scroll at all.
//
// The row count has to come from the textarea itself: only it knows the cursor
// is on that phantom next row, which is exactly what wrappedRows cannot see.
func composerCursorRows(ta *textarea.Model) int {
	lines := strings.Split(ta.Value(), "\n")
	rows := 0
	for i := 0; i < ta.Line() && i < len(lines); i++ {
		rows += wrappedRows(lines[i], ta.Width())
	}
	return rows + ta.LineInfo().RowOffset + 1
}

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }

func (m Model) update(msg tea.Msg) (Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if !m.ready {
			m.vp = viewport.New(
				viewport.WithWidth(msg.Width),
				viewport.WithHeight(max(msg.Height-headerHeight-4, 3)))
			m.vp.KeyMap = transcriptKeyMap()
			m.ready = true
		} else {
			m.vp.SetWidth(msg.Width)
		}
		m.bar.SetWidth(max(msg.Width-4, 10))
		m.layout() // the viewport must be sized before rebuild re-renders into it
		m.rebuild()
		return m, nil

	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
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
		// When gates are pending, three chords drive the countdown — and nothing
		// else does. Everything they don't claim falls through to the composer and
		// the viewport below, because in an armed auto-run every child tool call
		// raises a gate, so a blanket interception meant the keyboard was dead for
		// most of a run.
		if len(m.pending) > 0 && m.handleGateKey(msg) {
			// The gate ^Y/^X just answered can be the last thing holding the queue:
			// the turn that raised it has already reported, so no further driver or
			// child event is coming to release it. ^R falls in here too and is a
			// no-op — flushQueue re-checks busy() itself, and a paused gate is still
			// pending.
			m.flushQueue()
			m.rebuild()
			return m, nil
		}
		// Esc interjects: interrupt the in-flight turn to redirect. Not while a
		// gate is up, though: the PreToolUse hook that raised it is blocked on the
		// gate socket waiting for a decision, and interrupting the turn out from
		// under it is an unanswered-hook deadlock path. Until that is untangled,
		// answer the gate first (^Y/^X) and interject after.
		if msg.String() == "esc" && len(m.pending) == 0 && m.raise(Interject()).Accepted {
			m.rebuild()
			return m, nil
		}
		// Ctrl+G arms the run: switch from planning to auto-run. The driver check is
		// not redundant — a resume knows the session id before its process exists, and
		// arming into that gap would launch a second claude for the same session.
		if msg.String() == "ctrl+g" && m.phase == PhasePlan && m.sessionID != "" && m.drv != nil {
			cmd := m.applyAction(Arm(), nil)
			m.rebuild()
			return m, cmd
		}
		// Plain Enter sends; shift+enter, alt+enter and ctrl+j must NOT be claimed
		// here, or the textarea never sees the newline they are bound to. Under v1
		// all three had to send: a terminal reported the modified variants as a bare
		// CR, so the comparison could not see the modifier even in principle. v2
		// negotiates the Kitty protocol, so a terminal that speaks it now reports
		// them separately — and one that doesn't sends a bare CR, which is to say
		// shift+enter simply sends there. Hence ctrl+j and alt+enter as fallbacks.
		if isEnter(msg) {
			cmd := m.handleEnter()
			m.rebuild()
			return m, cmd
		}

	case tea.PasteMsg:
		// A dragged file arrives here as whatever the terminal typed for it —
		// shell-escaped, quoted, sometimes several at once. Decide what it is
		// *before* the textarea inserts it: a paste that is entirely file
		// references becomes absolute paths (attachPaste), and everything else
		// falls out of the switch to the routing below and is inserted verbatim.
		if m.attachPaste(msg.Content) {
			return m, nil
		}

	case ActionMsg:
		// The write seam. A second front end reaches the model only through here,
		// and it reaches exactly the same code the keyboard does (see action.go).
		cmd := m.applyAction(msg.Action, msg.Ack)
		// An action can be the last thing holding the queue — a gate answered over
		// HTTP is the same situation as one answered with ^Y, where the turn that
		// raised it has already reported and no further event is coming. flushQueue
		// is fully self-guarded, so this needs no conditions of its own.
		m.flushQueue()
		m.rebuild()
		return m, cmd

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
		m.onTurnEnd(msg.ev)
		// After onTurnEnd, which is what decides whether this really was the end of
		// the work: a queued message goes out only once nothing is left running.
		m.flushQueue()
		m.rebuild()
		cmds = append(cmds, waitEvent(m.drv.Events(), m.gen))

	case resumeMsg:
		cmds = append(cmds, m.applyResume(msg))
		m.rebuild()

	case streamClosedMsg:
		if msg.gen != m.gen {
			return m, nil // an old driver we deliberately swapped out
		}
		m.ended = true
		m.status = "session ended"
		// Nothing is left to answer, and an open panel would swallow every key.
		m.abandonAsk()
		m.abandonFleetAwait()
		m.appendEntry(entry{kind: eTurn, body: "──── session ended ────"})
		// Whatever was still queued will never be sent now; say so rather than
		// dropping it silently, which is the bug the queue exists to fix.
		m.reportUnsentQueue()
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

	case askMsg:
		// One socket, several different waits: a question blocks on a human, a
		// dispatch blocks on a whole local child process running a task, and the
		// fleet tools block on a remote engineer or on the fleet's own event
		// stream (Await).
		switch msg.p.Req.Tool {
		case mcp.ToolDispatch:
			m.startDispatch(msg.p)
		case mcp.ToolLaunchEngineer:
			m.startLaunchEngineer(msg.p)
		case mcp.ToolAwait:
			m.startAwait(msg.p)
		case mcp.ToolAnswerEngineer:
			m.startAnswerEngineer(msg.p)
		case mcp.ToolFleetStatus:
			m.startFleetStatus(msg.p)
		default:
			m.openAsk(msg.p)
		}
		m.rebuild()
		cmds = append(cmds, waitAsk(m.askReqs))

	case fleetMsg:
		m.ingestFleet(msg.ev)
		// A fleet event can be the last thing holding the queue, the same reason
		// childMsg flushes: nothing else is guaranteed to arrive after it.
		m.flushQueue()
		m.rebuild()
		if m.fleet != nil {
			cmds = append(cmds, waitFleet(m.fleet.Events()))
		}

	case childMsg:
		m.ingestChild(msg.ev)
		// A child event can be the last thing that happens in a run, and the queue
		// has to leave on it. Esc with a task running cancels the dispatches and
		// interrupts the parent: the parent's aborted turn reports first, while the
		// child is still shutting down, so the flush there is refused for an active
		// dispatch. Once the child finally reports, the parent is already idle and
		// no further driver event is coming — so without this call the queued
		// redirect would sit there forever, with an empty composer and no key that
		// releases it.
		m.flushQueue()
		m.rebuild()
		if m.dispatcher != nil {
			cmds = append(cmds, waitChild(m.dispatcher.Events()))
		}

	case askClosedMsg:
		// no more questions will arrive; nothing to re-arm
		return m, nil

	case tickMsg:
		m.now = time.Time(msg)
		m.spinFrame++ // animates the footer/header spinner; View() re-renders each tick
		m.expireDue()
		m.expireAsk()
		// A gate expiring on its own countdown can be the last thing holding the
		// queue, and nothing is guaranteed to arrive after it: the turn that raised
		// it has already reported, so without this the message would sit there
		// forever, with an empty composer and no key that releases it. The redraw
		// has to follow a send — this branch otherwise only rebuilds for a live
		// gate, which is exactly what just went away.
		flushed := m.flushQueue()
		if flushed || len(m.pending) > 0 || m.ask != nil {
			m.rebuild()
		}
		return m, tickCmd()
	}

	// Route remaining messages to the sub-components. A bracketed paste that was
	// not a path list arrives here: v2 delivers it as tea.PasteMsg rather than as
	// key runes, so it is not a tea.KeyPressMsg, none of the interception branches
	// above can claim it, and the textarea inserts the whole thing itself (newlines
	// included) — which is also why a pasted document can never be mistaken for an
	// Enter press. Keep any new interception keyed on the message type, not on "is
	// there text".
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	cmds = append(cmds, cmd)
	m.vp, cmd = m.vp.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

// handleGateKey processes controls while one or more permission gates are
// counting down, and reports whether it consumed the key. A false means the key
// belongs to the composer.
//
// These are chords rather than the bare a/s/p they used to be. The composer now
// sits under the countdown panel instead of being replaced by it, and next to a
// live text box a bare letter is not a command: typing the word "and" would have
// approved a tool and then paused the queue. ctrl+y/ctrl+x/ctrl+r are the free
// ones — none appear in the bubbles textarea DefaultKeyMap or in
// transcriptKeyMap, and ctrl+p/ctrl+n (composer line movement), ctrl+j
// (newline), ctrl+g (arm) and ctrl+c (quit) are all already spoken for.
//
// Each chord raises the Action a webview would send, rather than doing the work
// itself — the gate is named by the front item's tool_use id, not by "the front
// one", so the terminal and an HTTP client answer a gate through exactly the
// same code. Only the caller is called first: this function is only reached
// with at least one gate pending, which is what makes m.pending[0] safe here.
func (m *Model) handleGateKey(msg tea.KeyPressMsg) bool {
	frontID := func() string { return m.pending[0].p.Input.ToolUseID }
	switch msg.String() {
	case "ctrl+x": // stop/veto the front gate
		m.applyAction(GateDeny(frontID()), nil)
	case "ctrl+y": // approve the front gate immediately
		m.applyAction(GateAllow(frontID()), nil)
	case "ctrl+r": // pause / resume all countdowns
		// The negation, computed here: the chord is a toggle even though the
		// action is not, because the person pressing it is looking at the screen
		// and a stateless client is not.
		m.applyAction(GatePause(!m.paused), nil)
	default:
		return false
	}
	return true
}
