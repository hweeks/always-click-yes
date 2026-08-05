package engineer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
	"github.com/hweeks/always-click-yes/internal/gitops"
	"github.com/hweeks/always-click-yes/internal/mcp"
	"github.com/hweeks/always-click-yes/internal/supervisor"
)

// --- fake session ---

// fakeSession is the scripted double every test in this file drives Core
// against. Its Snapshot returns whatever cur holds *before* afterSnapshot (if
// set) prepares the next call's state — so a test's afterSnapshot hook reads
// as "having just shown the caller this, get ready to show them that".
type fakeSession struct {
	mu       sync.Mutex
	cur      Snapshot
	submits  []string
	armCalls int
	quitCall bool

	submitResult ActionResult // zero value means "accept"
	armResult    ActionResult // zero value means "accept"

	onArm         func(f *fakeSession) // runs while holding the lock, after armCalls++
	afterSnapshot func(f *fakeSession) // runs while holding the lock, after reading cur
}

func newFakeSession(initial Snapshot) *fakeSession {
	return &fakeSession{
		cur:          initial,
		submitResult: ActionResult{Accepted: true, Reason: "sent"},
		armResult:    ActionResult{Accepted: true, Reason: "armed"},
	}
}

func (f *fakeSession) Submit(text string) ActionResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submits = append(f.submits, text)
	return f.submitResult
}

func (f *fakeSession) Arm() ActionResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armCalls++
	if f.onArm != nil {
		f.onArm(f)
	}
	return f.armResult
}

func (f *fakeSession) Quit() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quitCall = true
}

func (f *fakeSession) Snapshot() Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.cur
	if f.afterSnapshot != nil {
		f.afterSnapshot(f)
	}
	return out
}

func (f *fakeSession) submitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.submits)
}

// --- fake git ---

// fakeGit stands in for gitops.Runner: it answers exactly the commands
// gitops.EnsureWorktree/CommitsAhead/Push/CreatePR issue, with no real git or
// gh binary involved. rev-parse and fetch always fail (no pre-existing
// branch, no origin), which is what steers EnsureWorktree/CommitsAhead onto
// their local-ref fallbacks.
type fakeGit struct {
	mu    sync.Mutex
	calls []string

	revListOut string
	revListErr error
	pushErr    error
	prURL      string
	prErr      error
	diffOut    string
}

func (g *fakeGit) run(_ context.Context, _ string, name string, args ...string) (string, error) {
	g.mu.Lock()
	g.calls = append(g.calls, name+" "+strings.Join(args, " "))
	g.mu.Unlock()

	switch {
	case name == "git" && len(args) >= 2 && args[0] == "rev-parse" && args[1] == "--verify":
		return "", fmt.Errorf("fakeGit: no such ref")
	case name == "git" && len(args) >= 1 && args[0] == "fetch":
		return "", fmt.Errorf("fakeGit: no origin")
	case name == "git" && len(args) >= 2 && args[0] == "worktree" && args[1] == "add":
		return "", nil
	case name == "git" && len(args) >= 1 && args[0] == "rev-list":
		return g.revListOut, g.revListErr
	case name == "git" && len(args) >= 1 && args[0] == "push":
		return "", g.pushErr
	case name == "git" && len(args) >= 1 && args[0] == "diff":
		return g.diffOut, nil
	case name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "create":
		if g.prErr != nil {
			return "", g.prErr
		}
		return g.prURL + "\n", nil
	default:
		return "", fmt.Errorf("fakeGit: unhandled command %s %v", name, args)
	}
}

