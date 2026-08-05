package gitops

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// maxPRBody is a sane cap on PR body size. gh (and GitHub's API behind it)
// rejects bodies well past this, and an engineer's report should never
// legitimately need more — clip rather than fail the whole PR over it.
const maxPRBody = 20000

// CreatePR opens a PR via the gh CLI and returns its URL, which gh prints on
// stdout. title must be non-empty; body is silently clipped to maxPRBody.
func CreatePR(ctx context.Context, run Runner, worktreeDir, base, branch, title, body string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", errors.New("gitops: PR title must not be empty")
	}
	body = clipRunes(body, maxPRBody)

	out, err := run(ctx, worktreeDir, "gh", "pr", "create",
		"--base", base, "--head", branch, "--title", title, "--body", body)
	if err != nil {
		return "", fmt.Errorf("gitops: gh pr create: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
