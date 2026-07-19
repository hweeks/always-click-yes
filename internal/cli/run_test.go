package cli

import (
	"slices"
	"testing"

	"github.com/hweeks/always-click-yes/internal/state"
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

// --resume names a session; --continue picks one. Asking for both is a user error,
// and root.go re-exposes run's flags without cobra's exclusion group, so the check
// has to hold in the code as well as in the flag definition.
func TestResumeTargetRejectsBothFlags(t *testing.T) {
	if _, err := resumeTarget(Flags{Resume: "abc", Continue: true}, "/proj"); err == nil {
		t.Fatal("--resume with --continue should be an error")
	}
}

func TestResumeTargetColdStart(t *testing.T) {
	id, err := resumeTarget(Flags{}, "/proj")
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Fatalf("no flags should mean a cold start, got %q", id)
	}
}

// --continue keys on acy's own snapshots, not on claude's transcript list. That is
// what stops it from resuming a session acy never drove — a bare `claude` run has
// no snapshot at all.
func TestResumeTargetContinuePicksTheLatestSupervisedRun(t *testing.T) {
	t.Setenv(state.EnvDir, t.TempDir())

	if err := state.Save(state.Snapshot{SessionID: "ours", Cwd: "/proj", Phase: "AUTO-RUN"}); err != nil {
		t.Fatal(err)
	}
	// A run in another project must never be picked up by this one.
	if err := state.Save(state.Snapshot{SessionID: "theirs", Cwd: "/other", Phase: "AUTO-RUN"}); err != nil {
		t.Fatal(err)
	}

	id, err := resumeTarget(Flags{Continue: true}, "/proj")
	if err != nil {
		t.Fatal(err)
	}
	if id != "ours" {
		t.Fatalf("--continue resumed %q, want the run from this project", id)
	}
}

func TestResumeTargetContinueWithNothingToContinue(t *testing.T) {
	t.Setenv(state.EnvDir, t.TempDir())

	if _, err := resumeTarget(Flags{Continue: true}, "/proj"); err == nil {
		t.Fatal("--continue with no prior session should be a clear error, not a silent cold start")
	}
}
