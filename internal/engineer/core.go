// Package engineer is the headless runtime that executes one ticket end to
// end: given an engineerwire.Spec, it drives a supervisor through PLAN and
// AUTO-RUN, escalates every AskUserQuestion to the architect, and on
// completion pushes the branch and opens the PR. Every outbound message —
// including the final Result — travels through an engineerwire.Journal, so a
// detached architect can always reattach and see exactly what happened.
//
// See docs/engineer-protocol.md for the wire contract this package speaks,
// and AGENTS.md for the supervisor it drives.
package engineer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
	"github.com/hweeks/always-click-yes/internal/gitops"
	"github.com/hweeks/always-click-yes/internal/supervisor"
	"github.com/hweeks/always-click-yes/internal/verify"
	"github.com/hweeks/always-click-yes/internal/version"
)

// defaultDeadmanHours bounds a run with no explicit spec.DeadmanHours: an
// engineer nobody is watching must eventually give up rather than run (and
// spend) forever.
const defaultDeadmanHours = 24.0

// defaultPollInterval is how often Run reads the session snapshot in
// AUTO-RUN, both to journal events and to check for stall/completion.
const defaultPollInterval = time.Second

// defaultStallIdle is how long AUTO-RUN may sit idle, with no Finish call,
// before Run sends a continuation nudge.
const defaultStallIdle = 5 * time.Minute

// maxNudges is how many fruitless nudges Run sends before giving up and
// reporting a stall. "Fruitless" means the run went idle again, with no
// Finish, before the next nudge.
const maxNudges = 2

// costEventThreshold is the minimum cost movement Run journals as an
// EventCost — small enough to notice a run spending money, large enough that
// AUTO-RUN's ~1s poll doesn't journal a "cost" event on every tick.
const costEventThreshold = 0.05

// instantApproveCountdown is the smallest positive gate countdown. Countdown
// <= 0 does NOT mean "auto-approve instantly": ui.New defaults a
// non-positive Countdown to 30s (internal/ui/model.go), so an engineer that
// wants nobody-is-watching, approve-on-the-next-tick behaviour has to ask for
// the smallest countdown that still elapses on ui's own ~120ms tick, not for
// zero.
const instantApproveCountdown = time.Millisecond

// Config configures one engineer run.
type Config struct {
	Spec engineerwire.Spec

	EngineerID string // this engineer process's own id, for Hello and the PR footer
	Host       string // defaults to os.Hostname()
	ACYVersion string // defaults to version.String()

	ClonePath   string // the shared clone EnsureWorktree starts the worktree from
	WorktreeDir string // where the isolated worktree is created
	LogPath     string // alog destination for the supervisor this run drives, if any

	GitRunner gitops.Runner // defaults to gitops.DefaultRunner

	// VerifyCommands are the commands finalize runs in the worktree to check
	// the engineer's work, populated from Spec.VerifyCommands by the caller
	// (engineerd) rather than read from Spec directly inside Core, so a test
	// can set it without constructing a whole Spec.
	VerifyCommands []string
	// VerifyTimeout is the per-command wall-clock ceiling for VerifyCommands,
	// populated from Spec.VerifyTimeoutSeconds by the same caller, for the
	// same reason.
	VerifyTimeout time.Duration
	// VerifyRunner defaults to verify.DefaultRunner, exactly as GitRunner
	// defaults to gitops.DefaultRunner below.
	VerifyRunner verify.Runner

	AskTimeout   time.Duration // defaults to defaultAskTimeout (15m)
	PollInterval time.Duration // defaults to defaultPollInterval (1s)
	StallIdle    time.Duration // defaults to defaultStallIdle (5m)

	// builder constructs the session Core drives. Nil in production — Run
	// falls back to buildSupervisorSession — and set only by this package's
	// own white-box tests, to a scripted fake.
	builder builder
}

// Core runs one Config to completion.
type Core struct {
	cfg     Config
	journal *engineerwire.Journal

	askTimeout   time.Duration
	pollInterval time.Duration
	stallIdle    time.Duration

	mu      sync.Mutex
	pending map[string]*pendingQuestion

	cancelOnce   sync.Once
	cancelCh     chan struct{}
	cancelReason string

	finishOnce sync.Once
}

