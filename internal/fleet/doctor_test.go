package fleet

import (
	"context"
	"errors"
	"fmt"
	"os"
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
		c := checkSSH(context.Background(), config.FleetHost{}, sr.run)
		if !c.OK || c.Name != "ssh" {
			t.Errorf("checkSSH = %+v", c)
		}
		if len(sr.calls) != 0 {
			t.Errorf("local host should not run any command, got %v", sr.calls)
		}
	})

	t.Run("remote host success", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"true": {},
		})
		c := checkSSH(context.Background(), config.FleetHost{SSH: "user@box1"}, sr.run)
		if !c.OK {
			t.Errorf("checkSSH = %+v, want OK", c)
		}
	})

	t.Run("remote host failure surfaces stderr", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"true": {stderr: "Permission denied (publickey).", err: errors.New("exit status 255")},
		})
		c := checkSSH(context.Background(), config.FleetHost{SSH: "user@box1"}, sr.run)
		if c.OK {
			t.Fatal("checkSSH should fail")
		}
		if !strings.Contains(c.Detail, "Permission denied") {
			t.Errorf("Detail = %q, want it to surface stderr", c.Detail)
		}
	})

	t.Run("remote host failure with no stderr falls back to the error", func(t *testing.T) {
		sr := newScriptedRunner(map[string]response{
			"true": {err: errors.New("dial tcp: connection refused")},
		})
		c := checkSSH(context.Background(), config.FleetHost{SSH: "box1"}, sr.run)
		if c.OK || !strings.Contains(c.Detail, "connection refused") {
			t.Errorf("checkSSH = %+v", c)
		}
	})
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
	checks := doctorWith(context.Background(), config.FleetHost{SSH: "nowhere"}, "main", sr.run)
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
		"git -C /srv/repo rev-parse --is-inside-work-tree":   {},
		"git -C /srv/repo ls-remote --exit-code origin main": {},
		"sh -c " + stateProbeScript:                          {},
	})
	h := config.FleetHost{SSH: "box1", ACYBin: "acy", RepoPath: "/srv/repo"}
	checks := doctorWith(context.Background(), h, "main", sr.run)
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

// TestDoctorSSHLive exercises the real ssh check against a live host, but
// only when ACY_SSH_DOCTOR_HOST names one — it never runs, and never fails
// CI, otherwise.
func TestDoctorSSHLive(t *testing.T) {
	target := os.Getenv("ACY_SSH_DOCTOR_HOST")
	if target == "" {
		t.Skip("ACY_SSH_DOCTOR_HOST not set; set it to a real ssh target (matching a .acy.json host's ssh field) to run this live")
	}
	h := config.FleetHost{SSH: target}
	c := checkSSH(context.Background(), h, runnerForHost(h))
	if !c.OK {
		t.Fatalf("live ssh check against %s failed: %s", target, c.Detail)
	}
}
