// Package ui implements the Bubble Tea supervisor interface: it renders the
// Claude conversation, sends user messages, and (in later milestones) shows the
// permission countdown and drives the phase state machine.
package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/fleet"
	"github.com/hweeks/always-click-yes/internal/gate"
	"github.com/hweeks/always-click-yes/internal/gitops"
	"github.com/hweeks/always-click-yes/internal/htmlrender"
	"github.com/hweeks/always-click-yes/internal/mcp"
	"github.com/hweeks/always-click-yes/internal/session"
	"github.com/hweeks/always-click-yes/internal/state"
)

// Config holds the wiring the model needs beyond the driver.
type Config struct {
	Ctx        context.Context      // cancels driver processes on shutdown
	Launcher   Launcher             // starts a claude driver for a given phase
	GateReqs   <-chan *gate.Pending // nil if no gate is active
	AskReqs    <-chan *mcp.Pending  // questions from acy's MCP server (nil = disabled)
	Countdown  time.Duration        // auto-approve delay per gated tool, and per question in AUTO-RUN
	LogPath    string               // debug log file path (shown in the UI), if any
	ConfigPath string               // .acy.json the run's settings came from (shown in the UI), if any
	// StartupNote is a human-readable notice shown once at startup, "" if
	// none — e.g. `acy arch` uses it to tell a human that fleet.stackMode
	// "ask" got silently downgraded to "off" because gh-stack wasn't
	// available. Empty for every caller but `acy arch`.
	StartupNote string
	MaxLines    int    // per-block line cap in the transcript (default 10)
	Cwd         string // the project this run belongs to (snapshot key)

	// RenderHTML asks for each entry to carry a server-rendered HTML fragment
	// (Frame.Entries[].HTML). Off by default, and `acy run` leaves it off: the
	// terminal has no use for HTML, and rendering markdown and re-highlighting
	// code into markup nobody reads would be work every ingested entry paid for.
	// The HTTP server behind the webview turns it on.
	RenderHTML bool

	// AltScreen puts the run in the alternate screen buffer. In Bubble Tea v2 the
	// alt-screen is a field on the tea.View the model returns, not a program
	// option, so the caller's choice has to reach the model rather than
	// tea.NewProgram. Headless callers (the tests, the e2e harness) leave it off.
	AltScreen bool

	// Sessions lists resumable sessions for the /resume picker (nil = disabled).
	Sessions func() ([]session.Info, error)

	// Dispatcher runs delegated tasks in child processes (nil = disabled, and a
	// Dispatch call is then refused rather than half-served).
	Dispatcher Dispatcher

	// Fleet runs the architect's remote engineers (nil = disabled, and the four
	// fleet tools are then refused with mcp.FleetUnavailable rather than
	// half-served). Only ever wired for a RoleArchitect session.
	Fleet FleetManager

	// Tickets is the architect's ticket board (nil = disabled, and ReadTickets/
	// UpdateTicket are then refused with mcp.TicketsUnavailable). Only ever
	// wired for a RoleArchitect session, alongside Fleet.
	Tickets TicketStore

	// GitRunner is AssembleStack's git/gh runner — the same gitops.Runner
	// shape the fleet's own PR watcher and engineer worktrees use. Only ever
	// wired for a RoleArchitect session, alongside Fleet; `acy arch` is the
	// only caller that sets it.
	GitRunner gitops.Runner

	// Trunk is the fleet's resolved base branch (fleet.baseBranch), the trunk
	// AssembleStack rebases and links against. Not a flag: `acy arch` is the
	// only caller that sets it, alongside Fleet.
	Trunk string

	// StackMode is the run's already-resolved effective fleet.stackMode,
	// never the raw configured value — the same value threaded to
	// ArchSystemPromptFor. Needed here too so AssembleStack can refuse when
	// it is "off". Not a flag: `acy arch` is the only caller that sets it.
	StackMode string

	// Resume is a session id to restore at startup: --resume/--continue set it, and
	// Init then rebuilds the run instead of cold-starting a plan session.
	Resume string

	// Restoring a run needs two sources, injected as functions so the ui tests never
	// touch a disk: LoadState/SaveState carry acy's own state (phase, plan, rounds,
	// cost — none of which claude records), and Replay reads claude's transcript back
	// as the events the UI would have ingested live. Any of them nil disables that
	// half: no persistence, or no transcript, but never a crash.
	LoadState func(id string) (state.Snapshot, bool, error)
	SaveState func(s state.Snapshot) error
	Replay    func(id string) ([]driver.Event, error)

	// Branch resolves the current git branch/SHA badge shown beside the phase
	// chip, cwd already baked in by the caller. Nil disables the badge:
	// internal/ui must never shell out to git itself.
	Branch func() (string, error)
}

