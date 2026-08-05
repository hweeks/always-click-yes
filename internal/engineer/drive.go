package engineer

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
	"github.com/hweeks/always-click-yes/internal/gitops"
)

// driveKind names how the AUTO-RUN polling loop ended.
type driveKind int

const (
	driveFinished  driveKind = iota // the session called Finish
	driveStalled                    // idle with no Finish, after maxNudges nudges
	driveCancelled                  // Cancel or ctx expiry
)

// driveResult is what the AUTO-RUN loop found. outcome is only meaningful for
// driveFinished — the model's own "completed"/"abandoned" — since a stall or
// a cancellation is not the session's verdict to give.
type driveResult struct {
	kind    driveKind
	outcome string
	summary string
	cost    float64
}

// drive polls sess once every c.pollInterval until the session calls Finish,
// goes idle for too long with no Finish (a stall), or ctx/Cancel ends the run
// first. It journals a phase/task_started/task_report/cost Event on every
// poll that shows a change worth narrating.
func (c *Core) drive(ctx context.Context, sess session) driveResult {
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	var lastPhase Phase
	var lastCost float64
	lastTasks := map[string]TaskRow{}
	var idleSince time.Time
	nudges := 0

	for {
		snap := sess.Snapshot()
		c.emitEvents(snap, &lastPhase, &lastCost, lastTasks)

		if snap.FinishOutcome != "" {
			return driveResult{kind: driveFinished, outcome: snap.FinishOutcome, summary: snap.FinishSummary, cost: snap.CostUSD}
		}

		if snap.Phase != PhaseAutoRun {
			idleSince = time.Time{}
		} else if snap.Busy {
			idleSince = time.Time{}
		} else {
			if idleSince.IsZero() {
				idleSince = time.Now()
			} else if time.Since(idleSince) >= c.stallIdle {
				if nudges >= maxNudges {
					return driveResult{kind: driveStalled, summary: stallSummary(nudges, snap), cost: snap.CostUSD}
				}
				nudges++
				sess.Submit(continuationPrompt)
				idleSince = time.Time{}
			}
		}

		select {
		case <-ctx.Done():
			return driveResult{kind: driveCancelled, summary: c.exitReason(ctx), cost: snap.CostUSD}
		case <-c.cancelCh:
			return driveResult{kind: driveCancelled, summary: c.exitReason(ctx), cost: snap.CostUSD}
		case <-ticker.C:
		}
	}
}

// emitEvents journals whatever changed in snap since the last poll: a phase
// transition, a task appearing or reporting in, or a cost movement past
// costEventThreshold. last* are the previous poll's readings, updated in
// place.
func (c *Core) emitEvents(snap Snapshot, lastPhase *Phase, lastCost *float64, lastTasks map[string]TaskRow) {
	if snap.Phase != *lastPhase {
		c.appendEvent(engineerwire.Event{Kind: engineerwire.EventPhase, Text: string(snap.Phase)})
		*lastPhase = snap.Phase
	}
	if math.Abs(snap.CostUSD-*lastCost) > costEventThreshold {
		c.appendEvent(engineerwire.Event{Kind: engineerwire.EventCost, CostUSD: snap.CostUSD})
		*lastCost = snap.CostUSD
	}
	for _, row := range snap.Tasks {
		prev, seen := lastTasks[row.ID]
		switch {
		case !seen:
			c.appendEvent(engineerwire.Event{Kind: engineerwire.EventTaskStarted, Text: row.Title})
		case prev.Outcome != row.Outcome || prev.Running != row.Running:
			c.appendEvent(engineerwire.Event{Kind: engineerwire.EventTaskReport, Text: row.Outcome, CostUSD: row.CostUSD})
		}
		lastTasks[row.ID] = row
	}
}

func (c *Core) appendEvent(e engineerwire.Event) {
	if _, err := c.journal.Append(e); err != nil {
		alog.Printf("engineer: event journal append failed: %v", err)
	}
}

// stallSummary describes the ledger state a stalled Result reports, so an
// architect reattaching later can see what the run had done before it gave up.
func stallSummary(nudges int, snap Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "run went idle with no Finish call after %d nudge(s)", nudges)
	if len(snap.Tasks) == 0 {
		b.WriteString("; no tasks were dispatched")
		return b.String()
	}
	b.WriteString("; task ledger: ")
	parts := make([]string, 0, len(snap.Tasks))
	for _, t := range snap.Tasks {
		outcome := t.Outcome
		switch {
		case t.Running:
			outcome = "running"
		case outcome == "":
			outcome = "—"
		}
		parts = append(parts, fmt.Sprintf("%s=%s", t.Title, outcome))
	}
	b.WriteString(strings.Join(parts, ", "))
	return b.String()
}

// finalize turns the session's own verdict (outcome, summary) into the final
// Result: it checks whether AUTO-RUN actually committed anything and, if so,
// pushes the branch and opens the PR. A push or PR failure overrides outcome
// with "failed" — the model may believe it finished, but nothing reached a
// remote anyone can review.
func (c *Core) finalize(ctx context.Context, outcome, summary string, cost float64) engineerwire.Result {
	spec := c.cfg.Spec

	ahead, err := gitops.CommitsAhead(ctx, c.cfg.GitRunner, c.cfg.WorktreeDir, spec.BaseBranch)
	if err != nil {
		return engineerwire.Result{Outcome: "failed", Summary: "checking commits ahead: " + err.Error(), CostUSD: cost}
	}
	if ahead == 0 {
		return engineerwire.Result{
			Outcome: outcome,
			Summary: summary + " (no commits were made; nothing to push)",
			CostUSD: cost,
		}
	}

	if err := gitops.Push(ctx, c.cfg.GitRunner, c.cfg.WorktreeDir, spec.Branch); err != nil {
		return engineerwire.Result{Outcome: "failed", Summary: "pushing branch: " + err.Error(), Branch: spec.Branch, CostUSD: cost}
	}

	title := fmt.Sprintf("%s: %s", spec.Ticket, spec.Title)
	body := summary + prFooter(c.cfg.EngineerID, spec.Ticket)
	prURL, err := gitops.CreatePR(ctx, c.cfg.GitRunner, c.cfg.WorktreeDir, spec.BaseBranch, spec.Branch, title, body)
	if err != nil {
		return engineerwire.Result{Outcome: "failed", Summary: "opening PR: " + err.Error(), Branch: spec.Branch, CostUSD: cost}
	}

	return engineerwire.Result{
		Outcome: outcome,
		Summary: summary,
		Branch:  spec.Branch,
		PRURL:   prURL,
		CostUSD: cost,
		Files:   changedFiles(ctx, c.cfg.GitRunner, c.cfg.WorktreeDir, spec.BaseBranch),
	}
}

func prFooter(engineerID, ticket string) string {
	return fmt.Sprintf("\n\n---\nEngineer: %s · Ticket: %s", engineerID, ticket)
}

// changedFiles lists what the run touched, via the same Runner as the rest of
// gitops rather than a bare exec.Command, so tests can intercept it too. A
// failed diff is not worth failing the whole Result over — it reports no
// files rather than turning a successful PR into a "failed" outcome.
func changedFiles(ctx context.Context, run gitops.Runner, dir, base string) []string {
	out, err := run(ctx, dir, "git", "diff", "--name-only", base+"..HEAD")
	if err != nil {
		alog.Printf("engineer: git diff --name-only failed: %v", err)
		return nil
	}
	var files []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}
