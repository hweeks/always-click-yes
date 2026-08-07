package e2e

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
	"github.com/hweeks/always-click-yes/internal/fleet"
	"github.com/hweeks/always-click-yes/internal/gitops"
)

// archStackPollInterval is both the PRWatcher's poll interval and the
// interval every local wait loop below sleeps for: nothing in this test
// waits on a real process or the network, so there is no reason for either
// to be slower than "fast enough to be reliable, not fast enough to spin".
const archStackPollInterval = 50 * time.Millisecond

// archStackWaitTimeout bounds every poll loop in this test. Generous relative
// to archStackPollInterval (dozens of polls) while still finishing well under
// a second for the whole test, since nothing here is actually slow.
const archStackWaitTimeout = 5 * time.Second

// TestE2EArchStackChainAgainstFakeGH proves the stacked-PR chain end to end
// against a real `gh`/`git` subprocess boundary, without ever spawning a real
// engineer or claude session: a fleet.Manager is wired up exactly as `acy
// arch` wires one (internal/cli/arch.go's runArch), against a fake
// fleet.Transport that never runs a process, but a *real* scratch git repo
// and a fake `gh` binary on PATH — so PRWatcher and StackKeeper's own
// gitops/git/gh subprocess calls run for real.
//
// It proves, all with real argv/JSON round-trips rather than an in-memory
// fleet.Transport fake: a StackOn launch's Spec.BaseBranch/StackTrunk come
// from the parent's branch and the fleet's trunk; its PR opens with
// --base <parent branch>; the chain registers as a stack bottom-to-top with
// the right trunk; a trunk merge triggers a `gh stack sync`; and a mid-stack
// merge does not.
func TestE2EArchStackChainAgainstFakeGH(t *testing.T) {
	// Hermetic git: no ~/.gitconfig, no system config, a fixed commit
	// identity — the same pattern internal/gitops/gitops_test.go's
	// hermeticRunner and TestE2EArchRunsEngineersInParallel use, needed here
	// because scratchGitRepo's initial commit runs on whatever machine CI
	// picks, and only some of them happen to have a global git identity.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "acy-arch-e2e")
	t.Setenv("GIT_AUTHOR_EMAIL", "acy-arch-e2e@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "acy-arch-e2e")
	t.Setenv("GIT_COMMITTER_EMAIL", "acy-arch-e2e@example.com")

	clonePath, _ := scratchGitRepo(t)
	argvFile, ghStateFile := stubGhStateful(t)

	ft := newFakeTransport()

	fleetCfg := config.FleetConfig{
		BaseBranch:        "main",
		StackMode:         "chain",
		EngineerBudgetUSD: new(2.0),
		DeadmanHours:      new(1.0),
		Hosts: []config.FleetHost{{
			Name:         "local",
			RepoPath:     clonePath,
			MaxEngineers: new(2),
		}},
	}

	prWatcherCtx, cancelPRWatcher := context.WithCancel(context.Background())
	t.Cleanup(cancelPRWatcher)
	watcher := fleet.NewPRWatcher(clonePath, gitops.DefaultRunner, archStackPollInterval, nil)
	// Bootstrap on the empty repo before anything is launched: a watcher's
	// very first poll only ever establishes a silent baseline (pollLocked's
	// bootstrap rule — PRs that already existed before the watcher started
	// are not "new"), never emitting a PREvent for whatever it finds. Without
	// this explicit bootstrap, that first poll races the real `gh pr create`
	// calls below, and on a slow enough run can land after both engineers'
	// PRs already exist — silently swallowing the very "open" transition the
	// stack-link trigger depends on. Bootstrapping here, before either PR
	// exists, guarantees both opens are always observed as real transitions
	// on a later poll (the same pattern manager_stack_test.go's own
	// "Bootstrap the watcher with no PRs yet" comment documents).
	if err := watcher.Refresh(context.Background()); err != nil {
		t.Fatalf("bootstrap PRWatcher poll: %v", err)
	}
	go watcher.Run(prWatcherCtx)

	keeper := fleet.NewStackKeeper(clonePath, gitops.DefaultRunner, fleetCfg.BaseBranch)

	manager := fleet.NewManager(fleetCfg, ft.forHost, fleet.WithPRWatcher(watcher, 0), fleet.WithStackKeeper(keeper))
	t.Cleanup(func() { closeManagerWithDeadline(t, manager, 20*time.Second) })

	rec := newArchStackEventRecorder()
	go rec.drain(manager)

	ctx := context.Background()

	// --- root engineer A: launched against trunk, no stacking ---
	stA, err := manager.Launch(ctx, fleet.LaunchReq{Ticket: "A"})
	if err != nil {
		t.Fatalf("Launch A: %v", err)
	}
	specA := ft.specFor("A")
	if specA.BaseBranch != "main" {
		t.Errorf("A's Spec.BaseBranch = %q, want %q", specA.BaseBranch, "main")
	}
	if specA.StackTrunk != "" {
		t.Errorf("A's Spec.StackTrunk = %q, want empty (not stacked)", specA.StackTrunk)
	}

	prURLA := completeEngineer(t, ft, clonePath, "A", specA)

	waitForCond(t, "A to reach done with a PR URL", func() bool {
		return engineerStatus(manager, "A").PRURL == prURLA
	}, func() string { return archStackDiag(t, argvFile, ghStateFile, rec) })

	// --- child engineer B: stacked on A ---
	stB, err := manager.Launch(ctx, fleet.LaunchReq{Ticket: "B", StackOn: "A"})
	if err != nil {
		t.Fatalf("Launch B (stacked on A): %v", err)
	}
	specB := ft.specFor("B")
	if specB.BaseBranch != stA.Branch {
		t.Errorf("B's Spec.BaseBranch = %q, want A's branch %q", specB.BaseBranch, stA.Branch)
	}
	if specB.StackTrunk != "main" {
		t.Errorf("B's Spec.StackTrunk = %q, want %q", specB.StackTrunk, "main")
	}
	if stB.StackBase != stA.Branch {
		t.Errorf("B's EngineerStatus.StackBase = %q, want A's branch %q", stB.StackBase, stA.Branch)
	}

	prURLB := completeEngineer(t, ft, clonePath, "B", specB)

	waitForCond(t, "B to reach done with a PR URL", func() bool {
		return engineerStatus(manager, "B").PRURL == prURLB
	}, func() string { return archStackDiag(t, argvFile, ghStateFile, rec) })

	// --- the chain registers as a stack, bottom-to-top, once B's PR (based
	// on A's branch) is observed open ---
	linkEv := waitForStackEvent(t, rec, "link", func() string { return archStackDiag(t, argvFile, ghStateFile, rec) })
	wantChain := []string{stA.Branch, stB.Branch}
	if len(linkEv.Branches) != len(wantChain) || linkEv.Branches[0] != wantChain[0] || linkEv.Branches[1] != wantChain[1] {
		t.Errorf("stack link event Branches = %v, want %v", linkEv.Branches, wantChain)
	}
	wantLinkCall := fmt.Sprintf("stack link --base main %s %s", stA.Branch, stB.Branch)
	waitForCond(t, "the fake gh's argv file to record "+wantLinkCall, func() bool {
		return strings.Contains(mustReadFile(t, argvFile), wantLinkCall)
	}, func() string { return archStackDiag(t, argvFile, ghStateFile, rec) })

	// --- a mid-stack merge (B's PR, based on A's branch, not trunk) must
	// never trigger a sync ---
	bNumber := prNumberForBranch(t, ghStateFile, stB.Branch)
	flipPRToMerged(t, ghStateFile, bNumber)

	waitForCond(t, "the fake gh to report B's PR as merged", func() bool {
		for _, e := range readGhState(t, ghStateFile) {
			if e.Number == bNumber && e.State == "MERGED" {
				return true
			}
		}
		return false
	}, func() string { return archStackDiag(t, argvFile, ghStateFile, rec) })

	time.Sleep(1 * time.Second)
	if ev := findStackEvent(rec, "sync"); ev != nil {
		t.Fatalf("unexpected stack sync event after a mid-stack merge: %+v", ev)
	}
	if strings.Contains(mustReadFile(t, argvFile), "stack sync") {
		t.Fatalf("unexpected `gh stack sync` call after a mid-stack merge; argv file:\n%s", mustReadFile(t, argvFile))
	}

	// --- a trunk merge (A's PR, based on main) must trigger a sync ---
	aNumber := prNumberForBranch(t, ghStateFile, stA.Branch)
	flipPRToMerged(t, ghStateFile, aNumber)

	syncEv := waitForStackEvent(t, rec, "sync", func() string { return archStackDiag(t, argvFile, ghStateFile, rec) })
	if syncEv.Err != nil {
		t.Errorf("stack sync event Err = %v, want nil", syncEv.Err)
	}
	waitForCond(t, "the fake gh's argv file to record a `gh stack sync` call", func() bool {
		return strings.Contains(mustReadFile(t, argvFile), "stack sync")
	}, func() string { return archStackDiag(t, argvFile, ghStateFile, rec) })
}

// completeEngineer simulates one fake engineer finishing its work: it opens
// a real PR through the fake gh with spec's base/branch (proving the actual
// --base gh sees), then delivers a Hello followed by a completed Result
// through the fake Transport's Attach stream for ticket — exactly the
// sequence fleet.Follow expects from a real engineer process, minus the
// process. Returns the PR URL gh handed back.
func completeEngineer(t *testing.T, ft *fakeTransport, clonePath, ticket string, spec engineerwire.Spec) string {
	t.Helper()
	prURL, err := gitops.CreatePR(context.Background(), gitops.DefaultRunner, clonePath,
		spec.BaseBranch, spec.Branch, "Ticket "+ticket, "opened by the archstack e2e test")
	if err != nil {
		t.Fatalf("creating PR for ticket %s (base=%s branch=%s): %v", ticket, spec.BaseBranch, spec.Branch, err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	ft.deliver(ticket, engineerwire.Hello{
		Type:            engineerwire.TypeHello,
		Seq:             1,
		At:              now,
		EngineerID:      ft.wireIDFor(ticket),
		ProtocolVersion: engineerwire.ProtocolVersion,
		ACYVersion:      "test",
		Host:            "local",
		PID:             1,
	})
	ft.deliver(ticket, engineerwire.Result{
		Type:    engineerwire.TypeResult,
		Seq:     2,
		At:      now,
		Outcome: "completed",
		Summary: "ticket " + ticket + " done",
		Branch:  spec.Branch,
		PRURL:   prURL,
	})
	return prURL
}

// engineerStatus looks up one ledger entry by ticket, or a zero value if the
// manager has none yet — safe to call from a poll loop before Launch's
// goroutine has recorded anything.
func engineerStatus(m *fleet.Manager, ticket string) fleet.EngineerStatus {
	for _, st := range m.Statuses() {
		if st.Ticket == ticket {
			return st
		}
	}
	return fleet.EngineerStatus{}
}

// prNumberForBranch finds the PR number the fake gh recorded for a head
// branch, failing the test if there is none yet.
func prNumberForBranch(t *testing.T, ghStateFile, branch string) int {
	t.Helper()
	for _, e := range readGhState(t, ghStateFile) {
		if e.HeadRefName == branch {
			return e.Number
		}
	}
	t.Fatalf("no PR recorded for branch %q in gh state file %s", branch, ghStateFile)
	return 0
}

// waitForCond polls cond until it holds or archStackWaitTimeout elapses,
// failing the test with diag's output otherwise. Mirrors ticketloop_test.go's
// waitForDisk, at this test's own (much shorter) interval and timeout — there
// is no live claude session to wait on here.
func waitForCond(t *testing.T, what string, cond func() bool, diag func() string) {
	t.Helper()
	deadline := time.Now().Add(archStackWaitTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(archStackPollInterval)
	}
	t.Fatalf("timed out after %s waiting for %s\n%s", archStackWaitTimeout, what, diag())
}

// archStackEventRecorder drains a Manager's Events() into a slice a test can
// poll without racing the manager's own forwarding goroutines — the fleet
// package's own drainStackEvent/drainEvent helpers (manager_stack_test.go,
// manager_test.go) are unexported and belong to package fleet, not this one.
type archStackEventRecorder struct {
	mu     sync.Mutex
	events []fleet.Event
}

func newArchStackEventRecorder() *archStackEventRecorder {
	return &archStackEventRecorder{}
}

// drain reads m.Events() until it closes (Manager.Close does this). Meant to
// run in its own goroutine for the life of the test.
func (r *archStackEventRecorder) drain(m *fleet.Manager) {
	for ev := range m.Events() {
		r.mu.Lock()
		r.events = append(r.events, ev)
		r.mu.Unlock()
	}
}

func (r *archStackEventRecorder) snapshot() []fleet.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]fleet.Event, len(r.events))
	copy(out, r.events)
	return out
}

