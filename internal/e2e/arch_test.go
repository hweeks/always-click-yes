package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/engineerd"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
	"github.com/hweeks/always-click-yes/internal/fleet"
	"github.com/hweeks/always-click-yes/internal/state"
	"github.com/hweeks/always-click-yes/internal/ui"
)

// Timeouts here are generous for the same reason autorun_test.go's are: every
// one of them waits on a real architect (and, for the completion wait, two
// real engineers underneath it) doing real work on a real subscription.
const (
	archPlanTimeout     = 5 * time.Minute
	archCompleteTimeout = 15 * time.Minute
)

// archPlanMessage hands the architect exactly two independent tickets and
// spells out the one behavior this test exists to prove: both engineers must
// be launched before either is awaited, so the fleet actually runs them in
// parallel rather than one at a time. ArchSystemPrompt already tells the
// model to "launch up to capacity, then Await" — capacity here is exactly 2,
// so the instruction and the fleet size agree — but the message restates it
// explicitly rather than trusting the system prompt alone to carry a live run.
const archPlanMessage = `We have exactly two independent engineering tickets to delegate to the fleet. ` +
	`You cannot write to this repo yourself — delegate both tickets, never attempt either one directly.

Ticket T1: create a file named GREETING.md at the repo root containing exactly the single line "hello from t1", and commit it.
Ticket T2: create a file named FAREWELL.md at the repo root containing exactly the single line "goodbye from t2", and commit it.

These two tickets are completely independent of one another: neither depends on the other's output and they touch different files. Plan for exactly these two tickets and nothing else — keep the plan short.

When you are armed: launch BOTH engineers (T1 and T2) before you Await either one. Do not call Await after only the first LaunchEngineer call returns — only after both LaunchEngineer calls have returned. Then Await repeatedly until both results have arrived, and call Finish once both engineers report outcome completed.`

