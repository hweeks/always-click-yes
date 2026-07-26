package ui

import (
	"fmt"
	"strings"
)

// The delegated-task ledger.
//
// /tokens says what the run spent; this says what it spent it *on*. A run that
// delegates does its real work in child processes whose transcripts scroll past
// and are gone, so by the end the only surviving account of a task is its row
// here: what it was asked to do, how it ended, and what that answer cost.
//
// Cache reads are the second column that matters, for the same reason they do
// in /tokens — a child that re-read its context on every turn is the one that
// quietly took the money, and its cost alone will not say so.

// taskTitleWidth bounds the title column. truncate appends an ellipsis, so the
// text is cut one rune short of the column to leave room for it.
const taskTitleWidth = 44

// taskReport is the /tasks ledger: one row per delegated task, oldest first.
func (m Model) taskReport() string {
	if len(m.tasks) == 0 {
		return "no tasks dispatched yet"
	}

	var b strings.Builder
	row := func(id, title, outcome, cost, cacheRead string) {
		fmt.Fprintf(&b, "%-6s %-*s %-11s %10s %8s\n", id, taskTitleWidth, title, outcome, cost, cacheRead)
	}

	row("id", "title", "outcome", "cost", "cache-r")

	var totalCost float64
	var totalRead int64
	for _, t := range m.tasks {
		// A task with no end time is still running, not a finished one that forgot
		// to say how it went — the difference decides whether its missing cost is
		// alarming or simply not in yet.
		outcome := t.Outcome
		switch {
		case t.Unfinished():
			outcome = "running…"
		case outcome == "":
			outcome = "—"
		}
		totalCost += t.CostUSD
		totalRead += t.Tokens.CacheRead
		row(truncate(t.ID, 5), truncate(t.Title, taskTitleWidth-1), outcome,
			fmt.Sprintf("$%.4f", t.CostUSD), fmtTokens(t.Tokens.CacheRead))
	}

	row("total", fmt.Sprintf("%d task(s)", len(m.tasks)), "",
		fmt.Sprintf("$%.4f", totalCost), fmtTokens(totalRead))

	// The ledger is trimmed to the newest entries but the dispatch count is not,
	// so on a long run the total below is not the total for the run.
	if m.dispatches > len(m.tasks) {
		fmt.Fprintf(&b, "\n%d task(s) dispatched; older rows were dropped — the totals cover only the %d shown",
			m.dispatches, len(m.tasks))
	}
	return strings.TrimRight(b.String(), "\n")
}
