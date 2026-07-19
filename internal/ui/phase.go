package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/mcp"
)

// Phase is a stage in the supervised run.
type Phase int

const (
	PhasePlan     Phase = iota // interactive: user chats & builds a plan
	PhaseAutoRun               // armed: claude works, gates auto-approve
	PhaseComplete              // the plan is reported done; back to a normal chat
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

	// Kickoff sends kickoffPrompt once the session is up. Arming sets it — that
	// prompt is what starts the work. A *resumed* auto-run must not: the run is
	// already underway, and telling it "the plan is approved, begin implementing it"
	// would start it over. Opt-in, so the zero value is the harmless one.
	Kickoff bool
}

// Launcher starts a claude driver for a launch spec. For PhaseAutoRun the
// launcher enables hooks and the default permission mode; for PhasePlan it uses
// plan mode without hooks. ResumeID continues an existing session in either mode.
type Launcher func(ctx context.Context, spec LaunchSpec) (*driver.Driver, error)

// PlanSystemPrompt is appended to claude's system prompt during PLAN.
//
// It has to carry more than it looks like it does. acy no longer launches the plan
// phase with --permission-mode plan — that mode refuses to execute *any* MCP tool
// call ("Cannot call mcp__acy__AskUserQuestion while in plan mode"), even with
// --allowedTools, which would make acy's own question picker unreachable in the one
// phase where a human is actually sitting there to answer it. So PLAN runs in
// `default` mode over a read-only --tools registry, and this prompt supplies the
// planning contract that plan mode used to inject — plus the two things only acy
// can tell the model: it cannot start its own work, and a human ends this phase.
var PlanSystemPrompt = strings.Join([]string{
	"You are in the PLAN phase of always-click-yes (acy), which supervises this session.",
	"",
	"Research the request and produce an implementation plan. Do NOT implement it: the tools",
	"that would let you write anything are not in your registry, and attempting them wastes a turn.",
	"",
	"You cannot leave this phase. There is no ExitPlanMode tool here and no way for you to",
	"approve your own plan or start the work — do not look for one. A HUMAN ends the plan phase,",
	"by reading your plan and pressing Ctrl+G, which arms the run and resumes this session with",
	"full tools. That keystroke is the only thing that starts the work.",
	"",
	"Two tools exist for talking to that human:",
	"  - " + mcp.Qualified(mcp.ToolAsk) + " — put a real choice to them and block for an answer.",
	"    Use it for any genuine fork: an ambiguous requirement, a decision between approaches.",
	"    Asking in prose instead surfaces no prompt and gets you no reply.",
	"  - " + mcp.Qualified(mcp.ToolPlan) + " — hand over the finished plan. Call it exactly once,",
	"    when the plan is done. It does not exit the phase; it only shows them the plan.",
	"",
	"After presenting the plan, STOP. Do not re-plan, do not summarize it again, and do not ask",
	"whether to proceed — nobody is waiting to answer that. If they want changes, they will say so.",
}, "\n")

// AutoRunSystemPrompt is appended during AUTO-RUN. The point of acy is that the
// human has walked away, so the model must know two things: a question is a last
// resort that will time out rather than wait, and the STATUS line it ends each
// reply with is what drives the run — acy reads it in this same session instead of
// paying a second process to judge completion.
var AutoRunSystemPrompt = strings.Join([]string{
	"You are in the AUTO-RUN phase of always-click-yes (acy). Your plan was approved; work through",
	"it to completion. Permission prompts are auto-approved on a countdown, so nobody is vetting",
	"each step.",
	"",
	"The human has very likely walked away. " + mcp.Qualified(mcp.ToolAsk) + " still exists, but a",
	"question auto-skips after the countdown and returns 'proceed with your best judgment'. So use",
	"it only where a wrong guess would be expensive or hard to undo. Otherwise decide, proceed, and",
	"say what you assumed.",
	"",
	"End EVERY reply with a line that is exactly one of:",
	"  STATUS: DONE      — every step of the approved plan is complete",
	"  STATUS: CONTINUE  — work remains",
	"acy reads that line each time you stop. CONTINUE (or a missing line) gets you nudged to keep",
	"going; DONE ends the run and hands your work back to the human to review. Do not claim DONE",
	"until it is true.",
}, "\n")

