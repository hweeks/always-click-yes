package tickets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// hermeticRunner mirrors internal/gitops's test helper: a gitops.Runner
// backed by the real git binary, isolated from the host's ~/.gitconfig and
// given a fixed commit identity so these tests pass in CI with no config at
// all.
func hermeticRunner(t *testing.T) func(ctx context.Context, dir, name string, args ...string) (string, error) {
	t.Helper()
	return func(ctx context.Context, dir, name string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=tickets-test",
			"GIT_AUTHOR_EMAIL=tickets-test@example.com",
			"GIT_COMMITTER_NAME=tickets-test",
			"GIT_COMMITTER_EMAIL=tickets-test@example.com",
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return stdout.String(), nil
	}
}

func mustGit(t *testing.T, run func(context.Context, string, string, ...string) (string, error), dir string, args ...string) string {
	t.Helper()
	out, err := run(context.Background(), dir, "git", args...)
	if err != nil {
		t.Fatalf("git %s (dir=%s): %v", strings.Join(args, " "), dir, err)
	}
	return out
}

func initGitRepo(t *testing.T, run func(context.Context, string, string, ...string) (string, error)) string {
	t.Helper()
	dir := t.TempDir()
	mustGit(t, run, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("writing README.md: %v", err)
	}
	mustGit(t, run, dir, "add", "README.md")
	mustGit(t, run, dir, "commit", "-m", "initial commit")
	return dir
}

// checkoutBranch creates and switches to a new branch in dir. Tests that
// actually want a push to reach origin need to be off "main" (or whatever
// protected branch is in play), since Commit now skips the push outright on
// a protected branch regardless of whether origin would have accepted it.
func checkoutBranch(t *testing.T, run func(context.Context, string, string, ...string) (string, error), dir, branch string) {
	t.Helper()
	mustGit(t, run, dir, "checkout", "-b", branch)
}

func commitCount(t *testing.T, run func(context.Context, string, string, ...string) (string, error), dir string) int {
	t.Helper()
	out := mustGit(t, run, dir, "rev-list", "--count", "HEAD")
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("parsing rev-list output %q: %v", out, err)
	}
	return n
}

// bareRefCommitCount counts commits reachable from ref (e.g. "refs/heads/main")
// directly in a bare repo, run with the bare directory itself as the git dir.
func bareRefCommitCount(t *testing.T, run func(context.Context, string, string, ...string) (string, error), bareDir, ref string) int {
	t.Helper()
	out := mustGit(t, run, bareDir, "rev-list", "--count", ref)
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("parsing rev-list output %q: %v", out, err)
	}
	return n
}

// bareHasRef reports whether ref exists in a bare repo, without failing the
// test if it does not — used to confirm a branch that only ever lived
// locally never reached origin.
func bareHasRef(run func(context.Context, string, string, ...string) (string, error), bareDir, ref string) bool {
	_, err := run(context.Background(), bareDir, "git", "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

func TestCommitOnlyWhenDirty(t *testing.T) {
	run := hermeticRunner(t)
	source := initGitRepo(t, run)

	// Give the repo a real (local, file-based) origin so the best-effort
	// push at the end of Commit succeeds — push-failure behavior has its own
	// test below.
	bare := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, run, "", "clone", "--bare", source, bare)
	repo := filepath.Join(t.TempDir(), "clone")
	mustGit(t, run, "", "clone", bare, repo)
	checkoutBranch(t, run, repo, "feature/x")

	s := New(repo, "direct", run)
	before := commitCount(t, run, repo)

	// Nothing under .acy/tickets yet: Commit must be a no-op.
	if err := s.Commit(context.Background(), "chore: ticket sync"); err != nil {
		t.Fatalf("Commit with nothing staged: %v", err)
	}
	if got := commitCount(t, run, repo); got != before {
		t.Fatalf("commit count = %d, want unchanged %d", got, before)
	}

	if err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Commit(context.Background(), "chore: ticket sync"); err != nil {
		t.Fatalf("Commit with a new ticket: %v", err)
	}
	if got := commitCount(t, run, repo); got != before+1 {
		t.Fatalf("commit count = %d, want %d", got, before+1)
	}

	// Nothing changed since: a second Commit must not add another commit.
	if err := s.Commit(context.Background(), "chore: ticket sync"); err != nil {
		t.Fatalf("Commit with nothing new staged: %v", err)
	}
	if got := commitCount(t, run, repo); got != before+1 {
		t.Fatalf("commit count = %d, want unchanged %d", got, before+1)
	}

	log := mustGit(t, run, repo, "log", "--format=%s")
	if !strings.Contains(log, "chore: ticket sync") {
		t.Fatalf("git log missing the commit message:\n%s", log)
	}
}

func TestCommitNoneModeIsNoop(t *testing.T) {
	run := hermeticRunner(t)
	repo := initGitRepo(t, run)

	s := New(repo, "none", run)
	before := commitCount(t, run, repo)

	if err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Commit(context.Background(), "chore: ticket sync"); err != nil {
		t.Fatalf("Commit in mode \"none\": %v", err)
	}
	if got := commitCount(t, run, repo); got != before {
		t.Fatalf("commit count = %d, want unchanged %d (mode \"none\" must not commit)", got, before)
	}

	status := mustGit(t, run, repo, "status", "--porcelain")
	if !strings.Contains(status, "?? .acy") {
		t.Fatalf("mode \"none\" should leave .acy/tickets untracked; git status:\n%s", status)
	}
}

func TestCommitPushFailureIsDistinguishable(t *testing.T) {
	run := hermeticRunner(t)
	source := initGitRepo(t, run)

	bare := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, run, "", "clone", "--bare", source, bare)

	repo := filepath.Join(t.TempDir(), "clone")
	mustGit(t, run, "", "clone", bare, repo)
	checkoutBranch(t, run, repo, "feature/x")

	// Remove the origin so the eventual `git push origin HEAD` fails cleanly,
	// without needing a network.
	mustGit(t, run, repo, "remote", "remove", "origin")

	s := New(repo, "direct", run)
	before := commitCount(t, run, repo)

	if err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	err := s.Commit(context.Background(), "chore: ticket sync")
	if err == nil {
		t.Fatal("Commit with no origin remote: want a push error, got nil")
	}
	if !errors.Is(err, ErrPushFailed) {
		t.Fatalf("Commit error = %v, want it to wrap ErrPushFailed", err)
	}

	// The commit itself must have gone through despite the push failure.
	if got := commitCount(t, run, repo); got != before+1 {
		t.Fatalf("commit count = %d, want %d (the local commit should survive a push failure)", got, before+1)
	}
}

