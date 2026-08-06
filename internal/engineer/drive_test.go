package engineer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
	"github.com/hweeks/always-click-yes/internal/verify"
)

// --- fake verify runner ---

// fakeVerifyResult is what fakeVerifyRunner hands back for one scripted
// command.
type fakeVerifyResult struct {
	output   string
	exitCode int
	err      error
}

// fakeVerifyRunner stands in for verify.Runner: it answers whatever
// VerifyCommands asks for, scripted per the full command string (the same
// text verify.Run uses as VerifyCheck.Name), with no real binary involved.
// When block > 0, run blocks until ctx ends instead of answering — how the
// timeout test simulates a command that outlives its per-command deadline.
type fakeVerifyRunner struct {
	mu    sync.Mutex
	calls []string
	byCmd map[string]fakeVerifyResult
	block time.Duration
}

func (f *fakeVerifyRunner) run(ctx context.Context, _ string, _ []string, argv []string) (string, int, error) {
	key := strings.Join(argv, " ")
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()

	if f.block > 0 {
		select {
		case <-ctx.Done():
			return "", -1, ctx.Err()
		case <-time.After(f.block):
		}
	}

	res, ok := f.byCmd[key]
	if !ok {
		return "", -1, fmt.Errorf("fakeVerifyRunner: unscripted command %q", key)
	}
	return res.output, res.exitCode, res.err
}

