package fleet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hweeks/always-click-yes/internal/gitops"
)

// stackRRCall and stackRRResponse mirror internal/gitops/stack_test.go's
// recordingRunner pattern exactly (call/response are already taken by other
// fleet test files' fixtures, hence the stackRR prefix).
type stackRRCall struct {
	dir  string
	name string
	args []string
}

type stackRRResponse struct {
	out string
	err error
}

type stackRecordingRunner struct {
	calls     []stackRRCall
	responses map[int]stackRRResponse
}

func (r *stackRecordingRunner) run(_ context.Context, dir, name string, args ...string) (string, error) {
	r.calls = append(r.calls, stackRRCall{dir: dir, name: name, args: append([]string(nil), args...)})
	if resp, ok := r.responses[len(r.calls)-1]; ok {
		return resp.out, resp.err
	}
	return "", nil
}

// countArgs is how many calls in rr.calls have exactly want as their args.
func countArgsSeen(rr *stackRecordingRunner, name string, contains ...string) int {
	n := 0
	for _, c := range rr.calls {
		if c.name != name {
			continue
		}
		match := true
		for _, want := range contains {
			found := false
			for _, a := range c.args {
				if a == want {
					found = true
					break
				}
			}
			if !found {
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

func TestStackKeeperLink(t *testing.T) {
	rr := &stackRecordingRunner{}
	k := NewStackKeeper("/repo", rr.run, "main")

	ev := k.Link(context.Background(), []string{"acy/a", "acy/b"})
	if ev.Err != nil {
		t.Fatalf("Link: %v", ev.Err)
	}
	if ev.Op != "link" || !reflect.DeepEqual(ev.Branches, []string{"acy/a", "acy/b"}) {
		t.Fatalf("Link event = %+v", ev)
	}

	if len(rr.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(rr.calls))
	}
	call := rr.calls[0]
	if call.dir != "/repo" {
		t.Errorf("dir = %q, want k.dir %q (not the worktree)", call.dir, "/repo")
	}
	want := []string{"stack", "link", "--base", "main", "acy/a", "acy/b"}
	if !reflect.DeepEqual(call.args, want) {
		t.Errorf("args = %#v, want %#v", call.args, want)
	}
}

func TestStackKeeperSyncCreatesWorktreeOnFirstCallOnly(t *testing.T) {
	rr := &stackRecordingRunner{responses: map[int]stackRRResponse{
		// git rev-parse --verify --quiet refs/heads/<random> must fail so
		// EnsureWorktree treats the branch as not already existing.
		0: {err: errors.New("not found")},
	}}
	k := NewStackKeeper("/repo", rr.run, "main")

	ev := k.Sync(context.Background())
	if ev.Err != nil {
		t.Fatalf("first Sync: %v", ev.Err)
	}

	if len(rr.calls) < 4 {
		t.Fatalf("calls = %d, want at least 4 (rev-parse, fetch, worktree add, gh stack sync)", len(rr.calls))
	}
	for _, c := range rr.calls[:len(rr.calls)-1] {
		if c.dir != "/repo" {
			t.Errorf("worktree-creation call %+v ran in %q, want k.dir %q", c, c.dir, "/repo")
		}
	}
	last := rr.calls[len(rr.calls)-1]
	if last.dir != k.WorktreeDir() {
		t.Errorf("gh stack sync ran in %q, want WorktreeDir() %q", last.dir, k.WorktreeDir())
	}
	if k.WorktreeDir() == k.dir {
		t.Fatalf("WorktreeDir() must differ from k.dir")
	}

	firstCallCount := len(rr.calls)

	ev2 := k.Sync(context.Background())
	if ev2.Err != nil {
		t.Fatalf("second Sync: %v", ev2.Err)
	}
	if len(rr.calls) != firstCallCount+1 {
		t.Fatalf("second Sync issued %d calls, want exactly 1 more (no repeated worktree creation)", len(rr.calls)-firstCallCount)
	}
	if n := countArgsSeen(rr, "git", "worktree", "add"); n != 1 {
		t.Errorf("git worktree add calls = %d, want 1 (not repeated on the second Sync)", n)
	}
}

func TestStackKeeperSyncReusesPreexistingWorktreeDir(t *testing.T) {
	rr := &stackRecordingRunner{}
	k := NewStackKeeper("/repo", rr.run, "main")

	if err := os.MkdirAll(k.WorktreeDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(k.WorktreeDir()) })
	if err := os.WriteFile(filepath.Join(k.WorktreeDir(), "dummy"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ev := k.Sync(context.Background())
	if ev.Err != nil {
		t.Fatalf("Sync: %v", ev.Err)
	}

	if len(rr.calls) != 1 {
		t.Fatalf("calls = %d, want 1 (straight to gh stack sync, no EnsureWorktree calls)", len(rr.calls))
	}
	if rr.calls[0].dir != k.WorktreeDir() {
		t.Errorf("dir = %q, want %q", rr.calls[0].dir, k.WorktreeDir())
	}
	if rr.calls[0].name != "gh" || len(rr.calls[0].args) < 2 || rr.calls[0].args[0] != "stack" || rr.calls[0].args[1] != "sync" {
		t.Errorf("call = %+v, want a gh stack sync", rr.calls[0])
	}
}

func TestStackKeeperSyncConflictNamesBranch(t *testing.T) {
	stackViewJSON := `{"trunk":"main","currentBranch":"acy/b","branches":[` +
		`{"name":"acy/a","needsRebase":true},` +
		`{"name":"acy/b","needsRebase":false}]}`

	rr := &stackRecordingRunner{responses: map[int]stackRRResponse{
		0: {err: errors.New("not found")}, // rev-parse: branch doesn't exist yet
		3: {err: fakeExitErr{3}},          // gh stack sync: rebase conflict
		4: {out: stackViewJSON},           // gh stack view --json
	}}
	k := NewStackKeeper("/repo", rr.run, "main")

	ev := k.Sync(context.Background())
	if ev.Err == nil {
		t.Fatal("Sync: want an error")
	}
	if !errors.Is(ev.Err, gitops.ErrStackConflict) {
		t.Fatalf("errors.Is(ev.Err, ErrStackConflict) = false, err = %v", ev.Err)
	}
	if ev.Branch != "acy/a" {
		t.Errorf("Branch = %q, want %q", ev.Branch, "acy/a")
	}
}

func TestStackKeeperCloseNoopWithoutPriorSync(t *testing.T) {
	rr := &stackRecordingRunner{}
	k := NewStackKeeper("/repo", rr.run, "main")

	k.Close(context.Background())

	if len(rr.calls) != 0 {
		t.Fatalf("calls = %d, want 0 (Close with no prior Sync must be a no-op)", len(rr.calls))
	}
}

func TestStackKeeperCloseRemovesWorktreeAfterSync(t *testing.T) {
	rr := &stackRecordingRunner{responses: map[int]stackRRResponse{
		0: {err: errors.New("not found")},
	}}
	k := NewStackKeeper("/repo", rr.run, "main")

	if ev := k.Sync(context.Background()); ev.Err != nil {
		t.Fatalf("Sync: %v", ev.Err)
	}
	before := len(rr.calls)

	k.Close(context.Background())

	closeCalls := rr.calls[before:]
	if len(closeCalls) != 2 {
		t.Fatalf("Close issued %d calls, want 2 (worktree remove, worktree prune): %+v", len(closeCalls), closeCalls)
	}
	for _, c := range closeCalls {
		if c.dir != "/repo" {
			t.Errorf("close call %+v ran in %q, want k.dir %q", c, c.dir, "/repo")
		}
	}
	if closeCalls[0].name != "git" || len(closeCalls[0].args) < 2 || closeCalls[0].args[0] != "worktree" || closeCalls[0].args[1] != "remove" {
		t.Errorf("first close call = %+v, want a git worktree remove", closeCalls[0])
	}
	if closeCalls[1].name != "git" || len(closeCalls[1].args) < 2 || closeCalls[1].args[0] != "worktree" || closeCalls[1].args[1] != "prune" {
		t.Errorf("second close call = %+v, want a git worktree prune", closeCalls[1])
	}

	// Safe to call twice: a second Close is a no-op.
	afterFirstClose := len(rr.calls)
	k.Close(context.Background())
	if len(rr.calls) != afterFirstClose {
		t.Errorf("second Close issued %d more calls, want 0", len(rr.calls)-afterFirstClose)
	}
}
