package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// fakeRunner drives a canned response per call index, so classification and
// ordering tests never touch a real process.
func fakeRunner(t *testing.T, calls ...func(ctx context.Context, dir string, env []string, argv []string) (string, int, error)) Runner {
	t.Helper()
	i := 0
	return func(ctx context.Context, dir string, env []string, argv []string) (string, int, error) {
		if i >= len(calls) {
			t.Fatalf("fakeRunner: call %d exceeds the %d configured calls", i, len(calls))
		}
		fn := calls[i]
		i++
		return fn(ctx, dir, env, argv)
	}
}

func TestRunClassification(t *testing.T) {
	tests := []struct {
		name       string
		call       func(ctx context.Context, dir string, env []string, argv []string) (string, int, error)
		wantStatus engineerwire.VerifyStatus
		wantExit   int
	}{
		{
			name: "passed",
			call: func(ctx context.Context, dir string, env []string, argv []string) (string, int, error) {
				return "ok", 0, nil
			},
			wantStatus: engineerwire.VerifyPassed,
			wantExit:   0,
		},
		{
			name: "failed",
			call: func(ctx context.Context, dir string, env []string, argv []string) (string, int, error) {
				return "boom", 1, nil
			},
			wantStatus: engineerwire.VerifyFailed,
			wantExit:   1,
		},
		{
			name: "skipped",
			call: func(ctx context.Context, dir string, env []string, argv []string) (string, int, error) {
				return "", -1, fmt.Errorf("%w: %s", ErrNotInstalled, argv[0])
			},
			wantStatus: engineerwire.VerifySkipped,
			wantExit:   -1,
		},
		{
			name: "timeout",
			call: func(ctx context.Context, dir string, env []string, argv []string) (string, int, error) {
				return "", -1, context.DeadlineExceeded
			},
			wantStatus: engineerwire.VerifyTimeout,
			wantExit:   -1,
		},
		{
			name: "error",
			call: func(ctx context.Context, dir string, env []string, argv []string) (string, int, error) {
				return "", -1, errors.New("launch failed")
			},
			wantStatus: engineerwire.VerifyError,
			wantExit:   -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := fakeRunner(t, tt.call)
			checks := Run(context.Background(), run, "/tmp", []string{"mytool --flag"}, 0)
			if len(checks) != 1 {
				t.Fatalf("len(checks) = %d, want 1", len(checks))
			}
			got := checks[0]
			if got.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tt.wantStatus)
			}
			if got.ExitCode != tt.wantExit {
				t.Errorf("ExitCode = %d, want %d", got.ExitCode, tt.wantExit)
			}
			if got.Name != "mytool --flag" {
				t.Errorf("Name = %q, want %q", got.Name, "mytool --flag")
			}
			if want := []string{"mytool", "--flag"}; !equalSlices(got.Argv, want) {
				t.Errorf("Argv = %v, want %v", got.Argv, want)
			}
		})
	}
}

func TestRunArgvParsing(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		wantArgv []string
	}{
		{name: "single word", command: "true", wantArgv: []string{"true"}},
		{name: "multi space", command: "go   test   ./...", wantArgv: []string{"go", "test", "./..."}},
		{name: "leading/trailing space", command: "  echo hi  ", wantArgv: []string{"echo", "hi"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotArgv []string
			run := fakeRunner(t, func(ctx context.Context, dir string, env []string, argv []string) (string, int, error) {
				gotArgv = argv
				return "", 0, nil
			})
			checks := Run(context.Background(), run, "/tmp", []string{tt.command}, 0)
			if len(checks) != 1 {
				t.Fatalf("len(checks) = %d, want 1", len(checks))
			}
			if !equalSlices(gotArgv, tt.wantArgv) {
				t.Errorf("argv passed to Runner = %v, want %v", gotArgv, tt.wantArgv)
			}
			if !equalSlices(checks[0].Argv, tt.wantArgv) {
				t.Errorf("VerifyCheck.Argv = %v, want %v", checks[0].Argv, tt.wantArgv)
			}
		})
	}
}

func TestRunEmptyCommandNoPanic(t *testing.T) {
	run := fakeRunner(t) // no calls configured: an empty command must never reach the Runner
	checks := Run(context.Background(), run, "/tmp", []string{"", "   "}, 0)
	if len(checks) != 2 {
		t.Fatalf("len(checks) = %d, want 2", len(checks))
	}
	for i, c := range checks {
		if c.Status != engineerwire.VerifyError {
			t.Errorf("checks[%d].Status = %q, want %q", i, c.Status, engineerwire.VerifyError)
		}
		if c.Argv != nil {
			t.Errorf("checks[%d].Argv = %v, want nil", i, c.Argv)
		}
	}
}

func TestRunNilAndEmptyCommands(t *testing.T) {
	run := fakeRunner(t)
	if got := Run(context.Background(), run, "/tmp", nil, 0); got != nil {
		t.Errorf("Run with nil commands = %v, want nil", got)
	}
	if got := Run(context.Background(), run, "/tmp", []string{}, 0); got != nil {
		t.Errorf("Run with empty commands = %v, want nil", got)
	}
}

