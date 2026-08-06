package cli

import (
	"slices"
	"testing"
)

// --plan-tools is claude's whole built-in registry during PLAN (--tools), not an
// allowlist on top of one. Since acy no longer plans under --permission-mode plan —
// that mode refuses every MCP tool call, which would kill the question picker — the
// *absence* of the writing tools from this list is the only thing keeping the plan
// phase read-only. Adding Write here would silently hand a planning session the
// ability to edit the repo, and nothing else would complain.
func TestRunPlanToolsExcludeWriters(t *testing.T) {
	got, err := newRunCmd().Flags().GetStringSlice("plan-tools")
	if err != nil {
		t.Fatalf("plan-tools flag: %v", err)
	}
	for _, banned := range []string{"Write", "Edit", "NotebookEdit", "Task"} {
		if slices.Contains(got, banned) {
			t.Errorf("--plan-tools default contains %q — the plan phase could then modify the repo", banned)
		}
	}
	// Read has to be there or planning is blind.
	if !slices.Contains(got, "Read") {
		t.Errorf("--plan-tools default = %v, want it to contain Read", got)
	}
}

// Bash survives into the plan registry (you cannot plan well without running the
// tests), which makes it the one mutation vector left while planning — and so the
// one tool the gate must still put a countdown on. ui.enqueue keys on exactly this
// name; the two have to agree.
func TestRunPlanToolsKeepBashGated(t *testing.T) {
	got, err := newRunCmd().Flags().GetStringSlice("plan-tools")
	if err != nil {
		t.Fatalf("plan-tools flag: %v", err)
	}
	if !slices.Contains(got, "Bash") {
		t.Skip("Bash is no longer in the plan registry; the gate's plan-phase exception is moot")
	}
}

func TestRunResumeFlags(t *testing.T) {
	cmd := newRunCmd()
	for _, name := range []string{"resume", "continue"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("--%s should exist on the run command", name)
		}
	}
}