func (f *fakeVerifyRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// --- shared session scripts ---

// finishAsSession scripts fs to reach PhaseAutoRun on Arm and then, on the
// first poll while idle in AUTO-RUN, call Finish with outcome/summary — the
// simplest script that drives Core.Run through finalize.
func finishAsSession(fs *fakeSession, outcome, summary string) {
	fs.onArm = func(f *fakeSession) { f.cur.Phase = PhaseAutoRun }
	fs.afterSnapshot = func(f *fakeSession) {
		if f.cur.Phase == PhaseAutoRun && f.cur.FinishOutcome == "" {
			f.cur.FinishOutcome = outcome
			f.cur.FinishSummary = summary
		}
	}
}

// journalLogEvents returns every EventLog-kind Event in j, in order.
func journalLogEvents(t *testing.T, j *engineerwire.Journal) []engineerwire.Event {
	t.Helper()
	msgs, err := j.ReplayFrom(1)
	if err != nil {
		t.Fatalf("ReplayFrom: %v", err)
	}
	var out []engineerwire.Event
	for _, m := range msgs {
		if e, ok := m.(engineerwire.Event); ok && e.Kind == engineerwire.EventLog {
			out = append(out, e)
		}
	}
	return out
}

// --- all checks pass ---

func TestFinalizeVerifyAllPass(t *testing.T) {
	fs := newFakeSession(Snapshot{Ready: true, Phase: PhasePlan})
	finishAsSession(fs, "completed", "added spin()")

	git := &fakeGit{revListOut: "1\n", prURL: "https://github.com/acme/widgets/pull/9"}
	spec := testSpec()
	cfg, j := newTestConfig(t, spec, git.run, fs)
	cfg.VerifyCommands = []string{"go build ./...", "gofmt -l ."}
	cfg.VerifyTimeout = time.Second
	fv := &fakeVerifyRunner{byCmd: map[string]fakeVerifyResult{
		"go build ./...": {exitCode: 0},
		"gofmt -l .":     {exitCode: 0},
	}}
	cfg.VerifyRunner = fv.run
	c := NewCore(cfg, j)

	result := c.Run(context.Background())

	if result.Outcome != "completed" {
		t.Fatalf("outcome = %q, want completed (model's own verdict, all checks passed)", result.Outcome)
	}
	if len(result.Verification) != 2 {
		t.Fatalf("verification = %#v, want 2 entries", result.Verification)
	}
	if result.Verification[0].Name != "go build ./..." || result.Verification[1].Name != "gofmt -l ." {
		t.Fatalf("verification order = %#v, want VerifyCommands order", result.Verification)
	}
	for i, chk := range result.Verification {
		if chk.Status != engineerwire.VerifyPassed {
			t.Fatalf("check %d status = %q, want passed", i, chk.Status)
		}
	}
	const wantHeader = "Verification (run by acy in the worktree, not reported by the session):"
	if !strings.Contains(result.Summary, wantHeader) {
		t.Fatalf("summary = %q, want it to contain the verify digest header", result.Summary)
	}
	if !strings.Contains(result.Summary, "go build ./... — passed (") {
		t.Fatalf("summary = %q, want a passed line for go build", result.Summary)
	}
	if !git.sawCall(wantHeader) {
		t.Fatal("PR body did not carry the verify digest")
	}

	assertOneResult(t, j)

	if got := journalLogEvents(t, j); len(got) != 2 {
		t.Fatalf("EventLog count = %d, want 2 (one per configured check): %#v", len(got), got)
	}
}

// --- one check fails ---

func TestFinalizeVerifyOneFails(t *testing.T) {
	fs := newFakeSession(Snapshot{Ready: true, Phase: PhasePlan})
	finishAsSession(fs, "completed", "added spin()")

	git := &fakeGit{revListOut: "1\n", prURL: "https://github.com/acme/widgets/pull/9"}
	spec := testSpec()
	cfg, j := newTestConfig(t, spec, git.run, fs)
	cfg.VerifyCommands = []string{"go build ./...", "go test -race ./..."}
	cfg.VerifyTimeout = time.Second
	fv := &fakeVerifyRunner{byCmd: map[string]fakeVerifyResult{
		"go build ./...":      {exitCode: 0},
		"go test -race ./...": {exitCode: 1, output: "--- FAIL: TestFoo\nfoo_test.go:12: boom"},
	}}
	cfg.VerifyRunner = fv.run
	c := NewCore(cfg, j)

	result := c.Run(context.Background())

	if result.Outcome != "failed" {
		t.Fatalf("outcome = %q, want failed (a check failed)", result.Outcome)
	}
	if !git.sawCall("pr create") {
		t.Fatal("PR must still open despite a failing check")
	}
	if !strings.Contains(result.Summary, "go test -race ./... — FAILED (exit 1,") {
		t.Fatalf("summary = %q, want it to name the failing command", result.Summary)
	}
	if len(result.Verification) != 2 || result.Verification[1].Status != engineerwire.VerifyFailed {
		t.Fatalf("verification = %#v, want second entry failed", result.Verification)
	}
	const wantExcerpt = "excerpt: --- FAIL: TestFoo\\nfoo_test.go:12: boom  [see Result.Verification for full output]"
	if !strings.Contains(result.Summary, wantExcerpt) {
		t.Fatalf("summary = %q, want the excerpt line %q", result.Summary, wantExcerpt)
	}

	assertOneResult(t, j)
}

// --- a check is skipped ---

func TestFinalizeVerifySkipped(t *testing.T) {
	fs := newFakeSession(Snapshot{Ready: true, Phase: PhasePlan})
	finishAsSession(fs, "completed", "added spin()")

	git := &fakeGit{revListOut: "1\n", prURL: "https://github.com/acme/widgets/pull/9"}
	spec := testSpec()
	cfg, j := newTestConfig(t, spec, git.run, fs)
	cfg.VerifyCommands = []string{"golangci-lint run ./..."}
	cfg.VerifyTimeout = time.Second
	fv := &fakeVerifyRunner{byCmd: map[string]fakeVerifyResult{
		"golangci-lint run ./...": {err: verify.ErrNotInstalled},
	}}
	cfg.VerifyRunner = fv.run
	c := NewCore(cfg, j)

	result := c.Run(context.Background())

	if result.Outcome != "completed" {
		t.Fatalf("outcome = %q, want completed (a skip is not a failure)", result.Outcome)
	}
	if len(result.Verification) != 1 || result.Verification[0].Status != engineerwire.VerifySkipped {
		t.Fatalf("verification = %#v, want one skipped entry", result.Verification)
	}
	if !strings.Contains(result.Summary, "golangci-lint run ./... — skipped (not installed on this host)") {
		t.Fatalf("summary = %q, want the skip named in the digest", result.Summary)
	}

	assertOneResult(t, j)
}

// --- a check times out ---

func TestFinalizeVerifyTimeout(t *testing.T) {
	fs := newFakeSession(Snapshot{Ready: true, Phase: PhasePlan})
	finishAsSession(fs, "completed", "added spin()")

	git := &fakeGit{revListOut: "1\n", prURL: "https://github.com/acme/widgets/pull/9"}
	spec := testSpec()
	cfg, j := newTestConfig(t, spec, git.run, fs)
	cfg.VerifyCommands = []string{"go test ./..."}
	cfg.VerifyTimeout = 5 * time.Millisecond
	fv := &fakeVerifyRunner{block: time.Second}
	cfg.VerifyRunner = fv.run
	c := NewCore(cfg, j)

	result := c.Run(context.Background())

	if result.Outcome != "completed" {
		t.Fatalf("outcome = %q, want completed (a timeout is not a failure)", result.Outcome)
	}
	if len(result.Verification) != 1 || result.Verification[0].Status != engineerwire.VerifyTimeout {
		t.Fatalf("verification = %#v, want one timeout entry", result.Verification)
	}

	assertOneResult(t, j)
}

// --- push fails after verification ran ---

func TestFinalizeVerifyRunsBeforePushFailure(t *testing.T) {
	fs := newFakeSession(Snapshot{Ready: true, Phase: PhasePlan})
	finishAsSession(fs, "completed", "done")

	git := &fakeGit{revListOut: "1\n", pushErr: fmt.Errorf("remote rejected: non-fast-forward")}
	spec := testSpec()
	cfg, j := newTestConfig(t, spec, git.run, fs)
	cfg.VerifyCommands = []string{"go build ./..."}
	cfg.VerifyTimeout = time.Second
	fv := &fakeVerifyRunner{byCmd: map[string]fakeVerifyResult{
		"go build ./...": {exitCode: 0},
	}}
	cfg.VerifyRunner = fv.run
	c := NewCore(cfg, j)

	result := c.Run(context.Background())

	if result.Outcome != "failed" {
		t.Fatalf("outcome = %q, want failed (push failed)", result.Outcome)
	}
	if len(result.Verification) != 1 {
		t.Fatalf("verification = %#v, want 1 entry even though push failed", result.Verification)
	}

	assertOneResult(t, j)
}

// --- ahead == 0 skips verification entirely ---

func TestFinalizeVerifySkippedWhenNoCommits(t *testing.T) {
	fs := newFakeSession(Snapshot{Ready: true, Phase: PhasePlan})
	finishAsSession(fs, "abandoned", "nothing needed changing")

	git := &fakeGit{revListOut: "0\n"}
	spec := testSpec()
	cfg, j := newTestConfig(t, spec, git.run, fs)
	cfg.VerifyCommands = []string{"go build ./..."}
	cfg.VerifyTimeout = time.Second
	fv := &fakeVerifyRunner{byCmd: map[string]fakeVerifyResult{
		"go build ./...": {exitCode: 0},
	}}
	cfg.VerifyRunner = fv.run
	c := NewCore(cfg, j)

	result := c.Run(context.Background())

	if fv.callCount() != 0 {
		t.Fatalf("verify runner called %d times, want 0 (no commits, nothing to verify)", fv.callCount())
	}
	if strings.Contains(result.Summary, "Verification (run by acy") {
		t.Fatalf("summary = %q, want no digest when there was nothing to push", result.Summary)
	}

	assertOneResult(t, j)
}

// --- VerifyCommands empty/nil: byte-identical to pre-verify behavior ---

func TestFinalizeVerifyEmptyCommandsIsNoOp(t *testing.T) {
	fs := newFakeSession(Snapshot{Ready: true, Phase: PhasePlan})
	finishAsSession(fs, "completed", "added spin()")

	git := &fakeGit{revListOut: "1\n", prURL: "https://github.com/acme/widgets/pull/9"}
	c, j := newTestCore(t, testSpec(), git.run, fs) // Config zero value: no VerifyCommands

	result := c.Run(context.Background())

	if result.Outcome != "completed" {
		t.Fatalf("outcome = %q, want completed", result.Outcome)
	}
	if result.Summary != "added spin()" {
		t.Fatalf("summary = %q, want it unchanged with no verify commands configured", result.Summary)
	}
	if len(result.Verification) != 0 {
		t.Fatalf("verification = %#v, want empty with no verify commands configured", result.Verification)
	}
	if got := journalLogEvents(t, j); len(got) != 0 {
		t.Fatalf("EventLog count = %d, want 0 with no verify commands configured: %#v", len(got), got)
	}

	assertOneResult(t, j)
}
