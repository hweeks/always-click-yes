package ui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/gate"
	"github.com/hweeks/always-click-yes/internal/mcp"
)

// --- gate event plumbing ---

type gateMsg struct{ p *gate.Pending }
type gateClosedMsg struct{}
type tickMsg time.Time

// askMsg carries a question claude is blocked on, arriving from acy's own MCP
// server over the ask socket.
type askMsg struct{ p *mcp.Pending }
type askClosedMsg struct{}

// waitGate blocks on the next incoming permission request.
func waitGate(ch <-chan *gate.Pending) tea.Cmd {
	return func() tea.Msg {
		if ch == nil {
			return nil
		}
		p, ok := <-ch
		if !ok {
			return gateClosedMsg{}
		}
		return gateMsg{p}
	}
}

// waitAsk blocks on the next incoming question.
func waitAsk(ch <-chan *mcp.Pending) tea.Cmd {
	return func() tea.Msg {
		if ch == nil {
			return nil
		}
		p, ok := <-ch
		if !ok {
			return askClosedMsg{}
		}
		return askMsg{p}
	}
}

// tickInterval drives both the gate countdown and the working spinner; keep it
// brisk enough for smooth spinner motion.
const tickInterval = 120 * time.Millisecond

func tickCmd() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// enqueue records a new pending gate with a fresh countdown deadline.
//
// Tools acy intercepts and answers itself (see intercepted in model.go) are
// allowed straight through and never queued. The PreToolUse hook matches "*", so
// in auto-run an AskUserQuestion would otherwise raise a gate at the same moment
// ingestToolUse opens the ask panel — and the panel wins both the key-routing and
// render races, leaving a countdown ticking invisibly until it auto-approved a
// duplicate execution of a tool the user had already answered.
func (m *Model) enqueue(p *gate.Pending) {
	tool := baseToolName(p.Input.ToolName)
	if intercepted[tool] {
		p.Resolve(gate.Decision{Behavior: gate.Allow, Reason: "handled by acy"})
		alog.Printf("gate: pass-through tool=%s (intercepted by acy)", p.Input.ToolName)
		return
	}
	// PLAN is a research phase with a human watching, and it now runs with the hook
	// wired (it no longer uses --permission-mode plan, which refuses MCP tool calls
	// and would make the question picker unreachable). Its safety comes from the
	// --tools registry instead: Write, Edit and friends do not exist there at all.
	// Bash is the one mutation vector that survives, so it keeps its countdown; a
	// countdown on every Read and Grep would just train the user to stop looking.
	if m.phase == PhasePlan && tool != "Bash" {
		p.Resolve(gate.Decision{Behavior: gate.Allow, Reason: "read-only tool during planning"})
		alog.Printf("gate: pass-through tool=%s (plan phase, read-only)", p.Input.ToolName)
		return
	}
	it := &gateItem{p: p}
	if m.paused {
		it.remaining = m.countdown
	} else {
		it.deadline = m.now.Add(m.countdown)
	}
	m.pending = append(m.pending, it)
	alog.Printf("gate: request tool=%s use_id=%s", p.Input.ToolName, p.Input.ToolUseID)
	m.appendEntry(entry{kind: eTool, title: p.Input.ToolName, body: "⏳ permission requested · " + toolArgs(p.Input.ToolInput)})
}

// expireDue auto-approves any gates whose countdown has elapsed.
func (m *Model) expireDue() {
	if m.paused {
		return
	}
	kept := m.pending[:0]
	for _, it := range m.pending {
		if !m.now.Before(it.deadline) {
			it.p.Resolve(gate.Decision{Behavior: gate.Allow, Reason: "auto-approved after countdown"})
			alog.Printf("gate: auto-approve tool=%s", it.p.Input.ToolName)
			m.appendEntry(entry{kind: eGood, body: "✔ auto-approved · ⚙ " + it.p.Input.ToolName})
		} else {
			kept = append(kept, it)
		}
	}
	m.pending = kept
}

// resolveFront answers the head-of-queue gate with an explicit decision.
func (m *Model) resolveFront(d gate.Decision, e entry) {
	if len(m.pending) == 0 {
		return
	}
	it := m.pending[0]
	it.p.Resolve(d)
	m.pending = m.pending[1:]
	alog.Printf("gate: %s tool=%s (%s)", d.Behavior, it.p.Input.ToolName, d.Reason)
	m.appendEntry(e)
}

// togglePause freezes or resumes every pending countdown.
func (m *Model) togglePause() {
	if m.paused {
		// resume: re-derive deadlines from the frozen remaining time
		for _, it := range m.pending {
			it.deadline = m.now.Add(it.remaining)
		}
		m.paused = false
	} else {
		for _, it := range m.pending {
			it.remaining = it.deadline.Sub(m.now)
			if it.remaining < 0 {
				it.remaining = 0
			}
		}
		m.paused = true
	}
}

// frontRemaining returns the time left on the head gate, or 0 if none.
func (m *Model) frontRemaining() time.Duration {
	if len(m.pending) == 0 {
		return 0
	}
	it := m.pending[0]
	var r time.Duration
	if m.paused {
		r = it.remaining
	} else {
		r = it.deadline.Sub(m.now)
	}
	return max(r, 0)
}
