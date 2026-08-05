package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/version"
)

// Check is the outcome of one doctor probe against a host.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// checkNames is the fixed order Doctor runs — and reports — checks in.
var checkNames = []string{"ssh", "acy", "claude", "gh", "repo", "state"}

// Runner runs name with args, either on this machine or wrapped for a
// remote host, and reports stdout and stderr separately. Doctor needs both:
// a refused key and an unknown host both just fail, but they fail with
// different stderr, and gitops.Runner's single merged error string would
// lose that distinction.
type Runner func(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)

// runnerForHost returns the Runner Doctor uses for h: direct exec for a
// local host, or every command wrapped in the same BatchMode ssh preamble
// the engineer transport uses.
func runnerForHost(h config.FleetHost) Runner {
	if h.SSH == "" {
		return runLocal
	}
	return sshRunner(h.SSH)
}

func runLocal(ctx context.Context, name string, args ...string) (string, string, error) {
	return runCaptured(exec.CommandContext(ctx, name, args...)) //nolint:gosec // name/args are doctor's own fixed argv, never user input
}

// sshRunner wraps every command in target's BatchMode ssh preamble. ssh
// itself joins its trailing arguments with a bare space before handing them
// to the remote shell, so a multi-word argument like "command -v claude"
// would come apart into extra positional parameters unless it is quoted —
// shellQuote is what keeps each argv element intact on the other end.
func sshRunner(target string) Runner {
	return func(ctx context.Context, name string, args ...string) (string, string, error) {
		parts := make([]string, 0, len(args)+1)
		parts = append(parts, shellQuote(name))
		for _, a := range args {
			parts = append(parts, shellQuote(a))
		}
		argv := append(sshBatchArgs(target), strings.Join(parts, " "))
		return runCaptured(exec.CommandContext(ctx, "ssh", argv...)) //nolint:gosec // target/argv are operator-configured, not user input
	}
}

// shellQuote wraps s in single quotes for a POSIX shell, escaping any single
// quote it contains.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func runCaptured(cmd *exec.Cmd) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	alog.Printf("fleet: doctor: %s %s", cmd.Path, strings.Join(cmd.Args[1:], " "))
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// detailFrom prefers stderr, since it usually says why a command failed;
// err alone is often just "exit status 1".
func detailFrom(stderr string, err error) string {
	if s := strings.TrimSpace(stderr); s != "" {
		return s
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// Doctor runs every check against h, in checkNames order. A failed "ssh"
// check short-circuits the rest — there is nothing on an unreachable host
// left to probe — and they come back OK=false with a Detail explaining they
// were skipped, rather than silently missing from the result.
func Doctor(ctx context.Context, h config.FleetHost, base string) []Check {
	return doctorWith(ctx, h, base, runnerForHost(h))
}

func doctorWith(ctx context.Context, h config.FleetHost, base string, run Runner) []Check {
	ssh := checkSSH(ctx, h, run)
	if !ssh.OK {
		checks := []Check{ssh}
		for _, name := range checkNames[1:] {
			checks = append(checks, Check{Name: name, OK: false, Detail: "skipped: ssh check failed"})
		}
		return checks
	}
	return []Check{
		ssh,
		checkACY(ctx, h, run),
		checkClaude(ctx, run),
		checkGH(ctx, run),
		checkRepo(ctx, h, base, run),
		checkState(ctx, run),
	}
}

// checkSSH is the only check that means anything different for a local
// host: there is no connection to make, so it is an automatic pass.
func checkSSH(ctx context.Context, h config.FleetHost, run Runner) Check {
	if h.SSH == "" {
		return Check{Name: "ssh", OK: true, Detail: "local host"}
	}
	_, stderr, err := run(ctx, "true")
	if err != nil {
		return Check{Name: "ssh", OK: false, Detail: detailFrom(stderr, err)}
	}
	return Check{Name: "ssh", OK: true}
}

// parseACYVersion extracts the version from `acy --version`'s output, per
// cli.Root's SetVersionTemplate: "<name> <version>\n". Only the last field
// is the version itself.
func parseACYVersion(out string) string {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return out
	}
	return fields[len(fields)-1]
}

// checkACY runs <acyBin> --version and compares it against this build's own
// version. A mismatch is a warning, not a failure — different acy builds on
// different fleet hosts is a real, working configuration — but a host whose
// acy predates the engineer subcommand cannot run one at all, which the
// "engineer --help" probe catches as a hard failure.
func checkACY(ctx context.Context, h config.FleetHost, run Runner) Check {
	stdout, stderr, err := run(ctx, h.ACYBin, "--version")
	if err != nil {
		return Check{Name: "acy", OK: false, Detail: detailFrom(stderr, err)}
	}
	remote := parseACYVersion(strings.TrimSpace(stdout))

	if _, stderr2, err2 := run(ctx, h.ACYBin, "engineer", "--help"); err2 != nil {
		return Check{Name: "acy", OK: false, Detail: "engineer subcommand unavailable: " + detailFrom(stderr2, err2)}
	}

	if local := version.String(); remote != local {
		return Check{Name: "acy", OK: true, Detail: fmt.Sprintf("version skew: remote %s, this build %s", remote, local)}
	}
	return Check{Name: "acy", OK: true, Detail: remote}
}

// claudeAuthStatus is the subset of `claude auth status --json` this check
// reads. It costs nothing to run — it reports cached login state, it does
// not call the model.
type claudeAuthStatus struct {
	LoggedIn   bool   `json:"loggedIn"`
	AuthMethod string `json:"authMethod"`
	Email      string `json:"email"`
}

// checkClaude prefers `claude auth status --json`: it is free (no model
// call) and answers the real question, whether this host can actually
// authenticate, not just whether a binary exists. If that command itself
// isn't there — an older claude, or claude missing entirely — this falls
// back to a bare PATH check and says plainly that auth was not verified,
// rather than guessing.
func checkClaude(ctx context.Context, run Runner) Check {
	stdout, _, err := run(ctx, "claude", "auth", "status", "--json")
	if err == nil {
		var st claudeAuthStatus
		if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &st); jsonErr == nil {
			if st.LoggedIn {
				return Check{Name: "claude", OK: true, Detail: fmt.Sprintf("logged in via %s as %s", st.AuthMethod, st.Email)}
			}
			return Check{Name: "claude", OK: false, Detail: "claude is installed but not logged in"}
		}
	}

	if _, _, pathErr := run(ctx, "sh", "-c", "command -v claude"); pathErr != nil {
		return Check{Name: "claude", OK: false, Detail: "claude not found on PATH"}
	}
	return Check{Name: "claude", OK: true, Detail: "claude found on PATH; auth was not verified (claude auth status unavailable)"}
}

