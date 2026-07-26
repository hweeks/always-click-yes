package ui

import (
	"fmt"
	"strings"

	"github.com/hweeks/always-click-yes/internal/state"
)

// Token accounting display.
//
// acy tracks tokens because cost alone cannot tell you *why* a run was
// expensive. A measured run of this repo spent $16.04, of which essentially all
// was 8.7M cache-read tokens against 75k of output: the model was not writing
// too much, it was re-reading an ever-growing conversation on every turn. Cache
// reads are discounted, not free, and at that volume the discount stops
// mattering.
//
// So the two numbers worth surfacing are the *reading* ones: how big the
// context is right now, and how much has been re-read in total.

// fmtTokens renders a token count compactly: 812, 38k, 1.4M.
func fmtTokens(n int64) string {
	switch {
	case n < 0:
		return "0"
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		if n < 10_000 {
			return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "k"
		}
		return fmt.Sprintf("%.0fk", float64(n)/1000)
	default:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1_000_000), ".0") + "M"
	}
}

// ctxNote describes the context the latest turn carried. The window is included
// when claude reported one, because "38k" means very different things against a
// 200k window and a 1M one.
func ctxNote(size, window int) string {
	if size <= 0 {
		return "ctx —"
	}
	if window <= 0 {
		return "ctx " + fmtTokens(int64(size))
	}
	return fmt.Sprintf("ctx %s/%s", fmtTokens(int64(size)), fmtTokens(int64(window)))
}

// tokenSummary is the compact header fragment: the live context size, then
// cumulative cache reads. The second number is the one that grows without bound
// when a single session is made to carry a whole job.
func (m Model) tokenSummary() string {
	all := m.allTokens()
	if all.IsZero() && m.lastContext == 0 {
		return ""
	}
	return fmt.Sprintf("%s · ⇣%s", ctxNote(m.lastContext, m.contextWindow), fmtTokens(all.CacheRead))
}

// tokenReport is the /tokens ledger. It separates the parent from its children
// because that split is the whole design: a parent that delegates should show a
// flat line while the children's totals climb.
func (m Model) tokenReport() string {
	var b strings.Builder
	row := func(label string, t state.Tokens, cost float64) {
		fmt.Fprintf(&b, "%-10s in %-7s out %-7s cache-w %-7s cache-r %-7s  $%.4f\n",
			label,
			fmtTokens(t.Input), fmtTokens(t.Output),
			fmtTokens(t.CacheCreate), fmtTokens(t.CacheRead), cost)
	}

	fmt.Fprintf(&b, "context now: %s\n\n", ctxNote(m.lastContext, m.contextWindow))
	row("parent", m.parentTokens, m.totalCost())
	if m.dispatches > 0 || !m.childTokens.IsZero() {
		row("children", m.childTokens, m.childCost)
		fmt.Fprintf(&b, "%-10s %d task(s) dispatched\n", "", m.dispatches)
	}
	row("total", m.allTokens(), m.grandTotalCost())

	if note := m.billingNote(); note != "" {
		fmt.Fprintf(&b, "\nbilled to %s", note)
	}
	return strings.TrimRight(b.String(), "\n")
}
