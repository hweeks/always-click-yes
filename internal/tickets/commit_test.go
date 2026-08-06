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

func commitCount(t *testing.T, run func(context.Context, string, string, ...string) (string, error), dir string) int {
	t.Helper()
	out := mustGit(t, run, dir, "rev-list", "--count", "HEAD")
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("parsing rev-list output %q: %v", out, err)
	}
	return n
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
