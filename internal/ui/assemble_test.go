package ui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hweeks/always-click-yes/internal/fleet"
	"github.com/hweeks/always-click-yes/internal/mcp"
)

// fakeAssembleExitErr satisfies gitops's local exitCoder interface without
// importing os/exec, mirroring internal/gitops/stack_test.go's fakeExitErr —
// that one is private to package gitops, so this is its own copy.
type fakeAssembleExitErr struct{ code int }

func (e fakeAssembleExitErr) Error() string { return fmt.Sprintf("exit %d", e.code) }
func (e fakeAssembleExitErr) ExitCode() int { return e.code }

// assembleCall is one recorded invocation of the fake gitops.Runner below.
type assembleCall struct {
	dir  string
	name string
	args []string
}

type assembleResponse struct {
	out string
	err error
}

// assembleRunner is a fake gitops.Runner that records every invocation in
// order and looks up its canned response by call index, mirroring
// internal/gitops/stack_test.go's recordingRunner (private to that package,
// so this is a small copy rather than an import).
type assembleRunner struct {
	calls     []assembleCall
	responses map[int]assembleResponse
}

func (r *assembleRunner) run(_ context.Context, dir, name string, args ...string) (string, error) {
	i := len(r.calls)
	r.calls = append(r.calls, assembleCall{dir: dir, name: name, args: append([]string(nil), args...)})
	if resp, ok := r.responses[i]; ok {
		return resp.out, resp.err
	}
	return "", nil
}

func neverCalledRunner(t *testing.T) func(context.Context, string, string, ...string) (string, error) {
	return func(_ context.Context, dir, name string, args ...string) (string, error) {
		t.Fatalf("gitRunner should not be invoked, got dir=%s %s %v", dir, name, args)
		return "", nil
	}
}

// doneTicket builds a finished, unstacked, PR'd engineer status — the shape
// every ticket AssembleStack accepts must have.
func doneTicket(ticket, branch, prURL string) fleet.EngineerStatus {
	return fleet.EngineerStatus{Ticket: ticket, Branch: branch, State: fleet.StateDone, PRURL: prURL}
}

func assembleArgs(tickets ...string) string {
	quoted := make([]string, len(tickets))
	for i, t := range tickets {
		quoted[i] = `"` + t + `"`
	}
	return fmt.Sprintf(`{"tickets":[%s]}`, strings.Join(quoted, ","))
}

// --- success ---

func TestAssembleStackSucceeds(t *testing.T) {
	fake := newFakeFleetManager()
	fake.statuses = []fleet.EngineerStatus{
		doneTicket("t1", "agent/e1-t1", "https://example.com/pr/1"),
		doneTicket("t2", "agent/e2-t2", "https://example.com/pr/2"),
	}
	rr := &assembleRunner{responses: map[int]assembleResponse{
		5: {out: "Stack #7 created with 2 PRs\n"},
	}}
	m := &Model{phase: PhaseAutoRun, fleet: fake, gitRunner: rr.run, cwd: "/repo", trunk: "main", stackMode: "chain"}

	p, reply := fleetPending(mcp.ToolAssembleStack, assembleArgs("t1", "t2"))
	m.startAssembleStack(p)

	got := answer(t, reply)
	for _, want := range []string{"7", "https://example.com/pr/2"} {
		if !strings.Contains(got, want) {
			t.Errorf("answer = %q, missing %q", got, want)
		}
	}

	if len(rr.calls) != 8 {
		t.Fatalf("calls = %d, want 8 (fetch, worktree add, init, rebase, push, link, worktree remove, prune); calls=%+v", len(rr.calls), rr.calls)
	}

	// git worktree bookkeeping runs against m.cwd, never the scratch worktree.
	for _, i := range []int{0, 1, 6, 7} {
		if rr.calls[i].name != "git" || rr.calls[i].dir != "/repo" {
			t.Errorf("call %d = %+v, want git in /repo", i, rr.calls[i])
		}
	}
	wtDir := rr.calls[1].args[3] // git worktree add --detach <dir> <startPoint>
	if wtDir == "" || wtDir == "/repo" {
		t.Fatalf("could not recover worktree dir from call 1: %+v", rr.calls[1])
	}

	// Every gh stack command runs in the scratch worktree, never m.cwd.
	for _, i := range []int{2, 3, 4, 5} {
		if rr.calls[i].name != "gh" || rr.calls[i].dir != wtDir {
			t.Errorf("call %d = %+v, want gh in %s", i, rr.calls[i], wtDir)
		}
	}

	wantInit := []string{"stack", "init", "agent/e1-t1", "agent/e2-t2", "--base", "main"}
	wantRebase := []string{"stack", "rebase"}
	wantPush := []string{"stack", "push"}
	wantLink := []string{"stack", "link", "--base", "main", "agent/e1-t1", "agent/e2-t2"}
	for i, want := range [][]string{wantInit, wantRebase, wantPush, wantLink} {
		if got := rr.calls[i+2].args; strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("call %d args = %v, want %v", i+2, got, want)
		}
	}

	if len(m.entries) != 1 {
		t.Fatalf("want 1 transcript entry, got %d: %+v", len(m.entries), m.entries)
	}
}

