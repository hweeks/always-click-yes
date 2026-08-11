package gitops

import (
	"context"
	"strings"
)

// CurrentBranch resolves the branch checked out in dir, for display rather
// than for git plumbing. A detached HEAD (e.g. mid-rebase, or a worktree
// checked out at a bare commit) has no branch name, so it falls back to the
// short SHA prefixed with "detached @ ".
func CurrentBranch(ctx context.Context, run Runner, dir string) (string, error) {
	out, err := run(ctx, dir, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(out)
	if branch != "HEAD" {
		return branch, nil
	}
	sha, err := run(ctx, dir, "git", "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return "detached @ " + strings.TrimSpace(sha), nil
}

// maxRefLen keeps the whole branch name comfortably under git's ref-name
// limits and any CI/PR-title display truncation.
const maxRefLen = 60

const branchPrefix = "acy/"

// BranchName builds acy/<ticket>-<slug> from a ticket id and a free-form
// title, sanitizing both to lowercase [a-z0-9-] and clipping so the whole ref
// never exceeds maxRefLen. Always yields a valid git ref component, even for
// an empty title or a ticket full of unicode.
func BranchName(ticket, title string) string {
	avail := maxRefLen - len(branchPrefix)

	t := sanitize(ticket)
	if t == "" {
		t = "task"
	}
	if len(t) > avail-2 { // leave room for at least "-x"
		t = strings.Trim(t[:avail-2], "-")
	}

	remaining := max(avail-len(t)-1, 1) // the dash between ticket and slug
	s := sanitize(title)
	if len(s) > remaining {
		s = strings.Trim(s[:remaining], "-")
	}
	if s == "" {
		s = "change"
	}

	return branchPrefix + t + "-" + s
}

// sanitize lowercases s and collapses every run of characters outside
// [a-z0-9] into a single '-', trimming leading/trailing dashes.
func sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
