package tickets

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/hweeks/always-click-yes/internal/alog"
)

// ErrPushFailed wraps a failed `git push` from Commit. It is distinguishable
// via errors.Is so a caller working against a protected main can ignore
// exactly this failure — the commit itself already landed locally — without
// swallowing an unrelated error from the same call.
var ErrPushFailed = errors.New("tickets: push failed")

// Commit stages .acy/tickets, commits it with msg if (and only if) that
// staged something, then best-effort pushes the current branch to origin.
// Mode "none" makes this a no-op entirely, for a project that keeps its
// ticket ledger out of the shared history.
func (s *Store) Commit(ctx context.Context, msg string) error {
	if s.Mode == "none" {
		return nil
	}

	// A pathspec git has never seen (no commits, no worktree entries) makes
	// `git add` fail outright rather than add nothing, so a repo with no
	// .acy/tickets yet is "nothing to commit", not an error.
	if _, err := os.Stat(s.dir()); os.IsNotExist(err) {
		return nil
	}

	if _, err := s.Run(ctx, s.Root, "git", "add", ".acy/tickets"); err != nil {
		return fmt.Errorf("tickets: git add: %w", err)
	}

	if _, err := s.Run(ctx, s.Root, "git", "diff", "--cached", "--quiet"); err == nil {
		return nil // nothing staged
	}

	if _, err := s.Run(ctx, s.Root, "git", "commit", "-m", msg); err != nil {
		return fmt.Errorf("tickets: git commit: %w", err)
	}
	alog.Printf("tickets: committed %q", msg)

	if _, err := s.Run(ctx, s.Root, "git", "push", "origin", "HEAD"); err != nil {
		alog.Printf("tickets: push to origin failed: %v", err)
		return fmt.Errorf("%w: %w", ErrPushFailed, err)
	}
	return nil
}
