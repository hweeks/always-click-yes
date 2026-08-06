package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

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
}

// Launcher starts a claude driver for a launch spec. For PhaseAutoRun the
// launcher enables hooks and the default permission mode; for PhasePlan it uses
// plan mode without hooks. ResumeID continues an existing session in either mode.
type Launcher func(ctx context.Context, spec LaunchSpec) (*driver.Driver, error)

// ParentSystemPrompt is appended to the supervising session's system prompt.
//
// There is exactly one of these now, where there used to be two. Arming no
// longer launches a new process — it flips a phase on the session already
// running — so a prompt chosen at startup has to serve the whole run, and the
// difference between the phases is carried by the tools instead: Dispatch
// refuses, in its own words, until the run is armed.
//
// It is about a third the length of the plan prompt it replaces, because most
// of that prompt was describing constraints that are now structurally true.
// "Do NOT implement it" needs no words when there is no tool that could; "you
// cannot leave this phase" needs none when the tool that would says so itself.
var ParentSystemPrompt = strings.Join([]string{
	"You are the lead on a supervised run. You have Read, Grep and Glob: you can understand this",
	"codebase, and you cannot change it.",
	"",
	"Work happens by delegation. " + mcp.Qualified(mcp.ToolDispatch) + " hands one task to a fresh",
	"engineer with the full toolset and blocks until they report back. They begin with no memory of",
	"this conversation, so a task has to stand alone: what to change, where, and how they will know",
	"it worked. One task per call, scoped so that a report can honestly say \"completed\". Read each",
	"report before you dispatch the next one.",
	"",
	mcp.Qualified(mcp.ToolPlan) + " shows the human a finished plan.",
	mcp.Qualified(mcp.ToolAsk) + " puts a real choice to them and blocks for an answer.",
	mcp.Qualified(mcp.ToolFinish) + " ends the run, once the work is done and you have seen it verified.",
}, "\n")

// ArchSystemPrompt is appended to the architect's system prompt in arch mode,
// in place of ParentSystemPrompt.
//
// The architect reads the codebase the same way the parent does — Read, Grep,
// Glob, and nothing that changes it — but delegates whole tickets to remote
// engineers instead of local children. LaunchEngineer starts a full acy
// instance on a fleet host: it plans its own subtasks in its own worktree and
// ends by opening a PR, so a brief has to stand completely alone, the way
// Dispatch's instruction does, scoped to one PR of work. Await is the
// architect's main loop rather than a blocking call, so the prompt says so
// plainly: launch to capacity, then Await, then react.
var ArchSystemPrompt = strings.Join([]string{
	"You are the architect of a fleet run. You have Read, Grep and Glob: you can understand this",
	"codebase, and you cannot change it.",
	"",
	"Work happens by delegation to remote engineers. " + mcp.Qualified(mcp.ToolLaunchEngineer) + " starts a full",
	"engineer instance on a fleet host: it plans its own subtasks in its own worktree and ends by opening a",
	"PR, so a brief must stand completely alone — ticket-sized, one PR of work. Launch up to capacity, then",
	mcp.Qualified(mcp.ToolAwait) + " — your main loop. A result means read it and launch the next ticket; a",
	"question means " + mcp.Qualified(mcp.ToolAnswerEngineer) + " from the plan — never leave a question waiting.",
	mcp.Qualified(mcp.ToolDispatch) + " still runs small local read/verify/fix jobs in this checkout.",
	"",
	"The ticket board is this run's memory. Once the plan is approved, " + mcp.Qualified(mcp.ToolCreateTicket) + " for",
	"each unit of work — one ticket per PR-sized piece — before launching any engineers; the brief you write",
	"becomes the engineer's whole work order. " + mcp.Qualified(mcp.ToolReadTickets) + " it on a resume, and after",
	"every merge. Keep statuses current with " + mcp.Qualified(mcp.ToolUpdateTicket) + " at every transition —",
	"launch to in-progress, PR opened to in-review, merged once the human merges it, or blocked with a note",
	"the moment an engineer is stuck. A resumed run has no memory of this conversation; the board is how it",
	"learns where it left off.",
	"",
	mcp.Qualified(mcp.ToolPlan) + " shows the human a finished plan.",
	mcp.Qualified(mcp.ToolAsk) + " puts a real choice to them and blocks for an answer.",
	mcp.Qualified(mcp.ToolFinish) + " ends the run, once the work is done and you have seen it verified.",
}, "\n")

