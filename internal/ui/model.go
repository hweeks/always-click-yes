// Package ui implements the Bubble Tea supervisor interface: it renders the
// Claude conversation, sends user messages, and (in later milestones) shows the
// permission countdown and drives the phase state machine.
package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/gate"
	"github.com/hweeks/always-click-yes/internal/judge"
	"github.com/hweeks/always-click-yes/internal/session"
)

// JudgeFunc runs an independent session that judges whether the approved plan is
// complete, given the plan text and the working session's final message.
type JudgeFunc func(ctx context.Context, plan, lastMsg string) (judge.Result, error)

// Config holds the wiring the model needs beyond the driver.
type Config struct {
	Ctx       context.Context      // cancels driver processes on shutdown
	Launcher  Launcher             // starts a claude driver for a given phase
	Judge     JudgeFunc            // independent completion judge (nil = manual verify)
	GateReqs  <-chan *gate.Pending // nil if no gate is active
	Countdown time.Duration        // auto-approve delay per gated tool
	LogPath   string               // debug log file path (shown in the UI), if any
	MaxLines  int                  // per-block line cap in the transcript (default 10)

	// Sessions lists resumable sessions for the /resume picker (nil = disabled).
	Sessions func() ([]session.Info, error)
}

// gateItem is a permission request the UI is counting down.
type gateItem struct {
	p         *gate.Pending
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

	entries   []entry
	sessionID string
	model     string
	mode      string
	status    string
	ended     bool
	planReady bool
	logPath   string
	maxLines  int

	// Billing. apiKeySource comes from claude's init event and says which account
	// actually paid; see billing().
	//
	// Cost needs two buckets because each claude process reports its own
	// total_cost_usd, cumulative within that process but reset by a --resume (and
	// the judge runs in a process of its own). So the current session's figure is
	// *assigned*, and finished sessions are *banked* — summing per turn would
	// double-count, and assigning across sessions would lose everything but the last.
	apiKeySource string
	costSettled  float64 // banked: previous driver generations + judge sessions
	costCurrent  float64 // the running session's latest total_cost_usd

	// slash-command / overlay state
	nextModel     string // --model override applied to the next launched session (/model)
	showHelp      bool   // the /help overlay is open
	sessionLister func() ([]session.Info, error)
	picking       bool           // the /resume session picker is open
	sessionList   []session.Info // sessions shown in the picker
	pickIdx       int            // selected row in the picker
	ask           *askState      // a pending AskUserQuestion the user is answering

	// gate / countdown state
	gateReqs  <-chan *gate.Pending
	countdown time.Duration
	pending   []*gateItem
	paused    bool
	now       time.Time

	// phase machine
	ctx             context.Context
	launcher        Launcher
	judge           JudgeFunc
	phase           Phase
	gen             int    // current driver generation
	turnText        string // assistant text accumulated for the current turn
	planBody        string // approved plan text, captured at ExitPlanMode
	preloaded       bool   // done-check prompt is sitting in the input, awaiting send
	awaitingVerdict bool   // a manual done-check was sent; next turn end carries the verdict
	verifying       bool   // an independent judge session is evaluating completion
	rounds          int    // auto-continue rounds taken in this run
	processing      bool   // a turn is in flight (model working)
	interrupted     bool   // user interrupted the current turn; don't auto-preload
	spinFrame       int    // advances every tick to animate the "working…" spinner
}

