package fleet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/gitops"
)

// StackEvent is what a StackKeeper operation produced, for a caller to log
// and forward onto Manager's Events() as KindStack.
type StackEvent struct {
	Op       string   // "link" or "sync"
	Branches []string // for a successful link: the full chain just registered, bottom-to-top
	Branch   string   // for a conflict: the branch gh-stack reported needing a human
	Err      error    // nil on success
}

// StackKeeper registers an arch-mode chain as a real GitHub stack as its PRs
// open (Link), and repairs it with `gh stack sync` whenever trunk moves
// underneath it (Sync). Link and Sync are the only two gh-stack operations
// this ever runs, and each needs a different working tree: see their own
// comments for why.
type StackKeeper struct {
	dir         string // the operator's own project directory — StackLink runs here directly
	run         gitops.Runner
	trunk       string
	worktreeDir string // computed once here, never caller-supplied afterward

	mu            sync.Mutex
	worktreeReady bool
}

// worktreeDirFor derives a deterministic worktree path from dir, so a
// crash-and-restart of the keeper finds the same leftover worktree rather
// than minting a new one every time. filepath.Abs is best-effort: if it
// fails (a bogus dir a caller should never actually pass), dir itself is
// hashed as-is rather than failing NewStackKeeper over a cosmetic detail.
func worktreeDirFor(dir string) string {
	hashInput := dir
	if abs, err := filepath.Abs(dir); err == nil {
		hashInput = abs
	}
	sum := sha256.Sum256([]byte(hashInput))
	return filepath.Join(os.TempDir(), "acy-stack-worktree-"+hex.EncodeToString(sum[:])[:16])
}

// NewStackKeeper builds a keeper over the repo at dir, whose trunk (the
// fleet's real base branch, not a stack's parent branch) is trunk.
func NewStackKeeper(dir string, run gitops.Runner, trunk string) *StackKeeper {
	return &StackKeeper{
		dir:         dir,
		run:         run,
		trunk:       trunk,
		worktreeDir: worktreeDirFor(dir),
	}
}

// WorktreeDir is where Sync does its work, for tests.
func (k *StackKeeper) WorktreeDir() string { return k.worktreeDir }

// Link registers branches (bottom-to-top) as a stack on GitHub.
//
// This runs gitops.StackLink directly in k.dir — the operator's own working
// tree — rather than in the dedicated worktree Sync uses. StackLink needs no
// local tracking and no checkout at all: it only reads/writes GitHub's own
// view of the PRs' base branches. That makes it the only stack operation
// safe to run in the operator's own working tree, and the only thing on this
// hot path (called synchronously from forwardPRWatcher's goroutine on every
// PR open) for exactly that reason — nothing here needs to wait on a
// worktree ever existing.
func (k *StackKeeper) Link(ctx context.Context, branches []string) StackEvent {
	_, err := gitops.StackLink(ctx, k.run, k.dir, k.trunk, branches)
	if err != nil {
		return StackEvent{Op: "link", Err: err}
	}
	return StackEvent{Op: "link", Branches: branches}
}

// Sync repairs the stack with gh-stack's one-shot fetch/reconcile/rebase/push
// flow, pruning branches for merged PRs as it goes — the dedicated worktree
// this runs in is disposable, so nothing depends on a stale local branch
// sticking around in it.
func (k *StackKeeper) Sync(ctx context.Context) StackEvent {
	if err := k.ensureWorktree(ctx); err != nil {
		return StackEvent{Op: "sync", Err: err}
	}

	err := gitops.StackSync(ctx, k.run, k.worktreeDir, true)
	if err == nil {
		return StackEvent{Op: "sync"}
	}

	ev := StackEvent{Op: "sync", Err: err}
	if errors.Is(err, gitops.ErrStackConflict) {
		// Best-effort only: a human still has to go look, this just saves
		// them the trip to find out which branch.
		ev.Branch = conflictBranch(ctx, k.run, k.worktreeDir)
	}
	return ev
}

// conflictBranch names the branch gh-stack needs a human to rebase by hand,
// preferring the one it flagged NeedsRebase, falling back to whichever
// branch is current. It only ever reads this from gitops.StackView's JSON —
// never gh's stderr — matching stack.go's own doctrine of branching on exit
// codes, never stderr text: stderr wording is for humans and can change
// across gh-stack releases, while the JSON shape is the one contract this
// package already relies on elsewhere.
func conflictBranch(ctx context.Context, run gitops.Runner, worktreeDir string) string {
	entries, err := gitops.StackView(ctx, run, worktreeDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.NeedsRebase {
			return e.Branch
		}
	}
	for _, e := range entries {
		if e.IsCurrent {
			return e.Branch
		}
	}
	return ""
}

// ensureWorktree makes sure k.worktreeDir exists and is ready for Sync,
// creating it on first use and doing nothing on every call after.
func (k *StackKeeper) ensureWorktree(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.worktreeReady {
		return nil
	}

	// A non-empty directory already at worktreeDir is not necessarily an
	// error: worktreeDir is deterministic per repo path (see worktreeDirFor),
	// so this is exactly as likely to be this same process's own earlier
	// bootstrap (a crash between creating it and setting worktreeReady) or a
	// crashed prior run's leftover as it is anything else. Never fail a live
	// run just because something is already sitting there — treat it as
	// ready and let Sync use it as-is.
	if entries, err := os.ReadDir(k.worktreeDir); err == nil && len(entries) > 0 {
		k.worktreeReady = true
		return nil
	}

	// EnsureWorktree requires a fresh branch to start the worktree on, but
	// nothing here ever commits to it — Sync's whole point is that gh-stack
	// manages every branch that matters. A random name each time means a
	// retry after a failed EnsureWorktree never collides with whatever name
	// the previous attempt already tried (and possibly partially created).
	randomBranch, err := randomKeeperBranch()
	if err != nil {
		return err
	}
	if err := gitops.EnsureWorktree(ctx, k.run, k.dir, k.worktreeDir, k.trunk, randomBranch); err != nil {
		return err
	}
	k.worktreeReady = true
	return nil
}

// randomKeeperBranch mints a throwaway branch name for ensureWorktree's
// EnsureWorktree call.
func randomKeeperBranch() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("fleet: generating stack keeper branch name: %w", err)
	}
	return "acy/stack-keeper-" + hex.EncodeToString(b), nil
}

// Close removes the dedicated worktree, if Sync ever created one. Errors are
// logged rather than returned or panicked on — a leftover worktree on
// shutdown is a cleanup nicety, not something worth failing an otherwise
// successful run over. Safe to call twice: worktreeReady is cleared first,
// so a second call sees nothing to do.
func (k *StackKeeper) Close(ctx context.Context) {
	k.mu.Lock()
	ready := k.worktreeReady
	k.worktreeReady = false
	k.mu.Unlock()
	if !ready {
		return
	}
	if err := gitops.RemoveWorktree(ctx, k.run, k.dir, k.worktreeDir); err != nil {
		alog.Printf("fleet: stack keeper: removing worktree %q: %v", k.worktreeDir, err)
	}
}
