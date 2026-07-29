package ui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/mcp"
	"github.com/hweeks/always-click-yes/internal/orchestrator"
	"github.com/hweeks/always-click-yes/internal/state"
)

// Delegation, from the UI's side.
//
// The parent session calls mcp__acy__Dispatch, which blocks on the ask socket
// exactly as a question does. The difference is who answers: a question waits
// for a human, a dispatch waits for a whole claude process to run a task and
// report. Both are already served by the same unbounded socket read, which is
// why this needed no new transport.

// Dispatcher is the subset of orchestrator.Orchestrator the model uses. Named
// here rather than imported wholesale so ui tests can supply a fake, matching
// how Launcher, Sessions and LoadState are already injected.
//
// It is held as an interface value — that is, a pointer — because Bubble Tea
// copies the Model on every Update, and copying a struct containing a mutex is
// the same class of bug as the strings.Builder crash this codebase already has
// scar tissue from.
type Dispatcher interface {
	Dispatch(ctx context.Context, p *mcp.Pending) (orchestrator.Status, error)
	Events() <-chan orchestrator.Event
	TaskFor(sessionID string) (string, bool)
	Statuses() []orchestrator.Status
	Ledger() []state.Task
	Totals() (state.Tokens, float64, int)
	Cancel(taskID, reason string)
	CancelAll(reason string)
	Active() int
}

type childMsg struct{ ev orchestrator.Event }

// waitChild blocks on the next child event. The orchestrator's channel is
// created once and closed only at shutdown, so unlike driver events this needs
// no generation counter — every event already names the task it belongs to.
func waitChild(ch <-chan orchestrator.Event) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return childMsg{ev: ev}
	}
}

// startDispatch answers a Dispatch call from the parent.
//
// Refusing here, at the moment of the call, is what lets the plan-phase system
// prompt stop explaining that the model cannot start its own work: it finds out
// by trying, once, and the explanation costs nothing on every other turn.
func (m *Model) startDispatch(p *mcp.Pending) {
	switch {
	case m.dispatcher == nil:
		p.Resolve(mcp.Answer{Text: mcp.DispatchUnavailable})
		m.appendEntry(entry{kind: eWarn, body: "a task was dispatched, but delegation is not wired in this session"})
		return
	case m.phase == PhasePlan:
		p.Resolve(mcp.Answer{Text: mcp.DispatchNotArmed})
		alog.Printf("dispatch: refused — the run is not armed")
		m.appendEntry(entry{kind: eMeta, body: "↯ dispatch declined — press Ctrl+G to arm the run"})
		return
	}

	st, err := m.dispatcher.Dispatch(m.ctx, p)
	if err != nil {
		m.appendEntry(entry{kind: eWarn, body: "dispatch rejected: " + err.Error()})
		return
	}
	// The orchestrator's ledger already holds this task, so read the count from
	// there rather than keeping a second tally that could drift from it.
	m.syncChildTotals()
	m.appendEntry(entry{kind: eTool, title: "dispatch " + st.Task.ID, body: dispatchBody(st.Task)})
	m.persist()
}

func dispatchBody(t orchestrator.Task) string {
	body := t.Title
	if t.Success != "" {
		body += "\ndone means: " + t.Success
	}
	return body
}

// ingestChild folds one child event into the transcript and the tallies.
//
// Child stream events go through the same ingest path as the parent's, so there
// is still exactly one content parser — but tagged with the task, and without
// touching any of the parent's turn state. A child's text is not the parent's
// turnText, and a child finishing is not the parent's turn ending.
func (m *Model) ingestChild(ev orchestrator.Event) {
	switch ev.Kind {
	case orchestrator.KindStarted:
		m.appendEntry(entry{kind: eMeta, body: fmt.Sprintf("▶ %s started · %s", ev.TaskID, ev.Title)})

	case orchestrator.KindStream:
		m.ingestChildStream(ev)

	case orchestrator.KindFinished:
		m.syncChildTotals()
		kind := eGood
		if ev.Report != nil && ev.Report.Outcome != orchestrator.OutcomeCompleted {
			kind = eWarn
		}
		body := fmt.Sprintf("■ %s %s", ev.TaskID, ev.Title)
		if ev.Report != nil {
			body += " — " + ev.Report.Outcome + "\n" + ev.Report.Summary
		}
		if ev.Status != nil {
			body += fmt.Sprintf("\n$%.4f · ⇣%s", ev.Status.Cost, fmtTokens(ev.Status.Tokens.CacheRead))
		}
		m.appendEntry(entry{kind: kind, body: body})
		m.persist()

	case orchestrator.KindFailed:
		m.syncChildTotals()
		body := fmt.Sprintf("✗ %s %s did not finish", ev.TaskID, ev.Title)
		if ev.Err != nil {
			body += ": " + ev.Err.Error()
		}
		m.appendEntry(entry{kind: eToolErr, body: body})
		m.persist()
	}
}

// ingestChildStream renders what a child is doing, badged with its task.
//
// The child's final assistant text is suppressed: with --json-schema it is the
// raw report JSON, which the KindFinished entry already renders properly and
// which would otherwise land in the transcript twice, once unreadably.
func (m *Model) ingestChildStream(ev orchestrator.Event) {
	e := ev.Ev
	switch e.Type {
	case driver.TypeAssistant:
		for _, b := range e.Message.Blocks() {
			switch b.Type {
			case driver.BlockText:
				if t := strings.TrimSpace(b.Text); t != "" && !looksLikeReportJSON(t) {
					m.appendEntry(entry{kind: eClaude, task: ev.TaskID, body: t})
				}
			case driver.BlockToolUse:
				// b.Name, not baseToolName(b.Name), matching what this has always
				// passed — changing it would start highlighting bodies the TUI has
				// never highlighted.
				body, raw, lang := toolBodyParts(b.Name, b.Input)
				m.appendEntry(entry{
					kind: eTool, task: ev.TaskID,
					title: baseToolName(b.Name), body: body, raw: raw, lang: lang,
				})
			}
		}
	case driver.TypeUser:
		for _, b := range e.Message.Blocks() {
			if b.Type == driver.BlockToolResult {
				kind := eToolOK
				if b.IsError {
					kind = eToolErr
				}
				m.appendEntry(entry{kind: kind, task: ev.TaskID, body: rawText(b.Content)})
			}
		}
	}
}

// looksLikeReportJSON spots the structured report echoed as assistant text.
func looksLikeReportJSON(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "{") && strings.Contains(s, `"outcome"`) && strings.Contains(s, `"summary"`)
}

// syncChildTotals pulls the children's spend into the model. The orchestrator is
// the authority — it sees every child, including ones whose events were dropped
// under load — so this reads rather than accumulates.
func (m *Model) syncChildTotals() {
	if m.dispatcher == nil {
		return
	}
	tok, cost, n := m.dispatcher.Totals()
	m.childTokens = tok
	m.childCost = cost
	m.dispatches = n
	m.tasks = m.dispatcher.Ledger()
}

// cancelDispatches stops any running task. Bound to the same key that
// interrupts the parent: interrupting only the parent would leave an orphaned
// child burning tokens on work whose result now has nowhere to go.
func (m *Model) cancelDispatches(reason string) {
	if m.dispatcher == nil || m.dispatcher.Active() == 0 {
		return
	}
	m.appendEntry(entry{kind: eWarn, body: "cancelling running tasks — " + reason})
	m.dispatcher.CancelAll(reason)
}