// findStackEvent returns the first recorded KindStack event whose Op matches,
// or nil.
func findStackEvent(r *archStackEventRecorder, op string) *fleet.StackEvent {
	for _, ev := range r.snapshot() {
		if ev.Kind == fleet.KindStack && ev.Stack != nil && ev.Stack.Op == op {
			return ev.Stack
		}
	}
	return nil
}

// waitForStackEvent polls for a KindStack event whose Op matches, failing the
// test with diag's output if none arrives within archStackWaitTimeout.
func waitForStackEvent(t *testing.T, r *archStackEventRecorder, op string, diag func() string) *fleet.StackEvent {
	t.Helper()
	var found *fleet.StackEvent
	waitForCond(t, fmt.Sprintf("a stack %q event", op), func() bool {
		found = findStackEvent(r, op)
		return found != nil
	}, diag)
	return found
}

// archStackDiag renders the fake gh's argv/state and every event recorded so
// far, for a timed-out wait's failure message.
func archStackDiag(t *testing.T, argvFile, ghStateFile string, rec *archStackEventRecorder) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("gh state:\n")
	for _, e := range readGhState(t, ghStateFile) {
		b.WriteString("  " + mustMarshal(t, e) + "\n")
	}
	b.WriteString("gh argv:\n" + mustReadFile(t, argvFile))
	b.WriteString("events:\n")
	for _, ev := range rec.snapshot() {
		fmt.Fprintf(&b, "  kind=%d ticket=%q stack=%+v\n", ev.Kind, ev.Ticket, ev.Stack)
	}
	return b.String()
}