// readOnlyParentTools are the tools that get no countdown when the supervising
// session itself calls them.
//
// Bash is deliberately absent. It is the one mutation vector that survives a
// read-only registry — `bash -c 'rm -rf'` is not a read — so it keeps its
// countdown. Everything here can only look.
//
// This is an allowlist rather than a denylist on purpose: a tool nobody thought
// about should count down, not sail through.
var readOnlyParentTools = map[string]bool{
	"Read": true, "Grep": true, "Glob": true,
	"WebFetch": true, "WebSearch": true,
	"ToolSearch": true, "TaskList": true, "TaskGet": true,
}

// answerTools are the tools a model uses to *return* something rather than to
// do something. They cannot touch the working tree, and a countdown on one buys
// no safety while costing the delay on every dispatched task.
var answerTools = map[string]bool{
	"StructuredOutput": true,
}

// gateItem is a permission request the UI is counting down.
type gateItem struct {
	p         *gate.Pending
	task      string        // the delegated task that raised it ("" = the parent)
	deadline  time.Time     // when it auto-approves (valid when not paused)
	remaining time.Duration // frozen time left (valid when paused)
}

// Model is the root Bubble Tea model.
type Model struct {
	drv *driver.Driver

	vp    viewport.Model
	input textarea.Model
	bar   progress.Model

	width, height int
	ready         bool
	altScreen     bool

	entries []entry
	// seq stamps each appended entry with an id a second front end can diff on.
	// It counts appends, not entries: /clear empties the slice and deliberately
	// leaves this alone, so an id is never handed out twice in one run.
	seq int
	// rc memoizes rebuild() across the 120ms tick — see rendercache.go.
	rc renderCache

	sessionID     string
	model         string
	mode          string
	status        string
	cooldownUntil time.Time // absolute retry deadline after a Claude 429
	ended         bool
	planReady     bool
	logPath       string
	configPath    string // .acy.json this run's settings came from, for the projection
	maxLines      int
	renderHTML    bool // stamp each entry with its HTML rendering (see Config.RenderHTML)

	// Billing. apiKeySource comes from claude's init event and says which account
	// actually paid; see billing().
	//
	// Cost needs two buckets because each claude process reports its own
	// total_cost_usd, cumulative within that process but reset by a --resume. So the
	// current session's figure is *assigned*, and finished sessions are *banked* —
	// summing per turn would double-count, and assigning across sessions would lose
	// everything but the last.
	apiKeySource string
	costSettled  float64 // banked: previous driver generations
	costCurrent  float64 // the running session's latest total_cost_usd

	// Tokens need none of that banking, because claude reports usage per turn
	// rather than per process — so these just accumulate, and go on accumulating
	// straight through a --resume.
	//
	// The split by spender is the point: work delegated to a child process costs
	// tokens, but it costs them over there, and parentTokens staying flat while
	// childTokens climbs is the whole thesis of the orchestrator made visible.
	parentTokens state.Tokens
	childTokens  state.Tokens
	childCost    float64
	dispatches   int
	tasks        []state.Task // the ledger, for the snapshot and the resume notice

	// interruptedTasks names tasks a restart caught mid-flight, so the resume
	// prompt can hand the decision to the session rather than guessing.
	interruptedTasks []string

	// resumedEngineers names engineers the restored snapshot still had
	// non-terminal, so the resume prompt can flag them the same way — the
	// fleet's counterpart to interruptedTasks. See resumeFleet.
	resumedEngineers []string

	// lastContext is the most recent turn's context size — a reading, not a
	// total. It is what shows a context growing without bound.
	lastContext   int
	contextWindow int // from modelUsage; 0 until a result event reports it

	// slash-command / overlay state
	nextModel     string // --model override applied to the next launched session (/model)
	showHelp      bool   // the /help overlay is open
	sessionLister func() ([]session.Info, error)
	picking       bool      // the /resume session picker is open
	sessionList   []pickRow // rows shown in the picker, built by pickRows when it opens
	pickIdx       int       // selected row in the picker
	ask           *askState // a pending AskUserQuestion the user is answering
	queueOpen     bool      // the /queue edit overlay is open
	queueCursor   int       // selected row in the queue-edit overlay

	// resume / persistence
	cwd       string // the project this run belongs to
	resumeID  string // session to restore at startup ("" = cold start)
	lineage   []string
	loadState func(id string) (state.Snapshot, bool, error)
	saveState func(s state.Snapshot) error
	replay    func(id string) ([]driver.Event, error)

	// branchResolver resolves the current git branch/SHA badge; nil disables
	// it. branch is the last value it resolved, "" until the first tick.
	branchResolver func() (string, error)
	branch         string

	// delegation. nil disables it: the tool is refused rather than half-served.
	dispatcher Dispatcher

	// the architect's fleet. nil disables it, the same way. fleetAwait is the
	// one Pending held for the next fleet event (like a gate holds); fleetBuf is
	// what arrived while nothing was holding one; engineers/fleetCapUsed/
	// fleetCapTotal are the mirror Frame and /fleet read — see syncFleet.
	fleet         FleetManager
	fleetAwait    *mcp.Pending
	fleetBuf      []fleet.Event
	engineers     []fleet.EngineerStatus
	fleetActive   int
	fleetCapUsed  int
	fleetCapTotal int

	// gitRunner, trunk and stackMode back AssembleStack — see Config.GitRunner/
	// Config.Trunk/Config.StackMode for what each means and who sets them.
	gitRunner gitops.Runner
	trunk     string
	stackMode string

	// tickets is the architect's ticket board. nil disables it, the same way
	// fleet does: ReadTickets/UpdateTicket are then refused rather than
	// half-served.
	tickets TicketStore

	// gate / countdown state
	gateReqs  <-chan *gate.Pending
	askReqs   <-chan *mcp.Pending
	countdown time.Duration
	pending   []*gateItem
	paused    bool
	now       time.Time

	// attached names the files a paste resolved into the composer, so the footer
	// can say the drag registered. It describes what is currently *in* the box —
	// clearComposer is the only place it dies, and it has to be, or a 📎 line
	// would sit under an empty composer promising a file the next message never
	// carries.
	attached []string

	// queued holds messages typed while the session was busy, waiting to go out
	// as one turn when it next falls idle. Plain queuedMsg values, deliberately:
	// Bubble Tea copies the Model on every Update, so anything with an internal
	// self-pointer in here is the strings.Builder crash again.
	//
	// It is never persisted. A queued message is transient intent, and one
	// surviving a crash to be delivered into a different phase is worse than one
	// that was lost.
	queued []queuedMsg
	// queueSeq mints each queuedMsg's id: monotonic, never reused, the same rule
	// entry.seq follows for the transcript.
	queueSeq int

	// phase machine
	ctx           context.Context
	launcher      Launcher
	phase         Phase
	gen           int       // current driver generation
	turnText      string    // assistant text accumulated for the current turn
	planBody      string    // approved plan text, captured at ExitPlanMode
	finishOutcome string    // "completed" | "abandoned", set once the session calls Finish
	finishSummary string    // the summary that came with it
	processing    bool      // a turn is in flight (model working)
	interrupted   bool      // user interrupted the current turn; don't auto-nudge
	spinFrame     int       // advances every tick to animate the "working…" spinner
	turnStart     time.Time // when the in-flight turn began, for the elapsed display
}