// New builds the initial model bound to a started driver.
func New(drv *driver.Driver, cfg Config) Model {
	// A textarea, not a textinput: the composer grows with its content (see
	// layout), and a textinput is single-line by construction — it scrolls
	// sideways and can only ever be one row tall.
	ta := textarea.New()
	ta.Placeholder = "type a message for Claude, Enter to send (Ctrl+C to quit)"
	ta.Prompt = "▸ "
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = maxInputRows
	ta.SetHeight(1)
	ta.Focus()
	// Enter sends, so a deliberate newline needs its own key; terminals can't
	// tell shift+enter from enter, so ctrl+j it is.
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j"), key.WithHelp("ctrl+j", "newline"))
	// ↑/↓ belong to the transcript, as /help promises. Leave the textarea's own
	// line movement on ctrl+p/ctrl+n.
	ta.KeyMap.LinePrevious = key.NewBinding(key.WithKeys("ctrl+p"))
	ta.KeyMap.LineNext = key.NewBinding(key.WithKeys("ctrl+n"))
	// The cursor-line highlight reads as a selection bar in a one-line composer.
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()

	if cfg.Countdown <= 0 {
		cfg.Countdown = 30 * time.Second
	}
	if cfg.MaxLines <= 0 {
		cfg.MaxLines = 10
	}

	m := Model{
		drv:       drv,
		input:     ta,
		bar:       progress.New(progress.WithoutPercentage()),
		status:    "planning",
		gateReqs:  cfg.GateReqs,
		countdown: cfg.Countdown,
		ctx:       cfg.Ctx,
		launcher:  cfg.Launcher,
		judge:     cfg.Judge,
		phase:     PhasePlan,
		logPath:   cfg.LogPath,
		maxLines:  cfg.MaxLines,

		sessionLister: cfg.Sessions,
	}
	m.appendEntry(entry{kind: eMeta, body: "Plan your task with Claude below. When the plan is ready, press Ctrl+G"})
	m.appendEntry(entry{kind: eMeta, body: "to arm — auto-run then approves each step after a countdown."})
	if m.logPath != "" {
		m.appendEntry(entry{kind: eMeta, body: "logging to " + m.logPath})
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
	cmds := []tea.Cmd{textarea.Blink, waitGate(m.gateReqs), tickCmd()}
	if m.drv != nil {
		cmds = append(cmds, waitEvent(m.drv.Events(), m.gen))
	} else if m.launcher != nil {
		cmds = append(cmds, launchCmd(m.ctx, m.launcher, LaunchSpec{Phase: PhasePlan}))
	}
	return tea.Batch(cmds...)
}

// appendEntry adds a structured entry to the transcript.
func (m *Model) appendEntry(e entry) { m.entries = append(m.entries, e) }

// transcript returns the plain-text concatenation of all entries (used by tests).
func (m *Model) transcript() string {
	var b strings.Builder
	for _, e := range m.entries {
		b.WriteString(e.title)
		b.WriteByte(' ')
		b.WriteString(e.body)
		b.WriteByte('\n')
	}
	return b.String()
}

// rebuild re-renders the transcript at the current width and scrolls to bottom.
func (m *Model) rebuild() {
	if !m.ready {
		return
	}
	m.vp.SetContent(renderEntries(m.entries, m.vp.Width, m.maxLines))
	m.vp.GotoBottom()
}

// ingest turns a decoded event into transcript entries and updates header state.
func (m *Model) ingest(ev driver.Event) {
	switch ev.Type {
	case driver.TypeSystem:
		if ev.IsInit() {
			m.sessionID = ev.SessionID
			m.model = ev.Model
			m.mode = ev.PermissionMode
			m.apiKeySource = ev.APIKeySource
			m.status = "ready"
			m.appendEntry(entry{kind: eMeta, body: fmt.Sprintf(
				"● session %s · model %s · mode %s", short(ev.SessionID), ev.Model, ev.PermissionMode)})
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
		m.processing = false
		m.status = "idle"
		m.appendEntry(entry{kind: eTurn, body: fmt.Sprintf(
			"──── turn complete · stop=%s · $%.4f ────", ev.StopReason, m.totalCost())})
	}
}

// totalCost is what the run has spent so far, across every claude process it has
// launched: the plan session, each resumed auto-run session, and every judge.
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

// intercepted lists the tools acy answers itself rather than letting claude run:
// ExitPlanMode is consumed to arm the run, AskUserQuestion is answered by the
// panel in ask.go. They must never reach the gate — a countdown for a tool we
// already handle would sit invisible behind the ask overlay and auto-approve a
// second, conflicting execution. See enqueue in gate.go.
var intercepted = map[string]bool{
	"ExitPlanMode":    true,
	"AskUserQuestion": true,
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

// ingestToolUse renders a tool call; ExitPlanMode gets a prominent plan box and
// AskUserQuestion opens the answer panel.
func (m *Model) ingestToolUse(b driver.ContentBlock) {
	switch baseToolName(b.Name) {
	case "ExitPlanMode":
		plan := planText(b.Input)
		if plan == "" {
			plan = "(the model proposed a plan but sent no text)"
		}
		m.planBody = plan
		m.appendEntry(entry{kind: ePlan, body: plan})
		m.planReady = true
		return
	case "AskUserQuestion":
		// A shape we can't parse falls through to a plain tool entry rather than
		// opening an empty panel the user could never dismiss.
		if a, ok := parseAsk(b.Input); ok {
			a.toolUseID = b.ID
			m.ask = a
			m.appendEntry(entry{kind: eMeta, body: "❓ Claude is asking a question — answer below"})
			return
		}
	}
	m.appendEntry(entry{kind: eTool, title: b.Name, body: toolBody(b.Name, b.Input)})
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

// toolBody renders a tool call's input as a multi-line preview for the
// transcript: the command for Bash, the file path plus content for Write, a
// minimal diff for Edit, the file path (and range) for Read. Everything else
// falls back to the one-line toolArgs summary. The line cap is applied at render
// time by clampBlock, so the full text is retained in the entry.
func toolBody(name string, raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return string(raw)
	}
	str := func(k string) string { s, _ := obj[k].(string); return s }

	switch name {
	case "Bash":
		return str("command")
	case "Write":
		return headed(fileHeader(obj), str("content"))
	case "Edit":
		return headed(fileHeader(obj), diffPreview(str("old_string"), str("new_string")))
	case "Read":
		return fileHeader(obj)
	}
	return toolArgs(raw)
}

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
