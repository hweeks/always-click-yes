package engineer

import (
	"context"

	"github.com/hweeks/always-click-yes/internal/hub"
	"github.com/hweeks/always-click-yes/internal/state"
	"github.com/hweeks/always-click-yes/internal/supervisor"
	"github.com/hweeks/always-click-yes/internal/ui"
)

// session is exactly what Core needs from a running supervisor: enough to
// drive one ticket end to end without depending on *hub.Hub or *ui.Model
// directly, so unit tests can substitute a scripted fake and never launch a
// claude process, spend money, or touch the network.
type session interface {
	// Submit sends text as if typed into the composer, queueing it if the
	// session is busy. It mirrors ui.ActionSubmit.
	Submit(text string) ActionResult
	// Arm flips PLAN into AUTO-RUN. It mirrors ui.ActionArm.
	Arm() ActionResult
	// Quit stops the underlying driver and ends the run.
	Quit()
	// Snapshot is a read of the run as it stands right now.
	Snapshot() Snapshot
}

// ActionResult is whether a session action was accepted, and why — mirroring
// ui.ActionResult without tying the interface to the ui package.
type ActionResult struct {
	Accepted bool
	Reason   string
}

// TaskRow is one delegated task, as the ledger remembers it.
type TaskRow struct {
	ID      string
	Title   string
	Outcome string
	CostUSD float64
	Running bool
}

// Phase names the stage a session is in.
type Phase string

const (
	PhasePlan     Phase = "PLAN"
	PhaseAutoRun  Phase = "AUTO-RUN"
	PhaseComplete Phase = "COMPLETE"
)

// Snapshot is a read of the run at one moment: exactly what Core needs to
// decide what to do next.
type Snapshot struct {
	// Ready reports whether a driver is attached yet. Launching one is
	// asynchronous, so Submit is refused until this is true.
	Ready bool

	Phase         Phase
	Processing    bool
	Busy          bool
	FinishOutcome string
	FinishSummary string
	CostUSD       float64
	Tokens        state.Tokens
	Tasks         []TaskRow
}

// builder constructs the session Core drives for one spec, and a func that
// releases everything it opened. The real implementation
// (buildSupervisorSession) calls supervisor.NewSupervisor and wraps its
// Model in a hub.Hub; tests inject a scripted fake instead by setting
// Config.builder directly (white-box, same package).
type builder func(ctx context.Context, f supervisor.Flags) (session, func(), error)

// buildSupervisorSession is the production builder: a real supervisor, driven
// headlessly through a hub.Hub. f.InterceptAsk is expected to already be
// wired to the caller's Ask interceptor.
func buildSupervisorSession(ctx context.Context, f supervisor.Flags) (session, func(), error) {
	sup, err := supervisor.NewSupervisor(ctx, f)
	if err != nil {
		return nil, nil, err
	}
	h := hub.New(sup.Model)
	closeFn := func() {
		h.Close()
		sup.Close()
	}
	return &hubSession{h: h}, closeFn, nil
}

// hubSession adapts a *hub.Hub to session. It is the whole of the "real
// adapter": Do for the two actions Core needs, Read plus the ui accessors for
// the snapshot.
type hubSession struct {
	h *hub.Hub
}

func (s *hubSession) Submit(text string) ActionResult {
	r := s.h.Do(ui.Submit(text))
	return ActionResult{Accepted: r.Accepted, Reason: r.Reason}
}

func (s *hubSession) Arm() ActionResult {
	r := s.h.Do(ui.Arm())
	return ActionResult{Accepted: r.Accepted, Reason: r.Reason}
}

func (s *hubSession) Quit() {
	s.h.Do(ui.Quit())
}

func (s *hubSession) Snapshot() Snapshot {
	var snap Snapshot
	s.h.Read(func(m ui.Model) {
		tasks := m.Tasks()
		rows := make([]TaskRow, 0, len(tasks))
		for _, t := range tasks {
			rows = append(rows, TaskRow{
				ID:      t.ID,
				Title:   t.Title,
				Outcome: t.Outcome,
				CostUSD: t.CostUSD,
				Running: t.Unfinished(),
			})
		}
		// Tokens is the parent and every dispatched child summed together —
		// the run's total, not either half of it.
		tokens := m.ParentTokens()
		tokens.Add(m.ChildTokens())
		snap = Snapshot{
			Ready:         m.HasDriver(),
			Phase:         Phase(m.Phase().String()),
			Processing:    m.Processing(),
			Busy:          m.Busy(),
			FinishOutcome: m.FinishOutcome(),
			FinishSummary: m.FinishSummary(),
			CostUSD:       m.GrandTotalCost(),
			Tokens:        tokens,
			Tasks:         rows,
		}
	})
	return snap
}
