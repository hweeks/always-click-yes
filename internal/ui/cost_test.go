package ui

import (
	"strings"
	"testing"

	"github.com/hweeks/always-click-yes/internal/driver"
)

func resultEvent(cost float64) driver.Event {
	return driver.Event{Type: driver.TypeResult, StopReason: "end_turn", TotalCostUSD: cost}
}

func initEvent(apiKeySource string) driver.Event {
	return driver.Event{
		Type: driver.TypeSystem, Subtype: "init",
		SessionID: "s1", Model: "opus", PermissionMode: "plan",
		APIKeySource: apiKeySource,
	}
}

// Within one claude process total_cost_usd is a running total, so successive
// result events replace rather than add.
func TestCostWithinASessionIsNotDoubleCounted(t *testing.T) {
	m := New(nil, Config{})
	m.ingest(resultEvent(0.10))
	m.ingest(resultEvent(0.25)) // the same process, further along

	if got := m.totalCost(); got != 0.25 {
		t.Errorf("totalCost() = %v, want 0.25 (the latest running total, not 0.35)", got)
	}
}

// A resumed session's process starts its own total from zero, so the previous
// phase's spend has to be banked or it disappears.
func TestCostSurvivesADriverSwap(t *testing.T) {
	m := New(nil, Config{})
	m.ingest(resultEvent(0.40)) // plan phase
	m.settleCost()              // driver swap: plan -> auto-run
	m.ingest(resultEvent(0.15)) // the resumed process, counting from zero

	if got := m.totalCost(); got != 0.55 {
		t.Errorf("totalCost() = %v, want 0.55 (plan + auto-run)", got)
	}
}

func TestBillingFromAPIKeySource(t *testing.T) {
	tests := []struct {
		source, label, note string
	}{
		{"", "", "billing unknown"}, // no init event yet
		{"none", "subscription", "subscription (not billed)"},
		{"ANTHROPIC_API_KEY", "API", "API (billed)"},
		{"temporary", "API", "API (billed)"},
	}
	for _, tc := range tests {
		m := New(nil, Config{})
		if tc.source != "" {
			m.ingest(initEvent(tc.source))
		}
		if got := m.billing(); got != tc.label {
			t.Errorf("apiKeySource %q: billing() = %q, want %q", tc.source, got, tc.label)
		}
		if got := m.billingNote(); got != tc.note {
			t.Errorf("apiKeySource %q: billingNote() = %q, want %q", tc.source, got, tc.note)
		}
	}
}

// The final tally has to say which account actually paid: on a subscription the
// dollar figure claude reports is notional, not a charge.
func TestCompleteEntryNamesTheBillingAccount(t *testing.T) {
	m := New(nil, Config{})
	m.ingest(initEvent("none"))
	m.ingest(resultEvent(0.75))
	m.finish("completed", "")

	last := m.entries[len(m.entries)-1].body
	if !strings.Contains(last, "$0.7500") || !strings.Contains(last, "subscription (not billed)") {
		t.Errorf("completion entry = %q, want the total and the billing account", last)
	}
}
