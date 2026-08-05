package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/fleet"
	"github.com/hweeks/always-click-yes/internal/gitops"
	"github.com/hweeks/always-click-yes/internal/state"
	"github.com/hweeks/always-click-yes/internal/tickets"
	"github.com/hweeks/always-click-yes/internal/ui"
)

// Timeouts here mirror arch_test.go's reasoning: every one of them waits on a
// real architect and, underneath it, a real engineer planning, being armed,
// working and finishing — all for real, on a real subscription. The two
// stage waits are named after what they are blocking on so a timeout dump
// says where the run actually died, rather than just "it didn't finish".
const (
	ticketLoopStageTimeout    = 10 * time.Minute
	ticketLoopCompleteTimeout = 20 * time.Minute
)

// ticketLoopPlanMessage hands the architect exactly two independent tickets
// under a PR cap of one, and spells out the transitions this test exists to
// prove: the board stays current at every step, and the cap — not fleet
// capacity — is what makes the architect wait before launching the second
// engineer. Ticket ids are lowercase ("t1"/"t2") because tickets.Store's id
// pattern is [a-z0-9-]+; CreateTicket refuses anything else. Which ticket
// launches first is pinned explicitly (t1, then t2) so the test can assert
// on PR #1 belonging to t1 and PR #2 to t2 without guessing at the model's
// choice.
const ticketLoopPlanMessage = `We have exactly two independent engineering tickets to delegate to a fleet. ` +
	`This run's PR cap is 1 — only one acy pull request may be open at a time. You cannot write to this repo ` +
	`yourself — delegate both tickets to engineers, never attempt either one directly.

Ticket t1: create a file named GREETING.md at the repo root containing exactly the single line "hello from t1", and commit it.
Ticket t2: create a file named FAREWELL.md at the repo root containing exactly the single line "goodbye from t2", and commit it.

These two tickets are independent of one another. Plan for exactly these two tickets and nothing else — keep the plan short.

When you are armed, follow this sequence exactly:
1. Call CreateTicket for BOTH tickets first, before launching anything. Their ids must be exactly "t1" and "t2" (lowercase).
2. Launch ONE engineer, for ticket t1 first. The moment you launch it, call UpdateTicket to set t1's status to in-progress.
3. Call Await in a loop and react to every event it hands back:
   - The moment you learn a PR was opened for t1 (its engineer's result carries a pr_url, or a PR event names its branch), call UpdateTicket to set t1's status to in-review.
   - The PR cap is 1: do NOT call LaunchEngineer for t2 until t1's PR has merged. If you try earlier, LaunchEngineer will refuse — that refusal is expected and means "Await again", not a failure worth reporting.
   - The moment Await reports that a PR has merged, call UpdateTicket to set the matching ticket's status to merged.
4. Only once t1 shows merged on the board, launch the engineer for t2, and repeat the exact same transitions for it: in-progress on launch, in-review the moment its PR opens, merged the moment its PR merges.
5. Call Finish, with outcome completed, only once BOTH t1 and t2 show status merged on the board — not before. Keep calling Await until that is true; do not finish early and do not give up.`

