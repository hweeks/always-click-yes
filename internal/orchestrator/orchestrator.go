// Package orchestrator runs delegated tasks in disposable `claude` child
// processes.
//
// It exists because of one measurement: a run of acy on its own repository
// spent $16.04, of which 8.7M tokens were cache reads and 75k were output. The
// model was not writing too much — a single session was carrying every file it
// had ever read into every turn that followed, and re-reading the lot each time.
//
// The fix is to stop letting the conversation you hold accumulate the work. A
// parent session keeps only the human conversation and one compact report per
// task; each task runs in a fresh child process with the full toolset, returns
// a structured report, and exits, taking its enormous context with it.
//
// This package owns the children and nothing else. It deliberately does not
// touch the UI's driver: internal/ui keeps a single parent driver guarded by a
// generation counter, and children live entirely outside that machinery.
package orchestrator

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/mcp"
	"github.com/hweeks/always-click-yes/internal/state"
)

// Task is one unit of delegated work, as the parent described it.
type Task struct {
	ID          string   // acy's own id, "t1"; stable and short enough to say out loud
	SessionID   string   // pre-assigned uuid, handed to claude as --session-id
	ToolUseID   string   // the parent's tool_use id, for correlating the reply
	Title       string   // a few words, for the transcript and the gate badge
	Instruction string   // what to do
	Context     []string // paths the parent already knows are relevant
	Success     string   // how the child will know it worked
	BudgetUSD   float64  // --max-budget-usd, 0 for none
}

// Child is a running claude process. *driver.Driver satisfies it; tests supply
// a scripted fake and never launch anything.
type Child interface {
	Events() <-chan driver.Event
	Send(string) error
	Stop()
}

// Spawn launches a Child for a Task. Injected the same way ui.Launcher is, so
// the wiring that gives children the gate hook lives in cli/run.go beside the
// parent's.
type Spawn func(ctx context.Context, t Task) (Child, error)

// EventKind distinguishes a child's stream traffic from its lifecycle, so the
// UI never has to sniff at event contents to know what it is looking at.
type EventKind int

const (
	KindStarted  EventKind = iota // the child process is up
	KindStream                    // one decoded event from the child
	KindFinished                  // terminal: Report is set
	KindFailed                    // terminal: Err is set
)

// Event is anything a child emits, tagged with the task that owns it.
type Event struct {
	TaskID string
	Title  string
	Kind   EventKind
	Ev     driver.Event
	Report *Report
	Status *Status
	Err    error
}

// Status is the ledger entry for one task.
type Status struct {
	Task    Task
	State   string // "queued" | "running" | "done" | "cancelled" | "failed"
	Outcome string // from the report, once there is one
	Started time.Time
	Ended   time.Time
	Cost    float64
	Tokens  state.Tokens
}

// Task states.
const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateDone      = "done"
	StateCancelled = "cancelled"
	StateFailed    = "failed"
)

// task is the mutable internal record. Status is the value copy handed out.
type task struct {
	Task
	pending *mcp.Pending
	started time.Time
	ended   time.Time
	cost    float64
	tokens  state.Tokens
	state   string
	outcome string
	cancel  context.CancelFunc
}

// Orchestrator runs delegated tasks, at most limit at a time.
type Orchestrator struct {
	spawn Spawn
	limit int
	now   func() time.Time

	events chan Event

	mu      sync.Mutex
	seq     int
	running map[string]*task
	bySess  map[string]string // child session id -> task id, for gate attribution
	queue   []*task
	ledger  []*task
	closed  bool

	wg sync.WaitGroup
}

// New builds an Orchestrator. limit is how many children may run at once; it is
// 1 in practice, because acy's own MCP server handles tools/call serially (a
// second Dispatch is not even read off stdin until the first returns) and
// because two children editing one working tree is a correctness hazard, not
// merely a display one.
func New(spawn Spawn, limit int) *Orchestrator {
	if limit < 1 {
		limit = 1
	}
	return &Orchestrator{
		spawn:   spawn,
		limit:   limit,
		now:     time.Now,
		events:  make(chan Event, 128),
		running: map[string]*task{},
		bySess:  map[string]string{},
	}
}

// Events is the stream of everything the children do. Created once and closed
// only by Close, so the UI's wait command can re-arm on it forever.
func (o *Orchestrator) Events() <-chan Event { return o.events }

