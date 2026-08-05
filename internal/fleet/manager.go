package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
	"github.com/hweeks/always-click-yes/internal/gitops"
	"github.com/hweeks/always-click-yes/internal/state"
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
	Started    time.Time
	Ended      time.Time
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
)

// Event is anything an engineer produces, tagged with which one it came
// from. Status is a snapshot taken at the moment of emission.
type Event struct {
	EngineerID string
	Host       string
	Ticket     string
	Kind       EventKind

	Progress *engineerwire.Event
	Question *engineerwire.Question
	Result   *engineerwire.Result

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
	started time.Time
	ended   time.Time

	answers chan any
	cancel  context.CancelFunc
	counted bool // true while this engineer still holds a host slot
}

func (e *engineer) toStatus() EngineerStatus {
	return EngineerStatus{
		EngineerID: e.id,
		Ticket:     e.ticket,
		Title:      e.title,
		Host:       e.hostName,
		Branch:     e.branch,
		State:      e.state,
		Outcome:    e.outcome,
		PRURL:      e.prURL,
		CostUSD:    e.cost,
		Tokens:     e.tokens,
		Started:    e.started,
		Ended:      e.ended,
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

	mu       sync.Mutex
	seq      int
	byID     map[string]*engineer
	ledger   []*engineer // oldest first
	hostLoad map[string]int
	closed   bool

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
		events:      make(chan Event, defaultEventsBuffer),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
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
// value.
func buildSpec(cfg config.FleetConfig, req LaunchReq, ticket, branch string) engineerwire.Spec {
	budget := req.BudgetUSD
	if budget <= 0 && cfg.EngineerBudgetUSD != nil {
		budget = *cfg.EngineerBudgetUSD
	}
	var deadman float64
	if cfg.DeadmanHours != nil {
		deadman = *cfg.DeadmanHours
	}
	return engineerwire.Spec{
		Ticket:  ticket,
		Title:   req.Title,
		Brief:   req.Brief,
		Success: req.Success,

		BaseBranch: cfg.BaseBranch,
		Branch:     branch,

		Model:       cfg.EngineerModel,
		ChildModel:  cfg.EngineerChildModel,
		ChildEffort: cfg.EngineerEffort,

		BudgetUSD:    budget,
		DeadmanHours: deadman,
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

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return EngineerStatus{}, errClosed
	}
	host, err := m.pickHostLocked(req.Host)
	if err != nil {
		m.mu.Unlock()
		return EngineerStatus{}, err
	}
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
	engCtx, cancel := context.WithCancel(m.baseCtx)
	eng.cancel = cancel
	m.hostLoad[host.Name]++
	eng.counted = true
	m.byID[eng.id] = eng
	m.ledger = append(m.ledger, eng)
	m.mu.Unlock()

	spec := buildSpec(m.cfg, req, ticket, eng.branch)
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

// runEngineer keeps Follow attached to eng for the rest of its life. Follow
// only ever returns nil, once a Result has arrived (handleMsg has already
// recorded it), or ctx.Err() once engCtx ends — and engCtx only ends via
// Cancel or Close, both of which have already finalised eng's state and
// freed its slot before cancelling it. Either way there is nothing left for
// this goroutine to do once Follow returns.
func (m *Manager) runEngineer(engCtx context.Context, eng *engineer, t Transport) {
	defer m.wg.Done()
	defer alog.Recover("fleet.Manager.runEngineer")

	onMsg := func(msg any) { m.handleMsg(eng, msg) }
	onReconnect := func(gap int64, attempt int) {
		m.mu.Lock()
		st := eng.toStatus()
		m.mu.Unlock()
		alog.Printf("fleet: %s reconnecting host=%s gap=%d attempt=%d", eng.id, eng.hostName, gap, attempt)
		m.emit(Event{EngineerID: eng.id, Host: eng.hostName, Ticket: eng.ticket, Kind: KindReconnected, Gap: gap, Attempt: attempt, Status: st})
	}

	_ = Follow(engCtx, t, eng.wireID, 1, eng.answers, onMsg, onReconnect)
}

// handleMsg records what one decoded engineerwire message means for eng's
// status and forwards it onto Events.
func (m *Manager) handleMsg(eng *engineer, msg any) {
	switch v := msg.(type) {
	case engineerwire.Hello:
		// Nothing to record: eng.state is already "running" from Launch.

	case engineerwire.Event:
		m.mu.Lock()
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

	m.CancelAll("the fleet is shutting down")
	m.wg.Wait()
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
