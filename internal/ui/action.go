package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/gate"
)

// The write seam.
//
// Frame (frame.go) let a second front end *read* the run. This is the other
// half: a semantic vocabulary for driving the model, so an HTTP handler can say
// "allow this gate" without synthesising a Ctrl+Y that the terminal would have
// had to be looking at to produce.
//
// The rule that shapes the whole file is that there is exactly ONE
// implementation of each behaviour and the terminal is a client of it like
// anything else. handleGateKey does not resolve a gate; it raises GateAllow.
// Ctrl+G does not arm; it raises Arm. That is not layering for its own sake —
// two implementations of "approve a tool" is precisely the kind of pair that
// drifts silently, and the one that drifts is the one nobody is watching.
//
// Two consequences worth stating out loud:
//
//   - Gates are answered by ToolUseID, never by position. Gates auto-approve on
//     their own countdown, so between a client rendering a list and its request
//     landing the head of the queue may be a different tool entirely. A stale id
//     must resolve nothing at all; falling back to "well, the front one then"
//     would approve a tool nobody looked at.
//
//   - Every action validates itself, even where the key routing in update.go
//     already guards the same thing. An HTTP caller does not press keys, so it
//     does not pass through that routing — the guard has to exist where the
//     behaviour does.

// ActionKind names one semantic action. These strings are protocol: a client
// sends them, so renaming a constant is a wire change, not a refactor.
type ActionKind string

const (
	ActionSubmit      ActionKind = "submit"      // send text (or run a /command) as if typed
	ActionArm         ActionKind = "arm"         // Ctrl+G: flip PLAN into AUTO-RUN
	ActionInterject   ActionKind = "interject"   // Esc: interrupt the in-flight turn
	ActionGateAllow   ActionKind = "gateAllow"   // approve one pending gate, by tool_use id
	ActionGateDeny    ActionKind = "gateDeny"    // veto one pending gate, by tool_use id
	ActionGatePause   ActionKind = "gatePause"   // freeze or resume every countdown
	ActionAskAnswer   ActionKind = "askAnswer"   // answer the open question
	ActionAskSkip     ActionKind = "askSkip"     // skip the open question
	ActionResume      ActionKind = "resume"      // restore a prior session by id
	ActionPickerClose ActionKind = "pickerClose" // Esc: dismiss the /resume picker, resuming nothing
	ActionSetModel    ActionKind = "setModel"    // /model: pick the next session's model
	ActionClear       ActionKind = "clear"       // /clear: empty the transcript view
	ActionDone        ActionKind = "done"        // /done: end the run by hand
	ActionQueueClear  ActionKind = "queueClear"  // /queue clear: drop held messages
	ActionQuit        ActionKind = "quit"        // stop the driver and exit
)

// actionKinds is every kind above. It exists for Valid, and it lives next to the
// constants so that adding one and forgetting this is a two-line diff away from
// being obvious.
var actionKinds = map[ActionKind]bool{
	ActionSubmit: true, ActionArm: true, ActionInterject: true,
	ActionGateAllow: true, ActionGateDeny: true, ActionGatePause: true,
	ActionAskAnswer: true, ActionAskSkip: true, ActionResume: true,
	ActionPickerClose: true, ActionSetModel: true, ActionClear: true,
	ActionDone: true, ActionQueueClear: true, ActionQuit: true,
}

// Valid reports whether k names an action at all.
//
// A transport uses it to tell two different things apart. An action the model
// *refused* is a domain answer — the run moved on, the gate is gone — and comes
// back as a rejected ActionResult with a reason. A kind that does not exist is a
// malformed request: the caller sent something this build has no vocabulary for,
// and an HTTP server answers that with a 400 rather than with a verdict.
// applyAction refuses an unknown kind too, so nothing depends on the check
// happening first; this only lets a caller say *which* kind of no it was.
func (k ActionKind) Valid() bool { return actionKinds[k] }

// Action is one thing a front end asks the model to do.
//
// One struct with a Kind rather than an interface with a type per action,
// because this is a wire format first: it has to survive encoding/json in both
// directions, and a client assembling one by hand should not have to know Go's
// type system. Fields that do not apply to a Kind are ignored, never inspected.
type Action struct {
	Kind ActionKind `json:"kind"`

	Text          string `json:"text,omitempty"`          // Submit
	ToolUseID     string `json:"toolUseId,omitempty"`     // GateAllow, GateDeny
	Paused        bool   `json:"paused,omitempty"`        // GatePause
	QuestionIndex int    `json:"questionIndex,omitempty"` // AskAnswer
	OptionIndices []int  `json:"optionIndices,omitempty"` // AskAnswer
	SessionID     string `json:"sessionId,omitempty"`     // Resume
	Name          string `json:"name,omitempty"`          // SetModel
	Summary       string `json:"summary,omitempty"`       // Done
}

