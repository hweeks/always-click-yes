package fleet

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/gitops"
)

// stackGHRunner is a gitops.Runner fake that dispatches on the command shape
// rather than call index, so it can serve both a PRWatcher's `gh pr list`
// polls and a StackKeeper's gh-stack/worktree calls in whatever order the
// forwardPRWatcher goroutine happens to issue them, and still record every
// call for assertions.
type stackGHRunner struct {
	mu sync.Mutex

	calls []stackRRCall

	prListQueue []stackRRResponse
	prListCalls int

	syncQueue []stackRRResponse
	syncCalls int

	stackViewOut string
	stackViewErr error
}

func (r *stackGHRunner) run(_ context.Context, dir, name string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, stackRRCall{dir: dir, name: name, args: append([]string(nil), args...)})

	switch {
	case name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "list":
		i := r.prListCalls
		r.prListCalls++
		if len(r.prListQueue) == 0 {
			return "[]", nil
		}
		if i >= len(r.prListQueue) {
			i = len(r.prListQueue) - 1
		}
		resp := r.prListQueue[i]
		return resp.out, resp.err

	case name == "gh" && len(args) >= 2 && args[0] == "stack" && args[1] == "sync":
		i := r.syncCalls
		r.syncCalls++
		if len(r.syncQueue) == 0 {
			return "", nil
		}
		if i >= len(r.syncQueue) {
			i = len(r.syncQueue) - 1
		}
		resp := r.syncQueue[i]
		return resp.out, resp.err

	case name == "gh" && len(args) >= 2 && args[0] == "stack" && args[1] == "link":
		return "", nil

	case name == "gh" && len(args) >= 2 && args[0] == "stack" && args[1] == "view":
		return r.stackViewOut, r.stackViewErr

	case name == "git" && len(args) >= 1 && args[0] == "rev-parse":
		return "", errors.New("no such ref") // branch doesn't exist yet, EnsureWorktree may proceed

	case name == "git" && len(args) >= 1 && args[0] == "fetch":
		return "", nil

	case name == "git" && len(args) >= 2 && args[0] == "worktree" && args[1] == "add":
		return "", nil

	case name == "git" && len(args) >= 2 && args[0] == "worktree":
		return "", nil
	}
	return "", nil
}

func (r *stackGHRunner) queuePRList(out string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prListQueue = append(r.prListQueue, stackRRResponse{out: out, err: err})
}

func (r *stackGHRunner) queueSync(out string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.syncQueue = append(r.syncQueue, stackRRResponse{out: out, err: err})
}

func (r *stackGHRunner) callsMatching(name string, args ...string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c.name != name {
			continue
		}
		if len(c.args) < len(args) {
			continue
		}
		match := true
		for i, a := range args {
			if c.args[i] != a {
				match = false
				break
			}
		}
		if match {
			n++
		}
	}
	return n
}

// drainStackEvent polls Events() until it sees a KindStack event or timeout
// elapses, forwarding every other event it sees into other (so callers that
// also expect a KindPR alongside it don't lose that event).
func drainStackEvent(t *testing.T, m *Manager, timeout time.Duration) (Event, []Event) {
	t.Helper()
	var other []Event
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-m.Events():
			if !ok {
				t.Fatal("Events channel closed unexpectedly")
			}
			if ev.Kind == KindStack {
				return ev, other
			}
			other = append(other, ev)
		case <-deadline:
			t.Fatal("timed out waiting for a KindStack event")
		}
	}
}