// --- refusals that must run no gh/git command ---

func TestAssembleStackRefusedWithoutFleet(t *testing.T) {
	m := &Model{phase: PhaseAutoRun}
	p, reply := fleetPending(mcp.ToolAssembleStack, assembleArgs("t1", "t2"))
	m.startAssembleStack(p)
	if got := answer(t, reply); got != mcp.FleetUnavailable {
		t.Errorf("answer = %q, want mcp.FleetUnavailable", got)
	}
}

func TestAssembleStackRefusedWhenNotArmed(t *testing.T) {
	m := &Model{phase: PhasePlan, fleet: newFakeFleetManager()}
	p, reply := fleetPending(mcp.ToolAssembleStack, assembleArgs("t1", "t2"))
	m.startAssembleStack(p)
	if got := answer(t, reply); got != mcp.AssembleStackNotArmed {
		t.Errorf("answer = %q, want mcp.AssembleStackNotArmed", got)
	}
}

func TestAssembleStackRefusedWhenStackModeOff(t *testing.T) {
	fake := newFakeFleetManager()
	m := &Model{phase: PhaseAutoRun, fleet: fake, stackMode: "off", gitRunner: neverCalledRunner(t)}
	p, reply := fleetPending(mcp.ToolAssembleStack, assembleArgs("t1", "t2"))
	m.startAssembleStack(p)

	got := answer(t, reply)
	if !strings.Contains(got, "off") {
		t.Errorf("answer = %q, want it to name the stack mode", got)
	}
	if !strings.Contains(got, "nothing was assembled") && !strings.Contains(got, "Nothing was assembled") {
		t.Errorf("answer = %q, want it to say nothing was assembled", got)
	}
}

func TestAssembleStackRefusedFewerThanTwoTickets(t *testing.T) {
	fake := newFakeFleetManager()
	m := &Model{phase: PhaseAutoRun, fleet: fake, stackMode: "chain", gitRunner: neverCalledRunner(t)}
	p, reply := fleetPending(mcp.ToolAssembleStack, assembleArgs("t1"))
	m.startAssembleStack(p)

	if got := answer(t, reply); !strings.Contains(got, "2") {
		t.Errorf("answer = %q, want it to name the minimum", got)
	}
}

func TestAssembleStackRefusedDuplicateTicket(t *testing.T) {
	fake := newFakeFleetManager()
	m := &Model{phase: PhaseAutoRun, fleet: fake, stackMode: "chain", gitRunner: neverCalledRunner(t)}
	p, reply := fleetPending(mcp.ToolAssembleStack, assembleArgs("t1", "t1"))
	m.startAssembleStack(p)

	if got := answer(t, reply); !strings.Contains(got, `"t1"`) {
		t.Errorf("answer = %q, want it to name the duplicated ticket t1", got)
	}
}