// The constructors. They exist so a call site reads as the action it is —
// GateAllow(id) rather than Action{Kind: ActionGateAllow, ToolUseID: id} — and
// so a new field on Action cannot silently mean "zero" at a caller that never
// heard of it.

// Submit sends text exactly as pressing Enter with it in the composer would,
// slash-command routing included.
func Submit(text string) Action { return Action{Kind: ActionSubmit, Text: text} }

// Arm is Ctrl+G: the plan is approved, start delegating.
func Arm() Action { return Action{Kind: ActionArm} }

// Interject is Esc: interrupt the in-flight turn so the user can redirect.
func Interject() Action { return Action{Kind: ActionInterject} }

// GateAllow approves the gate carrying this tool_use id, and only that one.
func GateAllow(toolUseID string) Action {
	return Action{Kind: ActionGateAllow, ToolUseID: toolUseID}
}

// GateDeny vetoes the gate carrying this tool_use id, and only that one.
func GateDeny(toolUseID string) Action {
	return Action{Kind: ActionGateDeny, ToolUseID: toolUseID}
}

// GatePause sets the paused state of every countdown.
//
// It states the state it wants rather than toggling, because a stateless client
// cannot toggle safely: between reading a frame that said "running" and its
// request arriving, a Ctrl+R in the terminal may already have paused it, and a
// toggle would then start the countdowns the user just froze.
func GatePause(paused bool) Action { return Action{Kind: ActionGatePause, Paused: paused} }

// AskAnswer answers the question currently on screen with the chosen options.
// The index names which question is being answered so a client cannot answer a
// question that has already been superseded — the same reasoning as gate ids.
func AskAnswer(questionIndex int, optionIndices []int) Action {
	return Action{Kind: ActionAskAnswer, QuestionIndex: questionIndex, OptionIndices: optionIndices}
}

// AskSkip abandons the open question, telling claude to use its judgment.
func AskSkip() Action { return Action{Kind: ActionAskSkip} }

// Resume restores a prior session by id.
func Resume(sessionID string) Action { return Action{Kind: ActionResume, SessionID: sessionID} }

// PickerClose is Esc in the /resume picker: dismiss it without resuming.
func PickerClose() Action { return Action{Kind: ActionPickerClose} }

// SetModel picks the model for the next launched or resumed session.
func SetModel(name string) Action { return Action{Kind: ActionSetModel, Name: name} }

// Clear empties the transcript view (not the conversation).
func Clear() Action { return Action{Kind: ActionClear} }

// Done ends the run by hand, for when the session stopped without calling Finish.
func Done(summary string) Action { return Action{Kind: ActionDone, Summary: summary} }

// QueueClear drops every held message, unsent.
func QueueClear() Action { return Action{Kind: ActionQueueClear} }

// Quit stops the driver and exits.
func Quit() Action { return Action{Kind: ActionQuit} }

// ActionResult is what came of an action: whether the model did it, and a
// sentence a human can read either way.
//
// Reason is populated on success too. "queued until the session falls idle" and
// "sent" are different outcomes of the same accepted Submit, and a client that
// can only see a boolean would have to re-derive which one happened from the
// next frame.
type ActionResult struct {
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason"`
}

func accepted(reason string) ActionResult { return ActionResult{Accepted: true, Reason: reason} }
func rejected(reason string) ActionResult { return ActionResult{Reason: reason} }

// ActionMsg carries an Action into the Bubble Tea event loop.
//
// Ack is optional and is nil when the TUI raises an action itself — the
// terminal can see what happened on screen. It is deliberately a field on the
// *message* and never on the Model: Bubble Tea copies the Model by value on
// every Update, and a channel parked in there would be copied along with it,
// with every copy able to answer a request exactly once between them.
type ActionMsg struct {
	Action Action
	Ack    chan<- ActionResult
}

// replyAction delivers a verdict without ever being able to block.
//
// A hung HTTP client is not a reason for the model loop to stop: the whole
// event loop is one goroutine, so a blocking send here would freeze the
// terminal, the countdowns and the driver reader behind a socket nobody is
// reading. A dropped acknowledgement costs that one caller its answer; a
// blocked one costs the run.
func replyAction(ack chan<- ActionResult, res ActionResult) {
	if ack == nil {
		return
	}
	select {
	case ack <- res:
	default:
	}
}

