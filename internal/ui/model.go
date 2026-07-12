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

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/gate"
)

// Config holds the wiring the model needs beyond the driver.
type Config struct {
	Ctx       context.Context      // cancels driver processes on shutdown
	Launcher  Launcher             // starts a claude driver for a given phase
	GateReqs  <-chan *gate.Pending // nil if no gate is active
	Countdown time.Duration        // auto-approve delay per gated tool
	LogPath   string               // debug log file path (shown in the UI), if any
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
	input textinput.Model
	bar   progress.Model

	width, height int
	ready         bool

	entries   []entry
	sessionID string
	model     string
	mode      string
	cost      float64
	status    string
	ended     bool
	planReady bool
	logPath   string

	// gate / countdown state
	gateReqs  <-chan *gate.Pending
	countdown time.Duration
	pending   []*gateItem
	paused    bool
	now       time.Time

	// phase machine
	ctx             context.Context
	launcher        Launcher
	phase           Phase
	gen             int    // current driver generation
	turnText        string // assistant text accumulated for the current turn
	preloaded       bool   // done-check prompt is sitting in the input, awaiting send
	awaitingVerdict bool   // a done-check was sent; next turn end carries the verdict
	processing      bool   // a turn is in flight (model working)
	interrupted     bool   // user interrupted the current turn; don't auto-preload
}

// New builds the initial model bound to a started driver.
func New(drv *driver.Driver, cfg Config) Model {
	ti := textinput.New()
	ti.Placeholder = "type a message for Claude, Enter to send (Ctrl+C to quit)"
	ti.Prompt = "▸ "
	ti.Focus()
	ti.CharLimit = 0

	if cfg.Countdown <= 0 {
		cfg.Countdown = 30 * time.Second
	}

	m := Model{
		drv:       drv,
		input:     ti,
		bar:       progress.New(progress.WithoutPercentage()),
		status:    "planning",
		gateReqs:  cfg.GateReqs,
		countdown: cfg.Countdown,
		ctx:       cfg.Ctx,
		launcher:  cfg.Launcher,
		phase:     PhasePlan,
		logPath:   cfg.LogPath,
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
	cmds := []tea.Cmd{textinput.Blink, waitGate(m.gateReqs), tickCmd()}
	if m.drv != nil {
		cmds = append(cmds, waitEvent(m.drv.Events(), m.gen))
	} else if m.launcher != nil {
		cmds = append(cmds, launchCmd(m.ctx, m.launcher, PhasePlan, ""))
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
	m.vp.SetContent(renderEntries(m.entries, m.vp.Width))
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
		m.cost = ev.TotalCostUSD
		m.processing = false
		m.status = "idle"
		m.appendEntry(entry{kind: eTurn, body: fmt.Sprintf(
			"──── turn complete · stop=%s · $%.4f ────", ev.StopReason, ev.TotalCostUSD)})
	}
}

// ingestToolUse renders a tool call; ExitPlanMode gets a prominent plan box.
func (m *Model) ingestToolUse(b driver.ContentBlock) {
	if b.Name == "ExitPlanMode" {
		plan := planText(b.Input)
		if plan == "" {
			plan = "(the model proposed a plan but sent no text)"
		}
		m.appendEntry(entry{kind: ePlan, body: plan})
		m.planReady = true
		return
	}
	m.appendEntry(entry{kind: eTool, title: b.Name, body: toolArgs(b.Input)})
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
