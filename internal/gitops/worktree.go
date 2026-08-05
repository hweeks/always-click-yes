package gitops

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// EnsureWorktree creates a new worktree at worktreeDir on a fresh branch,
// starting from origin/<baseBranch> when clonePath has a fetchable origin,
// falling back to the local baseBranch ref otherwise (tests and offline use).
// It errors clearly if branch already exists or worktreeDir is non-empty.
func EnsureWorktree(ctx context.Context, run Runner, clonePath, worktreeDir, baseBranch, branch string) error {
	entries, err := os.ReadDir(worktreeDir)
	if err == nil {
		if len(entries) > 0 {
			return fmt.Errorf("gitops: worktree dir %q already exists and is not empty", worktreeDir)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("gitops: checking worktree dir %q: %w", worktreeDir, err)
	}

	if _, err := run(ctx, clonePath, "git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return fmt.Errorf("gitops: branch %q already exists", branch)
	}

	startPoint := baseBranch
	if _, err := run(ctx, clonePath, "git", "fetch", "origin", baseBranch); err == nil {
		startPoint = "origin/" + baseBranch
	}

	if _, err := run(ctx, clonePath, "git", "worktree", "add", worktreeDir, "-b", branch, startPoint); err != nil {
		return fmt.Errorf("gitops: git worktree add: %w", err)
	}
	return nil
}

// RemoveWorktree removes the worktree and prunes its bookkeeping. It does not
// delete the branch — the branch may already be pushed and live on as a PR.
func RemoveWorktree(ctx context.Context, run Runner, clonePath, worktreeDir string) error {
	if _, err := run(ctx, clonePath, "git", "worktree", "remove", "--force", worktreeDir); err != nil {
		return fmt.Errorf("gitops: git worktree remove: %w", err)
	}
	if _, err := run(ctx, clonePath, "git", "worktree", "prune"); err != nil {
		return fmt.Errorf("gitops: git worktree prune: %w", err)
	}
	return nil
}

// CommitsAhead reports how many commits HEAD is ahead of baseRef, preferring
// origin/<baseRef> when that remote ref exists. Used to verify an engineer
// run actually committed something before pushing and opening a PR.
func CommitsAhead(ctx context.Context, run Runner, worktreeDir, baseRef string) (int, error) {
	ref := baseRef
	if _, err := run(ctx, worktreeDir, "git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+baseRef); err == nil {
		ref = "origin/" + baseRef
	}

	out, err := run(ctx, worktreeDir, "git", "rev-list", "--count", ref+"..HEAD")
	if err != nil {
		return 0, fmt.Errorf("gitops: git rev-list: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("gitops: parsing rev-list output %q: %w", out, err)
	}
	return n, nil
}

// Push pushes branch to origin, setting the upstream so a subsequent `git
// push` from the same worktree (or a human continuing the branch) needs no
// arguments.
func Push(ctx context.Context, run Runner, worktreeDir, branch string) error {
	if _, err := run(ctx, worktreeDir, "git", "push", "-u", "origin", branch); err != nil {
		return fmt.Errorf("gitops: git push: %w", err)
	}
	return nil
}