// New builds the initial model bound to a started driver.
func New(drv *driver.Driver, cfg Config) Model {
	// A textarea, not a textinput: the composer grows with its content (see
	// layout), and a textinput is single-line by construction — it scrolls
	// sideways and can only ever be one row tall.
	ta := textarea.New()
	ta.Placeholder = "type a message for Claude, Enter to send, Ctrl+J for a newline"
	ta.Prompt = "▸ "
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	// 0 = no limit, and it has to be 0. In bubbles MaxHeight is not merely the
	// visible-row cap it reads as: atContentLimit refuses InsertNewline once the
	// value holds MaxHeight *logical* lines, so maxInputRows here meant the ninth
	// newline in a message silently did nothing and a pasted plan document came
	// back out as a run-on sentence. The visible height is layout()'s job — it
	// clamps to maxInputRows and the textarea scrolls internally past that.
	ta.MaxHeight = 0
	ta.SetHeight(1)
	ta.Focus()
	// Enter sends, so a deliberate newline needs its own keys. shift+enter is only
	// a distinct key where the terminal speaks the Kitty keyboard protocol (which
	// Bubble Tea v2 negotiates on every run); everywhere else it arrives as a bare
	// Enter and sends, so alt+enter and ctrl+j are the portable fallbacks.
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter", "alt+enter", "ctrl+j"),
		key.WithHelp("ctrl+j", "newline"))
	// ↑/↓ belong to the transcript, as /help promises. Leave the textarea's own
	// line movement on ctrl+p/ctrl+n.
	ta.KeyMap.LinePrevious = key.NewBinding(key.WithKeys("ctrl+p"))
	ta.KeyMap.LineNext = key.NewBinding(key.WithKeys("ctrl+n"))
	// The cursor-line highlight reads as a selection bar in a one-line composer.
	// v2 keeps the styles behind a getter/setter pair rather than exposing
	// FocusedStyle/BlurredStyle directly, so read-modify-write is the whole idiom.
	taStyles := ta.Styles()
	taStyles.Focused.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(taStyles)

	if cfg.Countdown <= 0 {
		cfg.Countdown = 30 * time.Second
	}
	if cfg.MaxLines <= 0 {
		cfg.MaxLines = 10
	}

	m := Model{
		drv:        drv,
		input:      ta,
		bar:        progress.New(progress.WithoutPercentage()),
		status:     "planning",
		gateReqs:   cfg.GateReqs,
		askReqs:    cfg.AskReqs,
		countdown:  cfg.Countdown,
		ctx:        cfg.Ctx,
		launcher:   cfg.Launcher,
		phase:      PhasePlan,
		logPath:    cfg.LogPath,
		configPath: cfg.ConfigPath,
		maxLines:   cfg.MaxLines,
		altScreen:  cfg.AltScreen,
		renderHTML: cfg.RenderHTML,

		sessionLister: cfg.Sessions,
		dispatcher:    cfg.Dispatcher,
		fleet:         cfg.Fleet,
		tickets:       cfg.Tickets,
		gitRunner:     cfg.GitRunner,
		trunk:         cfg.Trunk,
		stackMode:     cfg.StackMode,

		cwd:       cfg.Cwd,
		resumeID:  cfg.Resume,
		loadState: cfg.LoadState,
		saveState: cfg.SaveState,
		replay:    cfg.Replay,

		branchResolver: cfg.Branch,
	}
	if m.resumeID != "" {
		m.status = "resuming…"
		m.appendEntry(entry{kind: eMeta, body: "↩ restoring session " + short(m.resumeID) + " …"})
	} else {
		m.appendEntry(entry{kind: eMeta, body: "Plan your task with Claude below. When the plan is ready, press Ctrl+G"})
		m.appendEntry(entry{kind: eMeta, body: "to arm — auto-run then approves each step after a countdown."})
	}
	if cfg.ConfigPath != "" {
		m.appendEntry(entry{kind: eMeta, body: "settings from " + cfg.ConfigPath})
	}
	if m.logPath != "" {
		m.appendEntry(entry{kind: eMeta, body: "logging to " + m.logPath})
	}
	if cfg.StartupNote != "" {
		m.appendEntry(entry{kind: eMeta, body: cfg.StartupNote})
	}
	return m
}

