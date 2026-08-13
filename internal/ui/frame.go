package ui

import (
	"time"

	"github.com/hweeks/always-click-yes/internal/state"
)

// Frame is the whole run, as a value.
//
// It exists because the TUI's state lives in unexported fields of Model, and a
// second front end (an HTTP server feeding a VS Code webview) has to render the
// same run without reaching into them. So Frame is the read seam: a pure value
// that marshals cleanly with encoding/json — no channels, no funcs, no pointers
// into model internals — and that a later milestone diffs to decide whether the
// client needs a new one at all.
//
// Two shape rules matter more than the field list, and both are load-bearing:
//
//   - Gates are identified by ToolUseID, never by index. An HTTP action answering
//     "gate 0" would answer whichever gate happened to be at the head when the
//     request landed, which after an auto-approve is the wrong tool.
//
//   - Countdowns travel as an absolute deadline, and there is deliberately no
//     "now" anywhere in Frame. The tick fires every 120ms; a frame carrying the
//     current time would differ on every one of them, so change detection would
//     degenerate into "always changed" and the server would push eight frames a
//     second forever. A client animates the countdown from its own clock.
//
// The documented contract is docs/webui-protocol.md. Change one, change both.
type Frame struct {
	Phase    string   `json:"phase"`  // "PLAN" | "AUTO-RUN" | "COMPLETE"
	Status   string   `json:"status"` // the one-line header state ("working…", "idle", …)
	Hint     Hint     `json:"hint"`   // the composer hint: text plus the kind that styles it
	Composer Composer `json:"composer"`

	SessionID string `json:"sessionId"` // claude's id, empty until its init event
	Model     string `json:"model"`     // the model claude reported at init
	Billing   string `json:"billing"`   // "subscription" | "API" | "" (unknown)

	Ended               bool  `json:"ended"`               // the stream closed; nothing left to send to
	Busy                bool  `json:"busy"`                // a turn, a gate or a task is in flight
	Processing          bool  `json:"processing"`          // a turn specifically
	PlanReady           bool  `json:"planReady"`           // a plan is on screen, waiting for Ctrl+G
	Paused              bool  `json:"paused"`              // every gate countdown is frozen
	ShowHelp            bool  `json:"showHelp"`            // the help overlay is open
	Picking             bool  `json:"picking"`             // the resume picker is open
	CooldownUntilUnixMs int64 `json:"cooldownUntilUnixMs"` // retry deadline after a rate limit, else 0

	// TurnStartUnixMs is when the in-flight turn began, 0 when idle. Absolute for
	// the same reason gate deadlines are: the client counts the elapsed time up
	// from it rather than being told a number that changes every tick.
	TurnStartUnixMs int64 `json:"turnStartUnixMs"`

	Cost   Cost   `json:"cost"`
	Tokens Ledger `json:"tokens"`

	// Dispatches counts every task this run delegated, including ones the ledger
	// has since trimmed — so it can exceed len(Tasks).
	Dispatches int `json:"dispatches"`

	Entries []Entry      `json:"entries"`
	Queue   []QueueItem  `json:"queue"` // messages held for the next idle moment
	Gates   []Gate       `json:"gates"` // head of the slice is the one on screen
	Ask     *Ask         `json:"ask"`   // null when no question is open
	Tasks   []Task       `json:"tasks"`
	Picker  []SessionRow `json:"picker"` // the /resume rows; empty unless Picking

	// Engineers is the architect's fleet ledger, oldest first; empty for a
	// session with no fleet wired. Fleet is capacity across the fleet's hosts —
	// zero/zero for the same reason.
	Engineers []Engineer   `json:"engineers"`
	Fleet     FleetSummary `json:"fleet"`

	// Tickets is the architect's ticket board, sorted by id; empty for a
	// session with no ticket store wired.
	Tickets []Ticket `json:"tickets"`

	// InterruptedTasks names the tasks a restart caught mid-flight, so a client
	// can say what a resumed run may have left half-done.
	InterruptedTasks []string `json:"interruptedTasks"`

	LogPath    string `json:"logPath"`
	ConfigPath string `json:"configPath"`
	Cwd        string `json:"cwd"`

	// Branch is the current git branch/SHA badge, "" when disabled or
	// unresolved. It only changes when a real branch switch resolves — see
	// Config.Branch.
	Branch string `json:"branch"`

	// FinishOutcome and FinishSummary are set once the session calls Finish —
	// "completed" or "abandoned", and the summary it gave. Both are omitted
	// before then, so a client can tell "not finished" from "finished with an
	// empty summary".
	FinishOutcome string `json:"finishOutcome,omitempty"`
	FinishSummary string `json:"finishSummary,omitempty"`
}

