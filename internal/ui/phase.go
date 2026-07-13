package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/judge"
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

// LaunchSpec describes the claude session a Launcher should start.
type LaunchSpec struct {
	Phase    Phase  // PhasePlan (interactive, ungated) or PhaseAutoRun (hooks + gated)
	ResumeID string // --resume <session-id>; set to continue a captured/prior session
	Model    string // --model override for this launch; empty = launcher default
}

// Launcher starts a claude driver for a launch spec. For PhaseAutoRun the
// launcher enables hooks and the default permission mode; for PhasePlan it uses
// plan mode without hooks. ResumeID continues an existing session in either mode.
type Launcher func(ctx context.Context, spec LaunchSpec) (*driver.Driver, error)

// doneCheckPrompt is preloaded as a manual fallback when the independent judge is
// unavailable or inconclusive. Its sentinel reply drives the loop-or-complete
// decision within the working session.
const doneCheckPrompt = "Have we completed every step of the approved plan? " +
	"If every step is done, reply with exactly: STATUS: DONE. " +
	"Otherwise reply STATUS: CONTINUE and keep working on the remaining steps."

// kickoffPrompt is injected when the user arms the run.
const kickoffPrompt = "The plan is approved. Begin implementing it now, working " +
	"through every step to completion."

// continuePrompt nudges the working session after the independent judge finds the
// plan unfinished. The judge's notes are appended when present.
const continuePrompt = "An independent reviewer determined the plan is not yet " +
	"complete. Keep working through the remaining steps to completion."

// maxAutoRounds caps how many times a CONTINUE verdict will auto-nudge the working
// session before handing control back to the user.
const maxAutoRounds = 10

// judgeTimeout bounds a single independent judge session so a hung check falls
// back to the manual verify path instead of stalling the run.
const judgeTimeout = 2 * time.Minute

// --- launch plumbing ---

type driverReadyMsg struct {
	drv   *driver.Driver
	phase Phase
}
type errMsg struct{ err error }

func launchCmd(ctx context.Context, l Launcher, spec LaunchSpec) tea.Cmd {
	return func() tea.Msg {
		d, err := l(ctx, spec)
		if err != nil {
			return errMsg{err}
		}
		return driverReadyMsg{drv: d, phase: spec.Phase}
	}
}

// verdictMsg carries the result of an independent judge session back into the
// event loop.
type verdictMsg struct {
	gen       int
	v         judge.Verdict
	rationale string
	cost      float64 // the judge's own session spend, to fold into the tally
	err       error
}