// A stacked PR opening (base is another acy/* branch, with a real chain of
// 2+ engineers behind it) registers the chain as a stack on GitHub.
func TestManagerStackOpenRegistersChain(t *testing.T) {
	mt := newMockTransport()
	gh := &stackGHRunner{}
	cfg := testFleetConfig(testHost("a", 2))
	cfg.StackMode = "chain"

	watcher := NewPRWatcher("/repo", gh.run, time.Minute, nil)
	keeper := NewStackKeeper("/repo", gh.run, cfg.BaseBranch)
	m := NewManager(cfg, mt.forHost, WithPRWatcher(watcher, 0), WithStackKeeper(keeper))
	t.Cleanup(m.Close)

	// Bootstrap the watcher with no PRs yet.
	gh.queuePRList(`[]`, nil)
	if err := watcher.poll(context.Background()); err != nil {
		t.Fatalf("bootstrap poll: %v", err)
	}

	parentSt, err := m.Launch(context.Background(), LaunchReq{Ticket: "P"})
	if err != nil {
		t.Fatalf("Launch parent: %v", err)
	}
	drainEvent(t, m, time.Second) // KindStarted

	finishEngineer(t, m, mt, "P", "https://example/pr/p")
	drainEvent(t, m, time.Second) // KindResult

	childSt, err := m.Launch(context.Background(), LaunchReq{Ticket: "C", StackOn: "P"})
	if err != nil {
		t.Fatalf("Launch child: %v", err)
	}
	drainEvent(t, m, time.Second) // KindStarted

	// The child's PR opens, based on the parent's branch: this is the trigger
	// that must register the two-branch chain as a stack.
	gh.queuePRList(fmt.Sprintf(
		`[{"url":"https://example/pr/p","state":"OPEN","headRefName":%q,"baseRefName":%q,"number":1},
		  {"url":"https://example/pr/c","state":"OPEN","headRefName":%q,"baseRefName":%q,"number":2}]`,
		parentSt.Branch, cfg.BaseBranch, childSt.Branch, parentSt.Branch), nil)
	if err := watcher.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	stackEv, others := drainStackEvent(t, m, 2*time.Second)
	if stackEv.Stack == nil || stackEv.Stack.Err != nil {
		t.Fatalf("stack event = %+v", stackEv.Stack)
	}
	want := []string{parentSt.Branch, childSt.Branch}
	if len(stackEv.Stack.Branches) != len(want) || stackEv.Stack.Branches[0] != want[0] || stackEv.Stack.Branches[1] != want[1] {
		t.Errorf("Stack.Branches = %v, want %v", stackEv.Stack.Branches, want)
	}

	var sawPR bool
	for _, ev := range others {
		if ev.Kind == KindPR {
			sawPR = true
		}
	}
	if !sawPR {
		t.Error("want a KindPR event for the child's PR opening, alongside the KindStack event")
	}

	if n := gh.callsMatching("gh", "stack", "link"); n != 1 {
		t.Errorf("gh stack link calls = %d, want 1", n)
	}
}

// A PR merging into trunk triggers a repair sync.
func TestManagerStackMergeToTrunkTriggersSync(t *testing.T) {
	mt := newMockTransport()
	gh := &stackGHRunner{}
	cfg := testFleetConfig(testHost("a", 1))

	watcher := NewPRWatcher("/repo", gh.run, time.Minute, nil)
	keeper := NewStackKeeper("/repo", gh.run, cfg.BaseBranch)
	m := NewManager(cfg, mt.forHost, WithPRWatcher(watcher, 0), WithStackKeeper(keeper))
	t.Cleanup(m.Close)

	gh.queuePRList(`[{"url":"https://example/pr/a","state":"OPEN","headRefName":"acy/a","baseRefName":"main","number":1}]`, nil)
	if err := watcher.poll(context.Background()); err != nil {
		t.Fatalf("bootstrap poll: %v", err)
	}

	gh.queuePRList(`[{"url":"https://example/pr/a","state":"MERGED","headRefName":"acy/a","baseRefName":"main","number":1}]`, nil)
	if err := watcher.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	stackEv, _ := drainStackEvent(t, m, 2*time.Second)
	if stackEv.Stack == nil || stackEv.Stack.Op != "sync" || stackEv.Stack.Err != nil {
		t.Fatalf("stack event = %+v", stackEv.Stack)
	}
	if n := gh.callsMatching("gh", "stack", "sync"); n != 1 {
		t.Errorf("gh stack sync calls = %d, want 1", n)
	}
}

// A PR merging into another acy/* branch (mid-stack) must never trigger a
// sync — only the merge that actually lands on trunk does.
func TestManagerStackMergeMidStackNeverTriggersSync(t *testing.T) {
	mt := newMockTransport()
	gh := &stackGHRunner{}
	cfg := testFleetConfig(testHost("a", 1))

	watcher := NewPRWatcher("/repo", gh.run, time.Minute, nil)
	keeper := NewStackKeeper("/repo", gh.run, cfg.BaseBranch)
	m := NewManager(cfg, mt.forHost, WithPRWatcher(watcher, 0), WithStackKeeper(keeper))
	t.Cleanup(m.Close)

	gh.queuePRList(`[{"url":"https://example/pr/b","state":"OPEN","headRefName":"acy/b","baseRefName":"acy/a","number":2}]`, nil)
	if err := watcher.poll(context.Background()); err != nil {
		t.Fatalf("bootstrap poll: %v", err)
	}

	gh.queuePRList(`[{"url":"https://example/pr/b","state":"MERGED","headRefName":"acy/b","baseRefName":"acy/a","number":2}]`, nil)
	if err := watcher.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	ev := drainEvent(t, m, 2*time.Second)
	if ev.Kind != KindPR {
		t.Fatalf("first event kind = %v, want KindPR", ev.Kind)
	}

	select {
	case second := <-m.Events():
		t.Fatalf("unexpected second event %+v, want none (mid-stack merge must not trigger a sync)", second)
	case <-time.After(200 * time.Millisecond):
	}

	if n := gh.callsMatching("gh", "stack", "sync"); n != 0 {
		t.Errorf("gh stack sync calls = %d, want 0", n)
	}
}