// Composer says whether the composer is the surface the keyboard is pointed
// at, so a client can blink its own cursor only while it's true. A plain
// boolean, unrelated to any clock — it does not change on its own between two
// frames of an idle run.
type Composer struct {
	Active bool `json:"active"`
}

// Cost splits the bill by who spent it. Parent is every claude process this
// supervisor drove itself; Child is the dispatched tasks, which report their
// spend to the orchestrator rather than through the driver.
type Cost struct {
	Parent float64 `json:"parent"`
	Child  float64 `json:"child"`
	Total  float64 `json:"total"`
}

// Tokens is one spender's tally.
type Tokens struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheCreate int64 `json:"cacheCreate"`
	CacheRead   int64 `json:"cacheRead"`
}

// Ledger is the run's token accounting. The parent/child split is the whole
// thesis of the orchestrator made visible: a run that delegates should show
// Parent flat while Child climbs.
type Ledger struct {
	Parent Tokens `json:"parent"`
	Child  Tokens `json:"child"`
	Total  Tokens `json:"total"`

	// Context is the most recent turn's context size — a reading, not a total —
	// and ContextWindow is what claude said it fits in (0 until a result event
	// reports one). "38k" means very different things against 200k and 1M.
	Context       int `json:"context"`
	ContextWindow int `json:"contextWindow"`
}

// Entry is one transcript item.
type Entry struct {
	// Seq identifies this entry across frames. It is monotonic in the order
	// entries were created and never reused, including across /clear — but it is
	// not a sort key; the slice is already in display order.
	Seq int `json:"seq"`

	Kind  string `json:"kind"`  // see entryKinds
	Title string `json:"title"` // a tool name, where there is one
	Body  string `json:"body"`  // ANSI-stripped plain text
	Raw   string `json:"raw"`   // the unhighlighted source behind Body
	Lang  string `json:"lang"`  // language hint for Raw ("bash", "diff", "go", …)
	Task  string `json:"task"`  // the delegated task this came from ("" = the parent)

	// HTML is this entry rendered as a sanitized HTML fragment by
	// internal/htmlrender — markdown for Claude's prose, chroma-highlighted code
	// for a tool call, escaped text for everything else. It is **empty unless
	// Config.RenderHTML was set**, which a terminal run never does, so a client
	// that wants it must ask for it and one that does not costs the run nothing.
	//
	// Styling travels separately: the fragment carries class names and no colors,
	// and htmlrender.StyleSheet supplies the chroma CSS. That is what lets a
	// client switch light/dark without re-rendering a transcript it already has —
	// and it is required by the webview's CSP, which forbids inline styles.
	HTML string `json:"html"`
}

// QueueItem is one message held for the next idle moment, as Frame projects
// it. ID is what a QueueEdit/QueueRemove action names, never a position — the
// same reason a Gate is identified by ToolUseID rather than by index.
type QueueItem struct {
	ID   int    `json:"id"`
	Text string `json:"text"`
}

