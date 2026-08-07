package fleet

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
	"github.com/hweeks/always-click-yes/internal/gitops"
	"github.com/hweeks/always-click-yes/internal/state"
	"github.com/hweeks/always-click-yes/internal/version"
)

// defaultEventsBuffer is generous on purpose: Events only drops progress
// frames on overflow (see emit), so a shallow buffer would turn ordinary
// backpressure into lost questions and results.
const defaultEventsBuffer = 256

// answerBuffer only ever needs to hold what arrives between an Answer/Cancel
// call and the next time Follow's bufferAnswers goroutine drains it — a few
// items at most, even across a reconnect.
const answerBuffer = 16

var errClosed = errors.New("fleet: the manager is shutting down")

// Engineer states.
const (
	StateLaunching = "launching"
	StateRunning   = "running"
	StateDone      = "done"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

func isTerminal(s string) bool {
	return s == StateDone || s == StateFailed || s == StateCancelled
}

// LaunchReq is one engineer to start.
type LaunchReq struct {
	Ticket  string
	Title   string
	Brief   string
	Success string

	Host      string  // optional: pin to this host by name, error if unknown or full
	BudgetUSD float64 // 0 = fleet default

	// StackOn is the ticket id of the PR this engineer's branch should stack
	// on top of. Empty launches against trunk exactly as today.
	StackOn string
}

// EngineerStatus is the ledger entry for one engineer.
type EngineerStatus struct {
	EngineerID string
	Ticket     string
	Title      string
	Host       string
	Branch     string
	State      string // launching | running | done | failed | cancelled
	Outcome    string
	PRURL      string
	CostUSD    float64
	Tokens     state.Tokens
	// LastSeq is the highest engineerwire seq this manager has observed for
	// the engineer — what a resume needs to re-follow from LastSeq+1 rather
	// than replaying its whole journal again.
	LastSeq int64
	Started time.Time
	Ended   time.Time
	// ProtocolVersion/ACYVersion are stamped from the engineer's Hello once
	// its handshake has passed — zero/empty until then, and left as-is for
	// an engineer resumed from mid-journal, whose Hello this process never
	// sees (see handleMsg's Hello case). What FleetStatus reads to show
	// version skew across a fleet.
	ProtocolVersion int
	ACYVersion      string

	// StackID and StackBase identify this engineer's place in a stack, if
	// any: StackID is the chain it belongs to, StackBase the parent branch
	// its own Branch is stacked on top of. Both empty for an unstacked
	// engineer launched against trunk.
	StackID   string
	StackBase string
}

// EventKind distinguishes what an Event is carrying, so a consumer never has
// to sniff at which pointer field is set.
type EventKind int

const (
	KindStarted     EventKind = iota // the engineer's process is up
	KindProgress                     // wraps an engineerwire.Event
	KindQuestion                     // wraps an engineerwire.Question
	KindResult                       // wraps an engineerwire.Result; terminal
	KindReconnected                  // Follow reattached after a drop
	KindFailed                       // terminal: Start failed, or the engineer was cancelled
	KindPR                           // wraps a PREvent from a PRWatcher; never dropped, like a result
	KindStack                        // wraps a *StackEvent from a StackKeeper; never dropped, like KindPR
)

// Event is anything an engineer produces, tagged with which one it came
// from. Status is a snapshot taken at the moment of emission.
//
// A KindPR event carries no EngineerID/Host/Ticket — a PR merge or close is
// observed from GitHub, not attributed to a particular engineer's Follow
// loop — so consumers must switch on Kind before reading those fields.
type Event struct {
	EngineerID string
	Host       string
	Ticket     string
	Kind       EventKind

	Progress *engineerwire.Event
	Question *engineerwire.Question
	Result   *engineerwire.Result
	PR       *PREvent
	Stack    *StackEvent

	Gap     int64
	Attempt int

	Err error

	Status EngineerStatus
}

// engineer is the mutable internal record; EngineerStatus is the value copy
// handed out.
type engineer struct {
	id       string
	ticket   string
	title    string
	hostName string
	branch   string
	wireID   string // engineerwire.Hello/StartAck's id, used only to talk to Transport

	state   string
	outcome string
	prURL   string
	cost    float64
	tokens  state.Tokens
	lastSeq int64
	started time.Time
	ended   time.Time

	answers chan any
	cancel  context.CancelFunc
	counted bool // true while this engineer still holds a host slot

	protocolVersion int
	acyVersion      string

	// stackID is the chain this engineer belongs to, if any — the key into
	// Manager.chains. Empty for an unstacked engineer.
	stackID string
	// stackBase is the parent's branch this engineer's own branch is
	// stacked on top of, populated only when it was launched with StackOn
	// (or restored by resumeOne from an entry that was). Empty otherwise.
	stackBase string

	// resumedMidJournal is true when this engineer was restored by Resume
	// from a LastSeq >= 1: its Follow re-attaches from LastSeq+1 > 1, and a
	// real journal never replays seq 1 (Hello) to a follower starting past
	// it. Its handshake already passed at the original Launch, in a process
	// this one may have no other record of, so handleMsg's Hello case must
	// not re-run the check against whatever a reattach happens to deliver.
	resumedMidJournal bool
}

func (e *engineer) toStatus() EngineerStatus {
	return EngineerStatus{
		EngineerID:      e.id,
		Ticket:          e.ticket,
		Title:           e.title,
		Host:            e.hostName,
		Branch:          e.branch,
		State:           e.state,
		Outcome:         e.outcome,
		PRURL:           e.prURL,
		CostUSD:         e.cost,
		Tokens:          e.tokens,
		LastSeq:         e.lastSeq,
		Started:         e.started,
		Ended:           e.ended,
		ProtocolVersion: e.protocolVersion,
		ACYVersion:      e.acyVersion,
		StackID:         e.stackID,
		StackBase:       e.stackBase,
	}
}

// Manager runs N concurrent engineers across the hosts in a fleet config: it
// picks a host, starts an engineer there, and keeps a Follow loop feeding a
// single Events stream and a status ledger, until each engineer reports a
// Result or is cancelled. It knows nothing about deciding *what* work to
// dispatch — that is the architect's job — the same split Transport already
// draws between running an engineer and deciding where.
type Manager struct {
	cfg         config.FleetConfig
	transports  func(config.FleetHost) Transport
	now         func() time.Time
	hostsByName map[string]config.FleetHost

	baseCtx    context.Context
	baseCancel context.CancelFunc

	prWatcher       *PRWatcher
	prCap           int
	prWatcherCancel context.CancelFunc // stops the forwarding goroutine; nil unless WithPRWatcher was used
	stackKeeper     *StackKeeper

	mu       sync.Mutex
	seq      int
	byID     map[string]*engineer
	ledger   []*engineer // oldest first
	hostLoad map[string]int
	closed   bool
	// chains maps a stack id to that chain's engineers, root first, newest
	// tip last — an ordering invariant maintained at every append (see
	// resolveStackOnLocked and resumeOne), never re-sorted.
	chains map[string][]*engineer
	// stackSeq mints stack ids ("s1", "s2", ...), the stack-chain
	// counterpart to seq. Protected by mu.
	stackSeq int
	// spentBefore is a resumed run's already-recorded engineer spend, seeded
	// via SeedSpent before this process's own ledger has caught up. See
	// spentLocked for why it is combined with the ledger sum by taking the
	// higher of the two rather than adding them.
	spentBefore float64
	// launched is set by the first Launch or Resume call. Resume must run
	// before any engineer is launched — a launch already assigns its "e1",
	// "e2" ids from seq, and admitting one before Resume has replayed a
	// prior ledger would let a fresh engineer collide with the ids Resume is
	// about to restore.
	launched bool

	events chan Event
	wg     sync.WaitGroup
}

// Option configures a Manager at construction time.
type Option func(*Manager)

// WithClock overrides time.Now, for deterministic Started/Ended timestamps
// in tests.
func WithClock(now func() time.Time) Option {
	return func(m *Manager) { m.now = now }
}

// WithEventsBuffer overrides the Events channel's buffer size.
func WithEventsBuffer(n int) Option {
	return func(m *Manager) {
		if n > 0 {
			m.events = make(chan Event, n)
		}
	}
}

// WithPRWatcher wires w's PREvents into Events() as KindPR — never dropped,
// the same rule a result gets — and makes Launch refuse once OpenCount()
// reaches prCap open acy/* PRs. prCap <= 0 disables the cap check.
//
// The manager only forwards w's events; it does not call w.Run itself. A
// caller that wants the watcher actually polling starts that loop
// separately (arch mode ties it to the supervisor's own ctx so its lifetime
// doesn't depend on the manager's), but Close still stops the forwarding
// goroutine this option starts.
func WithPRWatcher(w *PRWatcher, prCap int) Option {
	return func(m *Manager) {
		m.prWatcher = w
		m.prCap = prCap
	}
}

// WithStackKeeper wires k into forwardPRWatcher so an arch-mode chain gets
// registered as a real GitHub stack as its PRs open, and repaired with a
// sync whenever a merge lands on trunk underneath it. k is reacted to only
// inside forwardPRWatcher's own goroutine, so there is exactly one caller of
// Link/Sync and no need for StackKeeper to guard against concurrent calls
// itself. A WithStackKeeper with no WithPRWatcher is simply inert — nothing
// ever calls reactToStackTrigger without a watcher forwarding PREvents.
func WithStackKeeper(k *StackKeeper) Option {
	return func(m *Manager) { m.stackKeeper = k }
}

// NewManager builds a Manager for cfg. transports is injectable so tests can
// supply fakes instead of real processes; a nil transports defaults to
// ForHost.
func NewManager(cfg config.FleetConfig, transports func(config.FleetHost) Transport, opts ...Option) *Manager {
	if transports == nil {
		transports = ForHost
	}
	hostsByName := make(map[string]config.FleetHost, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		hostsByName[h.Name] = h
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:         cfg,
		transports:  transports,
		now:         time.Now,
		hostsByName: hostsByName,
		baseCtx:     ctx,
		baseCancel:  cancel,
		byID:        map[string]*engineer{},
		hostLoad:    map[string]int{},
		chains:      map[string][]*engineer{},
		events:      make(chan Event, defaultEventsBuffer),
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.prWatcher != nil {
		prCtx, prCancel := context.WithCancel(context.Background())
		m.prWatcherCancel = prCancel
		m.wg.Add(1)
		go m.forwardPRWatcher(prCtx)
	}
	return m
}

// forwardPRWatcher relays m.prWatcher's PREvents onto Events() as KindPR
// until ctx ends (Close cancels it via prWatcherCancel, before waiting on
// m.wg — using baseCtx here instead would deadlock, since baseCancel is only
// called after that wait).
func (m *Manager) forwardPRWatcher(ctx context.Context) {
	defer m.wg.Done()
	defer alog.Recover("fleet.Manager.forwardPRWatcher")

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-m.prWatcher.Events():
			if !ok {
				return
			}
			pr := ev
			m.emit(Event{Kind: KindPR, PR: &pr})
			if m.stackKeeper != nil {
				m.reactToStackTrigger(ctx, pr)
			}
		}
	}
}

