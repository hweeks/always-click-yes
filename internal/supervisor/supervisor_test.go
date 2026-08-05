package supervisor

import (
	"testing"

	"github.com/hweeks/always-click-yes/internal/state"
)

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
