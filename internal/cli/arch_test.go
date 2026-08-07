package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/gitops"
	"github.com/hweeks/always-click-yes/internal/supervisor"
)

// writeACYJSON marshals body as .acy.json in dir and chdirs the test there.
func writeACYJSON(t *testing.T, dir string, body map[string]any) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".acy.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
}

// TestArchRequiresFleetSection proves acy arch refuses to start rather than
// silently running an architect with no engineers to delegate to — the same
// clear-error shape TestFleetDoctorRequiresFleetSection proves for
// `acy fleet doctor`.
func TestArchRequiresFleetSection(t *testing.T) {
	t.Run("no .acy.json", func(t *testing.T) {
		t.Chdir(t.TempDir())
		f := supervisor.Flags{}
		_, err := resolveArchFlags(&f, func(string) bool { return false })
		if err == nil {
			t.Fatal("want an error when the project has no .acy.json")
		}
		if !strings.Contains(err.Error(), "fleet") {
			t.Errorf("error should mention the missing fleet section: %v", err)
		}
	})

	t.Run(".acy.json with no fleet key", func(t *testing.T) {
		writeACYJSON(t, t.TempDir(), map[string]any{"model": "opus"})
		f := supervisor.Flags{}
		_, err := resolveArchFlags(&f, func(string) bool { return false })
		if err == nil {
			t.Fatal("want an error when .acy.json has no fleet section")
		}
		if !strings.Contains(err.Error(), "fleet") {
			t.Errorf("error should mention the missing fleet section: %v", err)
		}
	})
}

// TestArchModelDefault proves --model only falls back to defaultArchModel
// when neither the flag nor .acy.json set one — an explicit choice, from
// either source, must always win.
func TestArchModelDefault(t *testing.T) {
	fleetSection := map[string]any{"hosts": []map[string]any{{"name": "local"}}}

	tests := []struct {
		name        string
		flagModel   string
		flagChanged bool
		fileModel   string
		want        string
	}{
		{name: "flag set", flagModel: "sonnet", flagChanged: true, want: "sonnet"},
		{name: "file set", fileModel: "haiku", want: "haiku"},
		{name: "neither set", want: defaultArchModel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := map[string]any{"fleet": fleetSection}
			if tt.fileModel != "" {
				body["model"] = tt.fileModel
			}
			writeACYJSON(t, t.TempDir(), body)

			f := supervisor.Flags{Model: tt.flagModel}
			changed := func(name string) bool { return name == "model" && tt.flagChanged }
			if _, err := resolveArchFlags(&f, changed); err != nil {
				t.Fatal(err)
			}
			if f.Model != tt.want {
				t.Errorf("model = %q, want %q", f.Model, tt.want)
			}
		})
	}
}

func TestArchCmdSharesRunFlags(t *testing.T) {
	archFlags := newArchCmd().Flags()
	runFlags := newRunCmd().Flags()
	for _, name := range []string{"model", "countdown", "child-model", "task-budget", "resume", "continue"} {
		if archFlags.Lookup(name) == nil {
			t.Errorf("acy arch is missing --%s", name)
		}
		if runFlags.Lookup(name) == nil {
			t.Errorf("acy run is missing --%s (test setup)", name)
		}
	}
}

// fakeExitErr fakes the ExitCode() method *exec.ExitError gets from its
// promoted *os.ProcessState, so a test can drive gitops.StackAvailable's
// exit-code-9 branch (-> ErrStackNotEnabled) without shelling out to a real
// gh.
type fakeExitErr struct{ code int }

func (e *fakeExitErr) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *fakeExitErr) ExitCode() int { return e.code }

// runnerUnavailable fails `gh stack --version` with a plain (non-exit-code)
// error, then fails the `gh extension list` fallback StackAvailable makes in
// response by returning output that doesn't mention gh-stack — the "gh-stack
// just isn't usable" case, with no ErrStackNotEnabled sentinel involved.
func runnerUnavailable(t *testing.T) gitops.Runner {
	t.Helper()
	return func(_ context.Context, _, _ string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "stack" {
			return "", errors.New("gh: command not found")
		}
		if len(args) > 0 && args[0] == "extension" {
			return "some-other-extension\t1.0.0\n", nil
		}
		t.Fatalf("unexpected runner call with args %v", args)
		return "", nil
	}
}

// runnerNotEnabled fails `gh stack --version` with exit code 9, the code
// gh-stack's own doc comment (internal/gitops/stack.go) assigns to "stacked
// PRs not enabled for this repository" — StackAvailable turns that directly
// into ErrStackNotEnabled without ever trying the extension-list fallback.
func runnerNotEnabled(t *testing.T) gitops.Runner {
	t.Helper()
	return func(_ context.Context, _, _ string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "stack" {
			return "", &fakeExitErr{code: 9}
		}
		t.Fatalf("unexpected runner call with args %v", args)
		return "", nil
	}
}

// runnerAvailable succeeds `gh stack --version` immediately, so
// StackAvailable never makes the extension-list fallback call.
func runnerAvailable(_ context.Context, _, _ string, _ ...string) (string, error) {
	return "gh-stack version 0.1.0", nil
}