// reactToStackTrigger decides, from one PREvent, whether m.stackKeeper has
// anything to do: a newly opened mid-stack PR needs registering, and any
// merge into trunk needs a repair sync. A merge into another acy/* branch
// (mid-stack) must never trigger a sync — an atomic stack merge reports each
// of its own PRs merging in turn, and reacting to every one of them would
// set off a rebase storm; only the merge that actually lands on trunk changes
// what the remaining chain needs to rebase onto.
func (m *Manager) reactToStackTrigger(ctx context.Context, ev PREvent) {
	switch {
	case ev.State == "open" && strings.HasPrefix(ev.Base, prHeadPrefix):
		m.reactToStackOpen(ctx, ev)
	case ev.State == "merged" && ev.Base == m.stackKeeper.trunk:
		m.logAndMaybeEmitStack(m.stackKeeper.Sync(ctx))
	}
}

func (m *Manager) reactToStackOpen(ctx context.Context, ev PREvent) {
	branches := m.ChainForBranch(ev.Head)
	if len(branches) < 2 {
		return // no chain yet, or a root with no stacked child (nothing to register)
	}
	m.logAndMaybeEmitStack(m.stackKeeper.Link(ctx, branches))
}

// logAndMaybeEmitStack applies the package's failure-handling rule for a
// StackEvent: success and an ErrStackConflict both reach the architect via
// KindStack (conflict must stop and surface to a human — never auto-resolve,
// never retried on a timer, only whatever the next natural PREvent triggers
// again). ErrStackLocked and any other gh failure are only logged, exactly
// PRWatcher.Run's "log and skip" discipline — a week-long unattended run must
// survive gh being briefly unreachable or another process briefly holding
// the stack lock.
func (m *Manager) logAndMaybeEmitStack(ev StackEvent) {
	switch {
	case ev.Err == nil:
		alog.Printf("fleet: stack %s ok", ev.Op)
		m.emit(Event{Kind: KindStack, Stack: &ev})
	case errors.Is(ev.Err, gitops.ErrStackConflict):
		alog.Printf("fleet: stack %s conflict on %q: %v", ev.Op, ev.Branch, ev.Err)
		m.emit(Event{Kind: KindStack, Stack: &ev})
	case errors.Is(ev.Err, gitops.ErrStackLocked):
		alog.Printf("fleet: stack %s: locked by another process, will retry on the next trigger: %v", ev.Op, ev.Err)
	default:
		alog.Printf("fleet: stack %s failed: %v", ev.Op, ev.Err)
	}
}