// --- a fake fleet.Transport: no engineer process, no claude session ---

// fakeEngine is one launched engineer's controllable half of a
// fakeTransport: the test delivers outbound wire messages (Hello/Result) on
// msgs, which Attach forwards to the manager via onMsg until a Result closes
// the loop — the same contract internal/fleet/manager_test.go's mockTransport
// implements for package fleet's own tests, reimplemented here since that
// type is unexported and this is a different package.
type fakeEngine struct {
	id   string
	msgs chan any
}

// fakeTransport is a fleet.Transport backed entirely by in-memory fakeEngines
// — no process, no journal, no claude. It exists so this test can drive
// fleet.Manager exactly as `acy arch` does while keeping the engineer side of
// the wire under the test's own control.
type fakeTransport struct {
	mu       sync.Mutex
	n        int
	engines  map[string]*fakeEngine
	byTicket map[string]*fakeEngine
	specs    map[string]engineerwire.Spec
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		engines:  map[string]*fakeEngine{},
		byTicket: map[string]*fakeEngine{},
		specs:    map[string]engineerwire.Spec{},
	}
}

func (ft *fakeTransport) forHost(config.FleetHost) fleet.Transport { return ft }

// specFor returns the Spec the manager actually handed Start for ticket.
func (ft *fakeTransport) specFor(ticket string) engineerwire.Spec {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.specs[ticket]
}

