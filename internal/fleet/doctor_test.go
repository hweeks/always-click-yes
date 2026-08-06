package fleet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/version"
)

// response is one scriptedRunner reply, keyed by the exact argv it answers.
type response struct {
	stdout, stderr string
	err            error
}

// scriptedRunner is a fake Runner: each command it sees (joined "name
// arg1 arg2...") must have a canned response, or the call fails loudly
// rather than silently returning nothing — a missing response usually means
// the check under test issued a command the test didn't expect.
type scriptedRunner struct {
	responses map[string]response
	calls     []string
}

func newScriptedRunner(responses map[string]response) *scriptedRunner {
	return &scriptedRunner{responses: responses}
}

func (s *scriptedRunner) run(_ context.Context, name string, args ...string) (string, string, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	s.calls = append(s.calls, key)
	r, ok := s.responses[key]
	if !ok {
		return "", "", fmt.Errorf("scriptedRunner: unscripted command %q", key)
	}
	return r.stdout, r.stderr, r.err
}

func TestCheckSSH(t *testing.T) {
	t.Run("local host is an automatic pass", func(t *testing.T) {
		sr := newScriptedRunner(nil)
		p := checkSSH(context.Background(), config.FleetHost{}, sr.run, sr.run, sr.run)
		if !p.check.OK || p.check.Name != "ssh" || !p.reachable {
			t.Errorf("checkSSH = %+v", p)
		}
		if len(sr.calls) != 0 {
			t.Errorf("local host should not run any command, got %v", sr.calls)
		}
	})

	t.Run("remote host success, no rc", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"true": {},
		})
		p := checkSSH(context.Background(), config.FleetHost{SSH: "user@box1"}, sr.run, sr.run, sr.run)
		if !p.check.OK || !p.reachable {
			t.Errorf("checkSSH = %+v, want OK and reachable", p)
		}
	})

	t.Run("remote host unreachable surfaces stderr and is not reachable", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"true": {stderr: "Permission denied (publickey).", err: errors.New("exit status 255")},
		})
		p := checkSSH(context.Background(), config.FleetHost{SSH: "user@box1"}, sr.run, sr.run, sr.run)
		if p.check.OK || p.reachable {
			t.Fatal("checkSSH should fail and be unreachable")
		}
		if !strings.Contains(p.check.Detail, "Permission denied") {
			t.Errorf("Detail = %q, want it to surface stderr", p.check.Detail)
		}
	})

	t.Run("remote host unreachable with no stderr falls back to the error", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"true": {err: errors.New("dial tcp: connection refused")},
		})
		p := checkSSH(context.Background(), config.FleetHost{SSH: "box1"}, sr.run, sr.run, sr.run)
		if p.check.OK || p.reachable || !strings.Contains(p.check.Detail, "connection refused") {
			t.Errorf("checkSSH = %+v", p)
		}
	})

	t.Run("ssh reachable but the rc wrapper is broken is a distinct diagnosis, not unreachable", func(t *testing.T) {
		bare := newScriptedRunner(map[string]response{"true": {}})
		full := newScriptedRunner(map[string]response{
			"true": {stderr: "bash: command not found", err: errors.New("exit status 127")},
		})
		pathOnly := newScriptedRunner(map[string]response{"marker": {stdout: "path-only"}})

		h := config.FleetHost{SSH: "spark", Rc: "~/.bashrc"}
		p := checkSSH(context.Background(), h, bare.run, pathOnly.run, full.run)
		if p.check.OK {
			t.Fatal("a broken rc wrapper should fail the ssh check")
		}
		if !p.reachable {
			t.Fatal("ssh itself succeeded, so the host should still be reported reachable")
		}
		if !strings.Contains(p.check.Detail, "bash") || !strings.Contains(p.check.Detail, "~/.bashrc") {
			t.Errorf("Detail = %q, want it to name the shell and the rc file", p.check.Detail)
		}
		stdout, _, err := p.rest(context.Background(), "marker")
		if err != nil || stdout != "path-only" {
			t.Errorf("rest runner should be pathOnly (PATH kept, rc dropped), got stdout=%q err=%v", stdout, err)
		}
	})

	t.Run("ssh reachable, no rc configured, rest runner is full", func(t *testing.T) {
		bare := newScriptedRunner(map[string]response{"true": {}})
		full := newScriptedRunner(map[string]response{"marker": {stdout: "full"}})
		p := checkSSH(context.Background(), config.FleetHost{SSH: "box1"}, bare.run, bare.run, full.run)
		if !p.check.OK || !p.reachable {
			t.Fatalf("checkSSH = %+v", p)
		}
		stdout, _, err := p.rest(context.Background(), "marker")
		if err != nil || stdout != "full" {
			t.Errorf("rest runner should be full when there's no rc to break, got stdout=%q err=%v", stdout, err)
		}
	})
}

