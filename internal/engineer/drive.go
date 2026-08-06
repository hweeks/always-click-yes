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
	"github.com/hweeks/always-click-yes/internal/state"
	"github.com/hweeks/always-click-yes/internal/verify"
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
	tokens  state.Tokens
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
			return driveResult{kind: driveFinished, outcome: snap.FinishOutcome, summary: snap.FinishSummary, cost: snap.CostUSD, tokens: snap.Tokens}
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
					return driveResult{kind: driveStalled, summary: stallSummary(nudges, snap), cost: snap.CostUSD, tokens: snap.Tokens}
				}
				nudges++
				sess.Submit(continuationPrompt)
				idleSince = time.Time{}
			}
		}

		select {
		case <-ctx.Done():
			return driveResult{kind: driveCancelled, summary: c.exitReason(ctx), cost: snap.CostUSD, tokens: snap.Tokens}
		case <-c.cancelCh:
			return driveResult{kind: driveCancelled, summary: c.exitReason(ctx), cost: snap.CostUSD, tokens: snap.Tokens}
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
// runs the configured verify commands, pushes the branch, and opens the PR. A
// push or PR failure overrides outcome with "failed" unconditionally — the
// model may believe it finished, but nothing reached a remote anyone can
// review. A failing verify check overrides outcome with "failed" too, but
// does not block the push/PR: the architect should still get a branch and a
// PR to look at, just one honestly marked as failing its checks.
func (c *Core) finalize(ctx context.Context, outcome, summary string, cost float64, tokens state.Tokens) engineerwire.Result {
	spec := c.cfg.Spec

	ahead, err := gitops.CommitsAhead(ctx, c.cfg.GitRunner, c.cfg.WorktreeDir, spec.BaseBranch)
	if err != nil {
		return engineerwire.Result{Outcome: "failed", Summary: "checking commits ahead: " + err.Error(), CostUSD: cost, Tokens: tokens}
	}
	// A run that changed nothing has nothing worth spending (potentially) ten
	// minutes verifying.
	if ahead == 0 {
		return engineerwire.Result{
			Outcome: outcome,
			Summary: summary + " (no commits were made; nothing to push)",
			CostUSD: cost,
			Tokens:  tokens,
		}
	}

	checks := verify.Run(ctx, c.cfg.VerifyRunner, c.cfg.WorktreeDir, c.cfg.VerifyCommands, c.cfg.VerifyTimeout)
	for _, check := range checks {
		c.appendEvent(engineerwire.Event{Kind: engineerwire.EventLog, Text: "verify: " + formatCheckLine(check)})
	}
	if digest := verifyDigest(checks); digest != "" {
		summary += "\n\n" + digest
	}
	failed := false
	for _, check := range checks {
		if check.Status == engineerwire.VerifyFailed {
			failed = true
			break
		}
	}

	if err := gitops.Push(ctx, c.cfg.GitRunner, c.cfg.WorktreeDir, spec.Branch); err != nil {
		return engineerwire.Result{Outcome: "failed", Summary: "pushing branch: " + err.Error(), Branch: spec.Branch, CostUSD: cost, Tokens: tokens, Verification: checks}
	}

	title := fmt.Sprintf("%s: %s", spec.Ticket, spec.Title)
	body := summary + prFooter(c.cfg.EngineerID, spec.Ticket)
	prURL, err := gitops.CreatePR(ctx, c.cfg.GitRunner, c.cfg.WorktreeDir, spec.BaseBranch, spec.Branch, title, body)
	if err != nil {
		return engineerwire.Result{Outcome: "failed", Summary: "opening PR: " + err.Error(), Branch: spec.Branch, CostUSD: cost, Tokens: tokens, Verification: checks}
	}

	if failed {
		outcome = "failed"
	}
	return engineerwire.Result{
		Outcome:      outcome,
		Summary:      summary,
		Branch:       spec.Branch,
		PRURL:        prURL,
		CostUSD:      cost,
		Tokens:       tokens,
		Files:        changedFiles(ctx, c.cfg.GitRunner, c.cfg.WorktreeDir, spec.BaseBranch),
		Verification: checks,
	}
}

// verifyExcerptMaxLines and verifyExcerptMaxChars cap how much of a failing
// check's output verifyDigest quotes inline: enough to name the failure, not
// enough to turn a PR body into a pasted test log. The full, already
// 8KiB-capped output always lives in Result.Verification.
const (
	verifyExcerptMaxLines = 3
	verifyExcerptMaxChars = 200
)

// formatCheckLine renders one check as a single-line "name — status
// (detail)" summary, shared by the per-check EventLog text and each line of
// verifyDigest so the wording of the two never drifts apart.
func formatCheckLine(c engineerwire.VerifyCheck) string {
	dur := fmt.Sprintf("%.1fs", float64(c.DurationMS)/1000)
	switch c.Status {
	case engineerwire.VerifyPassed:
		return fmt.Sprintf("%s — passed (%s)", c.Name, dur)
	case engineerwire.VerifyFailed:
		return fmt.Sprintf("%s — FAILED (exit %d, %s)", c.Name, c.ExitCode, dur)
	case engineerwire.VerifySkipped:
		return fmt.Sprintf("%s — skipped (not installed on this host)", c.Name)
	case engineerwire.VerifyTimeout:
		return fmt.Sprintf("%s — timeout (%s)", c.Name, dur)
	case engineerwire.VerifyError:
		return fmt.Sprintf("%s — error (%s)", c.Name, dur)
	default:
		return fmt.Sprintf("%s — %s", c.Name, c.Status)
	}
}

// verifyDigest renders checks as the human-readable block appended to the
// Result summary and PR body, so an architect reading either sees the same
// machine-collected verdict the session's own report didn't include. Returns
// "" for a nil/empty checks, so callers can append it unconditionally without
// an extra blank line when no verify commands were configured.
func verifyDigest(checks []engineerwire.VerifyCheck) string {
	if len(checks) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Verification (run by acy in the worktree, not reported by the session):")
	for _, check := range checks {
		b.WriteString("\n- ")
		b.WriteString(formatCheckLine(check))
		if check.Output == "" {
			continue
		}
		if check.Status != engineerwire.VerifyFailed && check.Status != engineerwire.VerifyError {
			continue
		}
		b.WriteString("\n  excerpt: ")
		b.WriteString(verifyExcerpt(check.Output))
		b.WriteString("  [see Result.Verification for full output]")
	}
	return b.String()
}

// verifyExcerpt collapses output to a single line capped at
// verifyExcerptMaxLines lines and verifyExcerptMaxChars characters, whichever
// is shorter. Newlines within the kept lines are rendered as literal `\n` so
// the excerpt stays one line in the digest.
func verifyExcerpt(output string) string {
	lines := strings.Split(output, "\n")
	if len(lines) > verifyExcerptMaxLines {
		lines = lines[:verifyExcerptMaxLines]
	}
	excerpt := strings.Join(lines, "\\n")
	runes := []rune(excerpt)
	if len(runes) > verifyExcerptMaxChars {
		excerpt = string(runes[:verifyExcerptMaxChars])
	}
	return excerpt
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