// Dispatch accepts a blocked tools/call and returns immediately, having either
// started the task or queued it. The Pending is resolved later, by the goroutine
// that runs the child — that is the whole trick: claude's turn stays blocked on
// a socket read while a different process does the work.
func (o *Orchestrator) Dispatch(ctx context.Context, p *mcp.Pending) (Status, error) {
	spec, err := parseDispatch(p.Req.Args)
	if err != nil {
		p.Resolve(mcp.Answer{Text: "This dispatch could not be read: " + err.Error() +
			". Nothing was run. Fix the arguments and call it again."})
		return Status{}, err
	}

	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		p.Resolve(mcp.Answer{Text: cancelText("the supervisor is shutting down")})
		return Status{}, errClosed
	}
	o.seq++
	t := &task{
		Task: Task{
			ID:          taskID(o.seq),
			SessionID:   newUUID(),
			ToolUseID:   p.Req.ToolUseID,
			Title:       spec.Title,
			Instruction: spec.Instruction,
			Context:     spec.Context,
			Success:     spec.Success,
			BudgetUSD:   spec.BudgetUSD,
		},
		pending: p,
		state:   StateQueued,
	}
	// Registered before the process exists, so a gate request from the child can
	// never arrive before acy knows who it belongs to.
	o.bySess[t.SessionID] = t.ID
	o.ledger = append(o.ledger, t)
	o.queue = append(o.queue, t)
	o.mu.Unlock()

	alog.Printf("dispatch: %s %q session=%s", t.ID, t.Title, t.SessionID)
	o.pump(ctx)
	return t.status(), nil
}

// pump starts as many queued tasks as the limit allows.
func (o *Orchestrator) pump(ctx context.Context) {
	for {
		o.mu.Lock()
		if o.closed || len(o.queue) == 0 || len(o.running) >= o.limit {
			o.mu.Unlock()
			return
		}
		t := o.queue[0]
		o.queue = o.queue[1:]
		o.running[t.ID] = t
		t.state = StateRunning
		t.started = o.now()
		cctx, cancel := context.WithCancel(ctx)
		t.cancel = cancel
		o.mu.Unlock()

		o.wg.Add(1)
		go o.run(cctx, t)
	}
}

// run owns one task from spawn to report. It always resolves the Pending — on
// success, on failure, and on cancellation — because the `acy mcp` process
// waiting on the other end of that socket belongs to the *parent's* process
// group. Killing the child does not release it, so a missed Resolve hangs the
// parent's turn forever.
func (o *Orchestrator) run(ctx context.Context, t *task) {
	defer o.wg.Done()
	defer alog.Recover("orchestrator.run")

	child, err := o.spawn(ctx, t.Task)
	if err != nil {
		o.finishFailed(t, err)
		return
	}

	o.emit(Event{TaskID: t.ID, Title: t.Title, Kind: KindStarted, Status: new(t.status())})

	if err := child.Send(taskPrompt(t.Task)); err != nil {
		child.Stop()
		o.finishFailed(t, err)
		return
	}

	out := o.consume(ctx, t, child)

	// Stop the child before finalising, always. Resolving the parent's call
	// first would tell it the task is over while the process is still alive and
	// possibly still writing to the working tree.
	child.Stop()

	switch out.kind {
	case outDone:
		o.finishDone(t, out.report)
	case outCancelled:
		o.finishCancelled(t, out.reason)
	case outAbandoned:
		o.finishCancelledSilently(t, out.reason)
	default:
		o.finishFailed(t, out.err)
	}
}

// What consume decided. It deliberately does not finalise the task itself, so
// that run can guarantee the child is dead first.
type outcomeKind int

const (
	outDone outcomeKind = iota
	outFailed
	outCancelled
	outAbandoned
)

type outcome struct {
	kind   outcomeKind
	report Report
	err    error
	reason string
}

// consume reads the child's stream to its first result event.
func (o *Orchestrator) consume(ctx context.Context, t *task, child Child) outcome {
	events := child.Events()
	for {
		select {
		case <-ctx.Done():
			return outcome{kind: outCancelled, reason: "the run was interrupted"}

		case <-t.pending.Done():
			// The parent died, so nobody is waiting for this any more. Stop
			// burning tokens on work whose result has nowhere to go.
			alog.Printf("dispatch: %s abandoned — the caller is gone", t.ID)
			return outcome{kind: outAbandoned, reason: "the caller went away"}

		case ev, open := <-events:
			if !open {
				return outcome{kind: outFailed, err: errStreamClosed}
			}
			o.note(t, ev)
			o.emit(Event{TaskID: t.ID, Title: t.Title, Kind: KindStream, Ev: ev})

			if ev.IsTurnEnd() {
				if r, ok := ParseReport(ev.StructuredOutput); ok {
					return outcome{kind: outDone, report: r}
				}
				return outcome{kind: outDone, report: degraded(noReportReason(ev))}
			}
		}
	}
}

