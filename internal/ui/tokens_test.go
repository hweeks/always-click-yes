package ui

import (
	"strings"
	"testing"

	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/state"
)

func usageEvent(in, out, cacheCreate, cacheRead int) driver.Event {
	return driver.Event{
		Type: driver.TypeResult, StopReason: "end_turn",
		Usage: &driver.Usage{
			InputTokens:              in,
			OutputTokens:             out,
			CacheCreationInputTokens: cacheCreate,
			CacheReadInputTokens:     cacheRead,
		},
		ModelUsage: map[string]driver.ModelUsage{
			"claude-fable-5": {ContextWindow: 1_000_000},
		},
	}
}

// The mirror image of TestCostWithinASessionIsNotDoubleCounted: usage is
// reported per turn, so successive results ADD. Cost and tokens are accumulated
// in opposite ways from the same event, which is exactly why this is pinned.
func TestTokensWithinASessionAccumulate(t *testing.T) {
	m := New(nil, Config{})
	m.ingest(usageEvent(10, 100, 5_000, 40_000))
	m.ingest(usageEvent(2, 250, 1_000, 60_000))

	got := m.parentTokens
	want := state.Tokens{Input: 12, Output: 350, CacheCreate: 6_000, CacheRead: 100_000}
	if got != want {
		t.Errorf("parentTokens = %+v, want %+v", got, want)
	}
}

// A driver swap banks cost because the next process restarts its total at zero.
// Tokens need no such handling — settleCost must leave them alone, and the tally
// must keep climbing straight through.
func TestTokensSurviveADriverSwapWithoutSettling(t *testing.T) {
	m := New(nil, Config{})
	m.ingest(usageEvent(1, 10, 1_000, 30_000))
	m.settleCost() // plan -> auto-run
	m.ingest(usageEvent(1, 10, 1_000, 70_000))

	if got := m.parentTokens.CacheRead; got != 100_000 {
		t.Errorf("CacheRead = %d, want 100000 (settling cost must not disturb tokens)", got)
	}
}

// lastContext is a reading, not a tally: it describes the turn that just ended.
func TestLastContextIsTheLatestTurnNotASum(t *testing.T) {
	m := New(nil, Config{})
	m.ingest(usageEvent(10, 100, 5_000, 40_000)) // context 45,010
	m.ingest(usageEvent(2, 250, 1_000, 60_000))  // context 61,002

	if got := m.lastContext; got != 61_002 {
		t.Errorf("lastContext = %d, want 61002", got)
	}
	if m.contextWindow != 1_000_000 {
		t.Errorf("contextWindow = %d, want 1000000", m.contextWindow)
	}
}

// A result without usage (an aborted turn) must not panic or zero the tally.
func TestResultWithoutUsageLeavesTheTallyIntact(t *testing.T) {
	m := New(nil, Config{})
	m.ingest(usageEvent(1, 10, 1_000, 30_000))
	m.ingest(driver.Event{Type: driver.TypeResult, StopReason: "aborted_streaming"})

	if got := m.parentTokens.CacheRead; got != 30_000 {
		t.Errorf("CacheRead = %d, want 30000", got)
	}
}

func TestAllTokensCombinesParentAndChildren(t *testing.T) {
	m := New(nil, Config{})
	m.parentTokens = state.Tokens{Input: 1, Output: 2, CacheCreate: 3, CacheRead: 4}
	m.childTokens = state.Tokens{Input: 10, Output: 20, CacheCreate: 30, CacheRead: 40}

	want := state.Tokens{Input: 11, Output: 22, CacheCreate: 33, CacheRead: 44}
	if got := m.allTokens(); got != want {
		t.Errorf("allTokens() = %+v, want %+v", got, want)
	}
	// Combining must not mutate the operands.
	if m.parentTokens.CacheRead != 4 {
		t.Error("allTokens() mutated parentTokens")
	}
}

func TestFmtTokens(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{812, "812"},
		{1_500, "1.5k"},
		{9_900, "9.9k"},
		{38_412, "38k"},
		{474_322, "474k"},
		{1_031_798, "1M"},
		{8_697_690, "8.7M"},
	}
	for _, c := range cases {
		if got := fmtTokens(c.in); got != c.want {
			t.Errorf("fmtTokens(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCtxNote(t *testing.T) {
	if got, want := ctxNote(0, 0), "ctx —"; got != want {
		t.Errorf("ctxNote(0,0) = %q, want %q", got, want)
	}
	if got, want := ctxNote(38_412, 0), "ctx 38k"; got != want {
		t.Errorf("ctxNote(38412,0) = %q, want %q", got, want)
	}
	if got, want := ctxNote(38_412, 1_000_000), "ctx 38k/1M"; got != want {
		t.Errorf("ctxNote(38412,1M) = %q, want %q", got, want)
	}
}

// The header must stay quiet until there is something to report, so a fresh
// session does not show a row of zeroes.
func TestTokenSummaryIsEmptyBeforeAnyTurn(t *testing.T) {
	m := New(nil, Config{})
	if got := m.tokenSummary(); got != "" {
		t.Errorf("tokenSummary() = %q, want empty", got)
	}
	m.ingest(usageEvent(10, 100, 5_000, 40_000))
	if got := m.tokenSummary(); !strings.Contains(got, "⇣40k") {
		t.Errorf("tokenSummary() = %q, want it to report 40k of cache reads", got)
	}
}

func TestTokenReportSeparatesParentFromChildren(t *testing.T) {
	m := New(nil, Config{})
	m.ingest(usageEvent(10, 100, 5_000, 40_000))
	m.childTokens = state.Tokens{CacheRead: 900_000}
	m.childCost = 2.50
	m.dispatches = 7

	got := m.tokenReport()
	for _, want := range []string{"parent", "children", "total", "40k", "900k", "940k"} {
		if !strings.Contains(got, want) {
			t.Errorf("tokenReport() missing %q:\n%s", want, got)
		}
	}
}