// TestCommitSkipsPushOnProtectedBranch proves that Commit refuses to push
// when the checked-out branch is "main" (the unconditionally protected
// default, with no BaseBranch configured), or when it equals a Store's
// explicitly configured BaseBranch — while the local commit still lands
// either way, and nothing new reaches the bare origin.
func TestCommitSkipsPushOnProtectedBranch(t *testing.T) {
	t.Run("main", func(t *testing.T) {
		run := hermeticRunner(t)
		source := initGitRepo(t, run)

		bare := filepath.Join(t.TempDir(), "origin.git")
		mustGit(t, run, "", "clone", "--bare", source, bare)
		repo := filepath.Join(t.TempDir(), "clone")
		mustGit(t, run, "", "clone", bare, repo)
		// Left checked out on "main" (the clone's default), deliberately.

		s := New(repo, "direct", run)
		before := commitCount(t, run, repo)
		bareBefore := bareRefCommitCount(t, run, bare, "refs/heads/main")

		if err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo}); err != nil {
			t.Fatalf("Put: %v", err)
		}

		err := s.Commit(context.Background(), "chore: ticket sync")
		if !errors.Is(err, ErrPushSkipped) {
			t.Fatalf("Commit error = %v, want it to wrap ErrPushSkipped", err)
		}

		if got := commitCount(t, run, repo); got != before+1 {
			t.Fatalf("commit count = %d, want %d (the local commit should still land)", got, before+1)
		}
		if got := bareRefCommitCount(t, run, bare, "refs/heads/main"); got != bareBefore {
			t.Fatalf("bare origin main commit count = %d, want unchanged %d (nothing should have been pushed)", got, bareBefore)
		}
	})

	t.Run("custom BaseBranch", func(t *testing.T) {
		run := hermeticRunner(t)
		source := initGitRepo(t, run)

		bare := filepath.Join(t.TempDir(), "origin.git")
		mustGit(t, run, "", "clone", "--bare", source, bare)
		repo := filepath.Join(t.TempDir(), "clone")
		mustGit(t, run, "", "clone", bare, repo)
		checkoutBranch(t, run, repo, "trunk")

		s := New(repo, "direct", run)
		s.BaseBranch = "trunk"
		before := commitCount(t, run, repo)

		if err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo}); err != nil {
			t.Fatalf("Put: %v", err)
		}

		err := s.Commit(context.Background(), "chore: ticket sync")
		if !errors.Is(err, ErrPushSkipped) {
			t.Fatalf("Commit error = %v, want it to wrap ErrPushSkipped", err)
		}

		if got := commitCount(t, run, repo); got != before+1 {
			t.Fatalf("commit count = %d, want %d (the local commit should still land)", got, before+1)
		}
		// "trunk" never existed on origin, and Commit must not have pushed it.
		if bareHasRef(run, bare, "refs/heads/trunk") {
			t.Fatal("bare origin has a trunk ref, want none — nothing should have been pushed")
		}
	})
}

// TestCommitPushesOnOrdinaryFeatureBranch is the regression check for the
// skip logic above: an ordinary feature branch (not main, master, or
// BaseBranch) must still push successfully, exactly as before.
func TestCommitPushesOnOrdinaryFeatureBranch(t *testing.T) {
	run := hermeticRunner(t)
	source := initGitRepo(t, run)

	bare := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, run, "", "clone", "--bare", source, bare)
	repo := filepath.Join(t.TempDir(), "clone")
	mustGit(t, run, "", "clone", bare, repo)
	checkoutBranch(t, run, repo, "feature/x")

	s := New(repo, "direct", run)
	s.BaseBranch = "trunk" // configured, but irrelevant here: not the checked-out branch
	bareBefore := bareRefCommitCount(t, run, bare, "refs/heads/main")

	if err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := s.Commit(context.Background(), "chore: ticket sync"); err != nil {
		t.Fatalf("Commit on an ordinary feature branch: %v", err)
	}

	if !bareHasRef(run, bare, "refs/heads/feature/x") {
		t.Fatal("bare origin has no feature/x ref, want the push to have created one")
	}
	if got := bareRefCommitCount(t, run, bare, "refs/heads/feature/x"); got != bareBefore+1 {
		t.Fatalf("bare origin feature/x commit count = %d, want %d (the push should have landed)", got, bareBefore+1)
	}
}