func (g *fakeGit) sawCall(substr string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// --- test scaffolding ---

func testSpec() engineerwire.Spec {
	return engineerwire.Spec{
		Ticket:     "eng-42",
		Title:      "Fix the thing",
		Brief:      "Make the widget spin.",
		Success:    "widget_test.go passes",
		BaseBranch: "main",
		Branch:     "acy/eng-42-fix-the-thing",
		BudgetUSD:  10,
	}
}

// newTestCore wires a Core around a scripted fake session and a fake git
// Runner, with every timing knob turned down so tests run in milliseconds
// rather than minutes. StallIdle is deliberately generous here — long enough
// that no test but TestRunStallsAfterNudges can trip it by accident.
func newTestCore(t *testing.T, spec engineerwire.Spec, git gitops.Runner, fs *fakeSession) (*Core, *engineerwire.Journal) {
	t.Helper()
	dir := t.TempDir()
	j, err := engineerwire.Open(dir)
	if err != nil {
		t.Fatalf("engineerwire.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	cfg := Config{
		Spec:         spec,
		EngineerID:   "eng-1",
		ClonePath:    dir + "/clone",
		WorktreeDir:  dir + "/wt",
		GitRunner:    git,
		PollInterval: 2 * time.Millisecond,
		AskTimeout:   50 * time.Millisecond,
		StallIdle:    500 * time.Millisecond,
		builder: func(context.Context, supervisor.Flags) (session, func(), error) {
			return fs, func() {}, nil
		},
	}
	return NewCore(cfg, j), j
}

// journalResults returns every Result in j, in order.
func journalResults(t *testing.T, j *engineerwire.Journal) []engineerwire.Result {
	t.Helper()
	msgs, err := j.ReplayFrom(1)
	if err != nil {
		t.Fatalf("ReplayFrom: %v", err)
	}
	var out []engineerwire.Result
	for _, m := range msgs {
		if r, ok := m.(engineerwire.Result); ok {
			out = append(out, r)
		}
	}
	return out
}

// assertOneResult is the invariant every exit path must uphold: exactly one
// Result in the journal, hello still first, and nothing follows the Result.
func assertOneResult(t *testing.T, j *engineerwire.Journal) engineerwire.Result {
	t.Helper()
	msgs, err := j.ReplayFrom(1)
	if err != nil {
		t.Fatalf("ReplayFrom: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("journal is empty, want at least hello + result")
	}
	if _, ok := msgs[0].(engineerwire.Hello); !ok {
		t.Fatalf("first message = %T, want engineerwire.Hello", msgs[0])
	}
	results := journalResults(t, j)
	if len(results) != 1 {
		t.Fatalf("journal has %d Result message(s), want exactly 1: %#v", len(results), results)
	}
	last := msgs[len(msgs)-1]
	if _, ok := last.(engineerwire.Result); !ok {
		t.Fatalf("last message = %T, want engineerwire.Result (nothing may follow a Result)", last)
	}
	return results[0]
}

// --- happy path ---

func TestRunHappyPathPushesAndOpensPR(t *testing.T) {
	fs := newFakeSession(Snapshot{Ready: true, Phase: PhasePlan})
	fs.onArm = func(f *fakeSession) { f.cur.Phase = PhaseAutoRun }

	polls := 0
	fs.afterSnapshot = func(f *fakeSession) {
		if f.cur.Phase != PhaseAutoRun || f.cur.FinishOutcome != "" {
			return
		}
		polls++
		switch polls {
		case 1:
			f.cur.Tasks = []TaskRow{{ID: "t1", Title: "add spin()", Running: true}}
			f.cur.CostUSD = 0.10
		case 2:
			f.cur.Tasks = []TaskRow{{ID: "t1", Title: "add spin()", Outcome: "completed", CostUSD: 0.42}}
			f.cur.CostUSD = 0.42
		default:
			f.cur.FinishOutcome = "completed"
			f.cur.FinishSummary = "added spin() and verified with widget_test.go"
			f.cur.CostUSD = 0.50
		}
	}

	git := &fakeGit{revListOut: "3\n", prURL: "https://github.com/acme/widgets/pull/7", diffOut: "widget.go\nwidget_test.go\n"}
	spec := testSpec()
	c, j := newTestCore(t, spec, git.run, fs)

	result := c.Run(context.Background())

	if result.Outcome != "completed" {
		t.Fatalf("outcome = %q, want completed", result.Outcome)
	}
	if result.Branch != spec.Branch {
		t.Fatalf("branch = %q, want %q", result.Branch, spec.Branch)
	}
	if result.PRURL != git.prURL {
		t.Fatalf("pr url = %q, want %q", result.PRURL, git.prURL)
	}
	if result.CostUSD != 0.50 {
		t.Fatalf("cost = %v, want 0.50", result.CostUSD)
	}
	if len(result.Files) != 2 || result.Files[0] != "widget.go" {
		t.Fatalf("files = %v, want [widget.go widget_test.go]", result.Files)
	}
	if fs.armCalls != 1 {
		t.Fatalf("armCalls = %d, want 1", fs.armCalls)
	}
	if fs.submitCount() == 0 {
		t.Fatal("brief was never submitted")
	}
	if !git.sawCall("worktree add") {
		t.Fatal("EnsureWorktree never ran git worktree add")
	}
	if !git.sawCall("push") {
		t.Fatal("Push never ran")
	}
	if !git.sawCall("pr create") {
		t.Fatal("CreatePR never ran")
	}

	assertOneResult(t, j)
}

// --- no commits ---

func TestRunNoCommitsSkipsPushAndPR(t *testing.T) {
	fs := newFakeSession(Snapshot{Ready: true, Phase: PhasePlan})
	fs.onArm = func(f *fakeSession) { f.cur.Phase = PhaseAutoRun }
	fs.afterSnapshot = func(f *fakeSession) {
		if f.cur.Phase == PhaseAutoRun && f.cur.FinishOutcome == "" {
			f.cur.FinishOutcome = "abandoned"
			f.cur.FinishSummary = "nothing needed changing"
		}
	}

	git := &fakeGit{revListOut: "0\n"}
	spec := testSpec()
	c, j := newTestCore(t, spec, git.run, fs)

	result := c.Run(context.Background())

	if result.Outcome != "abandoned" {
		t.Fatalf("outcome = %q, want abandoned (the model's own verdict, preserved)", result.Outcome)
	}
	if !strings.Contains(result.Summary, "no commits") {
		t.Fatalf("summary = %q, want it to say honestly that nothing was pushed", result.Summary)
	}
	if result.Branch != "" || result.PRURL != "" {
		t.Fatalf("branch/PR should be empty with no commits, got branch=%q pr=%q", result.Branch, result.PRURL)
	}
	if git.sawCall("push") || git.sawCall("pr create") {
		t.Fatal("Push/CreatePR ran despite zero commits ahead")
	}

	assertOneResult(t, j)
}

// --- push/PR failure ---

func TestRunPushFailureReportsFailed(t *testing.T) {
	fs := newFakeSession(Snapshot{Ready: true, Phase: PhasePlan})
	fs.onArm = func(f *fakeSession) { f.cur.Phase = PhaseAutoRun }
	fs.afterSnapshot = func(f *fakeSession) {
		if f.cur.Phase == PhaseAutoRun && f.cur.FinishOutcome == "" {
			f.cur.FinishOutcome = "completed"
			f.cur.FinishSummary = "done"
		}
	}

	git := &fakeGit{revListOut: "1\n", pushErr: fmt.Errorf("remote rejected: non-fast-forward")}
	c, j := newTestCore(t, testSpec(), git.run, fs)

	result := c.Run(context.Background())

	if result.Outcome != "failed" {
		t.Fatalf("outcome = %q, want failed", result.Outcome)
	}
	if !strings.Contains(result.Summary, "non-fast-forward") {
		t.Fatalf("summary = %q, want it to carry the push error", result.Summary)
	}
	assertOneResult(t, j)
}

// --- question escalation ---

func TestAnswerResolvesEscalatedQuestion(t *testing.T) {
	fs := newFakeSession(Snapshot{Ready: true})
	c, j := newTestCore(t, testSpec(), (&fakeGit{}).run, fs)

	req := mcp.Request{
		Tool:      mcp.ToolAsk,
		ToolUseID: "toolu_1",
		Args:      []byte(`{"questions":[{"question":"Which datastore?","header":"Storage","options":[{"label":"Postgres"},{"label":"SQLite"}]}]}`),
	}
	p, reply := mcp.NewPending(req)

	if !c.interceptAsk(p) {
		t.Fatal("interceptAsk returned false; every Ask must be claimed")
	}

	msgs, err := j.ReplayFrom(1)
	if err != nil {
		t.Fatalf("ReplayFrom: %v", err)
	}
	var q *engineerwire.Question
	for _, m := range msgs {
		if qq, ok := m.(engineerwire.Question); ok {
			q = &qq
		}
	}
	if q == nil {
		t.Fatal("no Question was journaled")
	}
	if q.QuestionID != "toolu_1" {
		t.Fatalf("question_id = %q, want toolu_1", q.QuestionID)
	}
	if len(q.Questions) != 1 || q.Questions[0].Header != "Storage" {
		t.Fatalf("questions decoded wrong: %#v", q.Questions)
	}

	if !c.Answer("toolu_1", "Postgres") {
		t.Fatal("Answer reported no pending question for toolu_1")
	}

	select {
	case a := <-reply:
		if a.Text != "Postgres" {
			t.Fatalf("resolved answer = %q, want Postgres", a.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("Answer did not resolve the Pending")
	}

	if c.Answer("toolu_1", "too late") {
		t.Fatal("Answer resolved the same question twice")
	}
}

// --- question timeout ---

func TestUnansweredQuestionFallsBackAfterTimeout(t *testing.T) {
	fs := newFakeSession(Snapshot{Ready: true})
	c, _ := newTestCore(t, testSpec(), (&fakeGit{}).run, fs)
	c.askTimeout = 15 * time.Millisecond

	req := mcp.Request{
		Tool:      mcp.ToolAsk,
		ToolUseID: "toolu_2",
		Args:      []byte(`{"questions":[{"question":"Proceed?","header":"Go","options":[{"label":"Yes"}]}]}`),
	}
	p, reply := mcp.NewPending(req)

	if !c.interceptAsk(p) {
		t.Fatal("interceptAsk returned false")
	}

	select {
	case a := <-reply:
		if a.Text != askTimeoutFallback {
			t.Fatalf("fallback answer = %q, want %q", a.Text, askTimeoutFallback)
		}
	case <-time.After(time.Second):
		t.Fatal("question never timed out")
	}

	c.mu.Lock()
	_, stillPending := c.pending["toolu_2"]
	c.mu.Unlock()
	if stillPending {
		t.Fatal("timed-out question was not dropped from the pending map")
	}
}

// --- cancel mid-run ---

func TestCancelMidRunReportsCancelled(t *testing.T) {
	fs := newFakeSession(Snapshot{Ready: true, Phase: PhasePlan})
	fs.onArm = func(f *fakeSession) {
		f.cur.Phase = PhaseAutoRun
		f.cur.Busy = true // never idle, never finishes — the only way out is Cancel
	}

	c, j := newTestCore(t, testSpec(), (&fakeGit{}).run, fs)

	done := make(chan engineerwire.Result, 1)
	go func() { done <- c.Run(context.Background()) }()

	time.Sleep(20 * time.Millisecond)
	c.Cancel("architect requested a stop")

	var result engineerwire.Result
	select {
	case result = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Cancel")
	}

	if result.Outcome != "cancelled" {
		t.Fatalf("outcome = %q, want cancelled", result.Outcome)
	}
	if !strings.Contains(result.Summary, "architect requested a stop") {
		t.Fatalf("summary = %q, want it to carry the cancel reason", result.Summary)
	}
	if !fs.quitCall {
		t.Error("session was never told to Quit")
	}

	assertOneResult(t, j)
}

// --- stall after nudges ---

func TestRunStallsAfterNudges(t *testing.T) {
	fs := newFakeSession(Snapshot{Ready: true, Phase: PhasePlan})
	fs.onArm = func(f *fakeSession) { f.cur.Phase = PhaseAutoRun }
	// Busy never goes true and Finish never comes: AUTO-RUN sits idle forever.

	spec := testSpec()
	dir := t.TempDir()
	j, err := engineerwire.Open(dir)
	if err != nil {
		t.Fatalf("engineerwire.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	git := &fakeGit{}
	cfg := Config{
		Spec:         spec,
		EngineerID:   "eng-1",
		ClonePath:    dir + "/clone",
		WorktreeDir:  dir + "/wt",
		GitRunner:    git.run,
		PollInterval: 2 * time.Millisecond,
		AskTimeout:   50 * time.Millisecond,
		StallIdle:    15 * time.Millisecond,
		builder: func(context.Context, supervisor.Flags) (session, func(), error) {
			return fs, func() {}, nil
		},
	}
	c := NewCore(cfg, j)

	result := c.Run(context.Background())

	if result.Outcome != "stalled" {
		t.Fatalf("outcome = %q, want stalled", result.Outcome)
	}
	if fs.submitCount() < 3 { // 1 brief + 2 nudges
		t.Fatalf("submits = %d, want at least 3 (brief + 2 nudges)", fs.submitCount())
	}
	if git.sawCall("push") || git.sawCall("pr create") {
		t.Fatal("a stalled run must not push or open a PR")
	}

	assertOneResult(t, j)
}

// --- build failure still journals a Result ---

func TestBuildFailureStillJournalsOneResult(t *testing.T) {
	spec := testSpec()
	dir := t.TempDir()
	j, err := engineerwire.Open(dir)
	if err != nil {
		t.Fatalf("engineerwire.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	cfg := Config{
		Spec:         spec,
		EngineerID:   "eng-1",
		ClonePath:    dir + "/clone",
		WorktreeDir:  dir + "/wt",
		GitRunner:    (&fakeGit{}).run,
		PollInterval: 2 * time.Millisecond,
		builder: func(context.Context, supervisor.Flags) (session, func(), error) {
			return nil, nil, fmt.Errorf("boom: no claude on PATH")
		},
	}
	c := NewCore(cfg, j)

	result := c.Run(context.Background())
	if result.Outcome != "failed" {
		t.Fatalf("outcome = %q, want failed", result.Outcome)
	}
	if !strings.Contains(result.Summary, "boom") {
		t.Fatalf("summary = %q, want it to carry the build error", result.Summary)
	}
	assertOneResult(t, j)
}
