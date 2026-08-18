package ui

import (
	"time"

	tea "charm.land/bubbletea/v2"

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
	// The merge guard is checked before anything else, including intercepted:
	// a deny here must never become a countdown, and it must never be waved
	// through as a pass-through allow either. See guard.go for what it does
	// and does not guarantee.
	if deny, reason := mergeGuardVerdict(tool, p.Input.ToolInput, m.protectedBranches()); deny {
		p.Resolve(gate.Decision{Behavior: gate.Deny, Reason: reason})
		alog.Printf("gate: deny tool=%s (merge guard: %s)", p.Input.ToolName, reason)
		m.appendEntry(entry{kind: eWarn, title: p.Input.ToolName, body: "⛔ denied by merge guard · " + reason})
		return
	}
	if intercepted[tool] {
		p.Resolve(gate.Decision{Behavior: gate.Allow, Reason: "handled by acy"})
		alog.Printf("gate: pass-through tool=%s (intercepted by acy)", p.Input.ToolName)
		return
	}
	// Answering is not acting. StructuredOutput is how a --json-schema session
	// hands back its result — it is the child's report, not a side effect — and
	// the hook matches "*", so it raised a countdown like anything else.
	//
	// Measured: two of them in one child, 30s each, a full minute spent guarding
	// the model's own answer. There is nothing here a human could sensibly veto;
	// vetoing it would only destroy the report and leave the task looking failed.
	if answerTools[tool] {
		p.Resolve(gate.Decision{Behavior: gate.Allow, Reason: "returning a result, not acting"})
		alog.Printf("gate: pass-through tool=%s (result delivery)", p.Input.ToolName)
		return
	}
	// Who is asking, not what phase we are in.
	//
	// This bypass used to key on `m.phase == PhasePlan`, which was sound only
	// because the plan registry has no Write or Edit in it. That reasoning dies
	// the moment a child shares this socket: a child carries the *full* registry,
	// and phase describes the parent. Keying on phase would auto-approve every
	// child edit with no countdown at all — the exact opposite of the intent.
	//
	// So: a request is waved through only when it comes from the supervising
	// session itself AND names a tool that cannot change anything. A child always
	// counts down, whatever it is doing and whatever phase the parent is in.
	taskID, fromChild := "", false
	if m.dispatcher != nil {
		taskID, fromChild = m.dispatcher.TaskFor(p.Input.SessionID)
	}
	if !fromChild && readOnlyParentTools[tool] {
		p.Resolve(gate.Decision{Behavior: gate.Allow, Reason: "read-only tool in the supervising session"})
		alog.Printf("gate: pass-through tool=%s (parent, read-only)", p.Input.ToolName)
		return
	}
	// ParentNoExec stands in, on codex, for a guarantee claude gets for free:
	// the supervising session's --tools registry simply has no Bash in it, so
	// on claude this branch is unreachable — there is nothing to deny because
	// there is nothing to call. Codex has no such registry filter (only
	// sandbox/approval policy wrapping an ever-present shell tool, per
	// Config.ParentNoExec's doc comment), so on a codex run the parent really
	// can ask to run one, and this is the only thing left to say no. That
	// makes it weaker in kind, not just in degree: a bug here removes a
	// constraint, where the same bug on claude's side would have nothing to
	// remove. A deny here must never become a countdown, exactly like the
	// merge guard's deny above — and it must never fire for a child, who is
	// meant to write.
	if !fromChild && m.parentNoExec {
		p.Resolve(gate.Decision{Behavior: gate.Deny, Reason: "the supervising session may only read"})
		alog.Printf("gate: deny tool=%s (parent, no-exec)", p.Input.ToolName)
		m.appendEntry(entry{kind: eWarn, title: p.Input.ToolName, body: "⛔ denied · the supervising session may only read"})
		return
	}
	it := &gateItem{p: p, task: taskID}
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

// resolveByID answers the one gate carrying this tool_use id, and reports the
// item it answered so the caller can name the tool in the transcript.
//
// Identity, not position, is the whole point. Gates auto-approve on their own
// countdown, so the head of the queue can change between a client rendering it
// and the answer arriving — and an unknown id therefore means "that gate is
// already gone", which resolves nothing at all. There is deliberately no
// fallback to the front of the queue: that would approve a tool nobody looked
// at, which is the one outcome a permission gate exists to prevent.
func (m *Model) resolveByID(id string, d gate.Decision) (*gateItem, bool) {
	for i, it := range m.pending {
		if it.p.Input.ToolUseID != id {
			continue
		}
		it.p.Resolve(d)
		// A fresh slice rather than a reslice or an in-place shuffle: Bubble Tea
		// copies the Model by value, so several copies share this backing array
		// and moving elements inside it edits a slice someone else is holding.
		kept := make([]*gateItem, 0, len(m.pending)-1)
		kept = append(kept, m.pending[:i]...)
		kept = append(kept, m.pending[i+1:]...)
		m.pending = kept
		alog.Printf("gate: %s tool=%s use_id=%s (%s)", d.Behavior, it.p.Input.ToolName, id, d.Reason)
		return it, true
	}
	return nil, false
}

// resolveFront answers the head-of-queue gate with an explicit decision. It is
// resolveByID with the head's own id, so there is one removal path rather than
// two that could disagree about what "answered" means.
func (m *Model) resolveFront(d gate.Decision, e entry) {
	if len(m.pending) == 0 {
		return
	}
	if _, ok := m.resolveByID(m.pending[0].p.Input.ToolUseID, d); ok {
		m.appendEntry(e)
	}
}

// setPaused puts every countdown into the requested state and reports whether
// that was a change. Explicit rather than a toggle, because a client that is
// not looking at the screen cannot know what it would be toggling from.
func (m *Model) setPaused(paused bool) bool {
	if m.paused == paused {
		return false
	}
	m.togglePause()
	return true
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