// ChildSystemPrompt is what a dispatched child runs under.
//
// It is short because almost everything the old auto-run prompt spelled out is
// now structurally true instead of instructed: there is no STATUS sentinel to
// remember (--json-schema validates the report), no completion loop to explain,
// and no plan to stay inside. What is left is the one thing a child cannot
// discover for itself — that its transcript is about to be thrown away, so the
// report is the only thing that will ever be read.
var ChildSystemPrompt = strings.Join([]string{
	"You are implementing one task for a supervised run. Nobody is watching: tool permissions",
	"auto-approve on a countdown, so decide and proceed.",
	"",
	"Do the task, verify it yourself — run the tests, read the file back — and return the report.",
	"That report is the only thing your caller will ever see: your transcript, your reasoning and",
	"this session all disappear when you finish. An honest 'partial' with a clear reason is worth",
	"more than a 'completed' that isn't, because the caller will build on whatever you tell them.",
	"",
	"Do not take on work beyond your task. Anything else you notice goes in followups.",
}, "\n")

// kickoffPrompt is sent when the user arms the run. Unlike before, it goes to
// the session already in front of them rather than to a freshly resumed process.
const kickoffPrompt = "The plan is approved. Begin now: dispatch the work one task at a time, " +
	"reading each report before the next. Call Finish when it is all done and verified."

// archKickoffPrompt is kickoffPrompt's fleet-loop counterpart. kickoffPrompt's
// "dispatch the work one task at a time" describes the wrong tool once the
// session has a fleet to run instead of a queue of local children.
const archKickoffPrompt = "The plan is approved. Begin now: launch engineers for the first tickets up to " +
	"capacity, then Await. Keep the pipeline full — react to each result by launching the next ticket, and " +
	"answer any question immediately. Call Finish when every ticket is merged-or-accounted-for."

// kickoffPromptFor picks the phase-appropriate kickoff message: hasFleet is
// Config.Fleet != nil, and arming has to say the right thing about which tool
// starts the work.
func kickoffPromptFor(hasFleet bool) string {
	if hasFleet {
		return archKickoffPrompt
	}
	return kickoffPrompt
}

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

// beginTurn marks a turn in flight: the header and working indicator flip on,
// and the elapsed clock starts.
func (m *Model) beginTurn() {
	m.processing = true
	m.turnStart = time.Now()
	m.status = "working…"
}

// busy reports whether the session has something in flight that a new user turn
// would land on top of: its own turn, a permission gate waiting to be answered
// (the turn that raised it is still open), or a delegated task the parent is
// blocked on.
func (m Model) busy() bool {
	return m.processing ||
		len(m.pending) > 0 ||
		(m.dispatcher != nil && m.dispatcher.Active() > 0)
}

// sendInput dispatches the current input box to claude, or queues it when the
// session is busy. It is the composer's half of the job and nothing else: read
// the box, submit what was in it, and empty the box if that worked.
//
// The submitting itself lives in submitText, because the composer is only one
// of the places text now comes from — a Submit action carries its own — and a
// second copy of "queue it if busy, otherwise send it and start a turn" is
// exactly the pair that would drift.
func (m *Model) sendInput() {
	if m.submitText(m.input.Value()).Accepted {
		m.clearComposer()
	}
}