// checkGH is a bare gh auth status: gh already prints exactly what a human
// needs on stderr and exits nonzero on anything short of a logged-in host.
func checkGH(ctx context.Context, run Runner) Check {
	_, stderr, err := run(ctx, "gh", "auth", "status")
	if err != nil {
		return Check{Name: "gh", OK: false, Detail: detailFrom(stderr, err)}
	}
	return Check{Name: "gh", OK: true}
}

// checkRepo confirms h.RepoPath is a git worktree with base reachable on
// origin. A local host with no origin remote (the common case for a repo
// that has never been pushed) falls back to verifying base exists locally
// instead of failing outright — a remote host has no such excuse, since
// engineer mode pushes its branch there.
func checkRepo(ctx context.Context, h config.FleetHost, base string, run Runner) Check {
	if _, stderr, err := run(ctx, "git", "-C", h.RepoPath, "rev-parse", "--is-inside-work-tree"); err != nil {
		return Check{Name: "repo", OK: false, Detail: detailFrom(stderr, err)}
	}

	if _, stderr, err := run(ctx, "git", "-C", h.RepoPath, "ls-remote", "--exit-code", "origin", base); err != nil {
		if h.SSH != "" {
			return Check{Name: "repo", OK: false, Detail: detailFrom(stderr, err)}
		}
		if _, stderr2, err2 := run(ctx, "git", "-C", h.RepoPath, "rev-parse", "--verify", base); err2 != nil {
			return Check{Name: "repo", OK: false, Detail: detailFrom(stderr2, err2)}
		}
		return Check{Name: "repo", OK: true, Detail: fmt.Sprintf("%s verified locally (no origin remote)", base)}
	}
	return Check{Name: "repo", OK: true}
}

// stateProbeScript mirrors internal/engineerd.RootDir's resolution of
// $ACY_STATE_DIR, else <user config dir>/acy, well enough to prove an
// engineer could actually write there: mkdir -p, touch, rm one probe file.
const stateProbeScript = `d="$ACY_STATE_DIR"
if [ -z "$d" ]; then
  if [ "$(uname)" = "Darwin" ]; then d="$HOME/Library/Application Support/acy"
  else d="${XDG_CONFIG_HOME:-$HOME/.config}/acy"
  fi
else
  d="$d"
fi
d="$d/engineers"
mkdir -p "$d" && f="$d/.doctor-probe-$$" && touch "$f" && rm -f "$f"`

func checkState(ctx context.Context, run Runner) Check {
	_, stderr, err := run(ctx, "sh", "-c", stateProbeScript)
	if err != nil {
		return Check{Name: "state", OK: false, Detail: detailFrom(stderr, err)}
	}
	return Check{Name: "state", OK: true}
}