// Events is the unified stream of everything every engineer does. Created
// once and closed only by Close.
func (m *Manager) Events() <-chan Event { return m.events }

// engineerMaxLocked is *h.MaxEngineers, defaulting to 1 for a host built by
// hand rather than through config.LoadFile (which already resolves this).
func engineerMaxLocked(h config.FleetHost) int {
	if h.MaxEngineers == nil {
		return 1
	}
	return *h.MaxEngineers
}

// pickHostLocked resolves req's host pin, or picks the least-loaded host
// with a free slot. Callers hold m.mu.
func (m *Manager) pickHostLocked(pin string) (config.FleetHost, error) {
	if pin != "" {
		h, ok := m.hostsByName[pin]
		if !ok {
			return config.FleetHost{}, fmt.Errorf("fleet: unknown host %q", pin)
		}
		load, max := m.hostLoad[pin], engineerMaxLocked(h)
		if load >= max {
			return config.FleetHost{}, fmt.Errorf("fleet: host %q is full (%d/%d engineers)", pin, load, max)
		}
		return h, nil
	}

	if len(m.cfg.Hosts) == 0 {
		return config.FleetHost{}, errors.New("fleet: no hosts configured")
	}

	var best config.FleetHost
	haveBest := false
	usage := make([]string, 0, len(m.cfg.Hosts))
	for _, h := range m.cfg.Hosts {
		load, max := m.hostLoad[h.Name], engineerMaxLocked(h)
		usage = append(usage, fmt.Sprintf("%s %d/%d", h.Name, load, max))
		if load >= max {
			continue
		}
		if !haveBest || load < m.hostLoad[best.Name] {
			best = h
			haveBest = true
		}
	}
	if !haveBest {
		return config.FleetHost{}, fmt.Errorf("fleet: no capacity: %s", strings.Join(usage, ", "))
	}
	return best, nil
}

// buildSpec turns a LaunchReq into the wire Spec an engineer is started
// with, applying fleet defaults wherever req leaves a field at its zero
// value. budget is the already-resolved figure from effectiveBudgetLocked —
// buildSpec itself applies no ceiling logic, so a launch cannot bypass the
// run ceiling by asking for a larger budget than Launch already clamped.
// base and trunk are Launch's already-resolved BaseBranch/StackTrunk — the
// parent's branch and the fleet's real trunk when req is stacked, or
// cfg.BaseBranch and "" when it is not — so buildSpec itself makes no
// decision about stacking.
func buildSpec(cfg config.FleetConfig, req LaunchReq, ticket, branch, base, trunk string, budget float64) engineerwire.Spec {
	var deadman float64
	if cfg.DeadmanHours != nil {
		deadman = *cfg.DeadmanHours
	}
	var verifyTimeout int
	if cfg.VerifyTimeoutSeconds != nil {
		verifyTimeout = *cfg.VerifyTimeoutSeconds
	}
	return engineerwire.Spec{
		Ticket:  ticket,
		Title:   req.Title,
		Brief:   req.Brief,
		Success: req.Success,

		BaseBranch: base,
		StackTrunk: trunk,
		Branch:     branch,

		Model:       cfg.EngineerModel,
		ChildModel:  cfg.EngineerChildModel,
		ChildEffort: cfg.EngineerEffort,

		BudgetUSD:    budget,
		DeadmanHours: deadman,

		VerifyCommands:       cfg.VerifyCommands,
		VerifyTimeoutSeconds: verifyTimeout,
	}
}

func engineerID(n int) string { return fmt.Sprintf("e%d", n) }