// Gate is a permission request counting down.
type Gate struct {
	// ToolUseID is the identity. An action that answers a gate names this, never
	// a position — a gate that auto-approves while a request is in flight would
	// otherwise shift the whole queue under it.
	ToolUseID string `json:"toolUseId"`

	Tool string `json:"tool"` // the tool name as claude reported it
	Task string `json:"task"` // the delegated task that raised it ("" = the parent)
	Args string `json:"args"` // the one-line argument preview the gate panel shows

	// DeadlineUnixMs is when this auto-approves, and RemainingMs is the frozen
	// time left once Frame.Paused is set. Exactly one of the two is ever
	// non-zero, which is what keeps a frame identical between ticks — and which
	// is why neither can be read without checking the other.
	DeadlineUnixMs int64 `json:"deadlineUnixMs"`
	RemainingMs    int64 `json:"remainingMs"`
}

// Ask is the question claude is blocked on. Only the current question travels:
// the earlier ones are answered and the later ones are not being asked yet.
type Ask struct {
	Header      string      `json:"header"`
	Question    string      `json:"question"`
	Index       int         `json:"index"` // 0-based position within the ask
	Total       int         `json:"total"`
	MultiSelect bool        `json:"multiSelect"`
	Cursor      int         `json:"cursor"`
	Options     []AskOption `json:"options"`

	// DeadlineUnixMs is when the question auto-skips; 0 in PLAN, where someone is
	// sitting right there and a question may wait forever.
	DeadlineUnixMs int64 `json:"deadlineUnixMs"`
}

// AskOption is one selectable answer.
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Selected    bool   `json:"selected"`
}

// Task is one delegated unit of work, as the ledger remembers it.
type Task struct {
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	Outcome string  `json:"outcome"`
	Cost    float64 `json:"cost"`
	Tokens  Tokens  `json:"tokens"`

	// Running is a task with no end time. Its blank outcome and zero cost are
	// "not in yet", not "finished badly", and a client has to be able to tell.
	Running bool `json:"running"`
}

// Engineer is one remote engineer in the architect's fleet, as the ledger
// remembers it.
type Engineer struct {
	ID      string  `json:"id"`
	Ticket  string  `json:"ticket"`
	Title   string  `json:"title"`
	Host    string  `json:"host"`
	State   string  `json:"state"` // launching | running | done | failed | cancelled
	Outcome string  `json:"outcome"`
	PRURL   string  `json:"prUrl"`
	CostUSD float64 `json:"costUsd"`
	Branch  string  `json:"branch"`
}

// FleetSummary is capacity across the fleet's hosts, plus how many engineers
// are counted as active within it.
type FleetSummary struct {
	Active        int `json:"active"`
	CapacityUsed  int `json:"capacityUsed"`
	CapacityTotal int `json:"capacityTotal"`
}

// Ticket is one line of the architect's ticket board, as Frame projects it —
// the summary a client lists, not the full brief ReadTickets/UpdateTicket
// hand the model.
type Ticket struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	PRURL  string `json:"prUrl"`
}

// SessionRow is one line of the /resume picker.
type SessionRow struct {
	ID            string `json:"id"`
	ModTimeUnixMs int64  `json:"modTimeUnixMs"`
	Summary       string `json:"summary"`

	// Label is acy's own state for the session — phase, task count, cost — and is
	// empty for a session acy never supervised. That emptiness is how the two are
	// told apart in the list.
	Label string `json:"label"`

	// Selected marks the picker's current row, so a client can render the same
	// highlight the TUI does without tracking the cursor itself.
	Selected bool `json:"selected"`
}

// entryKinds maps the internal styling enum to the wire names. A table rather
// than a String() method on ekind, because these strings are protocol: renaming
// an ekind constant must not silently rename a JSON value a client switches on.
var entryKinds = map[ekind]string{
	eMeta:     "meta",
	eYou:      "you",
	eClaude:   "claude",
	eThinking: "thinking",
	eTool:     "tool",
	eToolOK:   "toolOK",
	eToolErr:  "toolErr",
	ePlan:     "plan",
	eTurn:     "turn",
	eComplete: "complete",
	eGood:     "good",
	eWarn:     "warn",
	eQueued:   "queued",
}

