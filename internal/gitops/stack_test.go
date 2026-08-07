package gitops

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// fakeExitErr satisfies the package's local exitCoder interface without
// pulling in os/exec, so exit-code classification can be tested against a
// canned code instead of a real process failure.
type fakeExitErr struct{ code int }

func (e fakeExitErr) Error() string { return fmt.Sprintf("exit %d", e.code) }
func (e fakeExitErr) ExitCode() int { return e.code }

// recordingRunner is a fake Runner that records every invocation in order
// and looks up its canned response by call index, falling back to a default
// success response when no script entry exists for that index.
type recordingRunner struct {
	calls     []call
	responses map[int]response
}

type call struct {
	dir  string
	name string
	args []string
}

type response struct {
	out string
	err error
}

func (r *recordingRunner) run(ctx context.Context, dir, name string, args ...string) (string, error) {
	r.calls = append(r.calls, call{dir: dir, name: name, args: append([]string(nil), args...)})
	if resp, ok := r.responses[len(r.calls)-1]; ok {
		return resp.out, resp.err
	}
	return "", nil
}

func neverCalled(t *testing.T) Runner {
	return func(ctx context.Context, dir, name string, args ...string) (string, error) {
		t.Fatalf("runner should not be invoked, got %s %s %v", dir, name, args)
		return "", nil
	}
}

func TestStackAvailablePresent(t *testing.T) {
	rr := &recordingRunner{}
	if err := StackAvailable(context.Background(), rr.run, "/wt"); err != nil {
		t.Fatalf("StackAvailable: %v", err)
	}
	if len(rr.calls) != 1 {
		t.Fatalf("calls = %d, want 1 (should not fall back)", len(rr.calls))
	}
	want := []string{"stack", "--version"}
	if !reflect.DeepEqual(rr.calls[0].args, want) {
		t.Fatalf("args = %#v, want %#v", rr.calls[0].args, want)
	}
}

func TestStackAvailableNotEnabled(t *testing.T) {
	rr := &recordingRunner{responses: map[int]response{
		0: {err: fmt.Errorf("gh stack --version: %w: stacks not enabled", fakeExitErr{9})},
	}}
	err := StackAvailable(context.Background(), rr.run, "/wt")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrStackNotEnabled) {
		t.Fatalf("errors.Is ErrStackNotEnabled = false, err = %v", err)
	}
	if !strings.Contains(err.Error(), "public preview") {
		t.Fatalf("error missing public-preview explanation: %v", err)
	}
	if len(rr.calls) != 1 {
		t.Fatalf("calls = %d, want 1 (exit 9 should not fall back to extension list)", len(rr.calls))
	}
}

func TestStackAvailableFallbackFound(t *testing.T) {
	rr := &recordingRunner{responses: map[int]response{
		0: {err: fakeExitErr{1}},
		1: {out: "gh stack\tgithub/gh-stack\tv0.1.0\n"},
	}}
	if err := StackAvailable(context.Background(), rr.run, "/wt"); err != nil {
		t.Fatalf("StackAvailable: %v", err)
	}
	if len(rr.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(rr.calls))
	}
	want := []string{"extension", "list"}
	if !reflect.DeepEqual(rr.calls[1].args, want) {
		t.Fatalf("args = %#v, want %#v", rr.calls[1].args, want)
	}
}

func TestStackAvailableNotFound(t *testing.T) {
	rr := &recordingRunner{responses: map[int]response{
		0: {err: fakeExitErr{1}},
		1: {out: "some-other-extension\towner/repo\tv1.0.0\n"},
	}}
	err := StackAvailable(context.Background(), rr.run, "/wt")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "gh extension install github/gh-stack") {
		t.Fatalf("error missing install instructions: %v", err)
	}
}

