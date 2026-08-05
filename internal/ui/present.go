package ui

import (
	"fmt"
	"time"
)

// The presentation decisions, separated from the paint.
//
// view.go used to decide *what to say* inline with *how to draw it*: a switch
// that picked one of seven hint strings sat in the middle of the function that
// framed the composer, and the help text existed only as a list of already-styled
// lipgloss lines. That was fine while the terminal was the only front end. It
// stops being fine the moment a second one (the HTTP server feeding a VS Code
// webview) has to say the same things, because the alternative is two copies of
// every sentence drifting apart.
//
// So the rule this file draws is: content lives here, styling stays in view.go.
// Nothing in this file imports lipgloss or knows a color exists. Everything here
// is a pure function of plain values, which is also what makes it safe to hand
// to a JSON projection (see frame.go).

// HintKind names which composer hint is showing. The TUI uses it to pick a
// style; another front end can key a CSS class off the same value, and neither
// has to re-derive the condition that chose the text.
type HintKind string

const (
	HintGate      HintKind = "gate"      // a permission gate is counting down
	HintWorking   HintKind = "working"   // a turn is in flight
	HintBusy      HintKind = "busy"      // a delegated task is running
	HintPlanReady HintKind = "planReady" // a plan is on screen, waiting to be armed
	HintPlan      HintKind = "plan"      // PLAN, idle
	HintComplete  HintKind = "complete"  // the run finished; chat on to vet it
	HintDefault   HintKind = "default"   // AUTO-RUN, idle
)

// Hint is the line under the composer: what to say, and what kind of thing it is.
type Hint struct {
	Text string   `json:"text"`
	Kind HintKind `json:"kind"`
}

// composerHint picks the hint for a state. It is a pure function of the four
// facts that decide it, deliberately: the ordering between them is the whole
// content of this decision, and a method reading fields off the model would hide
// it behind state nobody can enumerate in a test.
//
// The order is not arbitrary. A pending gate outranks everything, because it is
// the one state where Esc does nothing (the PreToolUse hook that raised the gate
// is blocked on the socket, and interrupting the turn out from under it
// deadlocks) — so the hint must not offer it. Advertising a key that does
// nothing is how a user learns to stop reading the hint line.
func composerHint(gates int, processing, busy, planReady bool, phase Phase) Hint {
	switch {
	case gates > 0:
		return Hint{Kind: HintGate,
			Text: "working… · Enter queues your message · ^Y allow / ^X stop first · Ctrl+C to quit"}
	case processing:
		return Hint{Kind: HintWorking,
			Text: "working… · Esc to interject · Enter queues your message · Ctrl+C to quit"}
	case busy:
		return Hint{Kind: HintBusy,
			Text: "waiting on a task · Enter queues your message · Ctrl+C to quit"}
	case planReady && phase == PhasePlan:
		return Hint{Kind: HintPlanReady,
			Text: "📋 plan ready above · Ctrl+G to arm & run · or keep chatting to refine"}
	case phase == PhasePlan:
		return Hint{Kind: HintPlan,
			Text: "Enter to send · ^J newline · Ctrl+G to arm (start auto-run) · Ctrl+C to quit"}
	case phase == PhaseComplete:
		return Hint{Kind: HintComplete,
			Text: "plan complete · Enter to send a follow-up · ^J newline · Ctrl+C to quit"}
	default:
		return Hint{Kind: HintDefault, Text: "Enter to send · ^J newline · Ctrl+C to quit"}
	}
}

// hint is the composer hint for this model's current state.
func (m Model) hint() Hint {
	return composerHint(len(m.pending), m.processing, m.busy(), m.planReady, m.phase)
}

// --- overlay footer hints ---

// The three overlays each replace the composer, so each owns the footer line for
// as long as it is up. They are separate constants rather than one table because
// the ask hint is the only one that varies, and it varies twice.

const (
	helpFooterHint   = "press any key to close"
	pickerFooterHint = "↑/↓ move · Enter resume · Esc cancel"
)

// askFooterHint is the key legend for an open question. Space only appears for a
// multi-select, where it is the only way to pick a second option; on a
// single-select it would name a key that does nothing.
func askFooterHint(multiSelect bool) string {
	if multiSelect {
		return "↑/↓ move · Space toggle · Enter confirm · Esc skip"
	}
	return "↑/↓ move · Enter confirm · Esc skip"
}

// askAutoSkipNote is the suffix that says a question is on a clock. It exists
// only in AUTO-RUN, where the human has by assumption walked away — and it is
// said out loud because a countdown nobody can see is exactly how the gate bug
// happened.
func askAutoSkipNote(remaining time.Duration) string {
	return fmt.Sprintf(" · auto-skip in %ds", int(remaining.Seconds()+0.5))
}

// askProgressNote numbers a question within a multi-question ask, and says
// nothing at all when there is only one — "(1/1)" is noise.
func askProgressNote(idx, total int) string {
	if total <= 1 {
		return ""
	}
	return fmt.Sprintf("  (%d/%d)", idx+1, total)
}

