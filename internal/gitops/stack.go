package gitops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// gh stack (github/gh-stack) is a public-preview extension for managing
// chains of stacked pull requests. Two things a future reader is likely to
// get wrong:
//
//   - `gh stack view` without --json pages through less -R for a human and
//     will hang a non-TTY process forever. This package only ever calls
//     `gh stack view --json`.
//   - `gh stack modify`, `gh stack switch`, and `gh stack submit` (without
//     --auto) are interactive full-screen TUIs. Never invoke them here.
//
// As with the rest of this package, every command below is a fixed argv
// chosen by this file, never composed from caller input beyond branch/trunk
// names, and every execution goes through the caller-supplied Runner.

// Exit codes are gh-stack's real contract with callers — branch on these,
// never on stderr text, since stderr wording is for humans and can change
// across releases:
//
//	0  success
//	1  generic failure                          (no sentinel; wrapped as-is)
//	2  not in a stack / stack not found          -> ErrNoStack
//	3  rebase conflict                           -> ErrStackConflict
//	4  GitHub API failure                        -> ErrAPIFailure
//	5  invalid arguments                         (no sentinel; wrapped as-is)
//	6  disambiguation required                   -> ErrDisambiguation
//	7  rebase already in progress                -> ErrRebaseInProgress
//	8  stack locked by another process           -> ErrStackLocked
//	9  stacked PRs not enabled for this repo      -> ErrStackNotEnabled
//	10 modify session interrupted                (no sentinel; wrapped as-is)
var (
	ErrNoStack          = errors.New("not part of a stack")
	ErrStackConflict    = errors.New("stack rebase conflict")
	ErrAPIFailure       = errors.New("github api failure")
	ErrDisambiguation   = errors.New("stack disambiguation required")
	ErrRebaseInProgress = errors.New("stack rebase already in progress")
	ErrStackLocked      = errors.New("stack locked by another process")
	ErrStackNotEnabled  = errors.New("stacked prs not enabled for this repository")
)

// exitCoder is what *exec.ExitError satisfies via its promoted
// *os.ProcessState method. Declared locally so a test fake can satisfy it
// without importing os/exec.
type exitCoder interface {
	ExitCode() int
}

// ExitCode extracts a process exit code from err by unwrapping through
// whatever %w chain DefaultRunner and classify added.
func ExitCode(err error) (int, bool) {
	var ec exitCoder
	if errors.As(err, &ec) {
		return ec.ExitCode(), true
	}
	return 0, false
}

// sentinelForCode maps a gh-stack exit code to its sentinel, per the table
// above. Codes with no dedicated sentinel return nil.
func sentinelForCode(code int) error {
	switch code {
	case 2:
		return ErrNoStack
	case 3:
		return ErrStackConflict
	case 4:
		return ErrAPIFailure
	case 6:
		return ErrDisambiguation
	case 7:
		return ErrRebaseInProgress
	case 8:
		return ErrStackLocked
	case 9:
		return ErrStackNotEnabled
	default:
		return nil
	}
}

// classify wraps a Runner error from the gh stack subcommand named by op so
// callers can test with errors.Is against the sentinels above, while the
// runner's own error text — which carries gh's stderr, per DefaultRunner —
// survives verbatim in Error(). A human should see gh's actual complaint,
// not just a generic sentinel message.
func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	if code, ok := ExitCode(err); ok {
		if sentinel := sentinelForCode(code); sentinel != nil {
			return fmt.Errorf("gitops: %s: %w: %w", op, sentinel, err)
		}
	}
	return fmt.Errorf("gitops: %s: %w", op, err)
}

// StackAvailable is a cheap probe that the gh-stack extension is installed
// and usable. `gh stack --version` is the fastest real invocation available,
// but this is a preview extension and --version may not be a stable
// subcommand across releases, so on any failure this also falls back to
// checking `gh extension list` for the extension itself before giving up.
func StackAvailable(ctx context.Context, run Runner, dir string) error {
	if _, err := run(ctx, dir, "gh", "stack", "--version"); err == nil {
		return nil
	} else if code, ok := ExitCode(err); ok && code == 9 {
		return fmt.Errorf("gitops: gh stack --version: %w: stacked pull requests are a public preview feature and must be enabled for this repository: %w", ErrStackNotEnabled, err)
	}

	out, err := run(ctx, dir, "gh", "extension", "list")
	if err != nil {
		return fmt.Errorf("gitops: gh extension list: %w", err)
	}
	if strings.Contains(out, "gh-stack") {
		return nil
	}
	return errors.New("gitops: gh-stack extension not found: install it with `gh extension install github/gh-stack`")
}

// validateBranches rejects inputs that would either shell out with no
// arguments or silently pass an empty positional argument to gh — both are
// caller bugs, not conditions gh itself should have to diagnose.
func validateBranches(branches []string) error {
	if len(branches) == 0 {
		return errors.New("gitops: no branches given")
	}
	for _, b := range branches {
		if strings.TrimSpace(b) == "" {
			return errors.New("gitops: branch name must not be empty")
		}
	}
	return nil
}

// firstInt returns the value of the first run of ASCII digits in s, or 0 if
// there is none. gh stack link prints a human-readable "Stack #<N> ..."
// message on success; the number is informational only (StackLink's error
// return is the actual success signal), so a missing number is not an
// error.
func firstInt(s string) int {
	start := -1
	for i, r := range s {
		if r >= '0' && r <= '9' {
			if start == -1 {
				start = i
			}
			continue
		}
		if start != -1 {
			n, _ := strconv.Atoi(s[start:i])
			return n
		}
	}
	if start != -1 {
		n, _ := strconv.Atoi(s[start:])
		return n
	}
	return 0
}

