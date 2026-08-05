package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hweeks/always-click-yes/internal/engineerd"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

func TestEngineerCmdIsHiddenWithExpectedSubcommands(t *testing.T) {
	cmd := newEngineerCmd()
	if !cmd.Hidden {
		t.Error("the engineer command group should be hidden")
	}
	got := map[string]bool{}
	for _, sub := range cmd.Commands() {
		got[sub.Name()] = true
	}
	for _, name := range []string{"start", "__run", "attach", "tail"} {
		if !got[name] {
			t.Errorf("engineer command missing subcommand %q (have %v)", name, got)
		}
	}
}

func TestEngineerStartRequiresClone(t *testing.T) {
	cmd := newEngineerStartCmd()
	cmd.SetArgs(nil)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Error("start without --clone should fail flag validation before ever spawning anything")
	}
}

func TestEngineerRunRequiresDir(t *testing.T) {
	cmd := newEngineerRunCmd()
	cmd.SetArgs(nil)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Error("__run without --dir should fail flag validation")
	}
}

func TestEngineerAttachRequiresExactlyOneID(t *testing.T) {
	cmd := newEngineerAttachCmd()
	cmd.SetArgs(nil)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Error("attach with no id should fail arg validation")
	}
}

func TestEngineerTailRequiresExactlyOneID(t *testing.T) {
	cmd := newEngineerTailCmd()
	cmd.SetArgs([]string{"a", "b"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Error("tail with two ids should fail arg validation")
	}
}

func TestEngineerTailDefaultsToFollow(t *testing.T) {
	cmd := newEngineerTailCmd()
	got, err := cmd.Flags().GetBool("follow")
	if err != nil {
		t.Fatalf("follow flag: %v", err)
	}
	if !got {
		t.Error("--follow should default to true")
	}
}

func TestEngineerAttachDefaultsFromToOne(t *testing.T) {
	cmd := newEngineerAttachCmd()
	got, err := cmd.Flags().GetInt64("from")
	if err != nil {
		t.Fatalf("from flag: %v", err)
	}
	if got != 1 {
		t.Errorf("--from default = %d, want 1", got)
	}
}

// prepareEngineerStart is the part of `start` that resolves the state
// directory and writes spec.json — no child process, so it is safe to
// exercise directly. The real detached spawn is proven later by a guarded
// live e2e, not here.
func TestPrepareEngineerStartWritesSpec(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ACY_STATE_DIR", root)

	specLine, err := engineerwire.Marshal(engineerwire.Spec{
		Ticket:     "T1",
		Title:      "do the thing",
		Brief:      "brief",
		Success:    "success",
		BaseBranch: "main",
		Branch:     "eng/t1",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	id, dir, err := prepareEngineerStart(bytes.NewReader(specLine), "/path/to/clone")
	if err != nil {
		t.Fatalf("prepareEngineerStart: %v", err)
	}
	wantDir, err := engineerd.Dir(id)
	if err != nil {
		t.Fatalf("engineerd.Dir: %v", err)
	}
	if dir != wantDir {
		t.Errorf("dir = %q, want %q", dir, wantDir)
	}

	stored, err := engineerd.ReadSpec(dir)
	if err != nil {
		t.Fatalf("ReadSpec: %v", err)
	}
	if stored.Spec.Ticket != "T1" {
		t.Errorf("stored spec ticket = %q, want T1", stored.Spec.Ticket)
	}
	if stored.ClonePath != "/path/to/clone" {
		t.Errorf("stored clone path = %q, want /path/to/clone", stored.ClonePath)
	}
	if stored.WorktreeDir != filepath.Join(dir, "worktree") {
		t.Errorf("stored worktree dir = %q, want %q", stored.WorktreeDir, filepath.Join(dir, "worktree"))
	}
}

func TestPrepareEngineerStartRejectsNonSpecLine(t *testing.T) {
	t.Setenv("ACY_STATE_DIR", t.TempDir())

	line, err := engineerwire.Marshal(engineerwire.Cancel{Reason: "not a spec"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, _, err := prepareEngineerStart(bytes.NewReader(line), "/clone"); err == nil {
		t.Error("prepareEngineerStart accepted a non-Spec line")
	}
}

func TestTailEngineerHumanReadableOutput(t *testing.T) {
	t.Setenv("ACY_STATE_DIR", t.TempDir())

	id := engineerd.NewID()
	dir, err := engineerd.EnsureDir(id)
	if err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	j, err := engineerwire.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := j.Append(engineerwire.Hello{EngineerID: id, Host: "h", PID: 1}); err != nil {
		t.Fatalf("append hello: %v", err)
	}
	if _, err := j.Append(engineerwire.Result{Outcome: "completed", Summary: "all done"}); err != nil {
		t.Fatalf("append result: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var out bytes.Buffer
	if err := tailEngineer(t.Context(), dir, false, &out); err != nil {
		t.Fatalf("tailEngineer: %v", err)
	}
	got := out.String()
	for _, want := range []string{"hello", id, "result", "completed", "all done"} {
		if !strings.Contains(got, want) {
			t.Errorf("tail output = %q, want it to contain %q", got, want)
		}
	}
}
