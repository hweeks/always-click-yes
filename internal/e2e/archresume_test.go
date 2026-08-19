package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
	"github.com/hweeks/always-click-yes/internal/fleet"
	"github.com/hweeks/always-click-yes/internal/gitops"
	"github.com/hweeks/always-click-yes/internal/state"
	"github.com/hweeks/always-click-yes/internal/tickets"
	"github.com/hweeks/always-click-yes/internal/ui"
)

// archResumeJournalTimeout bounds how long we wait for the detached
// engineer's own journal to show it is alive (a Hello at seq 1) — fast,
// because unlike the other arch tests this one deliberately does not wait
// for the engineer to finish before moving on.
const archResumeJournalTimeout = 2 * time.Minute

// archResumePlanMessage hands the architect exactly one ticket and the same
// launch/board/Await/Finish sequence ticketLoopPlanMessage proves for two —
// trimmed to one ticket because this test's subject is not the merge loop,
// it is what happens to the *other* ticket's engineer when the architect
// supervising it dies mid-flight.
const archResumePlanMessage = `We have exactly one engineering ticket to delegate to a fleet. ` +
	`You cannot write to this repo yourself — delegate the ticket to an engineer, never attempt it directly.

Ticket t1: create a file named GREETING.md at the repo root containing exactly the single line "hello from t1", and commit it.

Plan for exactly this one ticket and nothing else — keep the plan short.

When you are armed, follow this sequence exactly:
1. Call CreateTicket for the ticket. Its id must be exactly "t1" (lowercase).
2. Launch the engineer for ticket t1. The moment you launch it, call UpdateTicket to set t1's status to in-progress.
3. Call Await in a loop and react to every event it hands back:
   - The moment you learn a PR was opened for t1 (its engineer's result carries a pr_url, or a PR event names its branch), call UpdateTicket to set t1's status to in-review.
   - The moment Await reports that the PR has merged, call UpdateTicket to set t1's status to merged.
4. Call Finish, with outcome completed, only once t1 shows status merged on the board — not before. Keep calling Await until that is true; do not finish early and do not give up.`