// TestSSHRunnerSourcesRc proves sshRunner wraps every command in a
// `<shell> -c 'source <rc>; ...'` invocation when the host declares Rc,
// using a stub "ssh" that just records the argv it was called with.
func TestSSHRunnerSourcesRc(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s' \"$*\" > " + shq(argvFile) + "\n"
	writeStub(t, dir, "ssh", script)
	withStubSSH(t, dir)

	run := sshRunner("box1", nil, "~/.zshrc", "")
	if _, _, err := run(context.Background(), "true"); err != nil {
		t.Fatalf("run: %v", err)
	}

	wantArgv := "-o BatchMode=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=4 box1 -- " +
		rcWrap("~/.zshrc", "", quoteArgv([]string{"true"}))
	if got := strings.TrimSpace(readFile(t, argvFile)); got != wantArgv {
		t.Errorf("argv = %q, want %q", got, wantArgv)
	}
}

// TestDoctorFixesTheSparkScenario reproduces the exact live bug: host
// "spark" is Ubuntu with bash only and no zsh, configured with rc:
// "~/.bashrc". Before the fix, rcWrap hardcoded zsh regardless of the rc
// file's own shell, so this host's "ssh" check — and every check after it —
// died with "zsh: command not found". Deriving the shell from the rc file's
// basename means this host now sources ~/.bashrc through bash, which the
// stub "ssh" below treats as present, and the whole pipeline passes.
func TestDoctorFixesTheSparkScenario(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *'zsh'*) echo 'zsh: command not found' >&2; exit 127 ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	writeStub(t, dir, "ssh", script)
	withStubSSH(t, dir)

	h := config.FleetHost{SSH: "spark", RepoPath: "/srv/repo", Rc: "~/.bashrc"}
	checks := Doctor(context.Background(), h, "main")
	if checks[0].Name != "ssh" || !checks[0].OK {
		t.Fatalf("ssh check = %+v, want OK now that the rc wrapper uses bash, not zsh", checks[0])
	}
}

// TestDoctorDiagnosesBrokenRcWrapperInsteadOfUnreachableHost proves the full
// Doctor() pipeline — runnerForHost's bare/pathOnly/full split, threaded
// through checkSSH — reports a broken rc wrapper as its own diagnosis,
// naming the shell and the rc file, instead of folding it into "ssh
// unreachable" and skipping every other check. This is BUG 2 from a live
// debugging session: a broken rc wrapper on an otherwise-healthy host used
// to read as six red "unreachable" lines, costing a whole round trip to
// diagnose.
func TestDoctorDiagnosesBrokenRcWrapperInsteadOfUnreachableHost(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *'fish -c'*) echo 'fish: command not found' >&2; exit 127 ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	writeStub(t, dir, "ssh", script)
	withStubSSH(t, dir)

	h := config.FleetHost{SSH: "spark", RepoPath: "/srv/repo", Rc: "~/.bashrc", Shell: "fish"}
	checks := Doctor(context.Background(), h, "main")

	if checks[0].Name != "ssh" || checks[0].OK {
		t.Fatalf("ssh check = %+v, want a broken-rc-wrapper failure", checks[0])
	}
	if !strings.Contains(checks[0].Detail, "fish") || !strings.Contains(checks[0].Detail, "~/.bashrc") {
		t.Errorf("Detail = %q, want it to name the shell and the rc file", checks[0].Detail)
	}
	for _, c := range checks[1:] {
		if !c.OK {
			t.Errorf("%s should still run (unwrapped by the broken rc) and pass, got %+v", c.Name, c)
		}
		if strings.Contains(c.Detail, "skipped") {
			t.Errorf("%s should not be skipped when only the rc wrapper is broken, got %+v", c.Name, c)
		}
	}
}