// --- gate panel ---

// gateStateLabel is the countdown readout on the permission panel. The paused
// string is padded to the width of the running one so the description beside it
// does not jump sideways every time ^R is pressed.
func gateStateLabel(secs int, paused bool) string {
	if paused {
		return "⏸  PAUSED         "
	}
	return fmt.Sprintf("⏳ auto-approve in %2ds", secs)
}

// gateQueuedNote counts the gates waiting behind the one on screen.
func gateQueuedNote(behind int) string {
	if behind <= 0 {
		return ""
	}
	return fmt.Sprintf("  (+%d queued)", behind)
}

// --- queue panel ---

// queueSummary is the queue panel's headline. It is the answer to "did that
// Enter do anything?", which used to be "no" — and it names the moment the
// messages leave, because "queued" alone does not say what releases them.
func queueSummary(n int) string {
	return fmt.Sprintf("⏳ %d queued · sends when this turn ends", n)
}

// queueMoreNote counts the tail the panel does not list.
func queueMoreNote(hidden int) string { return fmt.Sprintf("   (+%d more)", hidden) }

// --- help ---

// HelpRow is one line of the help overlay: the keys or command on the left, what
// it does on the right. A row with empty Keys is a continuation of the row above
// — the description wraps by hand rather than by measurement, so the columns
// stay aligned at any width.
type HelpRow struct {
	Keys        string `json:"keys"`
	Description string `json:"description"`
}

// HelpSection is a titled group of rows.
type HelpSection struct {
	Title string    `json:"title"`
	Rows  []HelpRow `json:"rows"`
}

// helpTitle is the overlay's heading.
const helpTitle = "always-click-yes · help"

// helpContent is everything the /help overlay says, as structure rather than as
// pre-styled lines. helpView renders it; a webview can render the same rows into
// a table without parsing ANSI back out of a string.
func helpContent() []HelpSection {
	return []HelpSection{{
		Title: "commands",
		Rows: []HelpRow{
			{"/help", "show this help"},
			{"/resume [id]", "restore a prior run — transcript, phase and cost (picker if no id)"},
			{"/model <name>", "set the model for the next launched/resumed session"},
			{"/queue", "list the messages waiting for the current turn to end"},
			{"/queue clear", "drop them all, unsent"},
			{"/clear", "clear the transcript view"},
			{"/log", "show the debug-log path"},
			{"/tokens", "token ledger: context size, cache reads and cost by spender"},
			{"/tasks", "delegated-task ledger: outcome, cost and cache reads per task"},
			{"/fleet", "the architect's engineer ledger: state, host, outcome and cost per engineer"},
			{"/tickets", "the architect's ticket board: every ticket's status, branch, PR and brief"},
			{"/done", "end the run by hand, if the session stopped without calling Finish"},
			{"/quit", "quit (same as Ctrl+C)"},
		},
	}, {
		Title: "while Claude is working",
		Rows: []HelpRow{
			{"Enter", "queues the message; the whole queue goes out as ONE turn when the"},
			{"", "turn ends — one turn, because each one re-bills the whole context"},
			{"Esc", "interrupts the turn, so the queue goes out as the redirect"},
			{"/queue", "see what is waiting · /queue clear drops it"},
			{"note", "the queue is never saved — a crash loses it rather than delivering"},
			{"", "it into whatever phase the run comes back in"},
		},
	}, {
		Title: "keys",
		Rows: []HelpRow{
			{"Enter", "send the message"},
			{"Ctrl+J", "newline without sending — works in every terminal"},
			{"Alt+Enter", "newline without sending — works in every terminal"},
			{"Shift+Enter", "newline, but ONLY where the terminal speaks the Kitty keyboard"},
			{"", "protocol (Ghostty, kitty, WezTerm, iTerm2 3.5+). Anywhere else it"},
			{"", "is indistinguishable from Enter and simply sends — use Ctrl+J"},
			{"paste", "a multi-line paste arrives whole; it never sends"},
			{"drag a file", "dropping or pasting a file path attaches it as an absolute path —"},
			{"", "Claude reads it with its own Read tool, so nothing is sent until it does"},
			{"Ctrl+G", "arm the plan (start auto-run)"},
			{"Esc", "interject / interrupt the current turn"},
			{"↑/↓ PgUp/PgDn", "scroll the transcript"},
			{"Ctrl+C", "quit"},
		},
	}, {
		Title: "while a gate is counting down",
		Rows: []HelpRow{
			{"Ctrl+Y", "allow the front tool now"},
			{"Ctrl+X", "stop (veto) the front tool"},
			{"Ctrl+R", "pause / resume every countdown"},
			{"any other key", "types into the message box as usual"},
			{"Esc", "does NOT interject — answer the gate first"},
		},
	}, {
		Title: "while Claude is asking a question",
		Rows: []HelpRow{
			{"↑/↓ j/k", "move between options"},
			{"Space", "toggle a choice (multi-select only)"},
			{"Enter", "confirm and go to the next question"},
			{"Esc", "skip the questions"},
		},
	}}
}