func TestAssembleStackRefusedUnknownTicket(t *testing.T) {
	fake := newFakeFleetManager()
	fake.statuses = []fleet.EngineerStatus{doneTicket("t1", "agent/e1-t1", "https://example.com/pr/1")}
	m := &Model{phase: PhaseAutoRun, fleet: fake, stackMode: "chain", gitRunner: neverCalledRunner(t)}
	p, reply := fleetPending(mcp.ToolAssembleStack, assembleArgs("t1", "t2"))
	m.startAssembleStack(p)

	if got := answer(t, reply); !strings.Contains(got, `"t2"`) {
		t.Errorf("answer = %q, want it to name the unknown ticket t2", got)
	}
}

func TestAssembleStackRefusedUnfinishedTicket(t *testing.T) {
	fake := newFakeFleetManager()
	fake.statuses = []fleet.EngineerStatus{
		doneTicket("t1", "agent/e1-t1", "https://example.com/pr/1"),
		{Ticket: "t2", Branch: "agent/e2-t2", State: fleet.StateRunning},
	}
	m := &Model{phase: PhaseAutoRun, fleet: fake, stackMode: "chain", gitRunner: neverCalledRunner(t)}
	p, reply := fleetPending(mcp.ToolAssembleStack, assembleArgs("t1", "t2"))
	m.startAssembleStack(p)

	got := answer(t, reply)
	if !strings.Contains(got, `"t2"`) {
		t.Errorf("answer = %q, want it to name ticket t2", got)
	}
	if !strings.Contains(got, fleet.StateRunning) {
		t.Errorf("answer = %q, want it to name the state %q", got, fleet.StateRunning)
	}
}

func TestAssembleStackRefusedNoOpenPR(t *testing.T) {
	fake := newFakeFleetManager()
	fake.statuses = []fleet.EngineerStatus{
		doneTicket("t1", "agent/e1-t1", "https://example.com/pr/1"),
		{Ticket: "t2", Branch: "agent/e2-t2", State: fleet.StateDone, PRURL: ""},
	}
	m := &Model{phase: PhaseAutoRun, fleet: fake, stackMode: "chain", gitRunner: neverCalledRunner(t)}
	p, reply := fleetPending(mcp.ToolAssembleStack, assembleArgs("t1", "t2"))
	m.startAssembleStack(p)

	if got := answer(t, reply); !strings.Contains(got, `"t2"`) {
		t.Errorf("answer = %q, want it to name ticket t2", got)
	}
}

func TestAssembleStackRefusedAlreadyStacked(t *testing.T) {
	fake := newFakeFleetManager()
	fake.statuses = []fleet.EngineerStatus{
		doneTicket("t1", "agent/e1-t1", "https://example.com/pr/1"),
		{Ticket: "t2", Branch: "agent/e2-t2", State: fleet.StateDone, PRURL: "https://example.com/pr/2", StackID: "s3"},
	}
	m := &Model{phase: PhaseAutoRun, fleet: fake, stackMode: "chain", gitRunner: neverCalledRunner(t)}
	p, reply := fleetPending(mcp.ToolAssembleStack, assembleArgs("t1", "t2"))
	m.startAssembleStack(p)

	got := answer(t, reply)
	if !strings.Contains(got, `"t2"`) {
		t.Errorf("answer = %q, want it to name ticket t2", got)
	}
	if !strings.Contains(got, "s3") {
		t.Errorf("answer = %q, want it to name the existing stack s3", got)
	}
}

// --- gh failures ---