// Launch picks a host, starts an engineer there, and returns once it has
// acknowledged — it does not wait for the engineer to finish. A goroutine
// keeps Follow running against the manager's single Events channel for the
// rest of the engineer's life.
func (m *Manager) Launch(ctx context.Context, req LaunchReq) (EngineerStatus, error) {
	ticket := strings.TrimSpace(req.Ticket)
	if ticket == "" {
		return EngineerStatus{}, errors.New("fleet: launch requires a ticket")
	}

	if err := m.checkPRCap(ctx); err != nil {
		return EngineerStatus{}, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return EngineerStatus{}, errClosed
	}
	if ceiling := m.runBudgetLocked(); ceiling > 0 {
		if spent := m.spentLocked(); spent >= ceiling {
			m.mu.Unlock()
			return EngineerStatus{}, fmt.Errorf(
				"fleet: the run budget of $%.2f is exhausted (spent $%.4f) — "+
					"ask the human to raise fleet.runBudgetUSD or Finish; do not retry automatically",
				ceiling, spent)
		}
	}
	var parent *engineer
	var stackID string
	base, trunk := m.cfg.BaseBranch, ""
	if stackOn := strings.TrimSpace(req.StackOn); stackOn != "" {
		var err error
		parent, stackID, err = m.resolveStackOnLocked(stackOn)
		if err != nil {
			m.mu.Unlock()
			return EngineerStatus{}, err
		}
		base, trunk = parent.branch, m.cfg.BaseBranch
	}

	m.launched = true
	host, err := m.pickHostLocked(req.Host)
	if err != nil {
		m.mu.Unlock()
		return EngineerStatus{}, err
	}
	budget := m.effectiveBudgetLocked(req.BudgetUSD)
	m.seq++
	eng := &engineer{
		id:       engineerID(m.seq),
		ticket:   ticket,
		title:    req.Title,
		hostName: host.Name,
		branch:   gitops.BranchName(ticket, req.Title),
		state:    StateLaunching,
		started:  m.now(),
		answers:  make(chan any, answerBuffer),
	}
	if parent != nil {
		eng.stackID = stackID
		eng.stackBase = parent.branch
		m.chains[stackID] = append(m.chains[stackID], eng)
	}
	engCtx, cancel := context.WithCancel(m.baseCtx)
	eng.cancel = cancel
	m.hostLoad[host.Name]++
	eng.counted = true
	m.byID[eng.id] = eng
	m.ledger = append(m.ledger, eng)
	m.mu.Unlock()

	spec := buildSpec(m.cfg, req, ticket, eng.branch, base, trunk, budget)
	transport := m.transports(host)
	ack, startErr := transport.Start(ctx, spec)

	m.mu.Lock()
	if startErr != nil {
		if !isTerminal(eng.state) {
			eng.state = StateFailed
			eng.ended = m.now()
			m.freeSlotLocked(eng)
		}
		st := eng.toStatus()
		m.mu.Unlock()
		cancel()
		alog.Printf("fleet: %s failed to start on host %s: %v", eng.id, host.Name, startErr)
		m.emit(Event{EngineerID: eng.id, Host: host.Name, Ticket: ticket, Kind: KindFailed, Err: startErr, Status: st})
		return st, fmt.Errorf("fleet: starting engineer on host %q: %w", host.Name, startErr)
	}
	eng.wireID = ack.EngineerID
	if !isTerminal(eng.state) {
		eng.state = StateRunning
	}
	st := eng.toStatus()
	m.mu.Unlock()

	alog.Printf("fleet: %s started ticket=%q host=%s branch=%s wire=%s", eng.id, ticket, host.Name, eng.branch, eng.wireID)
	m.emit(Event{EngineerID: eng.id, Host: host.Name, Ticket: ticket, Kind: KindStarted, Status: st})

	m.wg.Add(1)
	go m.runEngineer(engCtx, eng, transport)

	return st, nil
}

// checkPRCap refuses a launch when m.prWatcher shows prCap or more open
// acy/* PRs. It re-checks with a live Refresh before refusing — the cached
// snapshot may be up to a whole poll interval stale, and a merge that just
// landed shouldn't cost the architect a refusal it didn't need. nil when no
// watcher is configured or prCap <= 0 (uncapped).
func (m *Manager) checkPRCap(ctx context.Context) error {
	if m.prWatcher == nil || m.prCap <= 0 {
		return nil
	}
	if m.prWatcher.OpenCount() < m.prCap {
		return nil
	}
	if err := m.prWatcher.Refresh(ctx); err != nil {
		alog.Printf("fleet: pr cap refresh failed: %v", err)
	}
	if open := m.prWatcher.OpenCount(); open >= m.prCap {
		return prCapError(m.prCap, open, m.prWatcher.StackedCount(), m.prWatcher.OpenURLs())
	}
	return nil
}

// prCapError is the refusal Launch hands back verbatim to the architect
// (through startLaunchEngineer's wrapper): it names the cap, the count, and
// every open URL, and tells the model what to do next rather than just what
// it cannot do — the same shape as the mcp package's own refusal constants.
// stacked is how many further open acy/* PRs are mid-stack and therefore
// already excluded from open/prCap; it's surfaced as a parenthetical so the
// architect doesn't mistake the cap for lower headroom than it actually has.
func prCapError(prCap, open, stacked int, urls []string) error {
	if stacked > 0 {
		return fmt.Errorf("fleet: %d/%d acy PRs are open (%d more are mid-stack and uncapped) (%s) — Await merges before launching more",
			open, prCap, stacked, strings.Join(urls, ", "))
	}
	return fmt.Errorf("fleet: %d/%d acy PRs are open (%s) — Await merges before launching more",
		open, prCap, strings.Join(urls, ", "))
}