// TestE2EArchMergeLoopUnderPRCap is milestone 4's live proof: a real
// architect plans two independent tickets, creates both on the board, and
// then runs the merge-driven loop a PR cap of 1 is meant to enforce — launch
// one engineer, keep the board current through in-progress and in-review,
// Await the human's merge (simulated here by editing the stub gh's state),
// and only then launch the second. Fleet capacity is deliberately 2 (never
// the limiter); the cap is.
//
// It reuses TestE2EArchRunsEngineersInParallel's hermetic-git-env,
// scratch-bare-origin, stub-gh-argv-counting and engineer-journal-glob
// patterns, generalizing the stub `gh` one step further: `pr list` now
// answers from a small state file this test edits directly to simulate a
// human merging a PR, rather than a fixed canned reply.
func TestE2EArchMergeLoopUnderPRCap(t *testing.T) {
	requireLive(t)

	acyBin, err := acyBinary()
	if err != nil {
		t.Fatalf("build acy: %v", err)
	}

	// Hermetic git: no ~/.gitconfig, no system config, a fixed commit
	// identity — see TestE2EEngineerDetachedRunSurvivesReattach's comment.
	// The env has to travel on the process env because the git commands are
	// issued by real detached engineer processes, not by the test.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "acy-arch-e2e")
	t.Setenv("GIT_AUTHOR_EMAIL", "acy-arch-e2e@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "acy-arch-e2e")
	t.Setenv("GIT_COMMITTER_EMAIL", "acy-arch-e2e@example.com")

	stateDir := shortStateDir(t)
	t.Setenv(state.EnvDir, stateDir)

	clonePath, bareOrigin := scratchGitRepo(t)
	argvFile, ghStateFile := stubGhStateful(t)

	fleetCfg := config.FleetConfig{
		BaseBranch:         "main",
		EngineerModel:      "sonnet",
		EngineerChildModel: "sonnet",
		EngineerBudgetUSD:  new(4.0),
		DeadmanHours:       new(1.0),
		Hosts: []config.FleetHost{{
			Name: "local",
			// Capacity is 2 so it can never be the reason a second launch
			// waits — the PR cap below is the only thing this test wants
			// gating LaunchEngineer.
			RepoPath:     clonePath,
			ACYBin:       acyBin,
			MaxEngineers: new(2),
		}},
	}

	prWatcherCtx, cancelPRWatcher := context.WithCancel(context.Background())
	t.Cleanup(cancelPRWatcher)
	watcher := fleet.NewPRWatcher(clonePath, gitops.DefaultRunner, 2*time.Second, nil)
	go watcher.Run(prWatcherCtx)

	manager := fleet.NewManager(fleetCfg, fleet.ForHost, fleet.WithPRWatcher(watcher, 1))

	// Last-resort cleanup, registered before manager.Close so it runs after
	// (t.Cleanup is LIFO): give the manager a chance to cancel any engineer
	// still running over the wire, then SIGKILL every engineer process group
	// still recorded under the scratch state dir. See
	// TestE2EArchRunsEngineersInParallel's identical comment for why
	// manager.Close itself is bounded rather than a plain t.Cleanup(manager.Close).
	t.Cleanup(func() { killLeftoverEngineerProcesses(stateDir) })
	t.Cleanup(func() { closeManagerWithDeadline(t, manager, 20*time.Second) })

	ticketStore := tickets.New(clonePath, "direct", gitops.DefaultRunner)

	h := newHarness(t, options{
		Cwd:        clonePath,
		Model:      "sonnet",
		ChildModel: "sonnet",
		ArchMode:   true,
		Fleet:      manager,
		Tickets:    ticketStore,
	})

	h.typeAndSend(ticketLoopPlanMessage)
	h.waitFor("the architect's plan turn to end", archPlanTimeout, func(m ui.Model) bool {
		return m.SessionID() != "" && m.Status() == "idle"
	})

	if res := h.hub.Do(ui.Arm()); !res.Accepted {
		t.Fatalf("arming the architect was refused: %s", res.Reason)
	}
	h.waitFor("the architect to enter AUTO-RUN", 30*time.Second, func(m ui.Model) bool {
		return m.Phase() == ui.PhaseAutoRun
	})

	// --- stage 1: PR #1 opens for t1, and the board says so ---
	waitForDisk(t, "PR #1 to exist and ticket t1 to reach in-review", ticketLoopStageTimeout,
		func() bool {
			return len(readGhState(t, ghStateFile)) >= 1 && ticketStatus(t, clonePath, "t1") == tickets.StatusInReview
		},
		func() string { return ticketLoopDiag(t, h, clonePath, ghStateFile) })

	// The human merges PR #1. flipTime1 is the wall-clock instant the cap
	// should have been holding the second engineer back until — the
	// ordering assertion below proves the launch it eventually makes
	// happens strictly after this, not merely after PR #1 existed.
	flipTime1 := time.Now()
	flipPRToMerged(t, ghStateFile, 1)

	// --- stage 2: t1 is confirmed merged, PR #2 opens for t2 ---
	waitForDisk(t, "PR #2 to exist and ticket t2 to reach in-review", ticketLoopStageTimeout,
		func() bool {
			return len(readGhState(t, ghStateFile)) >= 2 && ticketStatus(t, clonePath, "t2") == tickets.StatusInReview
		},
		func() string { return ticketLoopDiag(t, h, clonePath, ghStateFile) })

	flipPRToMerged(t, ghStateFile, 2)

	h.waitFor("the fleet run to reach COMPLETE with both tickets merged", ticketLoopCompleteTimeout, func(m ui.Model) bool {
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

	// --- both tickets end merged on disk ---
	if got := ticketStatus(t, clonePath, "t1"); got != tickets.StatusMerged {
		t.Errorf("ticket t1 status = %q, want %q", got, tickets.StatusMerged)
	}
	if got := ticketStatus(t, clonePath, "t2"); got != tickets.StatusMerged {
		t.Errorf("ticket t2 status = %q, want %q", got, tickets.StatusMerged)
	}

	// --- ticket files were committed in the scratch clone's git log ---
	log := runGit(t, clonePath, "log", "--oneline")
	if !ticketCommitPattern.MatchString(log) {
		t.Errorf("no \"ticket <id>: ...\" commit found in git log:\n%s", log)
	}

	// --- on-disk proof: the bare origin has both branches, each with the
	// right file and content ---
	byTicket := map[string]ui.Engineer{}
	for _, e := range frame.Engineers {
		byTicket[e.Ticket] = e
	}
	e1, ok1 := byTicket["t1"]
	e2, ok2 := byTicket["t2"]
	if !ok1 || !ok2 {
		t.Fatalf("expected engineers for tickets t1 and t2, got %+v", frame.Engineers)
	}
	if !strings.HasPrefix(e1.Branch, "acy/t1-") {
		t.Errorf("t1's branch = %q, want an acy/t1-* branch", e1.Branch)
	}
	if !strings.HasPrefix(e2.Branch, "acy/t2-") {
		t.Errorf("t2's branch = %q, want an acy/t2-* branch", e2.Branch)
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

	// --- ordering: the cap held. The second engineer's hello timestamp
	// must be after the wall-clock instant the test merged PR #1 — proving
	// LaunchEngineer for t2 could not have succeeded before that merge. ---
	journalPaths, err := filepath.Glob(filepath.Join(stateDir, "engineers", "*", "journal.ndjson"))
	if err != nil || len(journalPaths) != 2 {
		t.Fatalf("expected 2 engineer journals under %s/engineers, got %v (err=%v)", stateDir, journalPaths, err)
	}
	var hellos []time.Time
	for _, jp := range journalPaths {
		hello, _ := readJournal(t, jp)
		if hello == nil {
			t.Fatalf("journal %s has no hello message", jp)
		}
		ht, err := time.Parse(time.RFC3339, hello.At)
		if err != nil {
			t.Fatalf("parsing hello.At %q in %s: %v", hello.At, jp, err)
		}
		hellos = append(hellos, ht)
	}
	secondHello := hellos[0]
	if hellos[1].After(secondHello) {
		secondHello = hellos[1]
	}
	if !secondHello.After(flipTime1) {
		t.Errorf("the second engineer's hello (%v) is not after PR #1's merge (%v) — the PR cap did not hold",
			secondHello, flipTime1)
	}

	// --- no engineer process (and, underneath it, no claude session) survives
	// the test ---
	waitForEngineersToExit(t, stateDir)
}

// ticketCommitPattern matches tickets.Store.Commit's own message shape
// ("ticket <id>: <status-or-created>"), proving the board's transitions were
// actually committed rather than only held in memory.
var ticketCommitPattern = regexp.MustCompile(`(?m)^[0-9a-f]+ ticket \S+: `)

// ticketStatus reads one ticket's current status directly off disk, in the
// same store the architect's UpdateTicket writes through. A missing ticket
// (not created yet) reads as "" rather than failing the poll it is almost
// always called from.
func ticketStatus(t *testing.T, clonePath, id string) string {
	t.Helper()
	st := tickets.New(clonePath, "none", gitops.DefaultRunner)
	tk, err := st.Get(id)
	if err != nil {
		return ""
	}
	return tk.Status
}

// ticketLoopDiag renders the board and the stub gh's state for a stage
// timeout's failure message — so a red run says what it was still waiting
// on rather than just that it waited.
func ticketLoopDiag(t *testing.T, h *harness, clonePath, ghStateFile string) string {
	t.Helper()
	st := tickets.New(clonePath, "none", gitops.DefaultRunner)
	ts, _ := st.List()
	var b strings.Builder
	b.WriteString("tickets:\n")
	for _, tk := range ts {
		b.WriteString("  " + tk.ID + ": " + tk.Status + "\n")
	}
	b.WriteString("gh state:\n")
	for _, e := range readGhState(t, ghStateFile) {
		b.WriteString("  " + mustMarshal(t, e) + "\n")
	}
	var transcript string
	h.read(func(m ui.Model) { transcript = m.Transcript() })
	b.WriteString("--- transcript ---\n" + transcript)
	return b.String()
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %+v: %v", v, err)
	}
	return string(b)
}

// waitForDisk polls cond until it holds, failing the test with diag's output
// if it never does within timeout. Unlike harness.waitFor, cond here reads
// on-disk state (the ticket board, the stub gh's state file) rather than the
// ui.Model, so it needs no harness lock.
func waitForDisk(t *testing.T, what string, timeout time.Duration, cond func() bool, diag func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s\n%s", timeout, what, diag())
}

// --- the stateful stub `gh` ---

// ghStateEntry is one PR as the stub gh's state file and prwatch.go's own
// ghPR both understand it: field names and case match prwatch.go's json
// tags exactly, since the watcher parses whatever `gh pr list` prints
// straight into that type.
type ghStateEntry struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	HeadRefName string `json:"headRefName"`
	State       string `json:"state"`
}

// stubGhStateful puts a fake `gh` first on PATH, ahead of anything real,
// backed by a small JSONL state file this test can edit directly to
// simulate a human merging a PR — generalizing stubGhCounting (arch_test.go)
// one step further: that stub only ever hands back a fixed reply for `pr
// list`; this one answers from state the test controls.
//
// `pr create` appends one line (a distinct, incrementing PR number, its
// --head branch, and state OPEN) and prints the PR's URL, matching real
// gh's behavior closely enough for gitops.CreatePR to read it back
// unmodified. `pr list` reprints every line as a JSON array, regardless of
// the --json field list it was asked for — the fields the watcher requests
// are exactly the fields this state already carries.
//
// The counter and the state file are both guarded by an mkdir-based lock,
// the same atomic-on-any-POSIX-filesystem primitive stubGhCounting uses,
// since the PR cap this test proves means at most one `pr create` is ever
// in flight at a time but the watcher's own polls run concurrently with it
// regardless.
func stubGhStateful(t *testing.T) (argvFile, stateFile string) {
	t.Helper()
	binDir := t.TempDir()
	argvFile = filepath.Join(t.TempDir(), "gh-argv.txt")
	stateFile = filepath.Join(t.TempDir(), "gh-state.jsonl")
	counterFile := filepath.Join(t.TempDir(), "gh-pr-counter")

	if err := os.WriteFile(stateFile, nil, 0o644); err != nil {
		t.Fatalf("seeding gh state file: %v", err)
	}
	if err := os.WriteFile(counterFile, []byte("0"), 0o644); err != nil {
		t.Fatalf("seeding gh pr counter: %v", err)
	}

	script := "#!/bin/sh\n" +
		"line=$(printf '%s' \"$*\" | tr '\\n' ' ')\n" +
		"printf '%s\\n' \"$line\" >> \"$ACY_E2E_GH_ARGV\"\n" +
		"\n" +
		"if [ \"$1\" = pr ] && [ \"$2\" = create ]; then\n" +
		"  shift 2\n" +
		"  head=\"\"\n" +
		"  while [ $# -gt 0 ]; do\n" +
		"    if [ \"$1\" = --head ]; then head=\"$2\"; shift 2; else shift; fi\n" +
		"  done\n" +
		"  lock=\"$ACY_E2E_GH_STATE.lock\"\n" +
		"  while ! mkdir \"$lock\" 2>/dev/null; do sleep 0.05; done\n" +
		"  n=$(cat \"$ACY_E2E_GH_COUNTER\")\n" +
		"  n=$((n + 1))\n" +
		"  echo \"$n\" > \"$ACY_E2E_GH_COUNTER\"\n" +
		"  printf '{\"number\":%d,\"url\":\"https://example.invalid/pr/%d\",\"headRefName\":\"%s\",\"state\":\"OPEN\"}\\n' " +
		"\"$n\" \"$n\" \"$head\" >> \"$ACY_E2E_GH_STATE\"\n" +
		"  rmdir \"$lock\"\n" +
		"  echo \"https://example.invalid/pr/$n\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"\n" +
		"if [ \"$1\" = pr ] && [ \"$2\" = list ]; then\n" +
		"  lock=\"$ACY_E2E_GH_STATE.lock\"\n" +
		"  while ! mkdir \"$lock\" 2>/dev/null; do sleep 0.05; done\n" +
		"  if [ ! -s \"$ACY_E2E_GH_STATE\" ]; then\n" +
		"    printf '[]'\n" +
		"  else\n" +
		"    awk 'BEGIN{printf \"[\"} {if (NR>1) printf \",\"; printf \"%s\", $0} END{printf \"]\"}' \"$ACY_E2E_GH_STATE\"\n" +
		"  fi\n" +
		"  rmdir \"$lock\"\n" +
		"  printf '\\n'\n" +
		"  exit 0\n" +
		"fi\n" +
		"\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(script), 0o755); err != nil { //nolint:gosec // a stub script, meant to be executable
		t.Fatalf("writing stub gh: %v", err)
	}

	t.Setenv("ACY_E2E_GH_ARGV", argvFile)
	t.Setenv("ACY_E2E_GH_STATE", stateFile)
	t.Setenv("ACY_E2E_GH_COUNTER", counterFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvFile, stateFile
}

// readGhState decodes the stub gh's state file, one ghStateEntry per line.
// A file that does not exist yet (no `pr create` has landed) reads as no
// entries rather than an error.
func readGhState(t *testing.T, path string) []ghStateEntry {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test-controlled scratch path
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading gh state %s: %v", path, err)
	}
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		return nil
	}
	var out []ghStateEntry
	for line := range strings.SplitSeq(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e ghStateEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("parsing gh state line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// flipPRToMerged is how this test simulates a human merging a PR: it
// rewrites the stub gh's state file with PR number's state changed to
// MERGED, leaving every other entry untouched. The rewrite is a temp-file-
// then-rename so a concurrent `gh pr list` poll (the watcher ticks every
// 2s) only ever sees the whole-old or whole-new file, never a torn one.
func flipPRToMerged(t *testing.T, path string, number int) {
	t.Helper()
	entries := readGhState(t, path)
	found := false
	for i := range entries {
		if entries[i].Number == number {
			entries[i].State = "MERGED"
			found = true
		}
	}
	if !found {
		t.Fatalf("flipPRToMerged: no PR #%d in state file %s", number, path)
	}

	var b strings.Builder
	for _, e := range entries {
		b.WriteString(mustMarshal(t, e))
		b.WriteByte('\n')
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("writing %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("renaming %s to %s: %v", tmp, path, err)
	}
}