// raise runs an action the TUI itself asked for.
//
// The terminal has no acknowledgement channel — it reads the outcome off the
// screen — but Esc needs the verdict to decide whether it consumed the key, so
// this hands the result straight back. It still goes through applyAction: the
// point of this file is that the keyboard is a client, not a second door.
func (m *Model) raise(a Action) ActionResult {
	ch := make(chan ActionResult, 1)
	m.applyAction(a, ch)
	return <-ch
}

// applyAction performs one action and acknowledges it. It is the single
// implementation of each behaviour: update.go's ActionMsg case and every
// terminal key handler both arrive here.
//
// The returned tea.Cmd is whatever the action needs the runtime to do next —
// launching a resume, quitting — and is nil for the many actions that only
// change state.
func (m *Model) applyAction(a Action, ack chan<- ActionResult) (cmd tea.Cmd) {
	res := rejected("unknown action " + string(a.Kind))
	defer func() {
		alog.Printf("action: %s accepted=%v (%s)", a.Kind, res.Accepted, res.Reason)
		replyAction(ack, res)
	}()

	switch a.Kind {
	case ActionSubmit:
		// The same fork handleEnter takes, so /tokens, /queue clear and the rest
		// behave identically from either front end. A command is routed even on an
		// ended session — /quit and /tokens are exactly what you still want then —
		// while text is not, because there is nothing left to send it to.
		text := strings.TrimSpace(a.Text)
		if name, args, ok := parseCommand(text); ok {
			cmd = m.runCommand(name, args)
			res = accepted("ran /" + name)
			break
		}
		res = m.submitText(text)

	case ActionArm:
		res = m.armAction()

	case ActionInterject:
		res = m.interjectAction()

	case ActionGateAllow:
		res = m.answerGate(a.ToolUseID,
			gate.Decision{Behavior: gate.Allow, Reason: "approved by user"},
			eGood, "✔ approved · ⚙ ")

	case ActionGateDeny:
		res = m.answerGate(a.ToolUseID,
			gate.Decision{Behavior: gate.Deny, Reason: "vetoed by user"},
			eWarn, "✋ vetoed · ⚙ ")

	case ActionGatePause:
		if !m.setPaused(a.Paused) {
			res = accepted("countdowns were already " + pausedWord(a.Paused))
			break
		}
		res = accepted("countdowns " + pausedWord(a.Paused))

	case ActionAskAnswer:
		cmd, res = m.answerAsk(a.QuestionIndex, a.OptionIndices)

	case ActionAskSkip:
		if m.ask == nil {
			res = rejected("no question is open")
			break
		}
		cmd = m.submitAsk(true)
		res = accepted("question skipped")

	case ActionResume:
		id := strings.TrimSpace(a.SessionID)
		if id == "" {
			res = rejected("no session id to resume")
			break
		}
		// A webview chooses a row through this action rather than through the
		// terminal's Enter key path, so dismiss the shared picker here.
		m.picking = false
		cmd = m.resumeSession(id)
		res = accepted("resuming " + short(id))

	case ActionPickerClose:
		if !m.picking {
			res = rejected("the resume picker is not open")
			break
		}
		// One literal, said twice: the transcript entry the TUI has always printed
		// on cancel, and the sentence the client is answered with.
		const cancelled = "resume cancelled"
		m.picking = false
		m.appendEntry(entry{kind: eMeta, body: cancelled})
		res = accepted(cancelled)

	case ActionSetModel:
		name := strings.TrimSpace(a.Name)
		if name == "" {
			res = rejected("no model name")
			break
		}
		cmd = m.runCommand("model", name)
		res = accepted("model set to " + name)

	case ActionClear:
		cmd = m.runCommand("clear", "")
		res = accepted("transcript cleared")

	case ActionDone:
		// runCommand says "this run is already finished" in the transcript, which
		// is the same sentence the TUI prints today — so read the phase first and
		// let the one implementation do the talking.
		already := m.phase == PhaseComplete
		cmd = m.runCommand("done", strings.TrimSpace(a.Summary))
		if already {
			res = rejected("this run is already finished")
			break
		}
		res = accepted("run finished")

	case ActionQueueClear:
		n := len(m.queued)
		cmd = m.runCommand("queue", "clear")
		res = accepted(fmt.Sprintf("dropped %s, unsent", plural(n, "queued message")))

	case ActionQuit:
		cmd = m.runCommand("quit", "")
		res = accepted("quitting")
	}
	return cmd
}