// Frame projects the model for a non-terminal front end. It is a read: nothing
// here mutates, and nothing here consults the clock.
func (m Model) Frame() Frame {
	return Frame{
		Phase:    m.phase.String(),
		Status:   m.status,
		Hint:     m.hint(),
		Composer: Composer{Active: m.composerActive()},

		SessionID: m.sessionID,
		Model:     m.model,
		Billing:   m.billing(),

		Ended:               m.ended,
		Busy:                m.busy(),
		Processing:          m.processing,
		PlanReady:           m.planReady,
		Paused:              m.paused,
		ShowHelp:            m.showHelp,
		Picking:             m.picking,
		CooldownUntilUnixMs: unixMs(m.cooldownUntil),

		TurnStartUnixMs: unixMs(m.turnStart),

		Cost: Cost{
			Parent: m.totalCost(),
			Child:  m.childCost,
			Total:  m.grandTotalCost(),
		},
		Tokens: Ledger{
			Parent:        frameTokens(m.parentTokens),
			Child:         frameTokens(m.childTokens),
			Total:         frameTokens(m.allTokens()),
			Context:       m.lastContext,
			ContextWindow: m.contextWindow,
		},
		Dispatches: m.dispatches,

		Entries: m.frameEntries(),
		// Copied, and copied into a non-nil slice: a caller must not be handed
		// the model's own backing array, and every list field marshals as [] so
		// a client never has to handle null and empty as two different things.
		Queue:  m.frameQueue(),
		Gates:  m.frameGates(),
		Ask:    m.frameAsk(),
		Tasks:  m.frameTasks(),
		Picker: m.framePicker(),

		Engineers: m.frameEngineers(),
		Fleet:     m.frameFleet(),
		Tickets:   m.frameTickets(),

		InterruptedTasks: strs(m.interruptedTasks),

		LogPath:    m.logPath,
		ConfigPath: m.configPath,
		Cwd:        m.cwd,
		Branch:     m.branch,

		FinishOutcome: m.finishOutcome,
		FinishSummary: m.finishSummary,
	}
}

func (m Model) frameEntries() []Entry {
	out := make([]Entry, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, Entry{
			Seq:  e.seq,
			Kind: entryKinds[e.kind],
			// Stripped, not styled: the body a tool entry carries is already
			// chroma's terminal256 output, and escape codes in a JSON string are
			// noise to every client that is not a terminal. Raw is what a webview
			// highlights instead.
			Title: e.title,
			Body:  stripAnsi(e.body),
			Raw:   e.raw,
			Lang:  e.lang,
			Task:  e.task,
			// Already rendered, at ingest, and empty when this run was not asked
			// for HTML — see stamp.
			HTML: e.html,
		})
	}
	return out
}

func (m Model) frameQueue() []QueueItem {
	out := make([]QueueItem, 0, len(m.queued))
	for _, q := range m.queued {
		out = append(out, QueueItem{ID: q.id, Text: q.text})
	}
	return out
}

func (m Model) frameGates() []Gate {
	out := make([]Gate, 0, len(m.pending))
	for _, it := range m.pending {
		g := Gate{
			ToolUseID: it.p.Input.ToolUseID,
			Tool:      it.p.Input.ToolName,
			Task:      it.task,
			// firstLine, matching the gate panel: toolArgs hands back a Bash
			// command verbatim, and a multi-line one would break a single-line
			// preview on either front end.
			Args: firstLine(toolArgs(it.p.Input.ToolInput)),
		}
		// Only one of the two is meaningful, and which one is a property of the
		// run rather than of the gate: togglePause freezes the remainder and
		// leaves the old deadline in place for the resume to re-derive from. A
		// client handed that stale deadline would animate towards a moment that
		// is never going to arrive.
		if m.paused {
			g.RemainingMs = it.remaining.Milliseconds()
		} else {
			g.DeadlineUnixMs = unixMs(it.deadline)
		}
		out = append(out, g)
	}
	return out
}