func TestCheckACY(t *testing.T) {
	local := version.String()

	t.Run("matching version", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"acy --version":       {stdout: "always-click-yes " + local},
			"acy engineer --help": {},
		})
		c := checkACY(context.Background(), config.FleetHost{ACYBin: "acy"}, sr.run)
		if !c.OK || c.Detail != local {
			t.Errorf("checkACY = %+v, want OK with detail %q", c, local)
		}
	})

	t.Run("version skew is OK with a note", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"acy --version":       {stdout: "always-click-yes v9.9.9"},
			"acy engineer --help": {},
		})
		c := checkACY(context.Background(), config.FleetHost{ACYBin: "acy"}, sr.run)
		if !c.OK {
			t.Errorf("version skew should be OK=true, got %+v", c)
		}
		if !strings.Contains(c.Detail, "version skew") || !strings.Contains(c.Detail, "v9.9.9") {
			t.Errorf("Detail = %q, want it to note the skew", c.Detail)
		}
	})

	t.Run("missing engineer subcommand fails despite a clean --version", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"acy --version":       {stdout: "always-click-yes " + local},
			"acy engineer --help": {stderr: "unknown command \"engineer\"", err: errors.New("exit status 1")},
		})
		c := checkACY(context.Background(), config.FleetHost{ACYBin: "acy"}, sr.run)
		if c.OK {
			t.Fatal("missing engineer subcommand should fail the check")
		}
		if !strings.Contains(c.Detail, "engineer subcommand unavailable") {
			t.Errorf("Detail = %q", c.Detail)
		}
	})

	t.Run("acy not runnable at all", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"acy --version": {err: errors.New("exec: \"acy\": executable file not found in $PATH")},
		})
		c := checkACY(context.Background(), config.FleetHost{ACYBin: "acy"}, sr.run)
		if c.OK {
			t.Fatal("want a failure")
		}
	})
}

func TestCheckClaude(t *testing.T) {
	t.Run("logged in", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"claude auth status --json": {stdout: `{"loggedIn":true,"authMethod":"claude.ai","email":"a@b.com"}`},
		})
		c := checkClaude(context.Background(), config.FleetHost{}, sr.run)
		if !c.OK || !strings.Contains(c.Detail, "a@b.com") {
			t.Errorf("checkClaude = %+v", c)
		}
	})

	t.Run("installed but not logged in", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"claude auth status --json": {stdout: `{"loggedIn":false}`},
		})
		c := checkClaude(context.Background(), config.FleetHost{}, sr.run)
		if c.OK {
			t.Fatal("not logged in should fail the check")
		}
		if strings.Contains(c.Detail, "fleet `path`") {
			t.Errorf("Detail = %q, should not hint at path for a not-logged-in host", c.Detail)
		}
	})

	t.Run("not logged in with a nonzero exit still parses the JSON", func(t *testing.T) {
		// Real claude exits 1 when not logged in, even though it still
		// prints valid, informative JSON on stdout.
		sr := newScriptedRunner(map[string]response{
			"claude auth status --json": {stdout: `{"loggedIn":false,"authMethod":"none"}`, err: errors.New("exit status 1")},
		})
		c := checkClaude(context.Background(), config.FleetHost{}, sr.run)
		if c.OK {
			t.Fatal("not logged in should fail the check even when the command itself exits nonzero")
		}
		if !strings.Contains(c.Detail, "not logged in") {
			t.Errorf("Detail = %q", c.Detail)
		}
	})

	t.Run("auth status unavailable, falls back to PATH and says so", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"claude auth status --json": {err: errors.New("exit status 1")},
			"sh -c command -v claude":   {stdout: "/usr/local/bin/claude"},
		})
		c := checkClaude(context.Background(), config.FleetHost{}, sr.run)
		if !c.OK {
			t.Errorf("checkClaude = %+v, want OK from the PATH fallback", c)
		}
		if !strings.Contains(c.Detail, "not verified") {
			t.Errorf("Detail = %q, want it to say auth wasn't verified", c.Detail)
		}
	})

	t.Run("claude missing entirely, no path configured, hints at the config", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"claude auth status --json": {err: errors.New("exec: \"claude\": executable file not found in $PATH")},
			"sh -c command -v claude":   {err: errors.New("exit status 1")},
		})
		c := checkClaude(context.Background(), config.FleetHost{}, sr.run)
		if c.OK {
			t.Fatal("want a failure when claude is nowhere to be found")
		}
		if !strings.Contains(c.Detail, "not found on PATH") {
			t.Errorf("Detail = %q", c.Detail)
		}
		if !strings.Contains(c.Detail, "fleet `path`") {
			t.Errorf("Detail = %q, want a hint about fleet `path`", c.Detail)
		}
	})

	t.Run("claude missing entirely, path already configured, no hint", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"claude auth status --json": {err: errors.New("exec: \"claude\": executable file not found in $PATH")},
			"sh -c command -v claude":   {err: errors.New("exit status 1")},
		})
		c := checkClaude(context.Background(), config.FleetHost{Path: []string{"/opt/homebrew/bin"}}, sr.run)
		if c.OK {
			t.Fatal("want a failure when claude is nowhere to be found")
		}
		if strings.Contains(c.Detail, "fleet `path`") {
			t.Errorf("Detail = %q, should not hint when path is already configured", c.Detail)
		}
	})
}