// verdict classifies the STATUS sentinel an auto-run turn ends with.
type verdict int

const (
	verdictUnclear  verdict = iota // no sentinel found
	verdictDone                    // every step reported complete
	verdictContinue                // work remains
)

// parseVerdict scans a turn's text for the STATUS sentinel. DONE wins ties.
func parseVerdict(text string) verdict {
	up := strings.ToUpper(text)
	if strings.Contains(up, "STATUS: DONE") || strings.Contains(up, "STATUS:DONE") {
		return verdictDone
	}
	if strings.Contains(up, "STATUS: CONTINUE") || strings.Contains(up, "STATUS:CONTINUE") {
		return verdictContinue
	}
	return verdictUnclear
}

// doneCheckPrompt asks the working session itself whether the plan is done. It is
// auto-sent when an idle turn carries no STATUS sentinel, and preloaded for the
// user to send by hand once the auto-round budget is spent.
const doneCheckPrompt = "Have we completed every step of the approved plan? " +
	"If every step is done, reply with exactly: STATUS: DONE. " +
	"Otherwise keep working on the remaining steps, and end your reply with STATUS: CONTINUE."

// kickoffPrompt is injected when the user arms the run.
const kickoffPrompt = "The plan is approved. Begin implementing it now, working " +
	"through every step to completion."

// continuePrompt nudges the working session onward after it stops with work still
// remaining (a STATUS: CONTINUE turn).
const continuePrompt = "Keep working through the remaining steps of the approved plan. " +
	"When every step is complete, stop and end your reply with exactly: STATUS: DONE."

// maxAutoRounds caps how many times the loop will auto-nudge the working session
// before handing control back to the user.
const maxAutoRounds = 10

// --- launch plumbing ---

type driverReadyMsg struct {
	drv     *driver.Driver
	phase   Phase
	kickoff bool
}

// resumedAutoRun reports an armed run coming back to life rather than starting.
// Arming is the only launch that kicks off work, so an auto-run arriving without a
// kickoff is one that was already underway.
func (m driverReadyMsg) resumedAutoRun() bool {
	return m.phase == PhaseAutoRun && !m.kickoff
}

type errMsg struct{ err error }