// resolveStackOnLocked resolves req.StackOn — a ticket id — against the
// manager's own ledger into the engineer to stack on top of, and the stack
// id the new engineer will join (freshly minted if parent is not yet part
// of a chain). Callers hold m.mu.
func (m *Manager) resolveStackOnLocked(stackOn string) (*engineer, string, error) {
	if m.cfg.StackMode == "off" {
		return nil, "", fmt.Errorf(
			"fleet: stacking is disabled for this fleet (fleet.stackMode is %q) — "+
				"set fleet.stackMode to \"chain\" or \"ask\" to stack engineers, or drop stackOn to launch against trunk",
			m.cfg.StackMode)
	}
	parent := m.findByTicketLocked(stackOn)
	if parent == nil {
		return nil, "", fmt.Errorf(
			"fleet: no engineer has been launched for ticket %q yet — launch it first, or drop stackOn to launch against trunk",
			stackOn)
	}
	// A parent's PR is the branch a child's own PR will target once a later
	// ticket registers the stack with GitHub (stack registration requires
	// every base in the chain to already exist as an open PR) — so a parent
	// that hasn't finished, or finished without opening one, has nothing yet
	// for a child to sit on top of.
	if !isTerminal(parent.state) || parent.prURL == "" {
		return nil, "", fmt.Errorf(
			"fleet: %s (ticket %q) has no open PR yet — a child can only stack once its parent has one; Await its result first",
			parent.id, stackOn)
	}
	// One child per parent: a stack is a linear chain that has to merge in
	// order, base branch first. A second child stacked on the same parent
	// branch would be a fork, not a stack — GitHub has no way to merge two
	// PRs targeting the same still-open PR's branch in sequence, and
	// whichever merges second would need a manual rebase anyway.
	if child := m.childOfLocked(parent); child != nil {
		return nil, "", fmt.Errorf(
			"fleet: %s (ticket %q) already stacks on %s's branch — a fork is not a stack and cannot merge linearly",
			child.id, child.ticket, parent.id)
	}

	stackID := parent.stackID
	if stackID == "" {
		m.stackSeq++
		stackID = fmt.Sprintf("s%d", m.stackSeq)
		parent.stackID = stackID
		m.chains[stackID] = append(m.chains[stackID], parent)
	}
	return parent, stackID, nil
}

// findByTicketLocked returns the most recently launched ledger entry for
// ticket, or nil. Callers hold m.mu.
func (m *Manager) findByTicketLocked(ticket string) *engineer {
	for i := len(m.ledger) - 1; i >= 0; i-- {
		if m.ledger[i].ticket == ticket {
			return m.ledger[i]
		}
	}
	return nil
}

// findByBranchLocked returns the most recently launched ledger entry whose
// branch is branch, or nil. Callers hold m.mu.
func (m *Manager) findByBranchLocked(branch string) *engineer {
	for i := len(m.ledger) - 1; i >= 0; i-- {
		if m.ledger[i].branch == branch {
			return m.ledger[i]
		}
	}
	return nil
}

// childOfLocked returns parent's child in its chain, if it has one. A
// parent not yet part of any chain (stackID == "") cannot have a child.
// Callers hold m.mu.
func (m *Manager) childOfLocked(parent *engineer) *engineer {
	if parent.stackID == "" {
		return nil
	}
	chain := m.chains[parent.stackID]
	for i, eng := range chain {
		if eng == parent {
			if i+1 < len(chain) {
				return chain[i+1]
			}
			return nil
		}
	}
	return nil
}

// runBudgetLocked is the fleet-wide ceiling on engineer spend, 0 meaning
// unlimited — config.FleetConfig.RunBudgetUSD left nil. Callers hold m.mu,
// though m.cfg itself never changes after construction.
func (m *Manager) runBudgetLocked() float64 {
	if m.cfg.RunBudgetUSD == nil {
		return 0
	}
	return *m.cfg.RunBudgetUSD
}

// spentLocked is the fleet's total engineer spend so far. Callers hold m.mu.
//
// It is the higher of the ledger's own cost sum and spentBefore rather than
// their sum: SeedSpent exists for a resumed run whose ledger has not yet
// been restored (cli/arch.go seeds it from the same snapshot that
// orchestrator.SeedSpent reads from), and Resume — which runs moments later
// — repopulates that very ledger with each engineer's own recorded cost.
// Adding the two would count that history twice; taking the max is correct
// whichever of the two has run so far, and still grows correctly once new
// engineers launch and add cost the seed never knew about.
func (m *Manager) spentLocked() float64 {
	var ledgerSum float64
	for _, eng := range m.ledger {
		ledgerSum += eng.cost
	}
	if m.spentBefore > ledgerSum {
		return m.spentBefore
	}
	return ledgerSum
}

// SeedSpent carries a resumed run's already-recorded engineer spend into a
// freshly constructed Manager, the fleet's counterpart to
// orchestrator.SeedSpent. cost is a cumulative total, not a delta — calling
// it more than once (or racing it against Resume restoring the same history
// into the ledger) never double-counts; see spentLocked.
func (m *Manager) SeedSpent(cost float64) {
	if cost <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cost > m.spentBefore {
		m.spentBefore = cost
	}
}

// effectiveBudgetLocked combines req's requested per-engineer budget, the
// fleet's configured default, and the money actually left under the run
// ceiling — mirroring orchestrator.effectiveBudgetLocked exactly, so a
// launch cannot bypass the fleet's own ceiling by asking for a bigger
// engineer budget than the run has left. Callers hold m.mu.
func (m *Manager) effectiveBudgetLocked(requested float64) float64 {
	budget := requested
	if budget <= 0 && m.cfg.EngineerBudgetUSD != nil {
		budget = *m.cfg.EngineerBudgetUSD
	}
	ceiling := m.runBudgetLocked()
	if ceiling <= 0 {
		return budget
	}
	remaining := ceiling - m.spentLocked()
	if budget <= 0 || budget > remaining {
		return remaining
	}
	return budget
}

// runEngineer keeps Follow attached to eng for the rest of its life. Follow
// only ever returns nil, once a Result has arrived (handleMsg has already
// recorded it), or ctx.Err() once engCtx ends — and engCtx only ends via
// Cancel or Close, both of which have already finalised eng's state and
// freed its slot before cancelling it. Either way there is nothing left for
// this goroutine to do once Follow returns.
func (m *Manager) runEngineer(engCtx context.Context, eng *engineer, t Transport) {
	defer m.wg.Done()
	defer alog.Recover("fleet.Manager.runEngineer")

	onMsg := func(msg any) { m.noteSeq(eng, msg); m.handleMsg(eng, msg) }
	onReconnect := func(gap int64, attempt int) {
		m.mu.Lock()
		st := eng.toStatus()
		m.mu.Unlock()
		alog.Printf("fleet: %s reconnecting host=%s gap=%d attempt=%d", eng.id, eng.hostName, gap, attempt)
		m.emit(Event{EngineerID: eng.id, Host: eng.hostName, Ticket: eng.ticket, Kind: KindReconnected, Gap: gap, Attempt: attempt, Status: st})
	}

	_ = Follow(engCtx, t, eng.wireID, 1, eng.answers, onMsg, onReconnect)
}