// note accumulates what a child event tells us about cost and tokens. Cost is
// this process's running total so it is assigned; usage is per turn so it adds.
func (o *Orchestrator) note(t *task, ev driver.Event) {
	if !ev.IsTurnEnd() {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	t.cost = ev.TotalCostUSD
	if u := ev.Usage; u != nil {
		t.tokens.Add(state.Tokens{
			Input:       int64(u.InputTokens),
			Output:      int64(u.OutputTokens),
			CacheCreate: int64(u.CacheCreationInputTokens),
			CacheRead:   int64(u.CacheReadInputTokens),
		})
	}
}

func (o *Orchestrator) finishDone(t *task, r Report) {
	o.mu.Lock()
	t.state = StateDone
	t.outcome = r.Outcome
	t.ended = o.now()
	delete(o.running, t.ID)
	st := t.status()
	o.mu.Unlock()

	alog.Printf("dispatch: %s finished outcome=%s cost=$%.4f cache_read=%d",
		t.ID, r.Outcome, st.Cost, st.Tokens.CacheRead)

	t.pending.Resolve(mcp.Answer{Text: r.Render(t.ID, t.Title)})
	o.emit(Event{TaskID: t.ID, Title: t.Title, Kind: KindFinished, Report: &r, Status: &st})
	o.pumpDetached()
}

func (o *Orchestrator) finishFailed(t *task, err error) {
	o.mu.Lock()
	t.state = StateFailed
	t.ended = o.now()
	delete(o.running, t.ID)
	st := t.status()
	o.mu.Unlock()

	alog.Printf("dispatch: %s failed: %v", t.ID, err)
	t.pending.Resolve(mcp.Answer{Text: t.ID + " " + t.Title + " — FAILED\n" +
		"This task could not be run: " + err.Error() +
		". Nothing was reported. The repository may be untouched or partially changed; check before relying on it."})
	o.emit(Event{TaskID: t.ID, Title: t.Title, Kind: KindFailed, Err: err, Status: &st})
	o.pumpDetached()
}

func (o *Orchestrator) finishCancelled(t *task, reason string) {
	o.finishCancelledWith(t, reason, true)
}

// finishCancelledSilently is for when the caller itself is gone: there is
// nothing to resolve, because the socket it was waiting on has already closed.
func (o *Orchestrator) finishCancelledSilently(t *task, reason string) {
	o.finishCancelledWith(t, reason, false)
}

func (o *Orchestrator) finishCancelledWith(t *task, reason string, resolve bool) {
	o.mu.Lock()
	if t.state == StateDone || t.state == StateFailed || t.state == StateCancelled {
		o.mu.Unlock()
		return
	}
	t.state = StateCancelled
	t.ended = o.now()
	delete(o.running, t.ID)
	st := t.status()
	o.mu.Unlock()

	if resolve {
		t.pending.Resolve(mcp.Answer{Text: cancelText(reason)})
	}
	o.emit(Event{TaskID: t.ID, Title: t.Title, Kind: KindFailed, Err: errCancelled, Status: &st})
	o.pumpDetached()
}

func cancelText(reason string) string {
	return "(this task was cancelled — " + reason + ". It did not run to completion and reported nothing; " +
		"any work it had already done is still on disk.)"
}

// pumpDetached starts the next queued task. It runs on a background context
// because the context that carried the finished task is now cancelled.
func (o *Orchestrator) pumpDetached() { o.pump(context.Background()) }

// Cancel stops one task. Cancel and CancelAll both resolve the blocked call, so
// the parent gets an answer rather than hanging.
func (o *Orchestrator) Cancel(taskID, reason string) {
	o.mu.Lock()
	t := o.running[taskID]
	var queued *task
	for i, q := range o.queue {
		if q.ID == taskID {
			queued = q
			o.queue = append(o.queue[:i], o.queue[i+1:]...)
			break
		}
	}
	o.mu.Unlock()

	switch {
	case t != nil:
		// Cancel the context and let the run goroutine unwind: it is the only
		// thing holding the child, and it stops the process before resolving.
		// Reaching in to stop the child from here would race its assignment and
		// could resolve the parent's call while the child was still writing.
		t.cancel()
	case queued != nil:
		// Nothing is running it, so there is no goroutine to do the honours.
		o.finishCancelled(queued, reason)
	}
}

// CancelAll stops everything in flight and everything waiting. Bound to the
// interrupt key: interrupting the parent alone would leave an orphaned child
// burning tokens on work nobody will read.
func (o *Orchestrator) CancelAll(reason string) {
	o.mu.Lock()
	ids := make([]string, 0, len(o.running)+len(o.queue))
	for id := range o.running {
		ids = append(ids, id)
	}
	for _, q := range o.queue {
		ids = append(ids, q.ID)
	}
	o.mu.Unlock()

	for _, id := range ids {
		o.Cancel(id, reason)
	}
}

// TaskFor maps a child's claude session id to its task. It is how a gate
// request coming off the shared socket is attributed to the child that raised
// it — origin, not phase, is what says whether a countdown is owed.
func (o *Orchestrator) TaskFor(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	id, ok := o.bySess[sessionID]
	return id, ok
}

// Statuses is the ledger, oldest first.
func (o *Orchestrator) Statuses() []Status {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Status, 0, len(o.ledger))
	for _, t := range o.ledger {
		out = append(out, t.status())
	}
	return out
}

