package cli

import (
	"slices"
	"testing"
)

// Plan mode has no gate wired in, so a tool missing from --plan-tools never runs
// while planning. Dropping AskUserQuestion from the default would not fail
// anything loudly — claude would just stop offering the tool, and the ask panel
// (ui/ask.go) would go quietly unreachable. Pin the default so that regression
// has to be deliberate.
func TestRunPlanToolsDefault(t *testing.T) {
	got, err := newRunCmd().Flags().GetStringSlice("plan-tools")
	if err != nil {
		t.Fatalf("plan-tools flag: %v", err)
	}
	for _, want := range []string{"Monitor", "AskUserQuestion"} {
		if !slices.Contains(got, want) {
			t.Errorf("--plan-tools default = %v, want it to contain %q", got, want)
		}
	}
}