// TestE2EArchRunsEngineersInParallel is arch mode's milestone proof: a real
// architect session plans two independent tickets, arms, and launches two
// real engineer instances that run concurrently rather than serially — each
// engineer pushes its own branch to a local bare origin and opens a PR
// through a stub `gh`, and the architect sees both finish before calling
// Finish itself.
//
// It reuses TestE2EEngineerDetachedRunSurvivesReattach's hermetic-git-env,
// scratch-bare-origin and short-state-dir patterns (the same AF_UNIX path
// length constraint applies here: every engineer's control.sock lives under
// $ACY_STATE_DIR/engineers/<id>/control.sock), and generalizes its stub `gh`
// to hand back a distinct PR URL per call so two concurrent `pr create`s can
// be told apart.
func TestE2EArchRunsEngineersInParallel(t *testing.T) {
	requireLive(t)

	acyBin, err := acyBinary()
	if err != nil {
		t.Fatalf("build acy: %v", err)
	}

	// Hermetic git: no ~/.gitconfig, no system config, a fixed commit
	// identity — see TestE2EEngineerDetachedRunSurvivesReattach's comment.
	// The env has to travel on the process env because the git commands are
	// issued by the real detached engineer processes, not by the test.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "acy-arch-e2e")
	t.Setenv("GIT_AUTHOR_EMAIL", "acy-arch-e2e@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "acy-arch-e2e")
	t.Setenv("GIT_COMMITTER_EMAIL", "acy-arch-e2e@example.com")

	stateDir := shortStateDir(t)
	t.Setenv(state.EnvDir, stateDir)

	clonePath, bareOrigin := scratchGitRepo(t)
	argvFile := stubGhCounting(t)

	fleetCfg := config.FleetConfig{
		BaseBranch:         "main",
		EngineerModel:      "sonnet",
		EngineerChildModel: "sonnet",
		EngineerBudgetUSD:  new(4.0),
		DeadmanHours:       new(1.0),
		Hosts: []config.FleetHost{{
			Name: "local",
			// RepoPath and ACYBin are normally defaulted by config.LoadFile's
			// resolve step; this test builds the FleetConfig by hand and
			// bypasses LoadFile entirely, so both have to be set explicitly.
			// Leaving ACYBin empty would make fleet.ForHost fall back to
			// os.Executable(), which under `go test` is the test binary — it
			// has no `engineer` subcommand, so every LaunchEngineer would
			// fail at Start.
			RepoPath:     clonePath,
			ACYBin:       acyBin,
			MaxEngineers: new(2),
		}},
	}
	manager := fleet.NewManager(fleetCfg, fleet.ForHost)

	// Last-resort cleanup, registered before manager.Close so it runs after
	// (t.Cleanup is LIFO): give the manager a chance to cancel any engineer
	// still running over the wire, then SIGKILL every engineer process group
	// still recorded under the scratch state dir, mirroring
	// TestE2EEngineerDetachedRunSurvivesReattach's own pgid cleanup but
	// generalized over however many engineer directories exist by then.
	//
	// manager.Close itself is bounded rather than plain t.Cleanup(manager.Close):
	// Close waits for every engineer's Follow loop to unwind, which bottoms out
	// in a local attach's cmd.Wait(); if that child never actually exits, Wait
	// never returns and this cleanup — registered to run before the SIGKILL
	// backstop above — would block it too, burning the whole -timeout budget
	// instead of reaching it. A run against attempt4.log's dump caught exactly
	// this: cmd.Wait() parked 18+ minutes inside fleet.runAttach.
	t.Cleanup(func() { killLeftoverEngineerProcesses(stateDir) })
	t.Cleanup(func() { closeManagerWithDeadline(t, manager, 20*time.Second) })

	h := newHarness(t, options{
		Cwd:        clonePath,
		Model:      "sonnet",
		ChildModel: "sonnet",
		ArchMode:   true,
		Fleet:      manager,
	})

	h.typeAndSend(archPlanMessage)
	h.waitFor("the architect's plan turn to end", archPlanTimeout, func(m ui.Model) bool {
		return m.SessionID() != "" && m.Status() == "idle"
	})

	if res := h.hub.Do(ui.Arm()); !res.Accepted {
		t.Fatalf("arming the architect was refused: %s", res.Reason)
	}
	h.waitFor("the architect to enter AUTO-RUN", 30*time.Second, func(m ui.Model) bool {
		return m.Phase() == ui.PhaseAutoRun
	})

	h.waitFor("the fleet run to reach COMPLETE", archCompleteTimeout, func(m ui.Model) bool {
		return m.Phase() == ui.PhaseComplete
	})

	var frame ui.Frame
	var finishOutcome, transcript string
	h.read(func(m ui.Model) {
		frame = m.Frame()
		finishOutcome = m.FinishOutcome()
		transcript = m.Transcript()
	})
	if finishOutcome != "completed" {
		t.Fatalf("architect finish outcome = %q, want completed\n--- transcript ---\n%s", finishOutcome, transcript)
	}

	byTicket := map[string]ui.Engineer{}
	for _, e := range frame.Engineers {
		byTicket[e.Ticket] = e
	}
	e1, ok1 := byTicket["T1"]
	e2, ok2 := byTicket["T2"]
	if !ok1 || !ok2 {
		t.Fatalf("expected engineers for tickets T1 and T2, got %+v", frame.Engineers)
	}
	for _, e := range []ui.Engineer{e1, e2} {
		if e.State != fleet.StateDone {
			t.Errorf("engineer %s (ticket %s) state = %q, want %q", e.ID, e.Ticket, e.State, fleet.StateDone)
		}
		if e.Outcome != "completed" {
			t.Errorf("engineer %s (ticket %s) outcome = %q, want completed", e.ID, e.Ticket, e.Outcome)
		}
		if e.PRURL == "" {
			t.Errorf("engineer %s (ticket %s) has no PR URL", e.ID, e.Ticket)
		}
	}
	if e1.PRURL != "" && e1.PRURL == e2.PRURL {
		t.Errorf("both engineers report the same PR URL %q, want distinct URLs", e1.PRURL)
	}
	t.Logf("PR URLs: T1=%s T2=%s", e1.PRURL, e2.PRURL)

	// --- on-disk proof: the bare origin has both branches, each with the
	// right file and content ---
	if !strings.HasPrefix(e1.Branch, "acy/t1-") {
		t.Errorf("T1's branch = %q, want an acy/t1-* branch", e1.Branch)
	}
	if !strings.HasPrefix(e2.Branch, "acy/t2-") {
		t.Errorf("T2's branch = %q, want an acy/t2-* branch", e2.Branch)
	}
	runGit(t, bareOrigin, "rev-parse", "--verify", "refs/heads/"+e1.Branch)
	runGit(t, bareOrigin, "rev-parse", "--verify", "refs/heads/"+e2.Branch)

	got1 := strings.TrimSpace(runGit(t, bareOrigin, "show", e1.Branch+":GREETING.md"))
	if got1 != "hello from t1" {
		t.Errorf("GREETING.md on %s = %q, want %q", e1.Branch, got1, "hello from t1")
	}
	got2 := strings.TrimSpace(runGit(t, bareOrigin, "show", e2.Branch+":FAREWELL.md"))
	if got2 != "goodbye from t2" {
		t.Errorf("FAREWELL.md on %s = %q, want %q", e2.Branch, got2, "goodbye from t2")
	}

	// --- the stub gh recorded exactly two `pr create` calls, distinct heads ---
	heads := prCreateHeads(t, argvFile)
	if len(heads) != 2 {
		t.Fatalf("expected exactly 2 `gh pr create` calls, got %d; argv file:\n%s", len(heads), mustReadFile(t, argvFile))
	}
	if heads[0] == heads[1] {
		t.Errorf("both gh pr create calls used the same --head branch %q", heads[0])
	}

	// --- parallelism: both engineers' hello timestamps precede either
	// engineer's result timestamp, proving they overlapped rather than ran
	// one after the other ---
	journalPaths, err := filepath.Glob(filepath.Join(stateDir, "engineers", "*", "journal.ndjson"))
	if err != nil || len(journalPaths) != 2 {
		t.Fatalf("expected 2 engineer journals under %s/engineers, got %v (err=%v)", stateDir, journalPaths, err)
	}
	var hellos, results []time.Time
	for _, jp := range journalPaths {
		hello, result := readJournal(t, jp)
		if hello == nil {
			t.Fatalf("journal %s has no hello message", jp)
		}
		if result == nil {
			t.Fatalf("journal %s has no result message", jp)
		}
		ht, err := time.Parse(time.RFC3339, hello.At)
		if err != nil {
			t.Fatalf("parsing hello.At %q in %s: %v", hello.At, jp, err)
		}
		rt, err := time.Parse(time.RFC3339, result.At)
		if err != nil {
			t.Fatalf("parsing result.At %q in %s: %v", result.At, jp, err)
		}
		hellos = append(hellos, ht)
		results = append(results, rt)
	}
	maxHello := hellos[0]
	if hellos[1].After(maxHello) {
		maxHello = hellos[1]
	}
	minResult := results[0]
	if results[1].Before(minResult) {
		minResult = results[1]
	}
	if !maxHello.Before(minResult) {
		t.Errorf("engineers do not appear to have overlapped: hello times %v, result times %v (want both hellos before either result)",
			hellos, results)
	}

	// --- no engineer process (and, underneath it, no claude session) survives
	// the test ---
	waitForEngineersToExit(t, stateDir)
}

// stubGhCounting puts a fake `gh` first on PATH, ahead of anything real: for
// `pr create` it hands back a distinct, incrementing PR URL per call
// (https://example.invalid/pr/1, /2, ...) rather than engineer_test.go's
// stubGh's single fixed URL, because this test runs two engineers concurrently
// and needs their PRs to be told apart. Every invocation's argv is appended to
// the returned file, one flattened line per call.
//
// The counter itself is a plain file guarded by an mkdir-based lock: mkdir is
// atomic on every POSIX filesystem this test runs on, and flock is not
// portable to a bare macOS shell, so this is the simplest thing that is
// actually safe against two `gh` invocations racing each other.
func stubGhCounting(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	argvFile := filepath.Join(t.TempDir(), "gh-argv.txt")
	counterFile := filepath.Join(t.TempDir(), "gh-pr-counter")
	if err := os.WriteFile(counterFile, []byte("0"), 0o644); err != nil {
		t.Fatalf("seeding gh pr counter: %v", err)
	}

	script := "#!/bin/sh\n" +
		"line=$(printf '%s' \"$*\" | tr '\\n' ' ')\n" +
		"printf '%s\\n' \"$line\" >> \"$ACY_E2E_GH_ARGV\"\n" +
		"if [ \"$1\" = pr ] && [ \"$2\" = create ]; then\n" +
		"  lock=\"$ACY_E2E_GH_COUNTER.lock\"\n" +
		"  while ! mkdir \"$lock\" 2>/dev/null; do sleep 0.05; done\n" +
		"  n=$(cat \"$ACY_E2E_GH_COUNTER\")\n" +
		"  n=$((n + 1))\n" +
		"  echo \"$n\" > \"$ACY_E2E_GH_COUNTER\"\n" +
		"  rmdir \"$lock\"\n" +
		"  echo \"https://example.invalid/pr/$n\"\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(script), 0o755); err != nil { //nolint:gosec // a stub script, meant to be executable
		t.Fatalf("writing stub gh: %v", err)
	}

	t.Setenv("ACY_E2E_GH_ARGV", argvFile)
	t.Setenv("ACY_E2E_GH_COUNTER", counterFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvFile
}

// prCreateHeads extracts the --head value from every `pr create` line the
// stub gh recorded.
func prCreateHeads(t *testing.T, argvFile string) []string {
	t.Helper()
	argv := mustReadFile(t, argvFile)
	var heads []string
	for line := range strings.SplitSeq(strings.TrimSpace(argv), "\n") {
		if !strings.HasPrefix(line, "pr create") {
			continue
		}
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "--head" && i+1 < len(fields) {
				heads = append(heads, fields[i+1])
			}
		}
	}
	return heads
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test-controlled scratch path
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// readJournal decodes every message in an engineer's journal.ndjson and
// returns its Hello and Result, if present.
func readJournal(t *testing.T, path string) (hello *engineerwire.Hello, result *engineerwire.Result) {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // test-controlled scratch path
	if err != nil {
		t.Fatalf("opening journal %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	dec := engineerwire.NewDecoder(f)
	for {
		msg, decErr := dec.Decode()
		if decErr != nil {
			break
		}
		switch m := msg.(type) {
		case engineerwire.Hello:
			h := m
			hello = &h
		case engineerwire.Result:
			r := m
			result = &r
		}
	}
	return hello, result
}

// waitForEngineersToExit asserts that every engineer directory under
// stateDir's own pid file disappears (RunDetachedTarget removes it right
// after core.Run returns) and its process actually stops, within
// pidCleanupTimeout — the same deadline and reasoning
// TestE2EEngineerDetachedRunSurvivesReattach uses for its one engineer,
// generalized over however many this test finds.
func waitForEngineersToExit(t *testing.T, stateDir string) {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(stateDir, "engineers", "*"))
	if err != nil {
		t.Fatalf("globbing engineer dirs under %s: %v", stateDir, err)
	}
	if len(dirs) == 0 {
		t.Fatalf("no engineer directories found under %s", stateDir)
	}
	for _, dir := range dirs {
		pidPath := filepath.Join(dir, engineerd.PIDFile)
		deadline := time.Now().Add(pidCleanupTimeout)
		for {
			b, statErr := os.ReadFile(pidPath) //nolint:gosec // test-controlled scratch path
			if os.IsNotExist(statErr) {
				break
			}
			if statErr == nil {
				if pid, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil && pid > 0 {
					if syscall.Kill(pid, 0) != nil {
						break // pid file lingering, but the process is already gone
					}
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("engineer at %s did not clean up its pid file within %s", dir, pidCleanupTimeout)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// closeManagerWithDeadline runs manager.Close in the background and waits up
// to deadline for it to finish, instead of blocking cleanup on it forever.
// Close cancels every engineer and waits for their Follow loops to unwind —
// ordinarily fast, but a wedged local attach can leave Close's wg.Wait()
// parked on a cmd.Wait() that never returns. If manager.Close itself never
// exits, killLeftoverEngineerProcesses (t.Cleanup runs LIFO, so it fires
// right after this one returns) still gets to SIGKILL the process groups,
// which frees the abandoned goroutine to finish on its own after the test.
func closeManagerWithDeadline(t *testing.T, manager *fleet.Manager, deadline time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		manager.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(deadline):
		t.Logf("manager.Close did not return within %s; leaving it running in the background and proceeding with cleanup", deadline)
	}
}

// killLeftoverEngineerProcesses is the hard backstop: for every engineer
// directory still recording a pid, SIGKILL its process group (engineers run
// under setsid, so pid == pgid — internal/cli/engineer_detach_unix.go). Errors
// are ignored throughout: a missing directory, a stale pid file, or a process
// that already exited are all the expected common case once the graceful
// paths above have run.
func killLeftoverEngineerProcesses(stateDir string) {
	pidFiles, err := filepath.Glob(filepath.Join(stateDir, "engineers", "*", engineerd.PIDFile))
	if err != nil {
		return
	}
	for _, pidPath := range pidFiles {
		b, err := os.ReadFile(pidPath) //nolint:gosec // test-controlled scratch path
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid <= 0 {
			continue
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}