// TestE2EArchResumeRecoversEngineer is the durability proof arch mode's
// design promises but arch_test.go and ticketloop_test.go never exercise:
// both of those keep the same architect process alive end to end, so
// neither one can tell whether a *dead* architect's engineer is actually
// recoverable.
//
// Phase 1 plans and arms a real architect, which launches one real detached
// engineer against a scratch repo. The moment the engineer's journal proves
// it is alive, phase 1's supervisor is abandoned exactly the way a crash
// would abandon it: its context is cancelled (which — via
// exec.CommandContext — really does kill the architect's claude process
// tree) and nothing else is touched. In particular, fleet.Manager.Close and
// CancelAll are never called: Manager's own baseCtx is rooted in
// context.Background(), independent of the supervisor's context (see
// fleet.NewManager), so cancelling the supervisor cannot reach the
// engineer's wire connection even by accident. That independence is what is
// actually being proven here — a real crash sends no cancel to the
// engineer, and this test would fail if some future refactor accidentally
// wired the two contexts together.
//
// While the architect is dead, the real detached engineer keeps working on
// its own, writes its Result to its journal, pushes its branch to the bare
// origin, and opens a PR through the stub gh — all with nobody watching.
//
// Phase 2 builds a brand new supervisor (new gate, new hook settings, new
// claude process) that resumes the dead architect's session id, with a
// fresh fleet.Manager, a fresh PRWatcher and a fresh ticket store — mirroring
// how `acy arch --continue` assembles a resumed run, not a test-only
// shortcut. The resumed run must come back armed, get the one resume prompt
// naming the re-attached engineer, learn the engineer's Result purely from
// replaying its journal (the architect that would have received it live is
// gone), update the board, and finish once the human (this test) merges the
// PR — all without ever launching a second engineer.
func TestE2EArchResumeRecoversEngineer(t *testing.T) {
	requireLive(t, "claude")

	acyBin, err := acyBinary()
	if err != nil {
		t.Fatalf("build acy: %v", err)
	}

	// Hermetic git: no ~/.gitconfig, no system config, a fixed commit
	// identity — see TestE2EEngineerDetachedRunSurvivesReattach's comment.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "acy-arch-e2e")
	t.Setenv("GIT_AUTHOR_EMAIL", "acy-arch-e2e@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "acy-arch-e2e")
	t.Setenv("GIT_COMMITTER_EMAIL", "acy-arch-e2e@example.com")

	// One state dir for the whole test: the engineer directory it holds is
	// the same real detached process across both phases — that persistence
	// is the entire point of a resume.
	stateDir := shortStateDir(t)
	t.Setenv(state.EnvDir, stateDir)

	clonePath, bareOrigin := scratchGitRepo(t)
	argvFile, ghStateFile := stubGhStateful(t)

	fleetCfg := config.FleetConfig{
		BaseBranch:         "main",
		EngineerModel:      "sonnet",
		EngineerChildModel: "sonnet",
		EngineerBudgetUSD:  new(3.0),
		DeadmanHours:       new(1.0),
		Hosts: []config.FleetHost{{
			Name:         "local",
			RepoPath:     clonePath,
			ACYBin:       acyBin,
			MaxEngineers: new(1),
		}},
	}

	// Registered first so it is the last cleanup to run (t.Cleanup is LIFO):
	// the hard backstop that SIGKILLs any engineer process group still
	// recorded under stateDir, regardless of what either phase's manager
	// managed to do gracefully.
	t.Cleanup(func() { killLeftoverEngineerProcesses(stateDir) })

	// --- phase 1: the doomed architect ---

	ctx1, cancel1 := context.WithCancel(context.Background())
	// Idempotent: the crash step below calls this explicitly, and this
	// t.Cleanup is only the backstop if the test fails before reaching it.
	t.Cleanup(cancel1)

	prWatcherCtx1, cancelPRWatcher1 := context.WithCancel(context.Background())
	t.Cleanup(cancelPRWatcher1)
	watcher1 := fleet.NewPRWatcher(clonePath, gitops.DefaultRunner, 2*time.Second, nil)
	go watcher1.Run(prWatcherCtx1)

	manager1 := fleet.NewManager(fleetCfg, fleet.ForHost, fleet.WithPRWatcher(watcher1, 0))
	// See killLeftoverEngineerProcesses above for why this alone is not
	// trusted to actually finish: manager1.Close cancels the engineer over
	// the wire and waits for its Follow loop to unwind, which is exactly
	// the graceful path this test's crash is supposed to skip having
	// happened *during* the run — but by the time t.Cleanup unwinds, both
	// phases are long done, so Close here is just ordinary teardown.
	t.Cleanup(func() { closeManagerWithDeadline(t, manager1, 20*time.Second) })

	ticketStore1 := tickets.New(clonePath, "direct", gitops.DefaultRunner)

	h1 := newHarness(t, options{
		Cwd:        clonePath,
		Ctx:        ctx1,
		Model:      "sonnet",
		ChildModel: "sonnet",
		ArchMode:   true,
		Fleet:      manager1,
		Tickets:    ticketStore1,
	})

	h1.typeAndSend(archResumePlanMessage)
	h1.waitFor("the architect's plan turn to end", archPlanTimeout, func(m ui.Model) bool {
		return m.SessionID() != "" && m.Status() == "idle"
	})

	var sessionID string
	h1.read(func(m ui.Model) { sessionID = m.SessionID() })

	if res := h1.hub.Do(ui.Arm()); !res.Accepted {
		t.Fatalf("arming the architect was refused: %s", res.Reason)
	}
	h1.waitFor("the architect to enter AUTO-RUN", 30*time.Second, func(m ui.Model) bool {
		return m.Phase() == ui.PhaseAutoRun
	})

	// Wait ONLY until the engineer daemon is up and working — its journal
	// exists and already holds its Hello (seq 1) — never until it finishes.
	// The whole point of what follows is that the architect dies with the
	// engineer still mid-flight.
	journalPath := waitForSingleEngineerJournal(t, stateDir, archResumeJournalTimeout)
	t.Logf("engineer journal alive at %s", journalPath)

	// --- simulate a crash ---
	//
	// A real crash sends no goodbyes. Cancelling ctx1 does take down the
	// architect's actual claude process (driver.Driver.Start uses
	// exec.CommandContext), which is the part of "the architect died" that
	// matters here — but it must NOT reach the engineer. fleet.Manager's
	// baseCtx is rooted in context.Background() (see fleet.NewManager),
	// independent of ctx1, and neither manager1.Close nor manager1.CancelAll
	// is called on this path — so if the engineer stops working after this
	// point, that is a durability bug, not an artifact of a well-behaved
	// shutdown this test is supposed to be skipping.
	cancel1()

	// --- between phases: the engineer finishes with nobody watching ---

	result := waitForJournalResult(t, journalPath, archCompleteTimeout)
	if result.Outcome != "completed" {
		t.Fatalf("engineer result outcome = %q, want completed (summary: %q)", result.Outcome, result.Summary)
	}
	if result.Branch == "" {
		t.Fatalf("engineer result carries no branch")
	}
	if !strings.HasPrefix(result.Branch, "acy/t1-") {
		t.Errorf("engineer branch = %q, want an acy/t1-* branch", result.Branch)
	}

	runGit(t, bareOrigin, "rev-parse", "--verify", "refs/heads/"+result.Branch)
	got := strings.TrimSpace(runGit(t, bareOrigin, "show", result.Branch+":GREETING.md"))
	if got != "hello from t1" {
		t.Errorf("GREETING.md on %s (bare origin) = %q, want %q", result.Branch, got, "hello from t1")
	}

	heads := prCreateHeads(t, argvFile)
	if len(heads) != 1 {
		t.Fatalf("expected exactly 1 `gh pr create` call while the architect was dead, got %d; argv file:\n%s",
			len(heads), mustReadFile(t, argvFile))
	}
	if heads[0] != result.Branch {
		t.Errorf("gh pr create --head = %q, want the engineer's own branch %q", heads[0], result.Branch)
	}

	// The human merges the one PR that exists.
	ghState := readGhState(t, ghStateFile)
	if len(ghState) != 1 {
		t.Fatalf("expected exactly 1 PR in the stub gh's state, got %d: %+v", len(ghState), ghState)
	}
	flipPRToMerged(t, ghStateFile, ghState[0].Number)

	// --- phase 2: the resurrection ---

	prWatcherCtx2, cancelPRWatcher2 := context.WithCancel(context.Background())
	t.Cleanup(cancelPRWatcher2)
	watcher2 := fleet.NewPRWatcher(clonePath, gitops.DefaultRunner, 2*time.Second, nil)
	go watcher2.Run(prWatcherCtx2)

	manager2 := fleet.NewManager(fleetCfg, fleet.ForHost, fleet.WithPRWatcher(watcher2, 0))
	t.Cleanup(func() { closeManagerWithDeadline(t, manager2, 20*time.Second) })

	// A fresh Store pointed at the same clonePath: the board itself lives on
	// disk (git-committed by the dead architect), not in this process, so a
	// new Store object reads exactly what phase 1 left behind — mirroring
	// how a fresh `acy arch --continue` process would open it.
	ticketStore2 := tickets.New(clonePath, "direct", gitops.DefaultRunner)

	h2 := newHarness(t, options{
		Cwd:        clonePath,
		Resume:     sessionID,
		Model:      "sonnet",
		ChildModel: "sonnet",
		ArchMode:   true,
		Fleet:      manager2,
		Tickets:    ticketStore2,
	})

	// The restored run must come back ARMED — existing resume semantics
	// (applyResume reads the persisted "AUTO-RUN" phase back) — and get
	// exactly one resume prompt naming the re-attached engineer.
	h2.waitFor("the resumed run to come back armed and name the re-attached engineer", 90*time.Second, func(m ui.Model) bool {
		return m.Phase() == ui.PhaseAutoRun && strings.Contains(m.Transcript(), "e1")
	})
	var transcriptAfterResume string
	h2.read(func(m ui.Model) { transcriptAfterResume = m.Transcript() })
	if !strings.Contains(transcriptAfterResume, "re-attached") {
		t.Errorf("resume transcript does not mention the re-attached engineer:\n%s", transcriptAfterResume)
	}
	// resumePrompt's opening line is unique to the one prompt a restored
	// auto-run is sent (ui.Model.resumePrompt) — kickoffPromptText's own
	// count-of-1 check in resume_test.go is the same reasoning: a second
	// copy would mean the resumed run was kicked off twice.
	const resumePromptText = "This run was interrupted and has been restored."
	if n := strings.Count(transcriptAfterResume, resumePromptText); n != 1 {
		t.Errorf("the resume prompt appears %d times, want exactly 1:\n%s", n, transcriptAfterResume)
	}

	h2.waitFor("the fleet run to reach COMPLETE with t1 merged", archCompleteTimeout, func(m ui.Model) bool {
		return m.Phase() == ui.PhaseComplete
	})

	var frame ui.Frame
	var finishOutcome, transcript string
	h2.read(func(m ui.Model) {
		frame = m.Frame()
		finishOutcome = m.FinishOutcome()
		transcript = m.Transcript()
	})
	if finishOutcome != "completed" {
		t.Fatalf("resumed architect finish outcome = %q, want completed\n--- transcript ---\n%s", finishOutcome, transcript)
	}

	// --- exactly one engineer and one PR, ever ---
	if len(frame.Engineers) != 1 {
		t.Fatalf("expected exactly 1 engineer on the resumed frame, got %d: %+v", len(frame.Engineers), frame.Engineers)
	}
	e := frame.Engineers[0]
	if e.Ticket != "t1" {
		t.Errorf("engineer ticket = %q, want t1", e.Ticket)
	}
	if e.State != fleet.StateDone {
		t.Errorf("engineer state = %q, want %q", e.State, fleet.StateDone)
	}
	if e.Outcome != "completed" {
		t.Errorf("engineer outcome = %q, want completed", e.Outcome)
	}
	if e.PRURL == "" {
		t.Error("resumed frame's engineer has no PR URL")
	}

	journalPaths, err := filepath.Glob(filepath.Join(stateDir, "engineers", "*", "journal.ndjson"))
	if err != nil {
		t.Fatalf("globbing engineer journals: %v", err)
	}
	if len(journalPaths) != 1 {
		t.Fatalf("expected exactly 1 engineer journal directory ever created, got %d: %v", len(journalPaths), journalPaths)
	}
	if len(prCreateHeads(t, argvFile)) != 1 {
		t.Fatalf("expected exactly 1 `gh pr create` call total (no second engineer launched), got %d",
			len(prCreateHeads(t, argvFile)))
	}

	// --- the board itself agrees ---
	if got := ticketStatus(t, clonePath, "t1"); got != tickets.StatusMerged {
		t.Errorf("ticket t1 status = %q, want %q", got, tickets.StatusMerged)
	}

	// --- no leaked processes ---
	waitForEngineersToExit(t, stateDir)
}

// waitForSingleEngineerJournal polls until exactly one engineer journal
// exists under stateDir and its Hello (seq 1) has actually been written —
// "the daemon is up and working", not merely that its directory was
// created.
func waitForSingleEngineerJournal(t *testing.T, stateDir string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		paths, err := filepath.Glob(filepath.Join(stateDir, "engineers", "*", "journal.ndjson"))
		if err == nil && len(paths) == 1 {
			if hello, _, decErr := tryDecodeJournal(paths[0]); decErr == nil && hello != nil {
				return paths[0]
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for a single engineer journal with a Hello under %s", timeout, stateDir)
	return ""
}

// waitForJournalResult polls path directly — bypassing any live architect
// entirely, which is exactly the missed-Result recovery path this test
// exists to prove — until it holds a Result message.
func waitForJournalResult(t *testing.T, path string, timeout time.Duration) *engineerwire.Result {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, result, err := tryDecodeJournal(path)
		if err == nil && result != nil {
			return result
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for a Result in %s", timeout, path)
	return nil
}

// tryDecodeJournal decodes every message in path's journal so far, without
// failing the test if the file does not exist yet — unlike readJournal
// (engineer_test.go), which is only ever called once a journal is already
// known to exist.
func tryDecodeJournal(path string) (hello *engineerwire.Hello, result *engineerwire.Result, err error) {
	f, err := os.Open(path) //nolint:gosec // test-controlled scratch path
	if err != nil {
		return nil, nil, err
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
	return hello, result, nil
}
