package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/engineerd"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
	"github.com/hweeks/always-click-yes/internal/state"
)

// engineerCompletionTimeout bounds the second attach's read-to-completion:
// generous, because it is waiting on a real claude session planning, being
// armed, dispatching a child, and finishing — all for real, on a real
// subscription.
const engineerCompletionTimeout = 10 * time.Minute

// pidCleanupTimeout bounds how long we wait, after a Result has streamed,
// for the detached engineer process to actually exit and remove its own pid
// file — the two things RunDetachedTarget does right after core.Run returns.
const pidCleanupTimeout = 30 * time.Second

// TestE2EEngineerDetachedRunSurvivesReattach proves the detached-engineer
// stack end to end, against a real claude subscription: `acy engineer start`
// launches a real, detached, setsid'd engineer process; a first `attach` is
// killed mid-stream and a second resumes with no gap or overlap in seq; the
// engineer finishes on its own, pushes its branch to a local bare origin, and
// opens a PR through a stub `gh` — proving the PR step fires with no real
// GitHub involved. No GitHub, no network beyond claude.
func TestE2EEngineerDetachedRunSurvivesReattach(t *testing.T) {
	requireLive(t)

	acyBin, err := acyBinary()
	if err != nil {
		t.Fatalf("build acy: %v", err)
	}

	// Hermetic git: no ~/.gitconfig, no system config, a fixed commit
	// identity — the same pattern internal/gitops/gitops_test.go uses,
	// except here the git commands are issued by the real detached engineer
	// process, not by the test, so the env has to travel on the process
	// env itself rather than on a Runner's explicit cmd.Env.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "acy-engineer-e2e")
	t.Setenv("GIT_AUTHOR_EMAIL", "acy-engineer-e2e@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "acy-engineer-e2e")
	t.Setenv("GIT_COMMITTER_EMAIL", "acy-engineer-e2e@example.com")

	t.Setenv(state.EnvDir, shortStateDir(t))

	clonePath, bareOrigin := scratchGitRepo(t)
	argvFile := stubGh(t)

	spec := engineerwire.Spec{
		Ticket:     "T1",
		Title:      "Greeting file",
		Brief:      "create GREETING.md containing the single line 'hello from the engineer' and commit it",
		Success:    "GREETING.md exists at the repo root and its exact contents are the single line 'hello from the engineer'",
		BaseBranch: "main",
		Branch:     "acy/t1-greeting",
		BudgetUSD:  3.00,
	}

	ack := startEngineer(t, acyBin, clonePath, spec)
	t.Logf("engineer started: id=%s dir=%s pid=%d", ack.EngineerID, ack.Dir, ack.PID)

	// Last-resort cleanup: try a graceful Cancel over the real control-socket
	// path first (which lets Core's own defer chain stop its claude session
	// cleanly), then unconditionally SIGKILL the negative pgid — the
	// engineer ran under setsid (internal/cli/engineer_detach_unix.go), so
	// its pid is also its pgid. A failed test here must never leave a claude
	// session running unwatched and burning tokens.
	t.Cleanup(func() {
		_ = engineerd.SendControl(ack.Dir, engineerwire.Cancel{Reason: "test cleanup"})
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && syscall.Kill(ack.PID, 0) == nil {
			time.Sleep(100 * time.Millisecond)
		}
		_ = syscall.Kill(-ack.PID, syscall.SIGKILL)
	})

	// --- first attach: read until the first `event`, then kill it (never the engineer) ---
	firstSeqs := attachUntilFirstEvent(t, acyBin, ack.EngineerID)
	highest := firstSeqs[len(firstSeqs)-1]
	for i, s := range firstSeqs {
		if s != int64(i+1) {
			t.Fatalf("first attach's seqs are not contiguous from 1: %v", firstSeqs)
		}
	}

	if err := syscall.Kill(ack.PID, 0); err != nil {
		t.Fatalf("engineer pid %d is not alive after killing the first attach: %v", ack.PID, err)
	}

	// --- second attach: resume from where the first left off, read to completion ---
	secondSeqs, result := attachToCompletion(t, acyBin, ack.EngineerID, highest+1)

	if secondSeqs[0] != highest+1 {
		t.Fatalf("second attach's first seq = %d, want %d (contiguous with the first attach)", secondSeqs[0], highest+1)
	}
	for i := 1; i < len(secondSeqs); i++ {
		if secondSeqs[i] != secondSeqs[i-1]+1 {
			t.Fatalf("gap or overlap in the second attach's seqs: %v", secondSeqs)
		}
	}

	if result == nil {
		t.Fatalf("second attach ended with no Result message; seqs seen: %v", secondSeqs)
	}
	if result.Outcome != "completed" {
		t.Fatalf("result outcome = %q, want completed (summary: %q)", result.Outcome, result.Summary)
	}
	if result.Branch != spec.Branch {
		t.Fatalf("result branch = %q, want %q", result.Branch, spec.Branch)
	}
	const wantPRURL = "https://example.invalid/pr/1"
	if result.PRURL != wantPRURL {
		t.Fatalf("result pr_url = %q, want %q", result.PRURL, wantPRURL)
	}
	if result.CostUSD <= 0 {
		t.Fatalf("result cost_usd = %v, want > 0", result.CostUSD)
	}

	// --- on-disk proof: the bare origin actually has the branch and the file ---
	runGit(t, bareOrigin, "rev-parse", "--verify", "refs/heads/"+spec.Branch)
	got := strings.TrimSpace(runGit(t, bareOrigin, "show", spec.Branch+":GREETING.md"))
	if got != "hello from the engineer" {
		t.Fatalf("GREETING.md on %s (in the bare origin) = %q, want %q", spec.Branch, got, "hello from the engineer")
	}

	// --- the stub gh actually fired, with the right --base/--head ---
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("reading stub gh argv file: %v", err)
	}
	if !strings.Contains(string(argv), "pr create --base main --head "+spec.Branch) {
		t.Fatalf("gh was not called as expected; argv file:\n%s", argv)
	}

	// --- the engineer process is gone, and it cleaned up its own pid file ---
	pidPath := filepath.Join(ack.Dir, engineerd.PIDFile)
	deadline := time.Now().Add(pidCleanupTimeout)
	for {
		_, statErr := os.Stat(pidPath)
		aliveErr := syscall.Kill(ack.PID, 0)
		if os.IsNotExist(statErr) && aliveErr != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("engineer pid %d / pid file did not clean up within %s (alive err=%v, pidfile stat err=%v)",
				ack.PID, pidCleanupTimeout, aliveErr, statErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// shortStateDir is a short-lived directory anchored at /tmp rather than
// t.TempDir() (or the default os.MkdirTemp("", ...), which resolves to the
// same $TMPDIR): an engineer's control.sock lives under
// $ACY_STATE_DIR/engineers/<id>, and unix's sockaddr_un caps the whole path
// at about 104 bytes on macOS. macOS's per-user $TMPDIR is already ~49
// bytes on its own, and the "/engineers/<id>/control.sock" suffix this test
// adds on top runs another ~44 — comfortably blowing the limit even with a
// short prefix, which is exactly what silently sank the first version of
// this test: ListenControl failed before core.Run (and its sendHello) ever
// ran, so the attach side saw nothing and just timed out. Anchoring at /tmp
// directly (available on both macOS and Linux) leaves plenty of headroom.
// internal/engineerd's own shortTempDir helper hits the same sockaddr_un
// limit but does not need this — its callers use the returned dir as the
// socket's directory directly, with no engineers/<id> nesting on top.
func shortStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "acy-e2e-eng-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// --- scratch git setup ---

// scratchGitRepo builds a non-bare working repo with one commit on main and a
// file:// bare repo added as its origin, with main already pushed — so the
// clone this returns can `git fetch origin main` (what
// gitops.EnsureWorktree needs) and, later, `git push` a new branch, all with
// no network involved.
func scratchGitRepo(t *testing.T) (clonePath, bareOrigin string) {
	t.Helper()
	root := t.TempDir()

	clonePath = filepath.Join(root, "clone")
	if err := os.MkdirAll(clonePath, 0o755); err != nil {
		t.Fatalf("mkdir clone: %v", err)
	}
	runGit(t, clonePath, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(clonePath, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("writing README.md: %v", err)
	}
	runGit(t, clonePath, "add", "README.md")
	runGit(t, clonePath, "commit", "-m", "initial commit")

	bareOrigin = filepath.Join(root, "origin.git")
	runGit(t, root, "init", "--bare", bareOrigin)
	runGit(t, clonePath, "remote", "add", "origin", "file://"+bareOrigin)
	runGit(t, clonePath, "push", "-u", "origin", "main")
	return clonePath, bareOrigin
}

// runGit runs a git command in dir and fails the test on error, returning
// stdout+stderr combined.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (dir=%s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// stubGh puts a fake `gh` first on PATH, ahead of anything real: for `pr
// create` it prints a fixed PR URL and exits 0, proving the PR step fires
// without ever touching GitHub. Every invocation's argv is appended (space
// joined, embedded newlines flattened) to the returned file, so the test can
// assert on the call it saw.
func stubGh(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	argvFile := filepath.Join(t.TempDir(), "gh-argv.txt")

	script := "#!/bin/sh\n" +
		"printf '%s' \"$*\" | tr '\\n' ' ' >> \"$ACY_E2E_GH_ARGV\"\n" +
		"printf '\\n' >> \"$ACY_E2E_GH_ARGV\"\n" +
		"if [ \"$1\" = pr ] && [ \"$2\" = create ]; then\n" +
		"  echo https://example.invalid/pr/1\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(script), 0o755); err != nil { //nolint:gosec // a stub script, meant to be executable
		t.Fatalf("writing stub gh: %v", err)
	}

	t.Setenv("ACY_E2E_GH_ARGV", argvFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvFile
}

// --- `acy engineer start` ---

// engineerStartAck mirrors the unexported startResult in internal/cli/engineer.go —
// the one JSON line `acy engineer start` prints on success.
type engineerStartAck struct {
	EngineerID string `json:"engineer_id"`
	Dir        string `json:"dir"`
	PID        int    `json:"pid"`
}

// startEngineer runs `acy engineer start --clone clonePath`, feeding it spec
// as its one stdin line, and decodes the ack it prints.
func startEngineer(t *testing.T, acyBin, clonePath string, spec engineerwire.Spec) engineerStartAck {
	t.Helper()
	line, err := engineerwire.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, acyBin, "engineer", "start", "--clone", clonePath)
	cmd.Stdin = bytes.NewReader(line)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("acy engineer start: %v\nstderr:\n%s", err, stderr.String())
	}

	var ack engineerStartAck
	if err := json.Unmarshal(bytes.TrimSpace(out), &ack); err != nil {
		t.Fatalf("decoding start ack %q: %v", out, err)
	}
	if ack.EngineerID == "" || ack.Dir == "" || ack.PID == 0 {
		t.Fatalf("incomplete start ack: %+v", ack)
	}
	return ack
}

// --- `acy engineer attach` ---

// attachMsg is one decoded wire message read off an attach process's stdout,
// or the error that ended the stream (io.EOF on a clean exit).
type attachMsg struct {
	msg    any
	err    error
	stderr string
}

// startAttach runs `acy engineer attach <id> --from <from>` and decodes its
// stdout as a stream of wire messages onto the returned channel, closed once
// the process's stdout hits EOF. The caller owns the process: kill it early,
// or just let it run to its own exit.
func startAttach(t *testing.T, acyBin, id string, from int64) (*exec.Cmd, <-chan attachMsg) {
	t.Helper()
	cmd := exec.Command(acyBin, "engineer", "attach", id, "--from", strconv.FormatInt(from, 10))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("attach stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting acy engineer attach --from %d: %v", from, err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	ch := make(chan attachMsg, 64)
	go func() {
		defer close(ch)
		dec := engineerwire.NewDecoder(stdout)
		for {
			msg, err := dec.Decode()
			if err != nil {
				ch <- attachMsg{err: err, stderr: stderr.String()}
				return
			}
			ch <- attachMsg{msg: msg}
		}
	}()
	return cmd, ch
}

// attachUntilFirstEvent attaches from seq 1, reads until (and including) the
// first `event` message, kills the attach process — never the engineer — and
// returns every seq it saw, in order.
func attachUntilFirstEvent(t *testing.T, acyBin, id string) []int64 {
	t.Helper()
	cmd, ch := startAttach(t, acyBin, id, 1)

	var seqs []int64
	timeout := time.After(planTimeout)
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				t.Fatalf("first attach's stream ended before an event arrived; seqs so far: %v", seqs)
			}
			if m.err != nil {
				t.Fatalf("first attach: decode error: %v\nstderr:\n%s", m.err, m.stderr)
			}
			seq, kind := seqAndKind(m.msg)
			seqs = append(seqs, seq)
			if kind == "event" {
				if err := cmd.Process.Kill(); err != nil {
					t.Fatalf("killing the first attach process: %v", err)
				}
				_ = cmd.Wait()
				return seqs
			}
		case <-timeout:
			t.Fatalf("timed out after %s waiting for the first attach to see an event; seqs so far: %v", planTimeout, seqs)
		}
	}
}

