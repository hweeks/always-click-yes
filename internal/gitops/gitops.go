// Package gitops is the deterministic git/gh layer behind engineer mode: it
// creates the isolated worktree an unattended run works in, and afterwards
// pushes the branch and opens the PR. None of it is driven by the model —
// every command here is a fixed argv chosen by this package.
package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/hweeks/always-click-yes/internal/alog"
)

// Runner executes name with args in dir and returns captured stdout. Every
// function in this package takes one, so tests can intercept gh while still
// exercising a real git binary.
type Runner func(ctx context.Context, dir string, name string, args ...string) (stdout string, err error)

// DefaultRunner runs the command with exec.CommandContext. On failure the
// returned error carries the captured stderr, since a bare exit-status error
// from *exec.Cmd names no reason.
func DefaultRunner(ctx context.Context, dir string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	alog.Printf("gitops: %s %s (dir=%s)", name, strings.Join(args, " "), dir)
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return stdout.String(), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}
