package ui

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

// Rounds is how many times a CONTINUE verdict has auto-nudged the working session.
func (m Model) Rounds() int { return m.rounds }

// Verifying reports whether an independent judge session is deciding completion.
func (m Model) Verifying() bool { return m.verifying }

// PendingGates is how many permission requests are counting down right now.
func (m Model) PendingGates() int { return len(m.pending) }

// PlanBody is the plan the judge grades against.
func (m Model) PlanBody() string { return m.planBody }

// StopDriver kills the claude process without any of the tidying a clean exit would
// do. It is how the e2e suite simulates the crash a resume has to recover from.
func (m *Model) StopDriver() {
	if m.drv != nil {
		m.drv.Stop()
	}
}
