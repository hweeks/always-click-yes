package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/driver"
)

// Phase is a stage in the supervised run.
type Phase int

const (
	PhasePlan     Phase = iota // interactive: user chats & builds a plan
	PhaseAutoRun               // armed: claude works, gates auto-approve
	PhaseComplete              // the plan is reported done
)

func (p Phase) String() string {
	switch p {
	case PhasePlan:
		return "PLAN"
	case PhaseAutoRun:
		return "AUTO-RUN"
	case PhaseComplete:
		return "COMPLETE"
	default:
		return "?"
	}
}

// Launcher starts a claude driver for a phase. For PhaseAutoRun the resumeID is
// the session captured during planning; the launcher enables hooks and the
// default permission mode. For PhasePlan it uses plan mode without hooks.
type Launcher func(ctx context.Context, phase Phase, resumeID string) (*driver.Driver, error)

// doneCheckPrompt is preloaded when a run goes idle. Its sentinel reply drives
// the loop-or-complete decision.
const doneCheckPrompt = "Have we completed every step of the approved plan? " +
	"If every step is done, reply with exactly: STATUS: DONE. " +
	"Otherwise reply STATUS: CONTINUE and keep working on the remaining steps."

// kickoffPrompt is injected when the user arms the run.
const kickoffPrompt = "The plan is approved. Begin implementing it now, working " +
	"through every step to completion."

// --- launch plumbing ---

type driverReadyMsg struct {
	drv   *driver.Driver
	phase Phase
}
type errMsg struct{ err error }

func launchCmd(ctx context.Context, l Launcher, phase Phase, resumeID string) tea.Cmd {
	return func() tea.Msg {
		d, err := l(ctx, phase, resumeID)
		if err != nil {
			return errMsg{err}
		}
		return driverReadyMsg{drv: d, phase: phase}
	}
}

// verdict classifies a done-check reply.
type verdict int

const (
	verdictNone verdict = iota
	verdictDone
	verdictContinue
)

func parseVerdict(text string) verdict {
	up := strings.ToUpper(text)
	// DONE wins if both somehow appear.
	if strings.Contains(up, "STATUS: DONE") || strings.Contains(up, "STATUS:DONE") {
		return verdictDone
	}
	if strings.Contains(up, "STATUS: CONTINUE") || strings.Contains(up, "STATUS:CONTINUE") {
		return verdictContinue
	}
	return verdictNone
}

// preloadDoneCheck drops the completion prompt into the input, ready to send.
func (m *Model) preloadDoneCheck() {
	m.input.SetValue(doneCheckPrompt)
	m.input.CursorEnd()
	m.preloaded = true
	m.status = "idle — press Enter to verify completion"
	m.appendEntry(entry{kind: eTurn, body: "──── idle · press Enter to ask “are we done?” ────"})
}

// sendInput dispatches the current input box to claude. If the preloaded
// done-check is being sent verbatim, it marks the next turn as a verdict turn.
func (m *Model) sendInput() {
	text := strings.TrimSpace(m.input.Value())
	if text == "" || m.ended || m.drv == nil || m.processing {
		return // ignore sends while a turn is in flight (Esc to interject first)
	}
	if m.preloaded && text == doneCheckPrompt {
		m.awaitingVerdict = true
	}
	m.preloaded = false
	m.interrupted = false
	m.turnText = ""
	_ = m.drv.Send(text)
	m.processing = true
	m.status = "working…"
	m.appendEntry(entry{kind: eYou, body: text})
	m.input.Reset()
}

// interject aborts the in-flight turn so the user can redirect. Returns false if
// there's nothing to interrupt.
func (m *Model) interject() bool {
	if m.drv == nil || !m.processing {
		return false
	}
	m.interrupted = true
	_ = m.drv.Interrupt()
	m.status = "interrupting…"
	m.appendEntry(entry{kind: eWarn, body: "✋ interrupting — the turn will stop; type to redirect"})
	return true
}

// onTurnEnd runs when a turn completes. In auto-run it either reads a pending
// verdict or preloads the next completion check.
func (m *Model) onTurnEnd(ev driver.Event) {
	if !ev.IsTurnEnd() || m.phase != PhaseAutoRun || len(m.pending) > 0 {
		return
	}
	if m.interrupted {
		m.interrupted = false
		m.awaitingVerdict = false
		m.status = "interrupted — type to redirect, then Enter"
		return
	}
	if m.awaitingVerdict {
		m.awaitingVerdict = false
		if parseVerdict(m.turnText) == verdictDone {
			m.phase = PhaseComplete
			m.status = "complete"
			alog.Printf("phase: COMPLETE (cost=$%.4f)", m.cost)
			m.appendEntry(entry{kind: eComplete, body: fmt.Sprintf("✅ plan complete · $%.4f total", m.cost)})
			return
		}
		// CONTINUE or unclear: the model kept working and is idle again — re-ask.
	}
	m.preloadDoneCheck()
}

// onDriverReady swaps in a freshly launched driver for a new phase.
func (m *Model) onDriverReady(msg driverReadyMsg) tea.Cmd {
	if m.drv != nil {
		m.drv.Stop() // stale generation; its events will be ignored
	}
	m.drv = msg.drv
	m.phase = msg.phase
	m.gen++
	m.turnText = ""
	alog.Printf("phase: %s (gen=%d)", msg.phase, m.gen)

	cmds := []tea.Cmd{waitEvent(m.drv.Events(), m.gen)}
	if msg.phase == PhaseAutoRun {
		m.awaitingVerdict = false
		m.preloaded = false
		_ = m.drv.Send(kickoffPrompt)
		m.processing = true
		m.status = "working…"
		m.appendEntry(entry{kind: eYou, body: kickoffPrompt})
	}
	m.rebuild()
	return tea.Batch(cmds...)
}