// --- external-event plumbing ---

type eventMsg struct {
	ev  driver.Event
	gen int
}
type streamClosedMsg struct{ gen int }

// waitEvent returns a command that blocks on the next event of driver generation
// gen. The generation lets stale events from a swapped-out driver be ignored.
func waitEvent(ch <-chan driver.Event, gen int) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return streamClosedMsg{gen: gen}
		}
		return eventMsg{ev: ev, gen: gen}
	}
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		textarea.Blink, waitGate(m.gateReqs), waitAsk(m.askReqs), tickCmd(),
		branchTickCmd(), resolveBranchCmd(m.branchResolver),
	}
	if m.dispatcher != nil {
		cmds = append(cmds, waitChild(m.dispatcher.Events()))
	}
	if m.fleet != nil {
		cmds = append(cmds, waitFleet(m.fleet.Events()))
	}
	switch {
	case m.drv != nil:
		cmds = append(cmds, waitEvent(m.drv.Events(), m.gen))
	case m.resumeID != "":
		// Restore first, launch second: the phase we come back in is the one the
		// snapshot recorded, not a cold PLAN.
		cmds = append(cmds, loadResumeCmd(m.resumeID, m.loadState, m.replay))
	case m.launcher != nil:
		cmds = append(cmds, launchCmd(m.ctx, m.launcher, LaunchSpec{Phase: PhasePlan}))
	}
	return tea.Batch(cmds...)
}