func TestAssembleStackRebaseConflictEscalates(t *testing.T) {
	fake := newFakeFleetManager()
	fake.statuses = []fleet.EngineerStatus{
		doneTicket("t1", "agent/e1-t1", "https://example.com/pr/1"),
		doneTicket("t2", "agent/e2-t2", "https://example.com/pr/2"),
	}
	rr := &assembleRunner{responses: map[int]assembleResponse{
		3: {err: fmt.Errorf("gh stack rebase: %w: conflict in agent/e2-t2", fakeAssembleExitErr{3})},
	}}
	m := &Model{phase: PhaseAutoRun, fleet: fake, gitRunner: rr.run, cwd: "/repo", trunk: "main", stackMode: "chain"}

	p, reply := fleetPending(mcp.ToolAssembleStack, assembleArgs("t1", "t2"))
	m.startAssembleStack(p)

	got := answer(t, reply)
	if !strings.Contains(got, "agent/e2-t2") {
		t.Errorf("answer = %q, want the conflicting branch named", got)
	}
	if !strings.Contains(strings.ToLower(got), "human") || !strings.Contains(strings.ToLower(got), "escalat") {
		t.Errorf("answer = %q, want it to say to escalate to a human rather than retry", got)
	}
	if strings.Contains(strings.ToLower(got), "retry automatically") == false && strings.Contains(strings.ToLower(got), "not retry") == false {
		t.Errorf("answer = %q, want it to say not to retry automatically", got)
	}

	// push and link must not have run past the conflict, but the worktree
	// must still be cleaned up (git worktree remove + prune).
	var sawRemove, sawPrune bool
	for _, c := range rr.calls {
		if c.name == "git" && len(c.args) > 0 && c.args[0] == "worktree" && len(c.args) > 1 {
			if c.args[1] == "remove" {
				sawRemove = true
			}
			if c.args[1] == "prune" {
				sawPrune = true
			}
		}
		if c.name == "gh" && len(c.args) > 1 && (c.args[1] == "push" || c.args[1] == "link") {
			t.Errorf("gh %v ran after a rebase conflict, want it to stop", c.args)
		}
	}
	if !sawRemove || !sawPrune {
		t.Error("the worktree was not cleaned up after a rebase conflict")
	}
}

func TestAssembleStackLeaseRejectionIsDistinct(t *testing.T) {
	fake := newFakeFleetManager()
	fake.statuses = []fleet.EngineerStatus{
		doneTicket("t1", "agent/e1-t1", "https://example.com/pr/1"),
		doneTicket("t2", "agent/e2-t2", "https://example.com/pr/2"),
	}
	rr := &assembleRunner{responses: map[int]assembleResponse{
		4: {err: fmt.Errorf("gh stack push: %w: ! [rejected] agent/e1-t1 -> agent/e1-t1 (stale info)", fakeAssembleExitErr{1})},
	}}
	m := &Model{phase: PhaseAutoRun, fleet: fake, gitRunner: rr.run, cwd: "/repo", trunk: "main", stackMode: "chain"}

	p, reply := fleetPending(mcp.ToolAssembleStack, assembleArgs("t1", "t2"))
	m.startAssembleStack(p)

	got := strings.ToLower(answer(t, reply))
	if !strings.Contains(got, "force-with-lease") && !strings.Contains(got, "someone has local commits") {
		t.Errorf("answer = %q, want it to surface the lease rejection distinctly from a generic failure", got)
	}
}

func TestAssembleStackGenericFailureSurfacesGHText(t *testing.T) {
	fake := newFakeFleetManager()
	fake.statuses = []fleet.EngineerStatus{
		doneTicket("t1", "agent/e1-t1", "https://example.com/pr/1"),
		doneTicket("t2", "agent/e2-t2", "https://example.com/pr/2"),
	}
	rr := &assembleRunner{responses: map[int]assembleResponse{
		2: {err: fmt.Errorf("gh stack init: %w: some unrelated gh failure xyz123", fakeAssembleExitErr{5})},
	}}
	m := &Model{phase: PhaseAutoRun, fleet: fake, gitRunner: rr.run, cwd: "/repo", trunk: "main", stackMode: "chain"}

	p, reply := fleetPending(mcp.ToolAssembleStack, assembleArgs("t1", "t2"))
	m.startAssembleStack(p)

	got := answer(t, reply)
	if !strings.Contains(got, "some unrelated gh failure xyz123") {
		t.Errorf("answer = %q, want gh's own error text intact", got)
	}
	if !strings.Contains(got, "AssembleStack failed:") {
		t.Errorf("answer = %q, want the AssembleStack failed prefix", got)
	}
}