// wireIDFor returns the synthetic engineer id Start minted for ticket.
func (ft *fakeTransport) wireIDFor(ticket string) string {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if eng, ok := ft.byTicket[ticket]; ok {
		return eng.id
	}
	return ""
}

// deliver pushes one outbound wire message to ticket's engine, for Attach to
// forward to the manager.
func (ft *fakeTransport) deliver(ticket string, msg any) {
	ft.mu.Lock()
	eng := ft.byTicket[ticket]
	ft.mu.Unlock()
	eng.msgs <- msg
}

func (ft *fakeTransport) Start(_ context.Context, spec engineerwire.Spec) (fleet.StartAck, error) {
	ft.mu.Lock()
	ft.n++
	id := fmt.Sprintf("fake%d", ft.n)
	eng := &fakeEngine{id: id, msgs: make(chan any, 16)}
	ft.engines[id] = eng
	ft.byTicket[spec.Ticket] = eng
	ft.specs[spec.Ticket] = spec
	ft.mu.Unlock()
	return fleet.StartAck{EngineerID: id, Dir: "", PID: 1000 + ft.n}, nil
}

// Attach streams eng's msgs channel to onMsg until a Result arrives (Follow's
// exit condition) or ctx ends. It never reads in — nothing in this test sends
// an Answer/Cancel, and fleet.Follow's own forwardAnswers goroutine only ever
// writes to it when there is something buffered to forward.
func (ft *fakeTransport) Attach(ctx context.Context, engineerID string, _ int64, _ io.Reader, onMsg func(any)) error {
	ft.mu.Lock()
	eng := ft.engines[engineerID]
	ft.mu.Unlock()
	if eng == nil {
		return fmt.Errorf("faketransport: unknown engineer %q", engineerID)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-eng.msgs:
			if !ok {
				return fmt.Errorf("faketransport: engine %q closed", engineerID)
			}
			onMsg(msg)
			if _, isResult := msg.(engineerwire.Result); isResult {
				return nil
			}
		}
	}
}