// noteSeq records the highest engineerwire seq observed for eng, so a later
// snapshot's Ledger() can tell a resume where to re-follow from.
func (m *Manager) noteSeq(eng *engineer, msg any) {
	s, ok := seqOf(msg)
	if !ok {
		return
	}
	m.mu.Lock()
	if s > eng.lastSeq {
		eng.lastSeq = s
	}
	m.mu.Unlock()
}

// runResumedEngineer re-attaches an engineer restored from a prior process's
// ledger, from fromSeq. Unlike runEngineer's forever-retrying Follow loop —
// which only ever meets a just-launched, just-acknowledged engineer — a
// resumed engineer may belong to a process that is simply gone: the host
// rebooted, the daemon crashed, its journal was cleaned up. So the first
// reattach failure that replays nothing at all is treated as terminal rather
// than transient: it marks the engineer failed, frees its slot, and stops
// retrying. A reattach that manages to replay even one message before a
// later drop is presumed alive and gets the ordinary KindReconnected
// treatment runEngineer gives any other drop.
func (m *Manager) runResumedEngineer(engCtx context.Context, cancel context.CancelFunc, eng *engineer, t Transport, fromSeq int64) {
	defer m.wg.Done()
	defer alog.Recover("fleet.Manager.runResumedEngineer")

	onMsg := func(msg any) { m.noteSeq(eng, msg); m.handleMsg(eng, msg) }
	onReconnect := func(gap int64, attempt int) {
		if attempt == 1 && gap == fromSeq-1 {
			m.mu.Lock()
			if !isTerminal(eng.state) {
				eng.state = StateFailed
				eng.outcome = "could not reattach after resume"
				eng.ended = m.now()
				m.freeSlotLocked(eng)
			}
			st := eng.toStatus()
			m.mu.Unlock()
			alog.Printf("fleet: %s resume: reattach failed with nothing replayed, giving up", eng.id)
			m.emit(Event{EngineerID: eng.id, Host: eng.hostName, Ticket: eng.ticket, Kind: KindFailed,
				Err: errors.New("could not reattach after resume — the daemon may be gone or its journal missing"), Status: st})
			cancel()
			return
		}
		m.mu.Lock()
		st := eng.toStatus()
		m.mu.Unlock()
		alog.Printf("fleet: %s reconnecting host=%s gap=%d attempt=%d", eng.id, eng.hostName, gap, attempt)
		m.emit(Event{EngineerID: eng.id, Host: eng.hostName, Ticket: eng.ticket, Kind: KindReconnected, Gap: gap, Attempt: attempt, Status: st})
	}

	_ = Follow(engCtx, t, eng.wireID, fromSeq, eng.answers, onMsg, onReconnect)
}

// handleMsg records what one decoded engineerwire message means for eng's
// status and forwards it onto Events.
func (m *Manager) handleMsg(eng *engineer, msg any) {
	switch v := msg.(type) {
	case engineerwire.Hello:
		// A resume that reattaches from a mid-journal seq (fromSeq > 1, set
		// on eng by resumeOne) never sees this in a real journal — Follow
		// only replays seq >= fromSeq, and Hello is always seq 1 — so its
		// handshake already passed at the original Launch and is not
		// re-checked here.
		if eng.resumedMidJournal {
			return
		}
		if v.ProtocolVersion != engineerwire.ProtocolVersion {
			reason := fmt.Sprintf(
				"protocol version mismatch: engineer speaks protocol v%d (acy %s), architect speaks protocol v%d (acy %s)",
				v.ProtocolVersion, v.ACYVersion, engineerwire.ProtocolVersion, version.String())
			alog.Printf("fleet: %s hello rejected: %s", eng.id, reason)
			_ = m.Cancel(eng.id, reason)
			return
		}
		m.mu.Lock()
		eng.protocolVersion = v.ProtocolVersion
		eng.acyVersion = v.ACYVersion
		m.mu.Unlock()

	case engineerwire.Event:
		m.mu.Lock()
		if v.Kind == engineerwire.EventCost {
			// A cost checkpoint is cumulative-in-process, the same
			// assign-not-add rule orchestrator.note applies to TotalCostUSD.
			eng.cost = v.CostUSD
		}
		st := eng.toStatus()
		m.mu.Unlock()
		ev := v
		m.emit(Event{EngineerID: eng.id, Host: eng.hostName, Ticket: eng.ticket, Kind: KindProgress, Progress: &ev, Status: st})

	case engineerwire.Question:
		m.mu.Lock()
		st := eng.toStatus()
		m.mu.Unlock()
		q := v
		m.emit(Event{EngineerID: eng.id, Host: eng.hostName, Ticket: eng.ticket, Kind: KindQuestion, Question: &q, Status: st})

	case engineerwire.Result:
		m.mu.Lock()
		if !isTerminal(eng.state) {
			eng.state = StateDone
			eng.outcome = v.Outcome
			eng.prURL = v.PRURL
			eng.cost = v.CostUSD
			eng.tokens = v.Tokens
			eng.ended = m.now()
			m.freeSlotLocked(eng)
		}
		st := eng.toStatus()
		m.mu.Unlock()
		alog.Printf("fleet: %s result outcome=%s cost=$%.4f host=%s", eng.id, v.Outcome, v.CostUSD, eng.hostName)
		r := v
		m.emit(Event{EngineerID: eng.id, Host: eng.hostName, Ticket: eng.ticket, Kind: KindResult, Result: &r, Status: st})
	}
}