func TestStackLinkArgv(t *testing.T) {
	rr := &recordingRunner{responses: map[int]response{
		0: {out: "Stack #4 created with 2 PRs\n"},
	}}
	n, err := StackLink(context.Background(), rr.run, "/wt", "main", []string{"feat-a", "feat-b"})
	if err != nil {
		t.Fatalf("StackLink: %v", err)
	}
	if n != 4 {
		t.Fatalf("n = %d, want 4", n)
	}
	want := []string{"stack", "link", "--base", "main", "feat-a", "feat-b"}
	if !reflect.DeepEqual(rr.calls[0].args, want) {
		t.Fatalf("args = %#v, want %#v", rr.calls[0].args, want)
	}
	if rr.calls[0].dir != "/wt" || rr.calls[0].name != "gh" {
		t.Fatalf("dir/name = %q/%q, want /wt/gh", rr.calls[0].dir, rr.calls[0].name)
	}
}

func TestStackLinkNoNumberInOutput(t *testing.T) {
	rr := &recordingRunner{responses: map[int]response{
		0: {out: "Stack linked successfully\n"},
	}}
	n, err := StackLink(context.Background(), rr.run, "/wt", "main", []string{"feat-a"})
	if err != nil {
		t.Fatalf("StackLink: %v", err)
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0", n)
	}
}

func TestStackLinkEmptyBranches(t *testing.T) {
	if _, err := StackLink(context.Background(), neverCalled(t), "/wt", "main", nil); err == nil {
		t.Fatal("expected error for empty branches")
	}
}

func TestStackLinkEmptyBranchName(t *testing.T) {
	if _, err := StackLink(context.Background(), neverCalled(t), "/wt", "main", []string{"feat-a", "   "}); err == nil {
		t.Fatal("expected error for blank branch name")
	}
}

func TestStackViewDecode(t *testing.T) {
	const stdout = `{
		"trunk": "main",
		"currentBranch": "feat-b",
		"branches": [
			{"name":"feat-a","base":"aaa111","isCurrent":false,"isMerged":false,"isQueued":false,"needsRebase":false},
			{"name":"feat-b","base":"bbb222","isCurrent":true,"isMerged":false,"isQueued":true,"needsRebase":true}
		]
	}`
	rr := &recordingRunner{responses: map[int]response{0: {out: stdout}}}
	entries, err := StackView(context.Background(), rr.run, "/wt")
	if err != nil {
		t.Fatalf("StackView: %v", err)
	}
	want := []StackEntry{
		{Position: 0, Branch: "feat-a", Base: "aaa111"},
		{Position: 1, Branch: "feat-b", Base: "bbb222", IsCurrent: true, Queued: true, NeedsRebase: true},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries = %#v, want %#v", entries, want)
	}
	wantArgs := []string{"stack", "view", "--json"}
	if !reflect.DeepEqual(rr.calls[0].args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", rr.calls[0].args, wantArgs)
	}
}

func TestStackViewRunnerError(t *testing.T) {
	rr := &recordingRunner{responses: map[int]response{
		0: {err: fmt.Errorf("gh stack view --json: %w: current branch is not part of a stack", fakeExitErr{2})},
	}}
	_, err := StackView(context.Background(), rr.run, "/wt")
	if !errors.Is(err, ErrNoStack) {
		t.Fatalf("errors.Is ErrNoStack = false, err = %v", err)
	}
}

func TestStackRebaseArgv(t *testing.T) {
	rr := &recordingRunner{}
	if err := StackRebase(context.Background(), rr.run, "/wt"); err != nil {
		t.Fatalf("StackRebase: %v", err)
	}
	want := []string{"stack", "rebase"}
	if !reflect.DeepEqual(rr.calls[0].args, want) {
		t.Fatalf("args = %#v, want %#v", rr.calls[0].args, want)
	}
}

func TestStackPushArgv(t *testing.T) {
	rr := &recordingRunner{}
	if err := StackPush(context.Background(), rr.run, "/wt"); err != nil {
		t.Fatalf("StackPush: %v", err)
	}
	want := []string{"stack", "push"}
	if !reflect.DeepEqual(rr.calls[0].args, want) {
		t.Fatalf("args = %#v, want %#v", rr.calls[0].args, want)
	}
}

func TestStackSyncArgv(t *testing.T) {
	rr := &recordingRunner{}
	if err := StackSync(context.Background(), rr.run, "/wt", false); err != nil {
		t.Fatalf("StackSync: %v", err)
	}
	want := []string{"stack", "sync"}
	if !reflect.DeepEqual(rr.calls[0].args, want) {
		t.Fatalf("args = %#v, want %#v", rr.calls[0].args, want)
	}
}