// runnerBlocksUntilCanceled proves resolveStackMode's timeout actually bounds
// the probe: it never returns on its own, only once ctx is done, which
// StackAvailable's fallback call also observes immediately since the same
// (already-expired) context is reused for it.
func runnerBlocksUntilCanceled(ctx context.Context, _, _ string, _ ...string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

// TestResolveStackMode covers resolveStackMode's full decision table: "off"
// never probes, "ask" downgrades on any probe failure without erroring, and
// "chain" turns the same failure into a hard, actionable error instead —
// see resolveStackMode's own doc comment in arch.go for why that asymmetry
// is deliberate.
func TestResolveStackMode(t *testing.T) {
	ctx := context.Background()

	t.Run("off never probes", func(t *testing.T) {
		cfg := &config.FleetConfig{StackMode: "off"}
		run := func(context.Context, string, string, ...string) (string, error) {
			t.Fatal("probe must not run when stackMode is \"off\"")
			return "", nil
		}
		mode, note, err := resolveStackMode(ctx, cfg, run, "/tmp", stackProbeTimeout)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mode != "off" {
			t.Errorf("mode = %q, want %q", mode, "off")
		}
		if note != "" {
			t.Errorf("note = %q, want empty", note)
		}
	})

	t.Run("ask downgrades on unavailable", func(t *testing.T) {
		cfg := &config.FleetConfig{StackMode: "ask"}
		mode, note, err := resolveStackMode(ctx, cfg, runnerUnavailable(t), "/tmp", stackProbeTimeout)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mode != "off" {
			t.Errorf("mode = %q, want %q", mode, "off")
		}
		if note == "" {
			t.Error("want a non-empty note explaining the downgrade")
		}
		if !strings.Contains(note, "gh extension install github/gh-stack") {
			t.Errorf("note should name the install fix: %q", note)
		}
	})

	t.Run("ask downgrades on ErrStackNotEnabled", func(t *testing.T) {
		cfg := &config.FleetConfig{StackMode: "ask"}
		mode, note, err := resolveStackMode(ctx, cfg, runnerNotEnabled(t), "/tmp", stackProbeTimeout)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mode != "off" {
			t.Errorf("mode = %q, want %q", mode, "off")
		}
		if note == "" {
			t.Error("want a non-empty note explaining the downgrade")
		}
		if !strings.Contains(note, "public preview") {
			t.Errorf("note should mention the public preview: %q", note)
		}
	})

	t.Run("chain errors on unavailable", func(t *testing.T) {
		cfg := &config.FleetConfig{StackMode: "chain"}
		_, _, err := resolveStackMode(ctx, cfg, runnerUnavailable(t), "/tmp", stackProbeTimeout)
		if err == nil {
			t.Fatal("want an error when stackMode is \"chain\" and gh-stack is unavailable")
		}
		if !strings.Contains(err.Error(), "gh extension install github/gh-stack") {
			t.Errorf("error should name the install fix: %v", err)
		}
	})

	t.Run("chain errors on ErrStackNotEnabled", func(t *testing.T) {
		cfg := &config.FleetConfig{StackMode: "chain"}
		_, _, err := resolveStackMode(ctx, cfg, runnerNotEnabled(t), "/tmp", stackProbeTimeout)
		if err == nil {
			t.Fatal("want an error when stackMode is \"chain\" and gh-stack is unavailable")
		}
		if !strings.Contains(err.Error(), "public preview") {
			t.Errorf("error should mention the public preview: %v", err)
		}
		if !errors.Is(err, gitops.ErrStackNotEnabled) {
			t.Errorf("error should wrap ErrStackNotEnabled so the real gh complaint is inspectable: %v", err)
		}
	})

	for _, m := range []string{"ask", "chain"} {
		t.Run(m+" unchanged when available", func(t *testing.T) {
			cfg := &config.FleetConfig{StackMode: m}
			mode, note, err := resolveStackMode(ctx, cfg, runnerAvailable, "/tmp", stackProbeTimeout)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mode != m {
				t.Errorf("mode = %q, want %q", mode, m)
			}
			if note != "" {
				t.Errorf("note = %q, want empty", note)
			}
		})
	}

	t.Run("timeout is treated as an ordinary probe failure", func(t *testing.T) {
		const shortTimeout = 20 * time.Millisecond

		start := time.Now()
		mode, note, err := resolveStackMode(ctx, &config.FleetConfig{StackMode: "ask"}, runnerBlocksUntilCanceled, "/tmp", shortTimeout)
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("took %s, want well under a second", elapsed)
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mode != "off" {
			t.Errorf("mode = %q, want %q", mode, "off")
		}
		if note == "" {
			t.Error("want a non-empty note explaining the downgrade")
		}

		start = time.Now()
		_, _, err = resolveStackMode(ctx, &config.FleetConfig{StackMode: "chain"}, runnerBlocksUntilCanceled, "/tmp", shortTimeout)
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("took %s, want well under a second", elapsed)
		}
		if err == nil {
			t.Fatal("want an error when stackMode is \"chain\" and the probe times out")
		}
	})
}
