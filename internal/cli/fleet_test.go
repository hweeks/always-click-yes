package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFleetDoctorRequiresFleetSection proves the clear-error path: a project
// with no .acy.json at all, and one whose .acy.json has no "fleet" key,
// both refuse rather than silently reporting on zero hosts.
func TestFleetDoctorRequiresFleetSection(t *testing.T) {
	t.Run("no .acy.json", func(t *testing.T) {
		t.Chdir(t.TempDir())
		err := runFleetDoctor(context.Background(), io.Discard, false)
		if err == nil {
			t.Fatal("want an error when the project has no .acy.json")
		}
		if !strings.Contains(err.Error(), "fleet") {
			t.Errorf("error should mention the missing fleet section: %v", err)
		}
	})

	t.Run(".acy.json with no fleet key", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".acy.json"), []byte(`{"model":"opus"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		err := runFleetDoctor(context.Background(), io.Discard, false)
		if err == nil {
			t.Fatal("want an error when .acy.json has no fleet section")
		}
		if !strings.Contains(err.Error(), "fleet") {
			t.Errorf("error should mention the missing fleet section: %v", err)
		}
	})
}

// TestFleetDoctorJSONLocalEndToEnd runs the real command against a
// local-only fleet config: no fakes anywhere in this path, from LoadFile
// through fleet.Doctor's real Runner to actual git/claude/gh/acy processes
// on this machine. It only asserts on the JSON shape, not on whether every
// check passes — that depends on what's installed on whatever machine runs
// this test, which is exactly why `acy fleet doctor` exists.
func TestFleetDoctorJSONLocalEndToEnd(t *testing.T) {
	dir := t.TempDir()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...) //nolint:gosec // fixed argv, test setup only
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init")
	run("commit", "--allow-empty", "-m", "seed")

	if err := os.WriteFile(filepath.Join(dir, ".acy.json"), []byte(`{"fleet": {"hosts": [{"name": "local"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var buf bytes.Buffer
	// The exit error (some checks legitimately fail on a machine with no
	// "acy"/"claude"/"gh" on PATH) is not the thing under test here.
	_ = runFleetDoctor(ctx, &buf, true)

	var reports []hostReport
	if err := json.Unmarshal(buf.Bytes(), &reports); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, buf.String())
	}
	if len(reports) != 1 || reports[0].Host != "local" {
		t.Fatalf("reports = %+v", reports)
	}
	wantNames := []string{"ssh", "acy", "claude", "gh", "repo", "state"}
	if len(reports[0].Checks) != len(wantNames) {
		t.Fatalf("checks = %+v, want %d entries", reports[0].Checks, len(wantNames))
	}
	for i, name := range wantNames {
		if reports[0].Checks[i].Name != name {
			t.Errorf("checks[%d].Name = %q, want %q", i, reports[0].Checks[i].Name, name)
		}
	}
	// The local host's own ssh check is always an automatic pass.
	if !reports[0].Checks[0].OK {
		t.Errorf("local host's ssh check should always pass: %+v", reports[0].Checks[0])
	}
}

func TestFleetCmdHasDoctorSubcommand(t *testing.T) {
	cmd := newFleetCmd()
	if cmd.Hidden {
		t.Error("the fleet command group should be visible")
	}
	found := false
	for _, sub := range cmd.Commands() {
		if sub.Name() == "doctor" {
			found = true
		}
	}
	if !found {
		t.Error("fleet command missing the doctor subcommand")
	}
}
