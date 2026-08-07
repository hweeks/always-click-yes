package fleet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGHRunner is a gitops.Runner (by method value, same shape) that returns
// queued responses in call order; once the queue is exhausted it repeats the
// last response so a caller that polls more than it queued still gets
// something deterministic.
type fakeGHRunner struct {
	mu    sync.Mutex
	calls int
	resp  []fakeGHResponse
}

type fakeGHResponse struct {
	out string
	err error
}

func (f *fakeGHRunner) run(_ context.Context, _ string, _ string, _ ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.calls++
	if len(f.resp) == 0 {
		return "[]", nil
	}
	if i >= len(f.resp) {
		i = len(f.resp) - 1
	}
	return f.resp[i].out, f.resp[i].err
}

func (f *fakeGHRunner) queue(out string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resp = append(f.resp, fakeGHResponse{out, err})
}

func (f *fakeGHRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeClock is an injectable clock a test advances by hand, so Refresh's
// 10s rate limit is deterministic rather than racing a real timer.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func drainPREvent(t *testing.T, w *PRWatcher, timeout time.Duration) PREvent {
	t.Helper()
	select {
	case ev := <-w.Events():
		return ev
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a PREvent")
	}
	panic("unreachable")
}

func assertNoPREvent(t *testing.T, w *PRWatcher) {
	t.Helper()
	select {
	case ev := <-w.Events():
		t.Fatalf("unexpected PREvent: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// --- diffing ---

// The very first poll only establishes a baseline: PRs that already existed
// before the watcher started must not flood the architect with a synthetic
// "opened" notice for every one of them.
func TestPRWatcherFirstPollIsSilentBaseline(t *testing.T) {
	gh := &fakeGHRunner{}
	gh.queue(`[{"url":"https://example/pr/1","state":"OPEN","headRefName":"acy/a-x","number":1}]`, nil)
	w := NewPRWatcher("/repo", gh.run, time.Minute, nil)

	if err := w.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	assertNoPREvent(t, w)
	if got := w.OpenCount(); got != 1 {
		t.Errorf("OpenCount() after baseline poll = %d, want 1", got)
	}
}

func TestPRWatcherDiffing(t *testing.T) {
	gh := &fakeGHRunner{}
	w := NewPRWatcher("/repo", gh.run, time.Minute, nil)

	// Baseline: nothing open yet.
	gh.queue(`[]`, nil)
	if err := w.poll(context.Background()); err != nil {
		t.Fatalf("poll 1 (baseline): %v", err)
	}
	assertNoPREvent(t, w)

	// A new PR opens.
	gh.queue(`[{"url":"https://example/pr/1","state":"OPEN","headRefName":"acy/a-x","number":1}]`, nil)
	if err := w.poll(context.Background()); err != nil {
		t.Fatalf("poll 2 (new open): %v", err)
	}
	ev := drainPREvent(t, w, time.Second)
	if ev.State != "open" || ev.Head != "acy/a-x" || ev.URL != "https://example/pr/1" || ev.Number != 1 {
		t.Fatalf("new-open event = %+v", ev)
	}

	// No change: silence.
	if err := w.poll(context.Background()); err != nil {
		t.Fatalf("poll 3 (no change): %v", err)
	}
	assertNoPREvent(t, w)

	// The PR merges, and a second one opens in the same poll.
	gh.queue(`[
		{"url":"https://example/pr/1","state":"MERGED","headRefName":"acy/a-x","number":1},
		{"url":"https://example/pr/2","state":"OPEN","headRefName":"acy/b-y","number":2}
	]`, nil)
	if err := w.poll(context.Background()); err != nil {
		t.Fatalf("poll 4 (merge + new open): %v", err)
	}
	seen := map[string]PREvent{}
	seen[drainPREvent(t, w, time.Second).Head] = PREvent{}
	seen[drainPREvent(t, w, time.Second).Head] = PREvent{}
	if _, ok := seen["acy/a-x"]; !ok {
		t.Error("want a merge event for acy/a-x")
	}
	if _, ok := seen["acy/b-y"]; !ok {
		t.Error("want a new-open event for acy/b-y")
	}

	// The second PR closes.
	gh.queue(`[
		{"url":"https://example/pr/1","state":"MERGED","headRefName":"acy/a-x","number":1},
		{"url":"https://example/pr/2","state":"CLOSED","headRefName":"acy/b-y","number":2}
	]`, nil)
	if err := w.poll(context.Background()); err != nil {
		t.Fatalf("poll 5 (close): %v", err)
	}
	ev = drainPREvent(t, w, time.Second)
	if ev.Head != "acy/b-y" || ev.State != "closed" {
		t.Fatalf("close event = %+v", ev)
	}
	assertNoPREvent(t, w)
}

func TestPRWatcherFiltersNonAcyHeads(t *testing.T) {
	gh := &fakeGHRunner{}
	gh.queue(`[]`, nil)
	w := NewPRWatcher("/repo", gh.run, time.Minute, nil)
	if err := w.poll(context.Background()); err != nil {
		t.Fatalf("poll 1: %v", err)
	}

	gh.queue(`[{"url":"https://example/pr/9","state":"OPEN","headRefName":"main","number":9}]`, nil)
	if err := w.poll(context.Background()); err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	assertNoPREvent(t, w)
	if got := w.OpenCount(); got != 0 {
		t.Errorf("OpenCount() with a non-acy head = %d, want 0", got)
	}
}

// --- OpenCount / OpenURLs ---

func TestPRWatcherOpenCountAndURLs(t *testing.T) {
	gh := &fakeGHRunner{}
	gh.queue(`[
		{"url":"https://example/pr/2","state":"OPEN","headRefName":"acy/b","number":2},
		{"url":"https://example/pr/1","state":"OPEN","headRefName":"acy/a","number":1},
		{"url":"https://example/pr/3","state":"MERGED","headRefName":"acy/c","number":3}
	]`, nil)
	w := NewPRWatcher("/repo", gh.run, time.Minute, nil)
	if err := w.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	if got := w.OpenCount(); got != 2 {
		t.Errorf("OpenCount() = %d, want 2", got)
	}
	want := []string{"https://example/pr/1", "https://example/pr/2"}
	got := w.OpenURLs()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("OpenURLs() = %v, want %v (sorted, merged excluded)", got, want)
	}
}

// --- stacked-PR root counting ---

// A three-deep stack of open acy/* PRs — a based on main, b based on a, c
// based on b — is one line of work landing on main through a single linear
// merge, so it must count as one against the cap: only a is a root, b and c
// are mid-stack and uncapped.
func TestPRWatcherStackCountsAsOneRoot(t *testing.T) {
	gh := &fakeGHRunner{}
	gh.queue(`[
		{"url":"https://example/pr/1","state":"OPEN","headRefName":"acy/a","baseRefName":"main","number":1},
		{"url":"https://example/pr/2","state":"OPEN","headRefName":"acy/b","baseRefName":"acy/a","number":2},
		{"url":"https://example/pr/3","state":"OPEN","headRefName":"acy/c","baseRefName":"acy/b","number":3}
	]`, nil)
	w := NewPRWatcher("/repo", gh.run, time.Minute, nil)
	if err := w.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	if got := w.OpenCount(); got != 1 {
		t.Errorf("OpenCount() = %d, want 1 (only the root)", got)
	}
	if got := w.StackedCount(); got != 2 {
		t.Errorf("StackedCount() = %d, want 2 (b and c are mid-stack)", got)
	}
	want := []string{"https://example/pr/1"}
	got := w.OpenURLs()
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("OpenURLs() = %v, want %v (root only, mid-stack excluded)", got, want)
	}
}

// Three independent, unrelated acy/* PRs (none based on another acy/*
// branch) are three separate lines of work, so all three count against the
// cap and none are stacked.
func TestPRWatcherIndependentPRsAllCountAsRoots(t *testing.T) {
	gh := &fakeGHRunner{}
	gh.queue(`[
		{"url":"https://example/pr/1","state":"OPEN","headRefName":"acy/a","baseRefName":"main","number":1},
		{"url":"https://example/pr/2","state":"OPEN","headRefName":"acy/b","baseRefName":"main","number":2},
		{"url":"https://example/pr/3","state":"OPEN","headRefName":"acy/c","baseRefName":"main","number":3}
	]`, nil)
	w := NewPRWatcher("/repo", gh.run, time.Minute, nil)
	if err := w.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	if got := w.OpenCount(); got != 3 {
		t.Errorf("OpenCount() = %d, want 3", got)
	}
	if got := w.StackedCount(); got != 0 {
		t.Errorf("StackedCount() = %d, want 0", got)
	}
}

// A PR based on a plain branch like "main" — a real, non-empty, non-acy/*
// base — must count as root just like the zero-value/empty-base case other
// tests exercise.
func TestPRWatcherMainBasedPRIsRoot(t *testing.T) {
	gh := &fakeGHRunner{}
	gh.queue(`[{"url":"https://example/pr/1","state":"OPEN","headRefName":"acy/a","baseRefName":"main","number":1}]`, nil)
	w := NewPRWatcher("/repo", gh.run, time.Minute, nil)
	if err := w.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	if got := w.OpenCount(); got != 1 {
		t.Errorf("OpenCount() = %d, want 1 (main-based PR is a root)", got)
	}
	if got := w.StackedCount(); got != 0 {
		t.Errorf("StackedCount() = %d, want 0", got)
	}
}

// A mid-stack PR that transitions from open to merged across polls must
// still produce a PREvent via the normal diffing path — counting changes,
// but which events fire (and when) does not.
func TestPRWatcherMidStackPRStillEmitsOnMerge(t *testing.T) {
	gh := &fakeGHRunner{}
	w := NewPRWatcher("/repo", gh.run, time.Minute, nil)

	// Baseline: a root and its mid-stack child, both open.
	gh.queue(`[
		{"url":"https://example/pr/1","state":"OPEN","headRefName":"acy/a","baseRefName":"main","number":1},
		{"url":"https://example/pr/2","state":"OPEN","headRefName":"acy/b","baseRefName":"acy/a","number":2}
	]`, nil)
	if err := w.poll(context.Background()); err != nil {
		t.Fatalf("poll 1 (baseline): %v", err)
	}
	assertNoPREvent(t, w)
	if got := w.StackedCount(); got != 1 {
		t.Fatalf("StackedCount() after baseline = %d, want 1", got)
	}

	// The mid-stack PR merges.
	gh.queue(`[
		{"url":"https://example/pr/1","state":"OPEN","headRefName":"acy/a","baseRefName":"main","number":1},
		{"url":"https://example/pr/2","state":"MERGED","headRefName":"acy/b","baseRefName":"acy/a","number":2}
	]`, nil)
	if err := w.poll(context.Background()); err != nil {
		t.Fatalf("poll 2 (mid-stack merge): %v", err)
	}
	ev := drainPREvent(t, w, time.Second)
	if ev.Head != "acy/b" || ev.State != "merged" || ev.Base != "acy/a" {
		t.Fatalf("mid-stack merge event = %+v", ev)
	}
	if got := w.StackedCount(); got != 0 {
		t.Errorf("StackedCount() after the merge = %d, want 0", got)
	}
}

// --- Refresh rate limit ---

func TestPRWatcherRefreshRateLimited(t *testing.T) {
	gh := &fakeGHRunner{}
	gh.queue(`[]`, nil)
	clock := newFakeClock(time.Unix(1000, 0))
	w := NewPRWatcher("/repo", gh.run, time.Minute, clock.Now)

	if err := w.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh 1: %v", err)
	}
	if got := gh.callCount(); got != 1 {
		t.Fatalf("gh calls after first Refresh = %d, want 1", got)
	}

	// Spammed immediately: no real poll.
	for range 5 {
		if err := w.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh (spammed): %v", err)
		}
	}
	if got := gh.callCount(); got != 1 {
		t.Errorf("gh calls after spamming Refresh = %d, want still 1 (rate-limited)", got)
	}

	// Past the window: a real poll happens again.
	clock.Advance(11 * time.Second)
	if err := w.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh after the window: %v", err)
	}
	if got := gh.callCount(); got != 2 {
		t.Errorf("gh calls after the window elapsed = %d, want 2", got)
	}
}