// Answer routes text to the engineer that asked questionID.
func (m *Manager) Answer(engineerID, questionID, text string) error {
	m.mu.Lock()
	eng, ok := m.byID[engineerID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("fleet: unknown engineer %q", engineerID)
	}
	if isTerminal(eng.state) {
		m.mu.Unlock()
		return fmt.Errorf("fleet: engineer %q has already finished", engineerID)
	}
	answers := eng.answers
	m.mu.Unlock()

	answers <- engineerwire.Answer{QuestionID: questionID, Text: text}
	return nil
}

// cancelGrace is how long Cancel gives Follow's answers pipeline to actually
// forward the engineerwire.Cancel it just enqueued before the per-engineer
// ctx is torn down. Without it, cancelling immediately races Follow's
// bufferAnswers goroutine, which selects between the freshly buffered
// message and the same ctx becoming Done — a race that can pick "done" and
// drop the notice before it ever reaches the wire. Follow's own reattach
// backoff starts at a full second, so a bound two orders of magnitude
// smaller than that still leaves Follow's loop stopped promptly.
const cancelGrace = 50 * time.Millisecond

// stackKeeperCloseTimeout bounds how long Close waits for the stack
// keeper's worktree removal before giving up and shutting down anyway — a
// stuck `git worktree remove` must not hang the whole fleet's shutdown.
const stackKeeperCloseTimeout = 10 * time.Second

// Cancel stops one engineer: it forwards an engineerwire.Cancel over the
// same answers channel Follow already drains, then — after cancelGrace, so
// that message has a real chance to reach the wire — ends engCtx so
// Follow's otherwise-eternal reattach loop stops even if the engineer never
// acknowledges. State and slot are finalised here, synchronously, rather
// than left for runEngineer to discover later.
func (m *Manager) Cancel(engineerID, reason string) error {
	m.mu.Lock()
	eng, ok := m.byID[engineerID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("fleet: unknown engineer %q", engineerID)
	}
	if isTerminal(eng.state) {
		m.mu.Unlock()
		return nil
	}
	eng.state = StateCancelled
	eng.outcome = reason
	eng.ended = m.now()
	m.freeSlotLocked(eng)
	st := eng.toStatus()
	answers, cancel := eng.answers, eng.cancel
	m.mu.Unlock()

	select {
	case answers <- engineerwire.Cancel{Reason: reason}:
	default:
		alog.Printf("fleet: %s cancel message dropped, answers buffer full", engineerID)
	}
	time.AfterFunc(cancelGrace, cancel)

	alog.Printf("fleet: %s cancelled: %s", engineerID, reason)
	m.emit(Event{EngineerID: eng.id, Host: eng.hostName, Ticket: eng.ticket, Kind: KindFailed,
		Err: fmt.Errorf("cancelled: %s", reason), Status: st})
	return nil
}

// CancelAll stops every engineer that has not already finished, oldest
// first.
func (m *Manager) CancelAll(reason string) {
	m.mu.Lock()
	ids := make([]string, 0, len(m.ledger))
	for _, eng := range m.ledger {
		if !isTerminal(eng.state) {
			ids = append(ids, eng.id)
		}
	}
	m.mu.Unlock()

	for _, id := range ids {
		_ = m.Cancel(id, reason)
	}
}

// freeSlotLocked releases eng's host slot exactly once. Callers hold m.mu.
func (m *Manager) freeSlotLocked(eng *engineer) {
	if eng.counted {
		m.hostLoad[eng.hostName]--
		eng.counted = false
	}
}

// Statuses is the ledger, oldest first.
func (m *Manager) Statuses() []EngineerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]EngineerStatus, 0, len(m.ledger))
	for _, eng := range m.ledger {
		out = append(out, eng.toStatus())
	}
	return out
}

// Chain returns stackID's engineers' branches in bottom-to-top order — the
// branch closest to trunk first, the newest tip last — what a later ticket
// needs to register the stack with GitHub in the right order. Empty for an
// unknown or empty stackID.
func (m *Manager) Chain(stackID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chainLocked(stackID)
}

// chainLocked is Chain's body, shared with ChainForBranch. Callers hold m.mu.
func (m *Manager) chainLocked(stackID string) []string {
	chain := m.chains[stackID]
	out := make([]string, len(chain))
	for i, eng := range chain {
		out[i] = eng.branch
	}
	return out
}

// ChainForBranch is Chain, looked up by one of the chain's own branches
// rather than its stack id — what reactToStackOpen needs when all a PREvent
// hands it is the head branch that just opened a PR. Empty if branch is
// unknown or belongs to no chain.
func (m *Manager) ChainForBranch(branch string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	eng := m.findByBranchLocked(branch)
	if eng == nil || eng.stackID == "" {
		return nil
	}
	return m.chainLocked(eng.stackID)
}

// Ledger is the run's engineers in the form the snapshot stores — the fleet's
// counterpart to orchestrator.Ledger. An engineer still running has a
// non-terminal State and no EndedAt, which is what Resume reads to tell a
// finished engineer from one that needs re-following.
func (m *Manager) Ledger() []state.Engineer {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]state.Engineer, 0, len(m.ledger))
	for _, eng := range m.ledger {
		out = append(out, state.Engineer{
			EngineerID: eng.id,
			WireID:     eng.wireID,
			Ticket:     eng.ticket,
			Title:      eng.title,
			Host:       eng.hostName,
			Branch:     eng.branch,
			StackID:    eng.stackID,
			StackBase:  eng.stackBase,
			State:      eng.state,
			Outcome:    eng.outcome,
			PRURL:      eng.prURL,
			CostUSD:    eng.cost,
			Tokens:     eng.tokens,
			LastSeq:    eng.lastSeq,
			StartedAt:  eng.started,
			EndedAt:    eng.ended,
		})
	}
	return out
}