func launchCmd(ctx context.Context, l Launcher, spec LaunchSpec) tea.Cmd {
	return func() tea.Msg {
		d, err := l(ctx, spec)
		if err != nil {
			return errMsg{err}
		}
		return driverReadyMsg{drv: d, phase: spec.Phase, kickoff: spec.Kickoff}
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

// sendInput dispatches the current input box to claude.
func (m *Model) sendInput() {
	text := strings.TrimSpace(m.input.Value())
	if text == "" || m.ended || m.drv == nil || m.processing {
		return // ignore sends while a turn is in flight (Esc to interject first)
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

// onTurnEnd runs when a turn completes. In auto-run the working session judges its
// own completion: the system prompt has it end every reply with a STATUS sentinel,
// and this reads it — no second process, no second context. The session that did
// the work already knows what it has done.
func (m *Model) onTurnEnd(ev driver.Event) {
	if !ev.IsTurnEnd() || m.phase != PhaseAutoRun || len(m.pending) > 0 {
		return
	}
	if m.interrupted {
		m.interrupted = false
		m.status = "interrupted — type to redirect, then Enter"
		return
	}
	m.checkCompletion()
}

// checkCompletion applies the auto-run loop to the last assistant turn: finish on
// DONE, otherwise nudge the same session onward.
func (m *Model) checkCompletion() {
	switch parseVerdict(m.turnText) {
	case verdictDone:
		m.markComplete()
	case verdictContinue:
		m.nudge(continuePrompt)
	default: // no sentinel: ask the session itself whether it is done
		m.nudge(doneCheckPrompt)
	}
}

// nudge keeps the run moving by prompting the working session. Each nudge spends
// one auto-round; past maxAutoRounds the loop stops driving itself and hands the
// done-check to the user instead.
func (m *Model) nudge(prompt string) {
	m.rounds++
	defer m.persist() // rounds moved; a crash must not hand the run a fresh budget
	if m.rounds > maxAutoRounds {
		alog.Printf("autorun: round cap reached (%d)", maxAutoRounds)
		m.appendEntry(entry{kind: eWarn, body: fmt.Sprintf(
			"still not done after %d auto-rounds — pausing; press Enter to verify manually", maxAutoRounds)})
		m.preloadDoneCheck()
		return
	}
	alog.Printf("autorun: nudge (round=%d)", m.rounds)
	m.turnText = ""
	if m.drv != nil {
		_ = m.drv.Send(prompt)
	}
	m.processing = true
	m.status = "working…"
	m.appendEntry(entry{kind: eYou, body: prompt})
}

// capturePlan makes sure the run is armed with a plan — the record of what the user
// approved, shown on resume and persisted in the snapshot.
//
// planBody is normally set from an ExitPlanMode tool call — but that tool does not
// exist in `claude -p`'s registry (see AGENTS.md), so in practice it never fires
// and planBody stays empty. The plan is still right there: it is the last thing the
// assistant said, which is exactly what the user just read and approved by pressing
// Ctrl+G. Use it.
func (m *Model) capturePlan() {
	if strings.TrimSpace(m.planBody) != "" {
		return
	}
	m.planBody = strings.TrimSpace(m.turnText)
}

// markComplete transitions the run to the COMPLETE phase: the session stays up,
// and the composer goes back to being a normal chat so the user can vet the work.
func (m *Model) markComplete() {
	m.phase = PhaseComplete
	m.status = "complete — vet the work below"
	total := m.totalCost()
	alog.Printf("phase: COMPLETE (cost=$%.4f, billing=%s)", total, m.billingNote())
	m.appendEntry(entry{kind: eComplete, body: fmt.Sprintf(
		"✅ plan complete · $%.4f total · %s — chat below to vet the work", total, m.billingNote())})
	m.persist()
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

	// turnText is the working session's last message — what checkCompletion reads.
	// A launch normally starts a fresh turn and has none. A *resumed* auto-run is
	// the exception: the replay went to the trouble of recovering the final
	// assistant turn precisely so the completion check below has something to read,
	// and clearing it would erase a STATUS: DONE the session may already have said.
	if !msg.resumedAutoRun() {
		m.turnText = ""
	}

	// Any question still on screen belongs to the driver we just stopped. Its mcp
	// child died with that process group, so nothing is listening — but resolve it
	// anyway rather than dropping it, and take the panel down before it starts
	// eating keystrokes meant for the new session.
	m.abandonAsk()
	alog.Printf("phase: %s (gen=%d)", msg.phase, m.gen)

	cmds := []tea.Cmd{waitEvent(m.drv.Events(), m.gen)}
	if msg.phase == PhaseAutoRun {
		m.preloaded = false
		if msg.kickoff {
			// Arming: this prompt is what sets the work going.
			_ = m.drv.Send(kickoffPrompt)
			m.processing = true
			m.status = "working…"
			m.appendEntry(entry{kind: eYou, body: kickoffPrompt})
		} else {
			// Resumed mid-run. A resumed auto-run *is* an idle auto-run, so it rejoins
			// the loop at the point every turn already ends at: read the last thing the
			// session said. If the work finished before acy died, the DONE sentinel is
			// sitting right there; otherwise the nudge picks the run back up.
			m.checkCompletion()
		}
	}
	m.persist()
	m.rebuild()
	return tea.Batch(cmds...)
}