// Ledger is the run's tasks in the form the snapshot stores. A task still
// running has no EndedAt, which is what a restart reads to discover that a child
// died mid-edit.
func (o *Orchestrator) Ledger() []state.Task {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]state.Task, 0, len(o.ledger))
	for _, t := range o.ledger {
		out = append(out, state.Task{
			ID:        t.ID,
			SessionID: t.SessionID,
			Title:     t.Title,
			Outcome:   t.outcome,
			CostUSD:   t.cost,
			Tokens:    t.tokens,
			StartedAt: t.started,
			EndedAt:   t.ended,
		})
	}
	return state.TrimTasks(out)
}

// Active is how many tasks are running or waiting to run.
func (o *Orchestrator) Active() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.running) + len(o.queue)
}

// Totals is what every child has spent between them.
func (o *Orchestrator) Totals() (state.Tokens, float64, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var tok state.Tokens
	var cost float64
	for _, t := range o.ledger {
		tok.Add(t.tokens)
		cost += t.cost
	}
	return tok, cost, len(o.ledger)
}

// Close cancels everything and releases the event stream. Safe to call twice.
func (o *Orchestrator) Close() {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	o.closed = true
	o.mu.Unlock()

	o.CancelAll("the supervisor is shutting down")
	o.wg.Wait()
	close(o.events)
}

// emit delivers an event to the UI, dropping it if the buffer is full rather
// than stalling a child's stream behind a slow renderer. A dropped frame costs
// a transcript line; a stalled child holds a pipe open and blocks the process.
func (o *Orchestrator) emit(ev Event) {
	select {
	case o.events <- ev:
	default:
		alog.Printf("dispatch: dropped a %d event for %s (renderer behind)", ev.Kind, ev.TaskID)
	}
}

func (t *task) status() Status {
	return Status{
		Task:    t.Task,
		State:   t.state,
		Outcome: t.outcome,
		Started: t.started,
		Ended:   t.ended,
		Cost:    t.cost,
		Tokens:  t.tokens,
	}
}

// taskPrompt is what the child is actually told. It is assembled here rather
// than by the parent so that every task arrives in the same shape, and so the
// parent cannot accidentally spend its own context writing a preamble.
func taskPrompt(t Task) string {
	var b strings.Builder
	b.WriteString(t.Instruction)
	if len(t.Context) > 0 {
		b.WriteString("\n\nRelevant paths: " + strings.Join(t.Context, ", "))
	}
	if t.Success != "" {
		b.WriteString("\n\nDone means: " + t.Success)
	}
	return b.String()
}

// noReportReason explains, in the report itself, why there is no report — the
// child ran out of budget, was interrupted, or ignored the schema.
func noReportReason(ev driver.Event) string {
	switch {
	case ev.TerminalReason == "aborted_streaming":
		return "it was interrupted mid-turn"
	case ev.IsError && ev.Result != "":
		return "it ended in an error (" + clip(ev.Result, 200) + ")"
	case ev.IsError:
		return "it ended in an error"
	case ev.Subtype != "" && ev.Subtype != "success":
		return "it ended with subtype " + ev.Subtype
	default:
		return "it returned no structured output"
	}
}
