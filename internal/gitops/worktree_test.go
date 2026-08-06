package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureWorktreeNoOriginFallback(t *testing.T) {
	run := hermeticRunner(t)
	ctx := context.Background()
	clonePath := initRepo(t, run, "main")
	baseTip := strings.TrimSpace(mustRun(t, run, clonePath, "git", "rev-parse", "main"))

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := EnsureWorktree(ctx, run, clonePath, worktreeDir, "main", "acy/t-1"); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	got := strings.TrimSpace(mustRun(t, run, worktreeDir, "git", "rev-parse", "HEAD"))
	if got != baseTip {
		t.Fatalf("worktree HEAD = %s, want base tip %s", got, baseTip)
	}
	branch := strings.TrimSpace(mustRun(t, run, worktreeDir, "git", "branch", "--show-current"))
	if branch != "acy/t-1" {
		t.Fatalf("worktree branch = %q, want acy/t-1", branch)
	}

	// Second call must error: branch already exists.
	if err := EnsureWorktree(ctx, run, clonePath, filepath.Join(t.TempDir(), "wt2"), "main", "acy/t-1"); err == nil {
		t.Fatal("expected error on second EnsureWorktree with the same branch, got nil")
	}
}

func TestEnsureWorktreeNonEmptyDir(t *testing.T) {
	run := hermeticRunner(t)
	ctx := context.Background()
	clonePath := initRepo(t, run, "main")

	worktreeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktreeDir, "occupied"), []byte("x"), 0o644); err != nil {
		t.Fatalf("writing occupied file: %v", err)
	}

	if err := EnsureWorktree(ctx, run, clonePath, worktreeDir, "main", "acy/t-1"); err == nil {
		t.Fatal("expected error for non-empty worktree dir, got nil")
	}
}

func TestEnsureWorktreeWithOrigin(t *testing.T) {
	run := hermeticRunner(t)
	ctx := context.Background()
	source := initRepo(t, run, "main")
	bare := cloneBareFrom(t, run, source)

	// Advance the bare repo's main past the clone's local main, so the test
	// can tell whether EnsureWorktree really started from origin/main.
	mustRun(t, run, source, "git", "commit", "--allow-empty", "-m", "origin-only commit")
	mustRun(t, run, source, "git", "push", bare, "main")
	originTip := strings.TrimSpace(mustRun(t, run, bare, "git", "rev-parse", "main"))

	clonePath := filepath.Join(t.TempDir(), "clone")
	mustRun(t, run, "", "git", "clone", bare, clonePath)

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := EnsureWorktree(ctx, run, clonePath, worktreeDir, "main", "acy/t-2"); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	got := strings.TrimSpace(mustRun(t, run, worktreeDir, "git", "rev-parse", "HEAD"))
	if got != originTip {
		t.Fatalf("worktree HEAD = %s, want origin tip %s", got, originTip)
	}
}

func TestCommitsAhead(t *testing.T) {
	run := hermeticRunner(t)
	ctx := context.Background()
	clonePath := initRepo(t, run, "main")

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := EnsureWorktree(ctx, run, clonePath, worktreeDir, "main", "acy/t-3"); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	n, err := CommitsAhead(ctx, run, worktreeDir, "main")
	if err != nil {
		t.Fatalf("CommitsAhead: %v", err)
	}
	if n != 0 {
		t.Fatalf("CommitsAhead before any commit = %d, want 0", n)
	}

	if err := os.WriteFile(filepath.Join(worktreeDir, "change.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("writing change.txt: %v", err)
	}
	mustRun(t, run, worktreeDir, "git", "add", "change.txt")
	mustRun(t, run, worktreeDir, "git", "commit", "-m", "a change")

	n, err = CommitsAhead(ctx, run, worktreeDir, "main")
	if err != nil {
		t.Fatalf("CommitsAhead: %v", err)
	}
	if n != 1 {
		t.Fatalf("CommitsAhead after one commit = %d, want 1", n)
	}
}

func TestPushToFileOrigin(t *testing.T) {
	run := hermeticRunner(t)
	ctx := context.Background()
	source := initRepo(t, run, "main")
	bare := cloneBareFrom(t, run, source)

	clonePath := filepath.Join(t.TempDir(), "clone")
	mustRun(t, run, "", "git", "clone", "file://"+bare, clonePath)

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	branch := "acy/t-4"
	if err := EnsureWorktree(ctx, run, clonePath, worktreeDir, "main", branch); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreeDir, "change.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("writing change.txt: %v", err)
	}
	mustRun(t, run, worktreeDir, "git", "add", "change.txt")
	mustRun(t, run, worktreeDir, "git", "commit", "-m", "a change")
	worktreeTip := strings.TrimSpace(mustRun(t, run, worktreeDir, "git", "rev-parse", "HEAD"))

	if err := Push(ctx, run, worktreeDir, branch); err != nil {
		t.Fatalf("Push: %v", err)
	}

	bareTip := strings.TrimSpace(mustRun(t, run, bare, "git", "rev-parse", branch))
	if bareTip != worktreeTip {
		t.Fatalf("bare repo %s = %s, want %s", branch, bareTip, worktreeTip)
	}
}

func TestRemoveWorktree(t *testing.T) {
	run := hermeticRunner(t)
	ctx := context.Background()
	clonePath := initRepo(t, run, "main")

	worktreeDir := filepath.Join(t.TempDir(), "wt")
	if err := EnsureWorktree(ctx, run, clonePath, worktreeDir, "main", "acy/t-5"); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	if err := RemoveWorktree(ctx, run, clonePath, worktreeDir); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}

	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Fatalf("worktree dir %q still exists after RemoveWorktree (err=%v)", worktreeDir, err)
	}

	out := mustRun(t, run, clonePath, "git", "worktree", "list")
	if strings.Contains(out, worktreeDir) {
		t.Fatalf("git worktree list still mentions %q:\n%s", worktreeDir, out)
	}
}