// appendEntry adds a structured entry to the transcript, stamping it with the
// next sequence number.
//
// seq is the id a non-terminal front end diffs on, so it must be monotonic and
// never reused — including across /clear, which drops the entries but leaves
// this counter alone. Resetting it there would hand a client two different
// entries with the same id and no way to tell which one it is looking at.
//
// raw defaults to the body with its ANSI removed. Most entries are plain text
// already, so that is simply the body back; the ones that aren't (tool calls)
// set raw and lang themselves at construction, where the language is still known.
func (m *Model) appendEntry(e entry) {
	m.entries = append(m.entries, m.stamp(e))
	m.markDirty()
}

// stamp gives an entry its id, fills in raw if the caller did not, and renders
// its HTML when a client asked for it. It is separate from appendEntry only
// because capReplay builds an entry it has to place at the *front* of the
// transcript; every other caller appends.
//
// seq is an identity, not a sort key. Entries always travel in transcript order,
// and capReplay's elision notice therefore carries a higher seq than the entries
// below it — it was minted later, describing older text. A client keys on seq to
// recognise an entry across frames, never to order them.
//
// The HTML is rendered here, once, for the same reason the syntax highlighting
// in body is: rebuild() used to re-render the transcript on every 120ms tick,
// and a projection built at read time would re-run goldmark and chroma over the
// whole history at that rate. rebuild() now memoizes across ticks (see
// rendercache.go), keyed on this very seq, but a resize still re-renders every
// entry — so this stays a one-time cost either way. Entries never change after
// they are stamped, so once is the right number of times. It is also why this
// is behind renderHTML — `acy run` would pay that cost for markup no terminal
// can display.
func (m *Model) stamp(e entry) entry {
	m.seq++
	e.seq = m.seq
	if e.raw == "" {
		e.raw = stripAnsi(e.body)
	}
	if m.renderHTML {
		// stripAnsi for the same reason Frame does it: a tool body is chroma's
		// terminal256 output, and escape codes mean nothing to a browser.
		e.html = htmlrender.Entry(entryKinds[e.kind], e.title, stripAnsi(e.body), e.raw, e.lang)
	}
	return e
}

// transcript returns the plain-text concatenation of all entries (used by tests
// and the e2e suite). Tool bodies carry syntax-highlighting ANSI; strip it so
// callers can match plain substrings.
func (m *Model) transcript() string {
	var b strings.Builder
	for _, e := range m.entries {
		b.WriteString(e.title)
		b.WriteByte(' ')
		b.WriteString(stripAnsi(e.body))
		b.WriteByte('\n')
	}
	return b.String()
}

// ansiRE matches SGR escape sequences (the only kind chroma and lipgloss emit).
var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripAnsi(s string) string { return ansiRE.ReplaceAllString(s, "") }

