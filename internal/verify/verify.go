// Package verify runs the machine-collected checks behind
// engineerwire.Result.Verification: the commands acy itself runs in an
// engineer's worktree after the session's own verdict, never the model's
// report of having run them. A Runner seam threads through Run exactly as
// gitops.Runner threads through internal/gitops, so tests fake a process
// instead of running one.
//
// Commands are parsed with strings.Fields, never a shell. There is no pipe,
// redirect, glob or quoting support — a command like `go test ./... | tee
// log` is passed through literally, and `tee`/`log` become extra, nonexistent
// arguments to `go`, not a pipeline. This mirrors gitops's own choice to hand
// exec.Command a fixed argv rather than a shell string: a verify command list
// is configuration a user or an architect wrote, and interpreting it through
// a shell would trade a predictable parse failure for an injection surface.
package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/hweeks/always-click-yes/internal/alog"
)

// Runner executes argv in dir with env and returns combined output and the
// process exit code.
type Runner func(ctx context.Context, dir string, env []string, argv []string) (output string, exitCode int, err error)

// ErrNotInstalled is returned when argv[0] cannot be found on PATH.
var ErrNotInstalled = errors.New("verify: binary not installed")

// DefaultRunner is the real Runner. It checks argv[0] against PATH first, so
// a missing binary comes back as ErrNotInstalled rather than a bare exec
// error, then runs the command with exec.CommandContext. Stdout and stderr
// are captured into one buffer in the order the process wrote them — a
// verify command's failure is usually easier to read interleaved than split
// across two streams.
func DefaultRunner(ctx context.Context, dir string, env []string, argv []string) (string, int, error) {
	if _, err := exec.LookPath(argv[0]); err != nil {
		return "", -1, fmt.Errorf("%w: %s", ErrNotInstalled, argv[0])
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	alog.Printf("verify: %s (dir=%s)", strings.Join(argv, " "), dir)
	err := cmd.Run()
	if err == nil {
		return buf.String(), 0, nil
	}
	// A context cancellation kills the process, which Wait reports as a
	// plain "signal: killed" *exec.ExitError with no trace of why — check
	// ctx.Err() itself so Run can tell a per-command timeout apart from an
	// ordinary non-zero exit.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return buf.String(), -1, ctxErr
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return buf.String(), exitErr.ExitCode(), nil
	}
	return buf.String(), -1, err
}
