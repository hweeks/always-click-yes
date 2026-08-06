package ui

import "github.com/hweeks/always-click-yes/internal/state"

// Accessors for driving the model from outside the package — specifically the live
// e2e suite (internal/e2e), which runs a real supervisor without a terminal and has
// to be able to ask it what happened. They are read-only views of state the TUI
// already renders, so nothing here widens what the model can be made to do.

// Phase is the stage the run is in: PLAN, AUTO-RUN or COMPLETE.
func (m Model) Phase() Phase { return m.phase }

// HasDriver reports whether a claude process is attached yet. Launching one is
// asynchronous, and sendInput drops messages until it lands — so a caller driving
// the model has to wait for this before it types.
func (m Model) HasDriver() bool { return m.drv != nil }

// SessionID is claude's id for the working session, empty until its init event.
func (m Model) SessionID() string { return m.sessionID }

// Status is the one-line state shown in the header ("working…", "idle", …).
func (m Model) Status() string { return m.status }

// Transcript is the conversation as plain text, titles included.
func (m Model) Transcript() string { return m.transcript() }

// TotalCost is what the run has spent across every claude process it launched.
func (m Model) TotalCost() float64 { return m.totalCost() }

// Dispatches is how many tasks this run has delegated to child processes. It
// replaces Rounds, which counted auto-nudges of a loop that no longer exists.
func (m Model) Dispatches() int { return m.dispatches }

// ParentTokens is everything the supervising session itself has spent. Keeping
// this bounded as a job grows is the point of delegating work to child
// processes, so the e2e suite asserts on it directly.
func (m Model) ParentTokens() state.Tokens { return m.parentTokens }

// ChildTokens is the total across every dispatched child process.
func (m Model) ChildTokens() state.Tokens { return m.childTokens }

// LastContext is how much context the most recent turn carried — a reading of
// the conversation's current size, not a running total.
func (m Model) LastContext() int { return m.lastContext }

// PendingGates is how many permission requests are counting down right now.
func (m Model) PendingGates() int { return len(m.pending) }

// PlanBody is the approved plan the run was armed with.
func (m Model) PlanBody() string { return m.planBody }

// FinishOutcome is "completed" or "abandoned", set once the session calls
// Finish; empty before then.
func (m Model) FinishOutcome() string { return m.finishOutcome }

// FinishSummary is the summary that came with FinishOutcome.
func (m Model) FinishSummary() string { return m.finishSummary }

// Busy reports whether the session has a turn, a gate, or a dispatched task in
// flight — see busy().
func (m Model) Busy() bool { return m.busy() }

// Processing reports whether a turn is in flight right now, as opposed to a
// gate or a dispatched task holding the session busy.
func (m Model) Processing() bool { return m.processing }

// GrandTotalCost is everything this run has spent, parent and child processes
// combined — see grandTotalCost().
func (m Model) GrandTotalCost() float64 { return m.grandTotalCost() }

// Tasks is the delegated-task ledger, oldest first. Copied, so a caller cannot
// mutate the model's own backing slice.
func (m Model) Tasks() []state.Task { return append([]state.Task(nil), m.tasks...) }

// StopDriver kills the claude process without any of the tidying a clean exit would
// do. It is how the e2e suite simulates the crash a resume has to recover from.
func (m *Model) StopDriver() {
	if m.drv != nil {
		m.drv.Stop()
	}
}