// ingest turns a decoded event into transcript entries and updates header state.
func (m *Model) ingest(ev driver.Event) {
	switch ev.Type {
	case driver.TypeSystem:
		if ev.IsInit() {
			m.adoptSession(ev.SessionID)
			m.model = ev.Model
			m.mode = ev.PermissionMode
			m.apiKeySource = ev.APIKeySource
			m.status = "ready"
			m.appendEntry(entry{kind: eMeta, body: fmt.Sprintf(
				"● session %s · model %s · mode %s", short(ev.SessionID), ev.Model, ev.PermissionMode)})
		}
	case driver.TypeRateLimit:
		if ev.RateLimitInfo != nil && ev.RateLimitInfo.ResetsAt > 0 && ev.RateLimitInfo.Status == "rejected" {
			until := time.Unix(ev.RateLimitInfo.ResetsAt, 0).Add(5 * time.Minute)
			m.cooldownUntil = until
			m.status = "cooling down — resumes " + until.Local().Format("3:04 PM")
			m.appendEntry(entry{kind: eWarn, body: "Claude rate limit reached — automatic retry is scheduled for " + until.Local().Format(time.Kitchen)})
		}
	case driver.TypeAssistant:
		for _, b := range ev.Message.Blocks() {
			switch b.Type {
			case driver.BlockText:
				if t := strings.TrimSpace(b.Text); t != "" {
					m.turnText += b.Text + "\n"
					m.appendEntry(entry{kind: eClaude, body: t})
				}
			case driver.BlockThinking:
				if t := strings.TrimSpace(b.Thinking); t != "" {
					m.appendEntry(entry{kind: eThinking, body: t})
				}
			case driver.BlockToolUse:
				m.ingestToolUse(b)
			}
		}
	case driver.TypeUser:
		for _, b := range ev.Message.Blocks() {
			if b.Type == driver.BlockToolResult {
				kind := eToolOK
				if b.IsError {
					kind = eToolErr
				}
				m.appendEntry(entry{kind: kind, body: rawText(b.Content)})
			}
		}
	case driver.TypeResult:
		// Assigned, not added: total_cost_usd is this process's running total.
		m.costCurrent = ev.TotalCostUSD
		// Added, not assigned: usage is this *turn's* count. Getting these two
		// the wrong way round is silent — both keep producing plausible numbers.
		m.parentTokens.Add(tokensOf(ev.Usage))
		m.lastContext = ev.Usage.ContextSize()
		m.noteContextWindow(ev.ModelUsage)
		m.processing = false
		m.status = "idle"
		m.appendEntry(entry{kind: eTurn, body: fmt.Sprintf(
			"──── turn complete · stop=%s · %s · $%.4f ────",
			ev.StopReason, ctxNote(m.lastContext, m.contextWindow), m.totalCost())})
	}
}

// tokensOf converts a turn's usage into the tally type. The conversion lives
// here rather than in state so that package state keeps depending on nothing.
func tokensOf(u *driver.Usage) state.Tokens {
	if u == nil {
		return state.Tokens{}
	}
	return state.Tokens{
		Input:       int64(u.InputTokens),
		Output:      int64(u.OutputTokens),
		CacheCreate: int64(u.CacheCreationInputTokens),
		CacheRead:   int64(u.CacheReadInputTokens),
	}
}

// noteContextWindow records the largest context window any model in the turn
// reported. A turn can touch several models (a small one does some internal
// work), and it is the main model's window that bounds the conversation.
func (m *Model) noteContextWindow(mu map[string]driver.ModelUsage) {
	for _, u := range mu {
		if u.ContextWindow > m.contextWindow {
			m.contextWindow = u.ContextWindow
		}
	}
}

// allTokens is the run's total across the parent and every child it dispatched.
func (m Model) allTokens() state.Tokens {
	t := m.parentTokens
	t.Add(m.childTokens)
	return t
}

// grandTotalCost includes child processes, which report their spend to the
// orchestrator rather than through this model's driver.
func (m Model) grandTotalCost() float64 { return m.totalCost() + m.childCost }

// totalCost is what the run has spent so far, across every claude process it has
// launched: the plan session and each resumed auto-run session.
func (m Model) totalCost() float64 { return m.costSettled + m.costCurrent }

// settleCost banks the running session's spend before its driver is replaced. A
// resumed session's process starts its own total from zero, so without this the
// plan phase's cost would vanish the moment auto-run began.
func (m *Model) settleCost() {
	m.costSettled += m.costCurrent
	m.costCurrent = 0
}

// billing names the account this run is charged to, per claude's init event:
// apiKeySource is "none" when the claude.ai login pays, and otherwise names the
// key's origin. Empty until the init event arrives.
func (m Model) billing() string {
	switch m.apiKeySource {
	case "":
		return ""
	case "none":
		return "subscription"
	default:
		return "API"
	}
}

// billingNote spells out, for the final tally, whether that dollar figure was
// actually charged. claude reports total_cost_usd either way, but on a
// subscription it is notional.
func (m Model) billingNote() string {
	switch m.billing() {
	case "subscription":
		return "subscription (not billed)"
	case "API":
		return "API (billed)"
	default:
		return "billing unknown"
	}
}