func TestCheckGH(t *testing.T) {
	t.Run("logged in", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{"gh auth status": {}})
		c := checkGH(context.Background(), config.FleetHost{}, sr.run)
		if !c.OK {
			t.Errorf("checkGH = %+v", c)
		}
	})

	t.Run("not logged in", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"gh auth status": {stderr: "You are not logged into any GitHub hosts.", err: errors.New("exit status 1")},
		})
		c := checkGH(context.Background(), config.FleetHost{}, sr.run)
		if c.OK || !strings.Contains(c.Detail, "not logged into") {
			t.Errorf("checkGH = %+v", c)
		}
		if strings.Contains(c.Detail, "fleet `path`") {
			t.Errorf("Detail = %q, should not hint at path for a not-logged-in host", c.Detail)
		}
	})

	t.Run("not found, no path configured, hints at the config", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"gh auth status": {stderr: "bash: line 1: gh: command not found", err: errors.New("exit status 127")},
		})
		c := checkGH(context.Background(), config.FleetHost{}, sr.run)
		if c.OK {
			t.Fatal("want a failure when gh is nowhere to be found")
		}
		if !strings.Contains(c.Detail, "fleet `path`") {
			t.Errorf("Detail = %q, want a hint about fleet `path`", c.Detail)
		}
	})

	t.Run("not found, path already configured, no hint", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"gh auth status": {stderr: "bash: line 1: gh: command not found", err: errors.New("exit status 127")},
		})
		c := checkGH(context.Background(), config.FleetHost{Path: []string{"/opt/homebrew/bin"}}, sr.run)
		if c.OK {
			t.Fatal("want a failure when gh is nowhere to be found")
		}
		if strings.Contains(c.Detail, "fleet `path`") {
			t.Errorf("Detail = %q, should not hint when path is already configured", c.Detail)
		}
	})
}

// TestCheckGo proves a missing Go toolchain is deliberately OK:true — a
// host with a prebuilt acy binary and no compiler (the real "spark" host is
// exactly this) is a working fleet member, not a broken one — while still
// reporting the version when a toolchain is present and hinting at fleet
// `path` when it's absent and unconfigured, same as claude and gh.
func TestCheckGo(t *testing.T) {
	t.Run("toolchain present reports its version", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"go version": {stdout: "go version go1.22.1 darwin/arm64\n"},
		})
		c := checkGo(context.Background(), config.FleetHost{}, sr.run)
		if !c.OK {
			t.Errorf("checkGo = %+v, want OK", c)
		}
		if !strings.Contains(c.Detail, "go1.22.1") {
			t.Errorf("Detail = %q, want it to report the version", c.Detail)
		}
	})

	t.Run("toolchain missing is OK with an explanatory detail, not a failure", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"go version": {stderr: "bash: line 1: go: command not found", err: errors.New("exit status 127")},
		})
		c := checkGo(context.Background(), config.FleetHost{Path: []string{"/opt/homebrew/bin"}}, sr.run)
		if !c.OK {
			t.Fatal("a missing Go toolchain must not fail the check — a prebuilt-binary-only host is a good fleet member")
		}
		if !strings.Contains(c.Detail, "no Go toolchain") {
			t.Errorf("Detail = %q, want it to explain the toolchain is absent", c.Detail)
		}
		if strings.Contains(c.Detail, "fleet `path`") {
			t.Errorf("Detail = %q, should not hint when path is already configured", c.Detail)
		}
	})

	t.Run("toolchain missing, no fleet path configured, hints at the config", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"go version": {err: errors.New("exec: \"go\": executable file not found in $PATH")},
		})
		c := checkGo(context.Background(), config.FleetHost{}, sr.run)
		if !c.OK {
			t.Fatal("a missing Go toolchain must not fail the check")
		}
		if !strings.Contains(c.Detail, "fleet `path`") {
			t.Errorf("Detail = %q, want a hint about fleet `path`", c.Detail)
		}
	})
}