// submitText sends one message to claude, or queues it when the session is
// busy.
//
// It used to drop it: `m.processing` was a refusal, and in an armed run
// something is in flight nearly all the time, so Enter did nothing and said
// nothing about it. The only genuine refusals left are the ones with nowhere to
// send to at all — and those are refusals rather than silence now, because a
// caller that is not a person watching a screen has no other way to find out.
func (m *Model) submitText(text string) ActionResult {
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return rejected("nothing to send")
	case m.ended:
		return rejected("the session has ended")
	case m.drv == nil:
		return rejected("no session is running")
	}
	if m.busy() {
		m.queued = append(m.queued, text)
		m.appendEntry(entry{kind: eQueued, body: text})
		return accepted("queued until the session falls idle")
	}
	m.interrupted = false
	m.turnText = ""
	_ = m.drv.Send(text)
	m.beginTurn()
	m.appendEntry(entry{kind: eYou, body: text})
	return accepted("sent")
}

// flushQueue sends everything typed while the session was busy, the moment it
// goes idle.
//
// One turn carrying every queued message, never one turn each: a turn re-bills
// the entire accumulated context (the measurement in AGENTS.md that this whole
// architecture exists to shrink), so N separate sends pay for the conversation N
// times over to deliver text the model will read in one go regardless.
//
// This is also, for free, the Esc/interject path: Esc aborts the turn, the
// aborted turn's `result` event lands here, and the queued message goes out as
// the redirect — the same code, without a second way to send.
//
// It reports whether it actually sent, because a send writes a transcript entry
// and starts a turn: a caller that only redraws under some other condition (the
// tick, which redraws for a live gate) would otherwise leave the user's own
// message off screen until something unrelated happened to redraw.
func (m *Model) flushQueue() bool {
	if len(m.queued) == 0 || m.ended || m.drv == nil || m.busy() {
		return false
	}
	text := strings.Join(m.queued, "\n\n")
	m.interrupted = false
	m.turnText = ""
	_ = m.drv.Send(text)
	m.appendEntry(entry{kind: eYou, body: text})
	m.beginTurn()
	m.queued = nil
	return true
}

// reportUnsentQueue prints anything still queued back into the transcript when
// the session ends under it. The messages are gone as far as claude is
// concerned; the least acy can do is leave them somewhere the user can copy
// them out of rather than swallowing what they typed.
func (m *Model) reportUnsentQueue() {
	if len(m.queued) == 0 {
		return
	}
	m.appendEntry(entry{kind: eWarn, body: fmt.Sprintf(
		"⚠ the session ended with %s never sent — copy anything you still want:\n\n%s",
		plural(len(m.queued), "queued message"), strings.Join(m.queued, "\n\n"))})
	m.queued = nil
}

// plural renders "1 thing" / "2 things".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// interject aborts the in-flight turn so the user can redirect. Returns false if
// there's nothing to interrupt.
func (m *Model) interject() bool {
	if m.drv == nil || !m.processing {
		return false
	}
	// Children first. The parent is blocked on a tool_result while a task runs,
	// so interrupting only the parent would leave an orphan burning tokens on
	// work whose answer now has nowhere to go — and cancelling a task is what
	// unblocks the parent's turn in the first place.
	m.cancelDispatches("interrupted by the user")
	m.interrupted = true
	_ = m.drv.Interrupt()
	m.status = "interrupting…"
	m.appendEntry(entry{kind: eWarn, body: "✋ interrupting — the turn will stop; type to redirect"})
	return true
}