func (m Model) frameAsk() *Ask {
	if m.ask == nil {
		return nil
	}
	q := m.ask.questions[m.ask.qIdx]
	opts := make([]AskOption, 0, len(q.options))
	for i, o := range q.options {
		opts = append(opts, AskOption{Label: o.label, Description: o.description, Selected: q.selected[i]})
	}
	return &Ask{
		Header:         q.header,
		Question:       q.question,
		Index:          m.ask.qIdx,
		Total:          len(m.ask.questions),
		MultiSelect:    q.multiSelect,
		Cursor:         q.cursor,
		Options:        opts,
		DeadlineUnixMs: unixMs(m.ask.deadline),
	}
}

func (m Model) frameTasks() []Task {
	out := make([]Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, Task{
			ID:      t.ID,
			Title:   t.Title,
			Outcome: t.Outcome,
			Cost:    t.CostUSD,
			Tokens:  frameTokens(t.Tokens),
			Running: t.Unfinished(),
		})
	}
	return out
}

// frameEngineers projects the fleet mirror syncFleet keeps — see fleet.go —
// rather than calling back into m.fleet, so Frame stays a read of the model's
// own state even when a fleet is wired.
func (m Model) frameEngineers() []Engineer {
	out := make([]Engineer, 0, len(m.engineers))
	for _, e := range m.engineers {
		out = append(out, Engineer{
			ID:      e.EngineerID,
			Ticket:  e.Ticket,
			Title:   e.Title,
			Host:    e.Host,
			State:   e.State,
			Outcome: e.Outcome,
			PRURL:   e.PRURL,
			CostUSD: e.CostUSD,
			Branch:  e.Branch,
		})
	}
	return out
}

func (m Model) frameFleet() FleetSummary {
	return FleetSummary{
		Active:        m.fleetActive,
		CapacityUsed:  m.fleetCapUsed,
		CapacityTotal: m.fleetCapTotal,
	}
}

// frameTickets reads the board directly rather than through a mirror kept in
// sync by an event stream — unlike the fleet, there is no push side to
// tickets, so a read is the only way to know its current state. A nil store
// or a read error both project as no tickets, matching how an unwired fleet
// projects as no engineers.
func (m Model) frameTickets() []Ticket {
	if m.tickets == nil {
		return []Ticket{}
	}
	ts, err := m.tickets.List()
	if err != nil {
		return []Ticket{}
	}
	out := make([]Ticket, 0, len(ts))
	for _, t := range ts {
		out = append(out, Ticket{ID: t.ID, Title: t.Title, Status: t.Status, PRURL: t.PR})
	}
	return out
}

func (m Model) framePicker() []SessionRow {
	// The rows were built by pickRows when the picker opened — the same call the
	// HTTP server makes through SessionRows — so all that is left here is the one
	// thing the model knows and the list does not: where the cursor is.
	out := make([]SessionRow, 0, len(m.sessionList))
	for i, s := range m.sessionList {
		row := s.SessionRow
		row.Selected = i == m.pickIdx
		out = append(out, row)
	}
	return out
}

// frameTokens converts the internal tally to the wire shape. The two are
// deliberately separate types: state.Tokens is a persistence format with its own
// snake_case tags, and letting it leak here would tie the protocol to the
// snapshot schema.
func frameTokens(t state.Tokens) Tokens {
	return Tokens{
		Input:       t.Input,
		Output:      t.Output,
		CacheCreate: t.CacheCreate,
		CacheRead:   t.CacheRead,
	}
}

// strs copies a string slice into one that is never nil.
func strs(in []string) []string { return append(make([]string, 0, len(in)), in...) }

// unixMs renders a time as milliseconds since the epoch, and a zero time as 0 —
// not as the epoch's own -6795364578871 nanoseconds, which is what UnixMilli
// gives you and which a client would happily animate a countdown against.
func unixMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