// verifyCmd runs the injected judge off the event loop and reports its verdict.
// The judge is bounded by judgeTimeout so a hung check degrades to manual verify.
func verifyCmd(ctx context.Context, j JudgeFunc, gen int, plan, lastMsg string) tea.Cmd {
	return func() tea.Msg {
		cctx, cancel := context.WithTimeout(ctx, judgeTimeout)
		defer cancel()
		res, err := j(cctx, plan, lastMsg)
		return verdictMsg{gen: gen, v: res.Verdict, rationale: res.Text, cost: res.CostUSD, err: err}
	}
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
	if text == "" || m.ended || m.drv == nil || m.processing || m.verifying {
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

// onTurnEnd runs when a turn completes. In auto-run it hands off to the
// independent judge (or, as a fallback, the manual done-check). It returns a
// command to run when a judge session must be launched.
func (m *Model) onTurnEnd(ev driver.Event) tea.Cmd {
	if !ev.IsTurnEnd() || m.phase != PhaseAutoRun || len(m.pending) > 0 {
		return nil
	}
	if m.interrupted {
		m.interrupted = false
		m.awaitingVerdict = false
		m.status = "interrupted — type to redirect, then Enter"
		return nil
	}
	if m.awaitingVerdict {
		// Manual fallback: the done-check was answered by the working session.
		m.awaitingVerdict = false
		if judge.ParseVerdict(m.turnText) == judge.VerdictDone {
			m.markComplete()
			return nil
		}
		// CONTINUE or unclear: the model kept working and is idle again — re-ask.
		m.preloadDoneCheck()
		return nil
	}
	// Fresh idle: hand off to the independent judge if one is wired, otherwise
	// fall back to the manual done-check.
	if m.judge != nil {
		return m.startVerification()
	}
	m.preloadDoneCheck()
	return nil
}

// markComplete transitions the run to the COMPLETE phase.
func (m *Model) markComplete() {
	m.phase = PhaseComplete
	m.status = "complete"
	total := m.totalCost()
	alog.Printf("phase: COMPLETE (cost=$%.4f, billing=%s)", total, m.billingNote())
	m.appendEntry(entry{kind: eComplete, body: fmt.Sprintf(
		"✅ plan complete · $%.4f total · %s", total, m.billingNote())})
}

// startVerification launches an independent judge session to decide whether the
// plan is complete, feeding it the approved plan and the working session's last
// message.
func (m *Model) startVerification() tea.Cmd {
	m.verifying = true
	m.preloaded = false
	m.status = "verifying completion (independent session)…"
	m.appendEntry(entry{kind: eTurn, body: "──── idle · asking an independent session “are we done?” ────"})
	alog.Printf("judge: verifying (round=%d)", m.rounds)
	return verifyCmd(m.ctx, m.judge, m.gen, m.planBody, m.turnText)
}

// onVerdict applies an independent judge's decision: complete on DONE, auto-nudge
// the working session on CONTINUE (up to maxAutoRounds), and fall back to a manual
// check on error or an unclear verdict.
func (m *Model) onVerdict(msg verdictMsg) {
	if msg.gen != m.gen {
		return // stale generation from a swapped-out driver
	}
	m.verifying = false
	m.costSettled += msg.cost // the judge ran in its own process; bank what it spent

	if msg.err != nil {
		alog.Printf("judge: error: %v", msg.err)
		m.appendEntry(entry{kind: eWarn, body: "judge unavailable: " + msg.err.Error() + " — press Enter to verify manually"})
		m.preloadDoneCheck()
		return
	}

	switch msg.v {
	case judge.VerdictDone:
		m.appendEntry(entry{kind: eGood, body: "⚖ independent session: DONE"})
		m.markComplete()
	case judge.VerdictContinue:
		m.rounds++
		if m.rounds > maxAutoRounds {
			m.appendEntry(entry{kind: eWarn, body: fmt.Sprintf(
				"⚖ still not done after %d auto-rounds — pausing; press Enter to verify manually", maxAutoRounds)})
			m.preloadDoneCheck()
			return
		}
		note := strings.TrimSpace(msg.rationale)
		body := "⚖ independent session: CONTINUE"
		if note != "" {
			body += " · " + firstLine(note)
		}
		m.appendEntry(entry{kind: eWarn, body: body})
		text := continuePrompt
		if note != "" {
			text += "\n\nReviewer notes: " + note
		}
		m.turnText = ""
		if m.drv != nil {
			_ = m.drv.Send(text)
		}
		m.processing = true
		m.status = "working…"
		m.appendEntry(entry{kind: eYou, body: continuePrompt})
	default: // unclear
		m.appendEntry(entry{kind: eWarn, body: "⚖ independent session gave no clear verdict — press Enter to verify manually"})
		m.preloadDoneCheck()
	}
}

// onDriverReady swaps in a freshly launched driver for a new phase.
func (m *Model) onDriverReady(msg driverReadyMsg) tea.Cmd {
	if m.drv != nil {
		m.drv.Stop() // stale generation; its events will be ignored
		m.settleCost()
	}
	m.drv = msg.drv
	m.phase = msg.phase
	m.gen++
	m.turnText = ""
	// Any question still on screen belongs to the driver we just stopped; its
	// tool_use id is meaningless to the new one, and the panel would otherwise
	// keep eating every keystroke.
	m.ask = nil
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
