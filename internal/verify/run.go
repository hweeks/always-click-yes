package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// maxOutputBytes caps a single check's captured output. A handful of these
// is a rounding error against the run that produced them, and an unbounded
// capture — a runaway build log, a stuck test loop printing forever — has no
// other backstop between the child process and the journal.
const maxOutputBytes = 8 << 10

// Run executes each command in order and returns one VerifyCheck per command
// that got to run. Commands are parsed with strings.Fields, never a shell —
// see the package doc for why. A nil run uses DefaultRunner; nil or empty
// commands returns nil.
//
// timeout, when > 0, bounds each command individually via
// context.WithTimeout(ctx, timeout); a timeout of 0 leaves a command bound
// only by ctx itself. If ctx ends — before a command starts, or because it
// was cancelled while one was running — Run stops immediately: no entry is
// appended for a command that hadn't started, and none is appended for one
// cut short mid-run either, since a result produced under a cancelled parent
// context is not a verdict about the command, it's a verdict about the
// caller giving up. The caller already knows why ctx ended; Run doesn't
// re-report it as a check result.
func Run(ctx context.Context, run Runner, dir string, commands []string, timeout time.Duration) []engineerwire.VerifyCheck {
	if len(commands) == 0 {
		return nil
	}
	if run == nil {
		run = DefaultRunner
	}

	env := StripACYLive(os.Environ())

	var checks []engineerwire.VerifyCheck
	for _, raw := range commands {
		if ctx.Err() != nil {
			break
		}

		argv := strings.Fields(raw)
		if len(argv) == 0 {
			checks = append(checks, engineerwire.VerifyCheck{
				Name:     raw,
				Status:   engineerwire.VerifyError,
				ExitCode: -1,
				Output:   "verify: empty command",
			})
			continue
		}

		cctx := ctx
		cancel := func() {}
		if timeout > 0 {
			cctx, cancel = context.WithTimeout(ctx, timeout)
		}

		start := time.Now()
		output, exitCode, err := run(cctx, dir, env, argv)
		dur := time.Since(start)
		cancel()

		if ctx.Err() != nil {
			break
		}

		out, truncated := truncateOutput(output)
		check := engineerwire.VerifyCheck{
			Name:       raw,
			Argv:       argv,
			ExitCode:   exitCode,
			Output:     out,
			Truncated:  truncated,
			DurationMS: dur.Milliseconds(),
		}

		switch {
		case err == nil && exitCode == 0:
			check.Status = engineerwire.VerifyPassed
		case err == nil:
			check.Status = engineerwire.VerifyFailed
		case errors.Is(err, ErrNotInstalled):
			check.Status = engineerwire.VerifySkipped
		case errors.Is(err, context.DeadlineExceeded):
			check.Status = engineerwire.VerifyTimeout
		default:
			check.Status = engineerwire.VerifyError
		}
		checks = append(checks, check)
	}
	return checks
}

// StripACYLive returns env with every ACY_LIVE-prefixed entry removed.
// ACY_LIVE=1 gates internal/e2e's live suite, which spends real money on
// live claude sessions; a verify command that inherited it could silently
// start a live run of its own.
func StripACYLive(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "ACY_LIVE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// truncateOutput caps s at maxOutputBytes. Over the cap, it keeps the first
// and last half of the budget, joined by a marker line naming how much was
// dropped, and reports truncated=true — never silently. Neither half ever
// splits a UTF-8 rune.
func truncateOutput(s string) (out string, truncated bool) {
	if len(s) <= maxOutputBytes {
		return s, false
	}
	half := maxOutputBytes / 2
	head := headBytes(s, half)
	tail := tailBytes(s, half)
	marker := fmt.Sprintf("\n[... %d bytes truncated ...]\n", len(s)-len(head)-len(tail))
	return head + marker + tail, true
}

// headBytes returns the first n bytes of s, backing off to the nearest
// preceding rune boundary rather than splitting one.
func headBytes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// tailBytes returns the last n bytes of s, advancing to the nearest
// following rune boundary rather than splitting one.
func tailBytes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