func TestCheckRepo(t *testing.T) {
	t.Run("origin has base", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"git -C /srv/repo rev-parse --is-inside-work-tree":   {},
			"git -C /srv/repo ls-remote --exit-code origin main": {},
		})
		c := checkRepo(context.Background(), config.FleetHost{RepoPath: "/srv/repo"}, "main", sr.run)
		if !c.OK {
			t.Errorf("checkRepo = %+v", c)
		}
	})

	t.Run("not a git worktree fails immediately", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"git -C /srv/repo rev-parse --is-inside-work-tree": {stderr: "not a git repository", err: errors.New("exit status 128")},
		})
		c := checkRepo(context.Background(), config.FleetHost{RepoPath: "/srv/repo"}, "main", sr.run)
		if c.OK {
			t.Fatal("want a failure")
		}
	})

	t.Run("local host with no origin falls back to a local verify", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"git -C /srv/repo rev-parse --is-inside-work-tree":   {},
			"git -C /srv/repo ls-remote --exit-code origin main": {err: errors.New("exit status 128"), stderr: "fatal: 'origin' does not appear to be a git repository"},
			"git -C /srv/repo rev-parse --verify main":           {},
		})
		c := checkRepo(context.Background(), config.FleetHost{RepoPath: "/srv/repo"}, "main", sr.run)
		if !c.OK {
			t.Errorf("checkRepo = %+v, want the local fallback to pass", c)
		}
		if !strings.Contains(c.Detail, "verified locally") {
			t.Errorf("Detail = %q", c.Detail)
		}
	})

	t.Run("local host, no origin, base also missing locally", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"git -C /srv/repo rev-parse --is-inside-work-tree":   {},
			"git -C /srv/repo ls-remote --exit-code origin main": {err: errors.New("exit status 128")},
			"git -C /srv/repo rev-parse --verify main":           {err: errors.New("exit status 128"), stderr: "fatal: Needed a single revision"},
		})
		c := checkRepo(context.Background(), config.FleetHost{RepoPath: "/srv/repo"}, "main", sr.run)
		if c.OK {
			t.Fatal("want a failure when base doesn't exist even locally")
		}
	})

	t.Run("remote host with no origin does not fall back", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"git -C /srv/repo rev-parse --is-inside-work-tree":   {},
			"git -C /srv/repo ls-remote --exit-code origin main": {err: errors.New("exit status 128"), stderr: "fatal: no origin"},
		})
		c := checkRepo(context.Background(), config.FleetHost{SSH: "box1", RepoPath: "/srv/repo"}, "main", sr.run)
		if c.OK {
			t.Fatal("want a failure")
		}
		for _, call := range sr.calls {
			if strings.Contains(call, "--verify") {
				t.Errorf("remote host should not fall back to a local verify, but called %q", call)
			}
		}
	})
}

func TestCheckState(t *testing.T) {
	t.Run("probe succeeds", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"sh -c " + stateProbeScript: {},
		})
		c := checkState(context.Background(), sr.run)
		if !c.OK {
			t.Errorf("checkState = %+v", c)
		}
	})

	t.Run("probe fails", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"sh -c " + stateProbeScript: {stderr: "mkdir: permission denied", err: errors.New("exit status 1")},
		})
		c := checkState(context.Background(), sr.run)
		if c.OK || !strings.Contains(c.Detail, "permission denied") {
			t.Errorf("checkState = %+v", c)
		}
	})
}

func TestDoctorWithShortCircuitsOnSSHFailure(t *testing.T) {
	sr := newScriptedRunner(map[string]response{
		"true": {stderr: "Could not resolve hostname", err: errors.New("exit status 255")},
	})
	checks := doctorWith(context.Background(), config.FleetHost{SSH: "nowhere"}, "main", sr.run, sr.run, sr.run)
	if len(checks) != len(checkNames) {
		t.Fatalf("got %d checks, want %d", len(checks), len(checkNames))
	}
	if checks[0].Name != "ssh" || checks[0].OK {
		t.Fatalf("checks[0] = %+v", checks[0])
	}
	for _, c := range checks[1:] {
		if c.OK {
			t.Errorf("%s should be skipped, not OK: %+v", c.Name, c)
		}
		if !strings.Contains(c.Detail, "skipped") {
			t.Errorf("%s Detail = %q, want it to say skipped", c.Name, c.Detail)
		}
	}
	if len(sr.calls) != 1 {
		t.Errorf("only the ssh check should have run a command, got %v", sr.calls)
	}
}