// attachToCompletion attaches from fromSeq and reads until the process's own
// stdout ends (Attach returns once it streams a Result), returning every seq
// seen and the Result, if any.
func attachToCompletion(t *testing.T, acyBin, id string, fromSeq int64) ([]int64, *engineerwire.Result) {
	t.Helper()
	cmd, ch := startAttach(t, acyBin, id, fromSeq)

	var seqs []int64
	var result *engineerwire.Result
	timeout := time.After(engineerCompletionTimeout)
loop:
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				break loop
			}
			if m.err != nil {
				break loop
			}
			seq, _ := seqAndKind(m.msg)
			seqs = append(seqs, seq)
			if r, ok := m.msg.(engineerwire.Result); ok {
				result = &r
			}
		case <-timeout:
			t.Fatalf("timed out after %s waiting for the engineer to finish; seqs seen so far: %v",
				engineerCompletionTimeout, seqs)
		}
	}

	if err := cmd.Wait(); err != nil {
		t.Fatalf("acy engineer attach --from %d exited with error: %v", fromSeq, err)
	}
	if len(seqs) == 0 {
		t.Fatalf("second attach (--from %d) saw no messages at all", fromSeq)
	}
	return seqs, result
}

// seqAndKind extracts seq and a short type name from a decoded wire message.
func seqAndKind(msg any) (seq int64, kind string) {
	switch m := msg.(type) {
	case engineerwire.Hello:
		return m.Seq, "hello"
	case engineerwire.Event:
		return m.Seq, "event"
	case engineerwire.Question:
		return m.Seq, "question"
	case engineerwire.Result:
		return m.Seq, "result"
	default:
		return 0, fmt.Sprintf("%T", msg)
	}
}
