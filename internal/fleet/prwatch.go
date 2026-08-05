package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/gitops"
)

// defaultPRPollInterval is how often Run polls gh when the caller does not
// override it.
const defaultPRPollInterval = 60 * time.Second

// prRefreshMinGap bounds how often Refresh will run a real poll — Launch
// calls it on every launch attempt once the cached count looks full, and a
// human watching a run may retry just as often, so this keeps that from
// turning into a gh call per attempt.
const prRefreshMinGap = 10 * time.Second

// prHeadPrefix is the only head-branch namespace PRWatcher tracks — every
// branch acy's own engineers create (gitops.BranchName), and nothing else in
// the repo's PR list.
const prHeadPrefix = "acy/"

// PREvent is one observed change to an acy/* PR: newly opened, or a state
// transition away from open (merged or closed).
type PREvent struct {
	URL    string
	Head   string
	Number int
	State  string // open | merged | closed
}

// prSnapshot is one head branch's PR as of the last successful poll.
type prSnapshot struct {
	URL    string
	Number int
	State  string
}

// PRWatcher polls `gh pr list` in a repo directory and diffs successive
// snapshots of acy/*-headed PRs against each other, so the rest of the fleet
// package can learn about merges and closes without shelling out itself.
// It knows nothing about launch capacity — that is Manager's WithPRWatcher —
// only what GitHub currently shows.
type PRWatcher struct {
	dir      string
	run      gitops.Runner
	interval time.Duration
	clock    func() time.Time

	// pollMu serializes every real gh call — Run's ticker and any number of
	// concurrent Refresh callers — so two polls can never race the diff
	// against w.last, and so Refresh's rate limit check-and-poll is atomic.
	pollMu sync.Mutex

	mu           sync.Mutex
	last         map[string]prSnapshot // head branch -> snapshot, as of the last successful poll
	lastPoll     time.Time
	polled       bool
	bootstrapped bool // true once the first poll has established a baseline

	events chan PREvent
}

// NewPRWatcher builds a watcher over the repo at dir. interval <= 0 defaults
// to 60s; clock nil defaults to time.Now — tests inject a fake so Refresh's
// rate limit is deterministic.
func NewPRWatcher(dir string, run gitops.Runner, interval time.Duration, clock func() time.Time) *PRWatcher {
	if interval <= 0 {
		interval = defaultPRPollInterval
	}
	if clock == nil {
		clock = time.Now
	}
	return &PRWatcher{
		dir:      dir,
		run:      run,
		interval: interval,
		clock:    clock,
		last:     map[string]prSnapshot{},
		events:   make(chan PREvent, defaultEventsBuffer),
	}
}

// Events is the stream of new-open and state-change PREvents. Created once;
// never closed (Run simply stops sending when ctx ends).
func (w *PRWatcher) Events() <-chan PREvent { return w.events }

// OpenCount is how many acy/* PRs were open as of the last poll.
func (w *PRWatcher) OpenCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, s := range w.last {
		if s.State == "open" {
			n++
		}
	}
	return n
}

// OpenURLs is the URLs of every acy/* PR open as of the last poll, sorted
// for a deterministic refusal message.
func (w *PRWatcher) OpenURLs() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	urls := make([]string, 0, len(w.last))
	for _, s := range w.last {
		if s.State == "open" {
			urls = append(urls, s.URL)
		}
	}
	sort.Strings(urls)
	return urls
}

// ghPR is one entry of `gh pr list --json url,state,headRefName,number`.
type ghPR struct {
	URL         string `json:"url"`
	State       string `json:"state"`
	HeadRefName string `json:"headRefName"`
	Number      int    `json:"number"`
}

// poll runs one real gh pr list and diffs it against the last snapshot.
func (w *PRWatcher) poll(ctx context.Context) error {
	w.pollMu.Lock()
	defer w.pollMu.Unlock()
	return w.pollLocked(ctx)
}

// Refresh is a synchronous poll on demand, internally rate-limited to at
// most one real poll per prRefreshMinGap — Launch calls this before refusing
// on a full-looking cache, and callers may call it as often as they like.
// The check and the poll happen under the same pollMu hold, so a burst of
// concurrent Refresh calls still produces at most one real gh call per
// window rather than racing the check.
func (w *PRWatcher) Refresh(ctx context.Context) error {
	w.pollMu.Lock()
	defer w.pollMu.Unlock()

	w.mu.Lock()
	skip := w.polled && w.clock().Sub(w.lastPoll) < prRefreshMinGap
	w.mu.Unlock()
	if skip {
		return nil
	}
	return w.pollLocked(ctx)
}

// Run polls on interval until ctx ends. A poll error — gh down, or a
// malformed reply — is logged and skipped rather than returned: GitHub being
// briefly unreachable must not end a week-long architect run.
func (w *PRWatcher) Run(ctx context.Context) {
	poll := func() {
		if err := w.poll(ctx); err != nil {
			alog.Printf("fleet: pr watcher poll failed: %v", err)
		}
	}
	poll()

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		}
	}
}

// pollLocked does the actual work; callers must hold pollMu.
func (w *PRWatcher) pollLocked(ctx context.Context) error {
	out, err := w.run(ctx, w.dir, "gh", "pr", "list", "--state", "all",
		"--json", "url,state,headRefName,number", "--limit", "100")

	w.mu.Lock()
	w.lastPoll = w.clock()
	w.polled = true
	w.mu.Unlock()

	if err != nil {
		return fmt.Errorf("fleet: gh pr list: %w", err)
	}

	var prs []ghPR
	if jsonErr := json.Unmarshal([]byte(out), &prs); jsonErr != nil {
		return fmt.Errorf("fleet: parsing gh pr list output: %w", jsonErr)
	}

	next := make(map[string]prSnapshot, len(prs))
	for _, pr := range prs {
		if !strings.HasPrefix(pr.HeadRefName, prHeadPrefix) {
			continue
		}
		next[pr.HeadRefName] = prSnapshot{URL: pr.URL, Number: pr.Number, State: strings.ToLower(pr.State)}
	}

	w.mu.Lock()
	prev := w.last
	bootstrap := !w.bootstrapped
	w.last = next
	w.bootstrapped = true
	w.mu.Unlock()

	// The first poll only establishes a baseline: PRs that already existed
	// before the watcher started are not "new" and must not flood the
	// architect with an opened notice for every one of them.
	if bootstrap {
		return nil
	}

	for head, snap := range next {
		old, existed := prev[head]
		switch {
		case !existed:
			if snap.State == "open" {
				w.emit(PREvent{URL: snap.URL, Head: head, Number: snap.Number, State: snap.State})
			}
		case old.State != snap.State:
			w.emit(PREvent{URL: snap.URL, Head: head, Number: snap.Number, State: snap.State})
		}
	}
	return nil
}

// emit is never dropped, matching the "pr" Kind's treatment on Manager's own
// Events channel: a lost merge/close notice would leave the architect
// waiting at the cap on a PR that has already landed.
func (w *PRWatcher) emit(ev PREvent) {
	w.events <- ev
}