func TestDoctorWithRunsEveryCheckInOrder(t *testing.T) {
	local := version.String()
	sr := newScriptedRunner(map[string]response{
		"true":                      {},
		"acy --version":             {stdout: "always-click-yes " + local},
		"acy engineer --help":       {},
		"claude auth status --json": {stdout: `{"loggedIn":true}`},
		"gh auth status":            {},
		"go version":                {stdout: "go version go1.22.1 darwin/arm64\n"},
		"git -C /srv/repo rev-parse --is-inside-work-tree":   {},
		"git -C /srv/repo ls-remote --exit-code origin main": {},
		"sh -c " + stateProbeScript:                          {},
	})
	h := config.FleetHost{SSH: "box1", ACYBin: "acy", RepoPath: "/srv/repo"}
	checks := doctorWith(context.Background(), h, "main", sr.run, sr.run, sr.run)
	if len(checks) != len(checkNames) {
		t.Fatalf("got %d checks, want %d", len(checks), len(checkNames))
	}
	for i, name := range checkNames {
		if checks[i].Name != name {
			t.Errorf("checks[%d].Name = %q, want %q", i, checks[i].Name, name)
		}
		if !checks[i].OK {
			t.Errorf("checks[%d] (%s) = %+v, want OK", i, name, checks[i])
		}
	}
}

// TestDoctorWithRunsRemainingChecksThroughPathOnlyWhenRcBroken proves
// doctorWith routes acy/claude/gh/repo/state through the pathOnly runner —
// not the broken full one, and not the bare one that would also drop a
// working PATH extension — once checkSSH has diagnosed the rc wrapper
// itself as the problem. full is scripted to answer nothing but the ssh
// check's own "true" probe, so if doctorWith mistakenly kept using it for
// the rest, every remaining check would fail on "unscripted command".
func TestDoctorWithRunsRemainingChecksThroughPathOnlyWhenRcBroken(t *testing.T) {
	local := version.String()
	bare := newScriptedRunner(map[string]response{"true": {}})
	full := newScriptedRunner(map[string]response{
		"true": {stderr: "bash: command not found", err: errors.New("exit status 127")},
	})
	pathOnly := newScriptedRunner(map[string]response{
		"acy --version":             {stdout: "always-click-yes " + local},
		"acy engineer --help":       {},
		"claude auth status --json": {stdout: `{"loggedIn":true}`},
		"gh auth status":            {},
		"go version":                {stdout: "go version go1.22.1 linux/arm64\n"},
		"git -C /srv/repo rev-parse --is-inside-work-tree":   {},
		"git -C /srv/repo ls-remote --exit-code origin main": {},
		"sh -c " + stateProbeScript:                          {},
	})
	h := config.FleetHost{SSH: "spark", ACYBin: "acy", RepoPath: "/srv/repo", Rc: "~/.bashrc"}
	checks := doctorWith(context.Background(), h, "main", bare.run, pathOnly.run, full.run)

	if len(checks) != len(checkNames) {
		t.Fatalf("got %d checks, want %d", len(checks), len(checkNames))
	}
	if checks[0].OK {
		t.Fatalf("ssh check = %+v, want the broken-rc failure", checks[0])
	}
	for _, c := range checks[1:] {
		if !c.OK {
			t.Errorf("%s should have run through the path-only runner and passed, got %+v", c.Name, c)
		}
	}
}

// TestDoctorSSHLive exercises the real ssh check against a live host, but
// only when ACY_SSH_DOCTOR_HOST names one — it never runs, and never fails
// CI, otherwise.
func TestDoctorSSHLive(t *testing.T) {
	target := os.Getenv("ACY_SSH_DOCTOR_HOST")
	if target == "" {
		t.Skip("ACY_SSH_DOCTOR_HOST not set; set it to a real ssh target (matching a .acy.json host's ssh field) to run this live")
	}
	h := config.FleetHost{SSH: target}
	bare, pathOnly, full := runnerForHost(h)
	p := checkSSH(context.Background(), h, bare, pathOnly, full)
	if !p.check.OK {
		t.Fatalf("live ssh check against %s failed: %s", target, p.check.Detail)
	}
}