// StackLink creates or extends a stack on GitHub from branches, given
// bottom-to-top, with no local tracking: missing PRs are created with
// correct base-branch chaining, and PRs with the wrong base are corrected.
// This is additive only — it never removes a branch from a stack.
func StackLink(ctx context.Context, run Runner, dir, trunk string, branches []string) (int, error) {
	if err := validateBranches(branches); err != nil {
		return 0, err
	}

	args := append([]string{"stack", "link", "--base", trunk}, branches...)
	out, err := run(ctx, dir, "gh", args...)
	if err != nil {
		return 0, classify("gh stack link", err)
	}
	return firstInt(out), nil
}

// stackViewJSON mirrors the top-level object `gh stack view --json` prints.
// It is an object, not an array — the branches array inside it is what
// StackView's []StackEntry return value is built from.
type stackViewJSON struct {
	Trunk         string       `json:"trunk"`
	CurrentBranch string       `json:"currentBranch"`
	Branches      []StackEntry `json:"branches"`
}

// StackEntry describes one branch in a stack. Verified against gh-stack
// v0.1.0 (gh CLI 2.86.0): running `gh stack init feat-a feat-b --base main`
// followed by `gh stack view --json` in a scratch repo with no GitHub
// remote configured produced:
//
//	{
//	  "trunk": "main",
//	  "currentBranch": "feat-b",
//	  "branches": [
//	    {"name":"feat-a","base":"<sha>","isCurrent":false,"isMerged":false,"isQueued":false,"needsRebase":false},
//	    {"name":"feat-b","base":"<sha>","isCurrent":true,"isMerged":false,"isQueued":false,"needsRebase":false}
//	  ]
//	}
//
// No remote was configured in that run, so PR-related fields never appeared
// on a branch entry; PRNumber and PRURL below are an unverified best guess
// at how a linked PR would serialize. Position is not a JSON field at all —
// gh-stack conveys stack order purely by array position — so StackView fills
// it in from the index. Decode leniently (no DisallowUnknownFields): this is
// a preview CLI and its JSON is expected to grow fields.
type StackEntry struct {
	Position    int    `json:"-"`
	Branch      string `json:"name"`
	Base        string `json:"base"`
	IsCurrent   bool   `json:"isCurrent"`
	Merged      bool   `json:"isMerged"`
	Queued      bool   `json:"isQueued"`
	NeedsRebase bool   `json:"needsRebase"`
	PRNumber    int    `json:"prNumber"`
	PRURL       string `json:"prUrl"`
}

// StackView reports the current stack. It only ever calls `gh stack view
// --json` — the human-facing `gh stack view` pages through less -R and will
// hang a non-TTY caller.
func StackView(ctx context.Context, run Runner, dir string) ([]StackEntry, error) {
	out, err := run(ctx, dir, "gh", "stack", "view", "--json")
	if err != nil {
		return nil, classify("gh stack view", err)
	}

	var parsed stackViewJSON
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return nil, fmt.Errorf("gitops: decode gh stack view --json output: %w", err)
	}
	for i := range parsed.Branches {
		parsed.Branches[i].Position = i
	}
	return parsed.Branches, nil
}

// StackRebase performs gh-stack's cascading rebase from trunk upward. A
// caller that gets ErrStackConflict back must stop and surface it to a
// human — see StackAssemble's doc comment for why this package never tries
// to auto-resolve one.
func StackRebase(ctx context.Context, run Runner, dir string) error {
	_, err := run(ctx, dir, "gh", "stack", "rebase")
	return classify("gh stack rebase", err)
}

// StackPush pushes every active branch in the stack with a per-branch
// --force-with-lease. This is not atomic: one branch can succeed while a
// sibling is rejected, in which case gh-stack leaves already-updated
// branches alone and the caller should retry.
func StackPush(ctx context.Context, run Runner, dir string) error {
	_, err := run(ctx, dir, "gh", "stack", "push")
	return classify("gh stack push", err)
}

// StackSync runs gh-stack's one-shot fetch/reconcile/rebase/push/link flow.
// With prune, branches for merged PRs are deleted locally once synced.
func StackSync(ctx context.Context, run Runner, dir string, prune bool) error {
	args := []string{"stack", "sync"}
	if prune {
		args = append(args, "--prune")
	}
	_, err := run(ctx, dir, "gh", args...)
	return classify("gh stack sync", err)
}

// StackAssemble turns N branches that are already pushed, but not yet a
// stack, into one: it initializes local tracking, cascades a rebase, pushes,
// and finally links the branches into a stack on GitHub — stopping at the
// first failure and returning it (each step already classifies its own
// error via the sentinels above).
//
// ErrStackConflict from the rebase step means stop and put a human in the
// loop: never attempt to auto-resolve a stack rebase conflict from this
// package. Because init enables git rerere, a conflict a human resolves once
// here will auto-resolve itself the next time the same rebase runs.
func StackAssemble(ctx context.Context, run Runner, dir, trunk string, branches []string) (int, error) {
	if err := validateBranches(branches); err != nil {
		return 0, err
	}

	initArgs := append([]string{"stack", "init"}, branches...)
	initArgs = append(initArgs, "--base", trunk)
	if _, err := run(ctx, dir, "gh", initArgs...); err != nil {
		return 0, classify("gh stack init", err)
	}
	if err := StackRebase(ctx, run, dir); err != nil {
		return 0, err
	}
	if err := StackPush(ctx, run, dir); err != nil {
		return 0, err
	}
	return StackLink(ctx, run, dir, trunk, branches)
}