// intercepted lists the tools acy handles itself, so they must never raise a gate
// countdown. Two of them are its own MCP tools, answered over the ask socket by
// ask.go; ExitPlanMode stays listed because a session resumed from an interactive
// claude run can still carry one.
//
// A countdown here would be worse than redundant: it would sit invisible behind the
// ask overlay (which outranks the gate panel in both key routing and rendering) and
// then "auto-approve" a tool acy had already answered. See enqueue in gate.go.
var intercepted = map[string]bool{
	"ExitPlanMode":         true,
	mcp.ToolAsk:            true,
	mcp.ToolDispatch:       true,
	mcp.ToolFinish:         true,
	mcp.ToolPlan:           true,
	mcp.ToolLaunchEngineer: true,
	mcp.ToolAwait:          true,
	mcp.ToolAnswerEngineer: true,
	mcp.ToolFleetStatus:    true,
	mcp.ToolAssembleStack:  true,
	mcp.ToolReadTickets:    true,
	mcp.ToolUpdateTicket:   true,
	mcp.ToolCreateTicket:   true,
}

// baseToolName strips an "mcp__<server>__" prefix so an MCP-provided tool is
// matched by the same name as its built-in counterpart.
func baseToolName(name string) string {
	if !strings.HasPrefix(name, "mcp__") {
		return name
	}
	if i := strings.LastIndex(name, "__"); i > len("mcp__")-1 {
		return name[i+len("__"):]
	}
	return name
}

// ingestToolUse renders a tool call. PresentPlan (and ExitPlanMode, from a session
// resumed out of an interactive claude) gets a prominent plan box.
//
// AskUserQuestion is rendered but NOT acted on here. The panel is opened by the ask
// socket instead — see openAsk. The split matters: this event says only that a
// question was asked, while the socket request carries the handle that can answer
// it, and claude's turn is blocked on that answer. Opening the panel from here too
// would race the socket and produce a second panel with no way to reply. Because
// the two paths write different state, their arrival order is irrelevant.
func (m *Model) ingestToolUse(b driver.ContentBlock) {
	switch baseToolName(b.Name) {
	case "ExitPlanMode", mcp.ToolPlan:
		plan := planText(b.Input)
		if plan == "" {
			plan = "(the model proposed a plan but sent no text)"
		}
		m.planBody = plan
		m.appendEntry(entry{kind: ePlan, body: plan})
		m.planReady = true
		return
	case mcp.ToolAsk:
		return // rendered by openAsk, which owns the answer
	case mcp.ToolDispatch:
		return // rendered by startDispatch, which owns the task
	case mcp.ToolLaunchEngineer, mcp.ToolAwait, mcp.ToolAnswerEngineer, mcp.ToolFleetStatus, mcp.ToolAssembleStack:
		return // rendered when the corresponding Pending resolves — see fleet.go
	case mcp.ToolReadTickets, mcp.ToolUpdateTicket, mcp.ToolCreateTicket:
		return // rendered when the corresponding Pending resolves — see tickets.go
	case mcp.ToolFinish:
		// The run ending, read from the tool call itself. The `acy mcp` child
		// answers Finish locally, so this event is the only place the outcome
		// appears — exactly the arrangement PresentPlan already uses.
		outcome, summary := finishText(b.Input)
		m.finish(outcome, summary)
		return
	}
	name := baseToolName(b.Name)
	body, raw, lang := toolBodyParts(name, b.Input)
	m.appendEntry(entry{kind: eTool, title: b.Name, body: body, raw: raw, lang: lang, styled: styledTools[name]})
}

// --- small formatting helpers ---

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	return truncate(s, 200)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// planText extracts the "plan" field from an ExitPlanMode tool input.
func planText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj struct {
		Plan string `json:"plan"`
	}
	if json.Unmarshal(raw, &obj) == nil && strings.TrimSpace(obj.Plan) != "" {
		return obj.Plan
	}
	return string(raw)
}