// --- error tolerance ---

func TestPRWatcherPollErrorTolerated(t *testing.T) {
	gh := &fakeGHRunner{}
	gh.queue(``, errors.New("gh: not authenticated"))
	w := NewPRWatcher("/repo", gh.run, time.Minute, nil)

	err := w.poll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("poll error = %v, want it to name the gh failure", err)
	}
	if got := w.OpenCount(); got != 0 {
		t.Errorf("OpenCount() after a failed poll = %d, want 0 (untouched)", got)
	}
	assertNoPREvent(t, w)

	// A later successful poll still works — the failure did not wedge
	// the watcher into thinking it already has a baseline snapshot from
	// nothing, nor leave it permanently broken.
	gh.queue(`[{"url":"https://example/pr/1","state":"OPEN","headRefName":"acy/a","number":1}]`, nil)
	if err := w.poll(context.Background()); err != nil {
		t.Fatalf("poll after recovery: %v", err)
	}
}

func TestPRWatcherMalformedJSONTolerated(t *testing.T) {
	gh := &fakeGHRunner{}
	gh.queue(`not json`, nil)
	w := NewPRWatcher("/repo", gh.run, time.Minute, nil)

	err := w.poll(context.Background())
	if err == nil {
		t.Fatal("poll with malformed JSON: want error, got nil")
	}
	if got := w.OpenCount(); got != 0 {
		t.Errorf("OpenCount() after malformed JSON = %d, want 0", got)
	}
	assertNoPREvent(t, w)
}

// --- Run loop ---

// Run must keep polling on interval and tolerate a poll error without ever
// stopping the loop — GitHub being briefly down must not kill a week-long
// architect run.
func TestPRWatcherRunPollsAndTolerantOfErrors(t *testing.T) {
	gh := &fakeGHRunner{}
	gh.queue(`[]`, nil)                          // initial poll on Run's start
	gh.queue(``, errors.New("gh: rate limited")) // a tick that fails
	gh.queue(`[{"url":"https://example/pr/1","state":"OPEN","headRefName":"acy/a","number":1}]`, nil)

	w := NewPRWatcher("/repo", gh.run, 20*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	waitFor(t, time.Second, func() bool { return gh.callCount() >= 3 })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
	}

	if got := w.OpenCount(); got != 1 {
		t.Errorf("OpenCount() after Run recovered = %d, want 1", got)
	}
}