// NewCore builds a Core for cfg. journal is the already-open journal this run
// writes to; its lifetime belongs to the caller, who should Close it after
// Run returns.
func NewCore(cfg Config, journal *engineerwire.Journal) *Core {
	if cfg.AskTimeout <= 0 {
		cfg.AskTimeout = defaultAskTimeout
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.StallIdle <= 0 {
		cfg.StallIdle = defaultStallIdle
	}
	if cfg.GitRunner == nil {
		cfg.GitRunner = gitops.DefaultRunner
	}
	if cfg.VerifyRunner == nil {
		cfg.VerifyRunner = verify.DefaultRunner
	}
	if cfg.Host == "" {
		if h, err := os.Hostname(); err == nil {
			cfg.Host = h
		}
	}
	if cfg.ACYVersion == "" {
		cfg.ACYVersion = version.String()
	}
	return &Core{
		cfg:          cfg,
		journal:      journal,
		askTimeout:   cfg.AskTimeout,
		pollInterval: cfg.PollInterval,
		stallIdle:    cfg.StallIdle,
		pending:      make(map[string]*pendingQuestion),
		cancelCh:     make(chan struct{}),
	}
}

// Cancel stops the run at the next opportunity, if it has not already
// finished. Safe to call more than once and safe to call before Run starts.
func (c *Core) Cancel(reason string) {
	c.cancelOnce.Do(func() {
		c.cancelReason = reason
		close(c.cancelCh)
	})
}

// errCancelled marks a wait cut short by Cancel or ctx expiry, as distinct
// from the wait's own condition simply not being met yet.
var errCancelled = errors.New("engineer: run cancelled")

// Run drives cfg's ticket end to end and returns the Result it journaled.
// Every exit path — success, no commits, a push/PR failure, a stall, a
// cancellation, a panic — journals exactly one Result before returning; see
// finish.
func (c *Core) Run(ctx context.Context) engineerwire.Result {
	c.sendHello()

	deadmanHours := c.cfg.Spec.DeadmanHours
	if deadmanHours <= 0 {
		deadmanHours = defaultDeadmanHours
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(deadmanHours*float64(time.Hour)))
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			alog.Printf("engineer: panic: %v", r)
			c.finish(engineerwire.Result{Outcome: "failed", Summary: fmt.Sprintf("panic: %v", r)})
		}
	}()

	if err := gitops.EnsureWorktree(ctx, c.cfg.GitRunner, c.cfg.ClonePath, c.cfg.WorktreeDir,
		c.cfg.Spec.BaseBranch, c.cfg.Spec.Branch); err != nil {
		return c.finish(engineerwire.Result{Outcome: "failed", Summary: "preparing worktree: " + err.Error()})
	}

	sess, closeSession, err := c.build(ctx)
	if err != nil {
		return c.finish(engineerwire.Result{Outcome: "failed", Summary: "starting supervisor: " + err.Error()})
	}
	defer closeSession()
	// Quit before closeSession runs (defers unwind LIFO): the driver gets a
	// clean stop, then the hub/supervisor resources it depended on go away.
	defer sess.Quit()

	if err := c.submitBrief(ctx, sess); err != nil {
		if errors.Is(err, errCancelled) {
			return c.finish(engineerwire.Result{Outcome: "cancelled", Summary: c.exitReason(ctx)})
		}
		return c.finish(engineerwire.Result{Outcome: "failed", Summary: err.Error()})
	}

	dr := c.drive(ctx, sess)
	switch dr.kind {
	case driveFinished:
		return c.finish(c.finalize(ctx, dr.outcome, dr.summary, dr.cost, dr.tokens))
	case driveCancelled:
		return c.finish(engineerwire.Result{Outcome: "cancelled", Summary: dr.summary, CostUSD: dr.cost, Tokens: dr.tokens})
	default: // driveStalled
		return c.finish(engineerwire.Result{Outcome: "stalled", Summary: dr.summary, CostUSD: dr.cost, Tokens: dr.tokens})
	}
}

// build constructs the session this run drives, via cfg.builder if the tests
// set one, otherwise via the real supervisor.
func (c *Core) build(ctx context.Context) (session, func(), error) {
	b := c.cfg.builder
	if b == nil {
		b = buildSupervisorSession
	}
	spec := c.cfg.Spec
	f := supervisor.Flags{
		Cwd:          c.cfg.WorktreeDir,
		Model:        spec.Model,
		ChildModel:   spec.ChildModel,
		ChildEffort:  spec.ChildEffort,
		RunBudget:    spec.BudgetUSD,
		Countdown:    instantApproveCountdown,
		LogPath:      c.cfg.LogPath,
		InterceptAsk: c.interceptAsk,
		// Left unset, supervisor.Flags.PlanTools is empty, which driver.Options
		// reads as "the full built-in registry" — the same Write/Edit/Bash acy
		// run always keeps out of the supervising session. Without this, an
		// engineer's supervising session could write and commit directly
		// instead of delegating through Dispatch, the one guarantee this
		// architecture exists to make (AGENTS.md, "Why the parent cannot write").
		PlanTools: supervisor.DefaultParentTools,
	}
	return b(ctx, f)
}