func TestRunOrdering(t *testing.T) {
	var seen []string
	run := fakeRunner(t,
		func(ctx context.Context, dir string, env []string, argv []string) (string, int, error) {
			seen = append(seen, argv[0])
			return "", 0, nil
		},
		func(ctx context.Context, dir string, env []string, argv []string) (string, int, error) {
			seen = append(seen, argv[0])
			return "", 1, nil
		},
		func(ctx context.Context, dir string, env []string, argv []string) (string, int, error) {
			seen = append(seen, argv[0])
			return "", 0, nil
		},
	)
	checks := Run(context.Background(), run, "/tmp", []string{"first", "second", "third"}, 0)
	if len(checks) != 3 {
		t.Fatalf("len(checks) = %d, want 3", len(checks))
	}
	if want := []string{"first", "second", "third"}; !equalSlices(seen, want) {
		t.Errorf("call order = %v, want %v", seen, want)
	}
	for i, name := range []string{"first", "second", "third"} {
		if checks[i].Name != name {
			t.Errorf("checks[%d].Name = %q, want %q", i, checks[i].Name, name)
		}
	}
}

func TestRunParentCancelShortCircuits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	run := fakeRunner(t,
		func(ctx context.Context, dir string, env []string, argv []string) (string, int, error) {
			calls++
			cancel() // simulate the parent giving up mid-command
			return "", 0, nil
		},
	)
	checks := Run(ctx, run, "/tmp", []string{"first", "second", "third"}, 0)
	if calls != 1 {
		t.Fatalf("Runner called %d times, want exactly 1 (parent cancel must stop the loop)", calls)
	}
	if len(checks) != 0 {
		t.Fatalf("checks = %v, want none: the in-flight command was cut short by the parent, not completed", checks)
	}
}

func TestRunParentAlreadyDoneSkipsEverything(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run := fakeRunner(t) // no calls expected at all
	checks := Run(ctx, run, "/tmp", []string{"first", "second"}, 0)
	if checks != nil {
		t.Fatalf("checks = %v, want nil when ctx is already done before Run starts", checks)
	}
}

func TestRunNilRunnerUsesDefaultRunner(t *testing.T) {
	// A nil Runner must fall back to DefaultRunner rather than panicking.
	checks := Run(context.Background(), nil, "/tmp", []string{"true"}, time.Second)
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(checks))
	}
	if checks[0].Status != engineerwire.VerifyPassed && checks[0].Status != engineerwire.VerifySkipped {
		t.Errorf("Status = %q, want passed or skipped (host may lack `true`)", checks[0].Status)
	}
}

func TestTruncateOutputUnderCap(t *testing.T) {
	s := strings.Repeat("a", maxOutputBytes-1)
	out, truncated := truncateOutput(s)
	if truncated {
		t.Errorf("truncated = true for output under the cap")
	}
	if out != s {
		t.Errorf("output under the cap was modified")
	}
}

func TestTruncateOutputAtCap(t *testing.T) {
	s := strings.Repeat("a", maxOutputBytes)
	out, truncated := truncateOutput(s)
	if truncated {
		t.Errorf("truncated = true for output exactly at the cap")
	}
	if out != s {
		t.Errorf("output exactly at the cap was modified")
	}
}

func TestTruncateOutputOverCap(t *testing.T) {
	head := strings.Repeat("H", 5000)
	tail := strings.Repeat("T", 5000)
	s := head + tail

	out, truncated := truncateOutput(s)
	if !truncated {
		t.Fatalf("truncated = false for output over the cap")
	}
	if len(out) > maxOutputBytes+200 {
		t.Errorf("truncated output len = %d, not bounded near maxOutputBytes (%d)", len(out), maxOutputBytes)
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("truncated output has no marker line: %q", out)
	}
	if !strings.HasPrefix(out, strings.Repeat("H", 100)) {
		t.Errorf("truncated output does not preserve the head of the original")
	}
	if !strings.HasSuffix(out, strings.Repeat("T", 100)) {
		t.Errorf("truncated output does not preserve the tail of the original")
	}
}

func TestTruncateOutputDoesNotSplitARune(t *testing.T) {
	// A multi-byte rune sitting right at the head/tail cut point must survive
	// whole, on one side or the other, never half-emitted as invalid UTF-8.
	// Since the marker itself is plain ASCII, the whole result is valid UTF-8
	// iff neither the head nor the tail cut a rune in half.
	rune3byte := "☃" // 3 bytes
	half := maxOutputBytes / 2

	filler := strings.Repeat("x", half-1)
	s := filler + rune3byte + strings.Repeat("y", maxOutputBytes)

	out, truncated := truncateOutput(s)
	if !truncated {
		t.Fatalf("expected truncation")
	}
	if !utf8.ValidString(out) {
		t.Errorf("truncated output split a rune: %q", out)
	}
}

func TestStripACYLive(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"ACY_LIVE=1",
		"ACY_LIVE=",
		"HOME=/home/x",
		"ACY_LIVE_OTHER=keepme", // shares the ACY_LIVE prefix but not the "=" boundary the filter keys on
	}
	got := StripACYLive(env)
	for _, kv := range got {
		if strings.HasPrefix(kv, "ACY_LIVE=") {
			t.Errorf("StripACYLive left %q in the result", kv)
		}
	}
	want := []string{"PATH=/usr/bin", "HOME=/home/x", "ACY_LIVE_OTHER=keepme"}
	if !equalSlices(got, want) {
		t.Errorf("StripACYLive(%v) = %v, want %v", env, got, want)
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