// A non-conflict gh stack sync failure is logged and skipped, never emitted
// as a KindStack event, and never stops the manager from forwarding further
// events.
func TestManagerStackSyncFailureLoggedNotEmitted(t *testing.T) {
	mt := newMockTransport()
	gh := &stackGHRunner{}
	cfg := testFleetConfig(testHost("a", 1))

	watcher := NewPRWatcher("/repo", gh.run, time.Minute, nil)
	keeper := NewStackKeeper("/repo", gh.run, cfg.BaseBranch)
	m := NewManager(cfg, mt.forHost, WithPRWatcher(watcher, 0), WithStackKeeper(keeper))
	t.Cleanup(m.Close)

	gh.queuePRList(`[{"url":"https://example/pr/a","state":"OPEN","headRefName":"acy/a","baseRefName":"main","number":1}]`, nil)
	if err := watcher.poll(context.Background()); err != nil {
		t.Fatalf("bootstrap poll: %v", err)
	}

	gh.queueSync("", fakeExitErr{1}) // generic failure, no sentinel

	gh.queuePRList(`[{"url":"https://example/pr/a","state":"MERGED","headRefName":"acy/a","baseRefName":"main","number":1}]`, nil)
	if err := watcher.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	ev := drainEvent(t, m, 2*time.Second)
	if ev.Kind != KindPR {
		t.Fatalf("event kind = %v, want KindPR", ev.Kind)
	}

	select {
	case second := <-m.Events():
		t.Fatalf("unexpected event %+v after a non-conflict sync failure, want none", second)
	case <-time.After(200 * time.Millisecond):
	}

	// The manager must still be alive: another PR event still forwards.
	gh.queuePRList(`[{"url":"https://example/pr/z","state":"OPEN","headRefName":"acy/z","baseRefName":"main","number":9}]`, nil)
	if err := watcher.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	next := drainEvent(t, m, 2*time.Second)
	if next.Kind != KindPR || next.PR == nil || next.PR.Head != "acy/z" {
		t.Fatalf("event = %+v, want the KindPR event for acy/z, proving the loop kept running", next)
	}
}

// A sync conflict produces exactly one KindStack event naming the branch,
// and closing the manager (or further merge triggers with no new PREvent)
// never produces an automatic retry — there is no timer in this design.
func TestManagerStackSyncConflictEmitsOnce(t *testing.T) {
	mt := newMockTransport()
	gh := &stackGHRunner{}
	gh.stackViewOut = `{"trunk":"main","currentBranch":"acy/a","branches":[{"name":"acy/a","needsRebase":true}]}`
	cfg := testFleetConfig(testHost("a", 1))

	watcher := NewPRWatcher("/repo", gh.run, time.Minute, nil)
	keeper := NewStackKeeper("/repo", gh.run, cfg.BaseBranch)
	m := NewManager(cfg, mt.forHost, WithPRWatcher(watcher, 0), WithStackKeeper(keeper))
	t.Cleanup(m.Close)

	gh.queuePRList(`[{"url":"https://example/pr/a","state":"OPEN","headRefName":"acy/a","baseRefName":"main","number":1}]`, nil)
	if err := watcher.poll(context.Background()); err != nil {
		t.Fatalf("bootstrap poll: %v", err)
	}

	gh.queueSync("", fakeExitErr{3}) // rebase conflict

	gh.queuePRList(`[{"url":"https://example/pr/a","state":"MERGED","headRefName":"acy/a","baseRefName":"main","number":1}]`, nil)
	if err := watcher.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	stackEv, _ := drainStackEvent(t, m, 2*time.Second)
	if stackEv.Stack == nil {
		t.Fatal("want a KindStack event")
	}
	if !errors.Is(stackEv.Stack.Err, gitops.ErrStackConflict) {
		t.Fatalf("errors.Is(Stack.Err, ErrStackConflict) = false, err = %v", stackEv.Stack.Err)
	}
	if stackEv.Stack.Branch != "acy/a" {
		t.Errorf("Stack.Branch = %q, want %q", stackEv.Stack.Branch, "acy/a")
	}

	// No further PREvent arrives, so no further sync attempt should ever
	// happen — confirmed by closing the manager and seeing no extra call.
	syncCallsBefore := gh.callsMatching("gh", "stack", "sync")
	m.Close()
	if n := gh.callsMatching("gh", "stack", "sync"); n != syncCallsBefore {
		t.Errorf("gh stack sync calls after Close = %d, want unchanged from %d (no timer-driven retry)", n, syncCallsBefore)
	}
}