// onTurnEnd runs when a turn completes.
//
// It no longer drives anything. The old loop read a STATUS sentinel out of the
// turn's text and, on anything short of DONE, sent another prompt — up to ten
// more full-context turns per run, each one re-billing the entire accumulated
// conversation just to ask "are you done yet?". That loop existed because a
// sentinel can be missed. A tool call cannot: the run ends when the session
// calls Finish, and if it stops without doing so the right response is a human,
// not another billed turn.
func (m *Model) onTurnEnd(ev driver.Event) {
	if !ev.IsTurnEnd() || m.phase != PhaseAutoRun || len(m.pending) > 0 {
		return
	}
	if m.interrupted {
		m.interrupted = false
		m.status = "interrupted — type to redirect, then Enter"
		return
	}
	// A task still running is the normal case: the parent is blocked on its
	// report and will carry on by itself when it arrives.
	if m.dispatcher != nil && m.dispatcher.Active() > 0 {
		m.status = "waiting on a task"
		return
	}
	m.status = "idle — no task running · type to continue, or /done to finish"
	m.appendEntry(entry{kind: eTurn, body: "──── idle · nothing running · type to continue ────"})
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

// onDriverReady swaps in a freshly launched driver.
//
// Much smaller than it was, because arming no longer comes through here. A
// launch is now only ever a cold start or a /resume — never a phase change —
// so there is no kickoff to send and no completion state to preserve across it.
func (m *Model) onDriverReady(msg driverReadyMsg) tea.Cmd {
	if m.drv != nil {
		m.drv.Stop() // stale generation; its events will be ignored
		m.settleCost()
	}
	m.drv = msg.drv
	m.phase = msg.phase
	m.gen++
	m.turnText = ""

	// Any question still on screen belongs to the driver we just stopped. Its mcp
	// child died with that process group, so nothing is listening — but resolve it
	// anyway rather than dropping it, and take the panel down before it starts
	// eating keystrokes meant for the new session.
	m.abandonAsk()
	m.abandonFleetAwait()
	alog.Printf("phase: %s (gen=%d)", msg.phase, m.gen)

	cmds := []tea.Cmd{waitEvent(m.drv.Events(), m.gen)}

	// A launch into AUTO-RUN can only be a restored run: arming no longer comes
	// through here. Send exactly one prompt to pick the work back up.
	//
	// Deleting the nudge loop deleted this with it, and a crashed run stopped
	// being resumable at all — it came back armed and then sat there forever.
	// The loop's fault was firing after every idle turn; firing once, because a
	// human explicitly asked to resume, is a different thing.
	if msg.phase == PhaseAutoRun {
		prompt := m.resumePrompt()
		_ = m.drv.Send(prompt)
		m.beginTurn()
		m.appendEntry(entry{kind: eYou, body: prompt})
		m.interruptedTasks = nil
		m.resumedEngineers = nil
	}

	m.persist()
	m.rebuild()
	return tea.Batch(cmds...)
}

// arm is Ctrl+G: the human has read the plan and approved it.
//
// It used to spawn a second claude process with --resume, which meant a second
// system prompt, a second cache warm-up, and a window in which a run had a
// session id but no driver. Now that the parent's tools are the same in both
// phases there is nothing to relaunch: the phase is a fact about what acy will
// allow, not about what process is running. Dispatch starts refusing to refuse.
func (m *Model) arm() {
	if m.drv == nil {
		// Ctrl+G already checks this, but arming is the one action that must not
		// half-happen: flipping the phase without sending the kickoff would leave
		// a run that looks armed and is doing nothing.
		m.appendEntry(entry{kind: eWarn, body: "cannot arm yet — no session is running"})
		return
	}
	m.capturePlan()
	m.phase = PhaseAutoRun
	m.planReady = false
	m.interrupted = false
	alog.Printf("phase: AUTO-RUN (armed in place, gen=%d)", m.gen)
	m.appendEntry(entry{kind: eGood, body: "▶ armed — delegating from here; Esc stops a running task"})

	prompt := kickoffPromptFor(m.fleet != nil)
	_ = m.drv.Send(prompt)
	m.beginTurn()
	m.appendEntry(entry{kind: eYou, body: prompt})
	m.persist()
}

// finish ends the run because the session called Finish. The session stays up:
// the point is that the human vets the work in the very session that did it.
func (m *Model) finish(outcome, summary string) {
	if m.phase == PhaseComplete {
		return
	}
	m.phase = PhaseComplete
	m.finishOutcome = outcome
	m.finishSummary = summary
	m.status = "complete — vet the work below"
	if outcome == "abandoned" {
		m.status = "abandoned — see the summary below"
	}
	total := m.grandTotalCost()
	alog.Printf("phase: COMPLETE outcome=%s cost=$%.4f billing=%s", outcome, total, m.billingNote())

	body := fmt.Sprintf("✅ run %s · $%.4f total · %s", outcome, total, m.billingNote())
	if summary != "" {
		body += "\n\n" + summary
	}
	body += "\n\nchat below to vet the work"
	m.appendEntry(entry{kind: eComplete, body: body})
	m.persist()
}