// Resume re-establishes the fleet's ledger from a prior process's snapshot:
// every entry is restored into the ledger as-is, and every entry still
// non-terminal gets its Follow loop re-attached from LastSeq+1 on its
// recorded host — the journal replays anything missed, including a Result
// that landed while the architect was dead, so a finished engineer resolves
// itself without any further action here. It also rebuilds m.chains from
// those same entries, since a resumed architect must be able to launch a
// child onto a chain built by a process that is now gone.
//
// It must run before any engineer is launched: a launch assigns its own
// "e1", "e2" ids starting from the manager's own counter, and admitting one
// before the prior ledger is restored would risk a fresh engineer's id
// colliding with — or simply outrunning — the ids being restored. Chain
// rebuilding shares that same constraint, for the same reason: entries are
// restored — and hence chains rebuilt — in ledger order (oldest first),
// which is guaranteed to be bottom-to-top order because a parent's launch
// always chronologically precedes any child stacked on it (a child cannot
// be launched until the parent has an open PR).
func (m *Manager) Resume(ctx context.Context, entries []state.Engineer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.launched {
		m.mu.Unlock()
		return errors.New("fleet: Resume must run before any engineer is launched")
	}
	m.launched = true
	for _, e := range entries {
		if n, ok := parseEngineerSeq(e.EngineerID); ok && n > m.seq {
			m.seq = n
		}
		if n, ok := parseStackSeq(e.StackID); ok && n > m.stackSeq {
			m.stackSeq = n
		}
	}
	m.mu.Unlock()

	for _, e := range entries {
		m.resumeOne(e)
	}
	return nil
}

// parseEngineerSeq extracts n from an "eN"-shaped ledger id, so Resume can
// seed the manager's own counter past every id it is restoring — otherwise
// the next fresh Launch would start back at "e1" and collide with a restored
// entry of the same name.
func parseEngineerSeq(id string) (int, bool) {
	if !strings.HasPrefix(id, "e") {
		return 0, false
	}
	n, err := strconv.Atoi(id[1:])
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseStackSeq extracts n from an "sN"-shaped stack id, the stackSeq
// counterpart to parseEngineerSeq.
func parseStackSeq(id string) (int, bool) {
	if !strings.HasPrefix(id, "s") {
		return 0, false
	}
	n, err := strconv.Atoi(id[1:])
	if err != nil {
		return 0, false
	}
	return n, true
}

// resumeOne restores one ledger entry. A terminal entry is simply
// re-recorded, so Statuses/frame show it in history; a non-terminal one also
// gets a Follow loop re-attached in the background.
func (m *Manager) resumeOne(e state.Engineer) {
	eng := &engineer{
		id:        e.EngineerID,
		wireID:    e.WireID,
		ticket:    e.Ticket,
		title:     e.Title,
		hostName:  e.Host,
		branch:    e.Branch,
		stackID:   e.StackID,
		stackBase: e.StackBase,
		state:     e.State,
		outcome:   e.Outcome,
		prURL:     e.PRURL,
		cost:      e.CostUSD,
		tokens:    e.Tokens,
		lastSeq:   e.LastSeq,
		started:   e.StartedAt,
		ended:     e.EndedAt,
		answers:   make(chan any, answerBuffer),
		// e.LastSeq >= 1 means the re-follow starts past seq 1 (Hello) —
		// see handleMsg's Hello case for why that skips the handshake check.
		resumedMidJournal: e.LastSeq >= 1,
	}

	m.mu.Lock()
	m.byID[eng.id] = eng
	m.ledger = append(m.ledger, eng)
	if e.StackID != "" {
		m.chains[e.StackID] = append(m.chains[e.StackID], eng)
	}
	if !e.Unfinished() {
		m.mu.Unlock()
		return
	}

	host, ok := m.hostsByName[e.Host]
	if !ok {
		eng.state = StateFailed
		eng.outcome = fmt.Sprintf("unknown host %q on resume", e.Host)
		eng.ended = m.now()
		st := eng.toStatus()
		m.mu.Unlock()
		alog.Printf("fleet: resume %s: unknown host %q", eng.id, e.Host)
		m.emit(Event{EngineerID: eng.id, Host: e.Host, Ticket: eng.ticket, Kind: KindFailed,
			Err: fmt.Errorf("fleet: unknown host %q", e.Host), Status: st})
		return
	}
	m.hostLoad[host.Name]++
	eng.counted = true
	engCtx, cancel := context.WithCancel(m.baseCtx)
	eng.cancel = cancel
	m.mu.Unlock()

	fromSeq := e.LastSeq + 1
	alog.Printf("fleet: resuming %s ticket=%q host=%s from seq=%d", eng.id, eng.ticket, host.Name, fromSeq)

	transport := m.transports(host)
	m.wg.Add(1)
	go m.runResumedEngineer(engCtx, cancel, eng, transport, fromSeq)
}

// Active is how many engineers are launching or running.
func (m *Manager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, eng := range m.ledger {
		if !isTerminal(eng.state) {
			n++
		}
	}
	return n
}

// Capacity is how many engineer slots are in use across every host, and how
// many exist in total.
func (m *Manager) Capacity() (used, total int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.cfg.Hosts {
		used += m.hostLoad[h.Name]
		total += engineerMaxLocked(h)
	}
	return used, total
}

// Close cancels every engineer, waits for their Follow loops to unwind, and
// releases the Events channel. Safe to call twice.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.mu.Unlock()

	if m.prWatcherCancel != nil {
		m.prWatcherCancel()
	}
	m.CancelAll("the fleet is shutting down")
	m.wg.Wait()
	if m.stackKeeper != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), stackKeeperCloseTimeout)
		m.stackKeeper.Close(closeCtx)
		cancel()
	}
	m.baseCancel()
	close(m.events)
}

// emit delivers ev to Events. Only KindProgress is dropped on overflow — a
// lost progress line costs a transcript entry, but a lost question, result
// or failure corrupts the run, so those block until there is room
// (orchestrator.emit documents the looser tradeoff this deliberately
// tightens).
func (m *Manager) emit(ev Event) {
	if ev.Kind == KindProgress {
		select {
		case m.events <- ev:
		default:
			alog.Printf("fleet: dropped a progress event for %s (renderer behind)", ev.EngineerID)
		}
		return
	}
	m.events <- ev
}