// sendHello journals Hello unconditionally, before anything else. The wire
// protocol guarantees hello is always seq 1; journaling it first — ahead of
// the worktree/supervisor setup docs/engineer-protocol.md lists before it —
// is what keeps that guarantee true even when setup itself fails, so a dead
// worktree still gets a Hello followed by its one Result rather than an
// empty journal.
func (c *Core) sendHello() {
	if _, err := c.journal.Append(engineerwire.Hello{
		EngineerID: c.cfg.EngineerID,
		ACYVersion: c.cfg.ACYVersion,
		Host:       c.cfg.Host,
		PID:        os.Getpid(),
	}); err != nil {
		alog.Printf("engineer: hello journal append failed: %v", err)
	}
}

// finish journals r exactly once and returns it. Every return path in Run
// goes through this, which is what makes "one Result per exit" a property of
// the code rather than something each branch has to remember to uphold.
func (c *Core) finish(r engineerwire.Result) engineerwire.Result {
	c.finishOnce.Do(func() {
		if _, err := c.journal.Append(r); err != nil {
			alog.Printf("engineer: result journal append failed: %v", err)
		}
	})
	return r
}

// exitReason names why the run is stopping early, for a cancelled Result's
// summary: an explicit Cancel names its own reason, and a bare ctx expiry
// means the deadman timeout fired.
func (c *Core) exitReason(ctx context.Context) string {
	select {
	case <-c.cancelCh:
		if c.cancelReason != "" {
			return c.cancelReason
		}
		return "cancelled"
	default:
	}
	if ctx.Err() != nil {
		return fmt.Sprintf("deadman timeout reached: %v", ctx.Err())
	}
	return "cancelled"
}

// briefText is the one message the engineer opens with: the whole of what it
// tells claude about the job, plus the instruction to plan and then wait to
// be armed.
//
// It also says, explicitly, not to push the branch or open a PR: drive (see
// gitops.Push / gitops.CreatePR above) does both automatically once Finish is
// called. Without this line a capable child reaches for `gh pr create` on its
// own initiative — standard PR etiquette for a coding agent — and drive's own
// PR ends up a duplicate.
func briefText(spec engineerwire.Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Ticket: %s\nTitle: %s\n\n%s\n\nSuccess criteria:\n%s\n\n",
		spec.Ticket, spec.Title, spec.Brief, spec.Success)
	b.WriteString("Plan briefly, then wait; the run will be armed for you. Commit your work locally, " +
		"but do not push the branch or open a pull request yourself: pushing and opening the PR happen " +
		"automatically once you call Finish.")
	return b.String()
}

// continuationPrompt is the one nudge Run sends when AUTO-RUN goes idle
// without a Finish call.
const continuationPrompt = "The run has been idle with no Finish call. If work remains, continue it. " +
	"If the approved work is actually done, call Finish now."

// waitUntil polls sess.Snapshot() every c.pollInterval until cond is true,
// returning the snapshot that satisfied it. It gives up on ctx expiry or
// Cancel, returning errCancelled (wrapped) either way.
func (c *Core) waitUntil(ctx context.Context, sess session, cond func(Snapshot) bool) (Snapshot, error) {
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		snap := sess.Snapshot()
		if cond(snap) {
			return snap, nil
		}
		select {
		case <-ctx.Done():
			return snap, fmt.Errorf("%w: %v", errCancelled, ctx.Err())
		case <-c.cancelCh:
			return snap, errCancelled
		case <-ticker.C:
		}
	}
}

// submitBrief sends the ticket brief once the session has a driver attached,
// waits for that plan turn to end, then arms the run.
func (c *Core) submitBrief(ctx context.Context, sess session) error {
	if _, err := c.waitUntil(ctx, sess, func(s Snapshot) bool { return s.Ready }); err != nil {
		return fmt.Errorf("waiting for session to start: %w", err)
	}
	res := sess.Submit(briefText(c.cfg.Spec))
	if !res.Accepted {
		return fmt.Errorf("brief was refused: %s", res.Reason)
	}
	if _, err := c.waitUntil(ctx, sess, func(s Snapshot) bool { return !s.Busy && !s.Processing }); err != nil {
		return fmt.Errorf("waiting for the plan turn to end: %w", err)
	}
	armRes := sess.Arm()
	if !armRes.Accepted {
		return fmt.Errorf("arming was refused: %s", armRes.Reason)
	}
	return nil
}
