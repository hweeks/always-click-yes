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
var checkNames = []string{"ssh", "acy", "claude", "gh", "go", "repo", "state"}

// Runner runs name with args, either on this machine or wrapped for a
// remote host, and reports stdout and stderr separately. Doctor needs both:
// a refused key and an unknown host both just fail, but they fail with
// different stderr, and gitops.Runner's single merged error string would
// lose that distinction.
type Runner func(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)

// runnerForHost returns three Runners for h, direct exec for a local host or
// every command wrapped in the same BatchMode ssh preamble the engineer
// transport uses, at increasing levels of wrapping: bare applies neither
// FleetHost.Path nor FleetHost.Rc — pure reachability — pathOnly applies
// Path but never Rc, and full applies both. checkSSH probes bare first and
// only escalates to full when bare succeeds, so a broken rc wrapper is
// diagnosed as exactly that instead of masquerading as an unreachable host;
// pathOnly is what the rest of the checks fall back to in that case, so a
// working PATH extension still applies even though the broken rc doesn't.
func runnerForHost(h config.FleetHost) (bare, pathOnly, full Runner) {
	if h.SSH == "" {
		return runLocal, runLocal, runLocal
	}
	bare = sshRunner(h.SSH, nil, "", "")
	pathOnly = sshRunner(h.SSH, h.Path, "", "")
	full = sshRunner(h.SSH, h.Path, h.Rc, h.Shell)
	return bare, pathOnly, full
}

func runLocal(ctx context.Context, name string, args ...string) (string, string, error) {
	return runCaptured(exec.CommandContext(ctx, name, args...)) //nolint:gosec // name/args are doctor's own fixed argv, never user input
}

// sshDoctorArgs composes the full ssh argv for running name+args on target,
// extending PATH first when dirs (FleetHost.Path) is set and sourcing rc
// first when rc (FleetHost.Rc) is set, through shell (FleetHost.Shell, or
// rcWrap's own derivation when empty). ssh itself joins its trailing
// arguments with a bare space before handing them to the remote shell, so a
// multi-word argument like "command -v claude" would come apart into extra
// positional parameters unless the whole thing is quoted and passed as one —
// which is also what lets pathPreamble's `export PATH=...; exec ` and
// rcWrap's `<shell> -c 'source ...; ...'` sit in front of it as a single
// command. When rc is empty this is byte-identical to the composition
// without an rc file configured.
func sshDoctorArgs(target string, dirs []string, rc, shell string, name string, args []string) []string {
	inner := pathPreamble(dirs) + quoteArgv(append([]string{name}, args...))
	return append(sshBatchArgs(target), rcWrap(rc, shell, inner))
}