// finishText pulls the outcome and summary out of a Finish call. A malformed
// call still ends the run: the session has said it is done, and refusing to
// believe it over a missing field would leave the run wedged with nothing
// driving it.
func finishText(raw json.RawMessage) (outcome, summary string) {
	var obj struct {
		Outcome string `json:"outcome"`
		Summary string `json:"summary"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &obj)
	}
	outcome = strings.TrimSpace(obj.Outcome)
	if outcome == "" {
		outcome = "completed"
	}
	return outcome, strings.TrimSpace(obj.Summary)
}

// toolBody renders a tool call's input as a multi-line preview for the
// transcript: the command for Bash, the file path plus content for Write, a
// minimal diff for Edit, the file path (and range) for Read. Everything else
// falls back to the one-line toolArgs summary. The line cap is applied at render
// time by clampBlock, so the full text is retained in the entry.
func toolBody(name string, raw json.RawMessage) string {
	body, _, _ := toolBodyParts(name, raw)
	return body
}

// toolBodyParts builds the transcript body and, beside it, the same text with no
// highlighting plus the language it is written in.
//
// One function rather than two so the two can't disagree: raw is the exact
// plain-text counterpart of body, produced by the same composition with
// highlight() left out, rather than a second guess at what body contains. The
// TUI takes body and ignores the rest; the JSON projection takes raw and lang
// and lets its own client colour them.
func toolBodyParts(name string, in json.RawMessage) (body, raw, lang string) {
	if len(in) == 0 {
		return "", "", ""
	}
	var obj map[string]any
	if json.Unmarshal(in, &obj) != nil {
		return string(in), string(in), ""
	}
	str := func(k string) string { s, _ := obj[k].(string); return s }

	path := str("file_path")
	if path == "" {
		path = str("path")
	}
	switch name {
	case "Bash":
		cmd := str("command")
		return highlight(cmd, "bash"), cmd, "bash"
	case "Write":
		header, content := fileHeader(obj), str("content")
		return headed(header, highlightFile(content, path)), headed(header, content), langForFile(path)
	case "Edit":
		header := fileHeader(obj)
		diff := diffPreview(str("old_string"), str("new_string"))
		return headed(header, highlight(diff, "diff")), headed(header, diff), "diff"
	case "Read":
		h := fileHeader(obj)
		return h, h, ""
	}
	args := toolArgs(in)
	return args, args, ""
}

// styledTools are the tools whose toolBody comes back syntax-highlighted, so
// their transcript entries must not be re-styled at render time — an outer
// foreground would fight the ANSI already in the text.
var styledTools = map[string]bool{"Bash": true, "Write": true, "Edit": true}

// fileHeader builds a "path (offset N, limit M)" line from a tool input, using
// file_path or path and any Read range fields.
func fileHeader(obj map[string]any) string {
	p, _ := obj["file_path"].(string)
	if p == "" {
		p, _ = obj["path"].(string)
	}
	var extra []string
	if v, ok := obj["offset"]; ok {
		extra = append(extra, fmt.Sprintf("offset %v", v))
	}
	if v, ok := obj["limit"]; ok {
		extra = append(extra, fmt.Sprintf("limit %v", v))
	}
	if len(extra) > 0 {
		return strings.TrimSpace(p + "  (" + strings.Join(extra, ", ") + ")")
	}
	return p
}

// headed joins a header line and a body block, dropping either if empty.
func headed(header, body string) string {
	header = strings.TrimSpace(header)
	body = strings.TrimRight(body, "\n")
	switch {
	case header == "":
		return body
	case strings.TrimSpace(body) == "":
		return header
	default:
		return header + "\n" + body
	}
}

// diffPreview renders old/new strings as a simple -/+ diff for an Edit preview.
func diffPreview(oldS, newS string) string {
	var b strings.Builder
	if strings.TrimSpace(oldS) != "" {
		for ln := range strings.SplitSeq(strings.TrimRight(oldS, "\n"), "\n") {
			b.WriteString("- " + ln + "\n")
		}
	}
	if strings.TrimSpace(newS) != "" {
		for ln := range strings.SplitSeq(strings.TrimRight(newS, "\n"), "\n") {
			b.WriteString("+ " + ln + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// toolArgs renders a tool's input as a readable (possibly multi-field) summary,
// preferring the fields that matter most for the common tools.
func toolArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return firstLine(string(raw))
	}
	if c, ok := obj["command"].(string); ok {
		return c
	}
	var parts []string
	for _, k := range []string{"file_path", "path", "pattern", "url", "old_string", "description"} {
		if v, ok := obj[k].(string); ok && v != "" {
			label := ""
			if k != "file_path" && k != "path" {
				label = k + ": "
			}
			parts = append(parts, label+firstLine(v))
		}
	}
	if len(parts) == 0 {
		for k, v := range obj {
			parts = append(parts, fmt.Sprintf("%s=%s", k, firstLine(fmt.Sprintf("%v", v))))
			if len(parts) >= 2 {
				break
			}
		}
	}
	return strings.Join(parts, "  ·  ")
}

// rawText extracts a readable string from a tool_result content field, which may
// be a JSON string or an array of {type:text,text:...} blocks.
func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []ContentText
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, " ")
	}
	return string(raw)
}

// ContentText is a minimal shape for text blocks inside a tool_result.
type ContentText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
