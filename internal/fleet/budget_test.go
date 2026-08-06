package fleet

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

const timeout = time.Second

// --- fleet-wide spend ceiling ---

// Launch refuses once the fleet's engineer spend reaches the configured
// ceiling, and the refusal names both the ceiling and the current spend —
// the same shape orchestrator's run-budget refusal takes.
func TestManagerLaunchRefusedAtRunBudgetCeiling(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 2))
	cfg.RunBudgetUSD = new(1.0)
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"}); err != nil {
		t.Fatalf("Launch 1: %v", err)
	}
	drainEvent(t, m, timeout) // started

	eng := mt.byTicket["T1"]
	eng.send(resultAt(1.5))
	drainEvent(t, m, timeout) // result
	waitFor(t, timeout, func() bool { return m.Active() == 0 })

	_, err := m.Launch(context.Background(), LaunchReq{Ticket: "T2"})
	if err == nil {
		t.Fatal("Launch at the run-budget ceiling: want error, got nil")
	}
	for _, want := range []string{"$1.00", "$1.5000", "fleet.runBudgetUSD", "Finish", "do not retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q missing %q", err.Error(), want)
		}
	}
}

// A ceiling of zero (the config default) means unlimited: Launch never
// refuses regardless of how much the ledger has accumulated.
func TestManagerLaunchUncappedRunBudgetNeverRefuses(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 2)) // RunBudgetUSD left nil
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"}); err != nil {
		t.Fatalf("Launch 1: %v", err)
	}
	drainEvent(t, m, timeout)
	eng := mt.byTicket["T1"]
	eng.send(resultAt(1000))
	drainEvent(t, m, timeout)
	waitFor(t, timeout, func() bool { return m.Active() == 0 })

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T2"}); err != nil {
		t.Fatalf("Launch with no run budget configured: %v", err)
	}
}

// --- per-engineer clamp math ---

// effectiveBudgetLocked combines the requested budget, the fleet default,
// and the money left under the ceiling — mirroring
// orchestrator.effectiveBudgetLocked's own table test. A launch can never
// get a bigger engineer budget than the run has left, no matter how big it
// asks.
func TestManagerEffectiveBudgetLockedTable(t *testing.T) {
	tests := []struct {
		name      string
		ceiling   float64 // 0 = unlimited
		spent     float64
		fleetDflt float64 // 0 = none configured
		requested float64 // 0 = "use the default"
		want      float64
	}{
		{name: "no ceiling, no request: falls back to fleet default", ceiling: 0, fleetDflt: 3, requested: 0, want: 3},
		{name: "no ceiling: requested wins outright", ceiling: 0, fleetDflt: 3, requested: 7, want: 7},
		{name: "ceiling with room: requested passes through unclamped", ceiling: 10, spent: 2, fleetDflt: 3, requested: 4, want: 4},
		{name: "requested exceeds remaining: clamped to remaining", ceiling: 10, spent: 8, fleetDflt: 3, requested: 5, want: 2},
		{name: "no request, default exceeds remaining: clamped to remaining", ceiling: 10, spent: 8, fleetDflt: 5, requested: 0, want: 2},
		{name: "no request, no default, ceiling has room: remaining stands in", ceiling: 10, spent: 8, fleetDflt: 0, requested: 0, want: 2},
		{name: "ceiling already exhausted: remaining floors at what is left, even zero", ceiling: 10, spent: 10, fleetDflt: 3, requested: 4, want: 0},
		{name: "ceiling overspent: remaining goes negative rather than clamping at zero", ceiling: 10, spent: 12, fleetDflt: 3, requested: 4, want: -2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mt := newMockTransport()
			cfg := testFleetConfig(testHost("a", 1))
			if tt.ceiling > 0 {
				cfg.RunBudgetUSD = new(tt.ceiling)
			} else {
				cfg.RunBudgetUSD = nil
			}
			if tt.fleetDflt > 0 {
				cfg.EngineerBudgetUSD = new(tt.fleetDflt)
			} else {
				cfg.EngineerBudgetUSD = nil
			}
			m := NewManager(cfg, mt.forHost)
			t.Cleanup(m.Close)

			m.mu.Lock()
			m.spentBefore = tt.spent
			got := m.effectiveBudgetLocked(tt.requested)
			m.mu.Unlock()

			if got != tt.want {
				t.Errorf("effectiveBudgetLocked(%v) = %v, want %v", tt.requested, got, tt.want)
			}
		})
	}
}

// The clamp is not just a computed number nobody uses — Launch actually
// hands the clamped figure to Transport.Start, so an engineer cannot spend
// past the run ceiling by asking its Spec for more than is left.
func TestManagerLaunchSendsClampedBudgetToTransport(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 1))
	cfg.RunBudgetUSD = new(5.0)
	cfg.EngineerBudgetUSD = new(10.0)
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	m.SeedSpent(2.0)

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	drainEvent(t, m, timeout)

	got := mt.specFor("T1").BudgetUSD
	if got != 3.0 {
		t.Errorf("Spec.BudgetUSD = %v, want 3 (ceiling 5 - spent 2, well under the fleet default of 10)", got)
	}
}

// --- SeedSpent ---

// SeedSpent's cumulative total counts toward the run ceiling immediately,
// with no engineer yet in the ledger to carry it.
func TestManagerSeedSpentCountsTowardCeiling(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 1))
	cfg.RunBudgetUSD = new(2.0)
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	m.SeedSpent(2.0)

	_, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"})
	if err == nil {
		t.Fatal("Launch after SeedSpent reaches the ceiling: want error, got nil")
	}
	if !strings.Contains(err.Error(), "$2.00") {
		t.Errorf("refusal %q does not name the ceiling", err.Error())
	}
}

// SeedSpent only ever raises the seeded figure, and never double-counts
// against the ledger's own cost sum once engineers land in it — spentLocked
// takes the higher of the two rather than adding them.
func TestManagerSeedSpentDoesNotDoubleCountAgainstLedger(t *testing.T) {
	mt := newMockTransport()
	cfg := testFleetConfig(testHost("a", 2))
	cfg.RunBudgetUSD = new(3.0)
	m := NewManager(cfg, mt.forHost)
	t.Cleanup(m.Close)

	// Seed with the same total a resumed engineer will shortly report itself.
	m.SeedSpent(2.0)

	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T1"}); err != nil {
		t.Fatalf("Launch 1: %v", err)
	}
	drainEvent(t, m, timeout)
	eng := mt.byTicket["T1"]
	eng.send(resultAt(2.0))
	drainEvent(t, m, timeout)
	waitFor(t, timeout, func() bool { return m.Active() == 0 })

	// If SeedSpent's 2.0 had been added to the ledger's own 2.0, spentLocked
	// would already read 4.0 against a ceiling of 3.0 and this would refuse.
	if _, err := m.Launch(context.Background(), LaunchReq{Ticket: "T2"}); err != nil {
		t.Fatalf("Launch 2 after the seeded engineer resolved: %v (want no double count)", err)
	}
}

// --- helpers ---

func resultAt(costUSD float64) engineerwire.Result {
	return engineerwire.Result{Outcome: "completed", CostUSD: costUSD}
}