// sshRunner wraps every command in target's BatchMode ssh preamble. When rc
// is set, that wrap sources it first, through the shell shellFor picks from
// rc and shell (FleetHost.Rc/FleetHost.Shell) — which is what makes the
// doctor "ssh" check's own bare `true` probe, run again through this same
// wrap once the unwrapped probe has already proven ssh itself reachable,
// double as a check that rc actually sources: if that shell is missing or
// the whole invocation is otherwise broken, checkSSH reports it as a broken
// rc wrapper — naming the shell and the rc file — instead of showing up as a
// mystery downstream in the claude/gh checks.
func sshRunner(target string, dirs []string, rc, shell string) Runner {
	return func(ctx context.Context, name string, args ...string) (string, string, error) {
		argv := sshDoctorArgs(target, dirs, rc, shell, name, args)
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

// Doctor runs every check against h, in checkNames order. A genuinely
// unreachable host short-circuits the rest — there is nothing left to probe
// — and they come back OK=false with a Detail explaining they were skipped,
// rather than silently missing from the result. A host that answers ssh but
// whose rc wrapper is broken is a different diagnosis (see checkSSH): the
// rest of the checks still run, just without the broken wrap.
func Doctor(ctx context.Context, h config.FleetHost, base string) []Check {
	bare, pathOnly, full := runnerForHost(h)
	return doctorWith(ctx, h, base, bare, pathOnly, full)
}

func doctorWith(ctx context.Context, h config.FleetHost, base string, bare, pathOnly, full Runner) []Check {
	probe := checkSSH(ctx, h, bare, pathOnly, full)
	if !probe.reachable {
		checks := []Check{probe.check}
		for _, name := range checkNames[1:] {
			checks = append(checks, Check{Name: name, OK: false, Detail: "skipped: ssh check failed"})
		}
		return checks
	}
	run := probe.rest
	return []Check{
		probe.check,
		checkACY(ctx, h, run),
		checkClaude(ctx, h, run),
		checkGH(ctx, h, run),
		checkGo(ctx, h, run),
		checkRepo(ctx, h, base, run),
		checkState(ctx, run),
	}
}

// sshProbe is checkSSH's outcome: the Check to report, whether ssh itself is
// reachable — which is what decides whether doctorWith short-circuits the
// rest of the checks — and which Runner the rest of the checks should use.
type sshProbe struct {
	check     Check
	reachable bool
	rest      Runner
}

// checkSSH is the only check that means anything different for a local
// host: there is no connection to make, so it is an automatic pass. For a
// remote host it probes in two stages: bare first — pure ssh reachability,
// no Path, no Rc — and, only if that succeeds and h.Rc is set, again
// through the full Path+Rc wrap this host declares. A bare success with a
// wrapped failure means ssh itself is fine and the rc wrapper is what's
// broken (wrong shell, or a shell the host doesn't have) — that used to
// fold into "ssh unreachable" and short-circuit every other check with a
// misleading "skipped: ssh check failed", when in fact ssh, claude, gh and
// everything else downstream could still be probed just fine. So this
// reports it as its own failure, naming the shell and the rc file, and
// leaves reachable true — the rest of the checks then run through pathOnly
// instead of full, keeping the PATH extension while dropping only the
// broken rc source.
func checkSSH(ctx context.Context, h config.FleetHost, bare, pathOnly, full Runner) sshProbe {
	if h.SSH == "" {
		return sshProbe{check: Check{Name: "ssh", OK: true, Detail: "local host"}, reachable: true, rest: full}
	}
	if _, stderr, err := bare(ctx, "true"); err != nil {
		return sshProbe{check: Check{Name: "ssh", OK: false, Detail: detailFrom(stderr, err)}, rest: full}
	}
	if h.Rc == "" {
		return sshProbe{check: Check{Name: "ssh", OK: true}, reachable: true, rest: full}
	}
	if _, stderr, err := full(ctx, "true"); err != nil {
		shell := shellFor(h.Rc, h.Shell)
		detail := fmt.Sprintf(
			"ssh reachable, but the rc wrapper failed: sourcing %s via %s errored (%s) — the host may not have %s installed",
			h.Rc, shell, detailFrom(stderr, err), shell,
		)
		return sshProbe{check: Check{Name: "ssh", OK: false, Detail: detail}, reachable: true, rest: pathOnly}
	}
	return sshProbe{check: Check{Name: "ssh", OK: true}, reachable: true, rest: full}
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

// pathHintSuffix is appended to a doctor Detail when "claude" or "gh" comes
// back not-found and the host has no fleet `path` configured to fix it.
// Non-interactive ssh hands the remote command a minimal PATH, so a binary
// living somewhere non-standard (~/.local/bin, /opt/homebrew/bin — where
// claude and gh actually live on plenty of real machines) is invisible to
// this check and to the detached engineer daemon it stands in for.
const pathHintSuffix = "; if it is installed somewhere non-standard, add its directory to this host's fleet `path` in .acy.json"

// withPathHint appends pathHintSuffix to detail when h has no fleet `path`
// configured, so the hint never duplicates advice an operator has already
// acted on.
func withPathHint(detail string, h config.FleetHost) string {
	if len(h.Path) == 0 {
		return detail + pathHintSuffix
	}
	return detail
}

// looksNotFound reports whether detail describes a missing binary rather
// than some other failure — a local exec.ErrNotFound message, or the
// remote shell's "not found"/"command not found" wording over ssh.
func looksNotFound(detail string) bool {
	return strings.Contains(strings.ToLower(detail), "not found")
}

// checkClaude prefers `claude auth status --json`: it is free (no model
// call) and answers the real question, whether this host can actually
// authenticate, not just whether a binary exists. If that command itself
// isn't there — an older claude, or claude missing entirely — this falls
// back to a bare PATH check and says plainly that auth was not verified,
// rather than guessing.
func checkClaude(ctx context.Context, h config.FleetHost, run Runner) Check {
	stdout, _, _ := run(ctx, "claude", "auth", "status", "--json")
	// claude exits nonzero when not logged in, even though it still prints
	// valid, informative JSON on stdout — so this parses stdout regardless
	// of the command's exit status, and only falls back to a bare PATH check
	// when stdout itself isn't the JSON this subcommand promises.
	var st claudeAuthStatus
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &st); jsonErr == nil {
		if st.LoggedIn {
			return Check{Name: "claude", OK: true, Detail: fmt.Sprintf("logged in via %s as %s", st.AuthMethod, st.Email)}
		}
		return Check{Name: "claude", OK: false, Detail: "claude is installed but not logged in"}
	}

	if _, _, pathErr := run(ctx, "sh", "-c", "command -v claude"); pathErr != nil {
		return Check{Name: "claude", OK: false, Detail: withPathHint("claude not found on PATH", h)}
	}
	return Check{Name: "claude", OK: true, Detail: "claude found on PATH; auth was not verified (claude auth status unavailable)"}
}

// checkGH is a bare gh auth status: gh already prints exactly what a human
// needs on stderr and exits nonzero on anything short of a logged-in host.
func checkGH(ctx context.Context, h config.FleetHost, run Runner) Check {
	_, stderr, err := run(ctx, "gh", "auth", "status")
	if err != nil {
		detail := detailFrom(stderr, err)
		if looksNotFound(detail) {
			detail = withPathHint(detail, h)
		}
		return Check{Name: "gh", OK: false, Detail: detail}
	}
	return Check{Name: "gh", OK: true}
}

// checkGo probes for a Go toolchain via `go version`, so an operator finds
// out whether a host can build acy from source before a release ships with
// no binaries to download — GitHub Actions has already done that once (see
// AGENTS.md), and the fleet's only recourse then is building from each
// host's own clone. Go is frequently missing from a non-interactive ssh
// PATH even when installed (real example: /opt/homebrew/bin/go, reachable
// only because that directory is listed in the host's fleet `path`), so a
// not-found result gets the same withPathHint treatment as claude and gh.
// Unlike those checks, though, a missing toolchain is not a failure: a host
// that only ever runs a prebuilt acy binary is a perfectly good fleet
// member — it just cannot build from source — so this reports OK:true with
// an explanatory Detail either way, the same "OK anyway" call checkACY
// makes for version skew. It never invokes a build itself; probing for the
// toolchain and its version is the entire scope.
func checkGo(ctx context.Context, h config.FleetHost, run Runner) Check {
	stdout, stderr, err := run(ctx, "go", "version")
	if err != nil {
		detail := "no Go toolchain (host cannot build acy from source): " + detailFrom(stderr, err)
		if looksNotFound(detail) {
			detail = withPathHint(detail, h)
		}
		return Check{Name: "go", OK: true, Detail: detail}
	}
	return Check{Name: "go", OK: true, Detail: strings.TrimSpace(stdout)}
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