func pausedWord(paused bool) string {
	if paused {
		return "paused"
	}
	return "running"
}

// armAction is Ctrl+G's behaviour with its guards restated.
//
// update.go checks the same three things before it will route the key, and that
// check stays there — it decides whether the *keyboard* consumed Ctrl+G. This
// one decides whether the run may be armed at all, which is a different
// question the moment a caller arrives over a socket instead of a keyboard.
func (m *Model) armAction() ActionResult {
	switch {
	case m.phase != PhasePlan:
		return rejected("the run is not in PLAN — it is already " + m.phase.String())
	case m.sessionID == "":
		// Nothing to arm yet: claude has not emitted init, which it does not do
		// until the first user message.
		return rejected("no session id yet — send a message first")
	case m.drv == nil:
		// A resume knows its session id before the process exists. arm() refuses
		// this itself and says so in the transcript; call it so that sentence comes
		// from the one place that has ever printed it.
		m.arm()
		return rejected("no session is running")
	}
	m.arm()
	return accepted("armed")
}

// interjectAction is Esc's behaviour with its guards restated.
//
// The pending-gate refusal is not duplication for its own sake. The PreToolUse
// hook that raised the gate is blocked on the gate socket waiting for a
// decision, and interrupting the turn out from under it is an unanswered-hook
// deadlock — a deadlock a caller reaches by HTTP just as easily as by Esc.
func (m *Model) interjectAction() ActionResult {
	switch {
	case len(m.pending) > 0:
		return rejected("answer the pending gate first — its hook is blocked and interrupting would deadlock it")
	case m.drv == nil:
		return rejected("no session is running")
	case !m.processing:
		return rejected("nothing is in flight to interrupt")
	}
	m.interject()
	return accepted("interrupting")
}

// answerGate resolves exactly the gate named by id.
//
// The refusal is the interesting half: an id that names nothing resolves
// nothing. It is the normal case rather than an error — a gate auto-approves on
// its own countdown, so a client can perfectly reasonably be holding an id that
// stopped existing a moment ago — and the only wrong answer is to shrug and
// take the front of the queue instead.
func (m *Model) answerGate(id string, d gate.Decision, kind ekind, prefix string) ActionResult {
	it, ok := m.resolveByID(id, d)
	if !ok {
		return rejected("no gate is pending for tool_use id " + quoteID(id))
	}
	m.appendEntry(entry{kind: kind, body: prefix + it.p.Input.ToolName})
	return accepted(d.Behavior + " " + it.p.Input.ToolName)
}

// answerAsk answers the open question the way Enter does in the ask panel:
// record the picks, advance to the next question, submit on the last one.
func (m *Model) answerAsk(qIdx int, opts []int) (tea.Cmd, ActionResult) {
	if m.ask == nil {
		return nil, rejected("no question is open")
	}
	// By index, for the same reason gates are by id: a question that has been
	// answered or auto-skipped is gone, and answering "the current one" from a
	// client that last looked a second ago answers whatever replaced it.
	if qIdx != m.ask.qIdx {
		return nil, rejected(fmt.Sprintf(
			"question %d is not the one being asked (that is question %d)", qIdx, m.ask.qIdx))
	}
	q := &m.ask.questions[m.ask.qIdx]
	if len(opts) == 0 {
		return nil, rejected("no option chosen")
	}
	if len(opts) > 1 && !q.multiSelect {
		return nil, rejected("this question takes a single option")
	}
	for _, i := range opts {
		if i < 0 || i >= len(q.options) {
			return nil, rejected(fmt.Sprintf("option %d is out of range (%d options)", i, len(q.options)))
		}
	}

	// Replaced wholesale rather than toggled, which is the difference between a
	// keyboard and a client: Space toggles because the person can see what is
	// already ticked, while optionIndices is a complete statement of the answer.
	q.selected = map[int]bool{}
	for _, i := range opts {
		q.selected[i] = true
	}

	if m.ask.qIdx < len(m.ask.questions)-1 {
		m.ask.qIdx++
		return nil, accepted("answered; next question")
	}
	return m.submitAsk(false), accepted("answered")
}

// quoteID renders an id for a refusal message, naming an empty one rather than
// printing two bare quotes into the middle of a sentence.
func quoteID(id string) string {
	if id == "" {
		return "(empty)"
	}
	return `"` + id + `"`
}