func TestStackSyncArgvPrune(t *testing.T) {
	rr := &recordingRunner{}
	if err := StackSync(context.Background(), rr.run, "/wt", true); err != nil {
		t.Fatalf("StackSync: %v", err)
	}
	want := []string{"stack", "sync", "--prune"}
	if !reflect.DeepEqual(rr.calls[0].args, want) {
		t.Fatalf("args = %#v, want %#v", rr.calls[0].args, want)
	}
}

func TestExitCodeSentinels(t *testing.T) {
	cases := []struct {
		code     int
		sentinel error
	}{
		{2, ErrNoStack},
		{3, ErrStackConflict},
		{4, ErrAPIFailure},
		{6, ErrDisambiguation},
		{7, ErrRebaseInProgress},
		{8, ErrStackLocked},
		{9, ErrStackNotEnabled},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("code=%d", c.code), func(t *testing.T) {
			marker := fmt.Sprintf("original runner detail for code %d", c.code)
			rr := &recordingRunner{responses: map[int]response{
				0: {err: fmt.Errorf("gh stack rebase: %w: %s", fakeExitErr{c.code}, marker)},
			}}
			err := StackRebase(context.Background(), rr.run, "/wt")
			if err == nil {
				t.Fatal("expected error")
			}
			if !errors.Is(err, c.sentinel) {
				t.Fatalf("errors.Is sentinel = false, err = %v", err)
			}
			if !strings.Contains(err.Error(), marker) {
				t.Fatalf("error %q missing original runner text %q", err.Error(), marker)
			}
			if code, ok := ExitCode(err); !ok || code != c.code {
				t.Fatalf("ExitCode = %d,%v want %d,true", code, ok, c.code)
			}
		})
	}
}

func TestStackAssembleOrderAndArgv(t *testing.T) {
	rr := &recordingRunner{responses: map[int]response{
		3: {out: "Stack #7 created\n"},
	}}
	n, err := StackAssemble(context.Background(), rr.run, "/wt", "main", []string{"feat-a", "feat-b"})
	if err != nil {
		t.Fatalf("StackAssemble: %v", err)
	}
	if n != 7 {
		t.Fatalf("n = %d, want 7", n)
	}
	if len(rr.calls) != 4 {
		t.Fatalf("calls = %d, want 4", len(rr.calls))
	}
	wantInit := []string{"stack", "init", "feat-a", "feat-b", "--base", "main"}
	wantRebase := []string{"stack", "rebase"}
	wantPush := []string{"stack", "push"}
	wantLink := []string{"stack", "link", "--base", "main", "feat-a", "feat-b"}
	for i, want := range [][]string{wantInit, wantRebase, wantPush, wantLink} {
		if !reflect.DeepEqual(rr.calls[i].args, want) {
			t.Fatalf("call %d args = %#v, want %#v", i, rr.calls[i].args, want)
		}
	}
}

func TestStackAssembleStopsAtRebaseConflict(t *testing.T) {
	rr := &recordingRunner{responses: map[int]response{
		1: {err: fmt.Errorf("gh stack rebase: %w: conflict in feat-b", fakeExitErr{3})},
	}}
	_, err := StackAssemble(context.Background(), rr.run, "/wt", "main", []string{"feat-a", "feat-b"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrStackConflict) {
		t.Fatalf("errors.Is ErrStackConflict = false, err = %v", err)
	}
	if len(rr.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (init, rebase only — push/link must not run)", len(rr.calls))
	}
}

func TestStackAssembleEmptyBranches(t *testing.T) {
	if _, err := StackAssemble(context.Background(), neverCalled(t), "/wt", "main", nil); err == nil {
		t.Fatal("expected error for empty branches")
	}
}

func TestStackAssembleEmptyBranchName(t *testing.T) {
	if _, err := StackAssemble(context.Background(), neverCalled(t), "/wt", "main", []string{""}); err == nil {
		t.Fatal("expected error for blank branch name")
	}
}
