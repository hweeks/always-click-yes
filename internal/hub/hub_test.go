package hub

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/gate"
	"github.com/hweeks/always-click-yes/internal/ui"
)

// The models here are built the way internal/ui's own tests build them: no
// driver, no launcher, only injected fakes. Nothing can reach a claude process,
// so the actions these tests drive are the ones that need none — Clear,
// SetModel, QueueClear — plus real gate requests pushed onto the gate channel,
// which is exactly how the gate server delivers them in production.

const (
	// settle is how long a test waits for something that should already have
	// happened. Generous, because a loaded -race build is slow and a flaky
	// timeout here would be indistinguishable from the bug it is meant to catch.
	settle = 5 * time.Second

	// quiet is how long a test watches a stream that must stay silent. The model
	// ticks every 120ms, so this is ~8 Updates that had better produce nothing.
	quiet = time.Second
)

// testHub starts a Hub and stops it when the test ends.
func testHub(t *testing.T, cfg ui.Config) *Hub {
	t.Helper()
	h := New(ui.New(nil, cfg))
	t.Cleanup(h.Close)
	return h
}

// recvFrame takes the next frame, or fails saying what it was waiting for.
func recvFrame(t *testing.T, ch <-chan Update, what string) Update {
	t.Helper()
	select {
	case u := <-ch:
		return u
	case <-time.After(settle):
		t.Fatalf("timed out after %s waiting for %s", settle, what)
		return Update{}
	}
}

// noFrame fails if anything is emitted within quiet.
func noFrame(t *testing.T, ch <-chan Update, what string) {
	t.Helper()
	select {
	case u := <-ch:
		t.Fatalf("%s: an unexpected frame arrived (rev %d): %s", what, u.Rev, u.JSON)
	case <-time.After(quiet):
	}
}

// frameOf decodes a frame back into the value it was made from.
func frameOf(t *testing.T, u Update) ui.Frame {
	t.Helper()
	var f ui.Frame
	if err := json.Unmarshal(u.JSON, &f); err != nil {
		t.Fatalf("frame rev %d does not unmarshal: %v\n%s", u.Rev, err, u.JSON)
	}
	return f
}

// lastEntry is the newest line of the transcript, which is where every action
// these tests take leaves its mark.
func lastEntry(t *testing.T, u Update) string {
	t.Helper()
	f := frameOf(t, u)
	if len(f.Entries) == 0 {
		t.Fatalf("frame rev %d has no entries", u.Rev)
	}
	return f.Entries[len(f.Entries)-1].Body
}

// bashPending is a permission request in the shape the PreToolUse hook delivers
// one, with the tool_use id that identifies it.
func bashPending(id, cmd string) (*gate.Pending, <-chan gate.Decision) {
	in := gate.PreToolUseInput{
		ToolName:  "Bash",
		ToolUseID: id,
		ToolInput: json.RawMessage(`{"command":"` + cmd + `"}`),
	}
	return gate.NewPending(in)
}

// The property this whole package exists to preserve.
//
// The model ticks every 120ms for its countdowns and its spinner, so Update runs
// constantly whether or not anything happened. ui.Frame carries no "now" for
// exactly this reason, and the Hub compares marshalled bytes — so an idle run
// must produce the frame it starts with and then nothing at all, forever. Get
// this wrong and the webview is handed eight frames a second for the lifetime of
// a run that is sitting there doing nothing.
//
// The gate at the end is the control. It is not decoration: a Hub whose loop had
// died would also emit no frames, and a countdown can only expire inside a
// tickMsg — so an auto-approval arriving after that silent second is proof the
// loop was running through all of it.
func TestIdleRunEmitsNoFramesButKeepsTicking(t *testing.T) {
	gates := make(chan *gate.Pending, 1)
	h := testHub(t, ui.Config{GateReqs: gates, Countdown: 300 * time.Millisecond})

	frames, unsub := h.Subscribe()
	defer unsub()

	first := recvFrame(t, frames, "the opening frame")
	if first.Rev != 1 {
		t.Errorf("first frame is rev %d, want 1", first.Rev)
	}

	noFrame(t, frames, "an idle run")

	p, decision := bashPending("toolu_idle", "echo hi")
	gates <- p

	gated := recvFrame(t, frames, "the gate to reach the model")
	if got := frameOf(t, gated); len(got.Gates) != 1 || got.Gates[0].ToolUseID != "toolu_idle" {
		t.Fatalf("gates = %+v, want the one we raised", got.Gates)
	}

	select {
	case d := <-decision:
		if d.Behavior != gate.Allow {
			t.Fatalf("decision = %+v, want an auto-approval", d)
		}
	case <-time.After(settle):
		t.Fatal("the countdown never expired — the loop stopped ticking during the silent second")
	}
}

// The other half of that bargain: a run nobody is watching does not build a
// frame at all.
//
// Update runs at least every 120ms, and building a Frame means copying the whole
// transcript and marshalling it. The silence test above proves nothing is
// *sent*; this proves nothing is *made*. Rev is the instrument, since it only
// moves when a frame is actually built: three changes with nobody subscribed,
// and the first frame a subscriber ever sees is still rev 1.
func TestNoSubscribersMeansNoFrameBuilding(t *testing.T) {
	h := testHub(t, ui.Config{})

	for _, name := range []string{"one", "two", "three"} {
		if res := h.Do(ui.SetModel(name)); !res.Accepted {
			t.Fatalf("SetModel(%s) was refused: %s", name, res.Reason)
		}
	}

	frames, unsub := h.Subscribe()
	defer unsub()
	first := recvFrame(t, frames, "the priming frame")
	if first.Rev != 1 {
		t.Errorf("first frame is rev %d, want 1 — a frame was built for nobody", first.Rev)
	}
	// And it is the *current* state, not a stale one: the changes that happened
	// while nobody was listening are all in the frame that was finally built.
	if body := lastEntry(t, first); !strings.Contains(body, "three") {
		t.Errorf("primed frame shows %q, want the newest state", body)
	}
}

// Current builds on demand, which is what GET /api/frame is: a reader that never
// subscribes and still has to see the run as it is right now.
func TestCurrentBuildsOnDemand(t *testing.T) {
	h := testHub(t, ui.Config{})

	first := h.Current()
	if first.Rev != 1 || first.JSON == nil {
		t.Fatalf("Current = rev %d (json nil: %v), want the opening frame", first.Rev, first.JSON == nil)
	}
	// Asking again with nothing changed is the same frame at the same revision:
	// rev counts distinct frames, and a reader polling does not invent them.
	if again := h.Current(); again.Rev != first.Rev {
		t.Errorf("a second Current = rev %d, want %d", again.Rev, first.Rev)
	}

	if res := h.Do(ui.SetModel("opus")); !res.Accepted {
		t.Fatalf("SetModel was refused: %s", res.Reason)
	}
	next := h.Current()
	if next.Rev != first.Rev+1 {
		t.Errorf("Current after a change = rev %d, want %d", next.Rev, first.Rev+1)
	}
	if !strings.Contains(string(next.JSON), "opus") {
		t.Errorf("Current does not show the change: %s", next.JSON)
	}
}

// A subscriber's first two events must not be the same frame. Subscribe
// registers before it primes — losing a frame published in that gap would be
// worse — so the duplicate has to be suppressed rather than avoided.
func TestSubscribeDoesNotDuplicateTheFirstFrame(t *testing.T) {
	h := testHub(t, ui.Config{})

	// A reader already subscribed, so the loop is publishing rather than skipping
	// the build: that is the state in which the new subscriber's priming frame
	// and the loop's fan-out can collide.
	other, unsubOther := h.Subscribe()
	defer unsubOther()
	recvFrame(t, other, "the opening frame")

	frames, unsub := h.Subscribe()
	defer unsub()
	first := recvFrame(t, frames, "the priming frame")

	// Nothing has changed, so nothing more may arrive — not even the same frame
	// again under the same id.
	noFrame(t, frames, "after priming")

	if res := h.Do(ui.SetModel("opus")); !res.Accepted {
		t.Fatalf("SetModel was refused: %s", res.Reason)
	}
	if next := recvFrame(t, frames, "the frame the action changed"); next.Rev <= first.Rev {
		t.Errorf("rev = %d, want it past the priming frame's %d", next.Rev, first.Rev)
	}
}

// A change emits exactly one frame, and the revision moves by exactly one.
func TestStateChangeEmitsOneFrame(t *testing.T) {
	h := testHub(t, ui.Config{})

	frames, unsub := h.Subscribe()
	defer unsub()
	first := recvFrame(t, frames, "the opening frame")

	if res := h.Do(ui.SetModel("opus")); !res.Accepted {
		t.Fatalf("SetModel was refused: %s", res.Reason)
	}

	u := recvFrame(t, frames, "the frame the action changed")
	if u.Rev != first.Rev+1 {
		t.Errorf("rev = %d, want %d — one action, one frame", u.Rev, first.Rev+1)
	}
	if body := lastEntry(t, u); !strings.Contains(body, "opus") {
		t.Errorf("newest entry = %q, want it to record the model change", body)
	}
	// And then nothing: the action is over, so the ticks that follow it are as
	// silent as the ones before.
	noFrame(t, frames, "after the action")
}

// A subscriber that does not read for a while and then reads once sees the
// newest state, not a stale one. The mailbox is one deep and a new frame
// displaces an undelivered one, so what is lost is the middle of the story and
// never its ending.
func TestSlowSubscriberSeesTheNewestFrame(t *testing.T) {
	h := testHub(t, ui.Config{})

	slow, unsubSlow := h.Subscribe()
	defer unsubSlow()
	fast, unsubFast := h.Subscribe()
	defer unsubFast()

	first := recvFrame(t, fast, "the opening frame")
	<-slow // the same opening frame, taken so both start level

	// Three changes, with the slow subscriber never reading. The fast one is how
	// the test knows all three have been published: without it, reading `slow`
	// straight after the last Do would race the loop's own publish.
	for _, name := range []string{"one", "two", "three"} {
		if res := h.Do(ui.SetModel(name)); !res.Accepted {
			t.Fatalf("SetModel(%s) was refused: %s", name, res.Reason)
		}
	}
	var newest Update
	for newest.Rev < first.Rev+3 {
		newest = recvFrame(t, fast, "the third change to be published")
	}

	got := recvFrame(t, slow, "the slow subscriber's single read")
	if got.Rev != newest.Rev {
		t.Errorf("slow subscriber read rev %d, want the newest (%d)", got.Rev, newest.Rev)
	}
	if body := lastEntry(t, got); !strings.Contains(body, "three") {
		t.Errorf("slow subscriber saw %q, want the newest state", body)
	}
}

// A subscriber that never reads must not be able to slow the model down, nor to
// starve a subscriber that does. There is no back-pressure on the loop at all:
// its only send is into a mailbox it is allowed to overwrite.
func TestBlockedSubscriberDoesNotStallTheLoop(t *testing.T) {
	h := testHub(t, ui.Config{})

	_, unsubBlocked := h.Subscribe() // never read from, so its mailbox stays full
	defer unsubBlocked()
	live, unsubLive := h.Subscribe()
	defer unsubLive()

	first := recvFrame(t, live, "the opening frame")

	const changes = 20
	start := time.Now()
	for i := range changes {
		if res := h.Do(ui.SetModel(string(rune('a' + i)))); !res.Accepted {
			t.Fatalf("change %d was refused: %s", i, res.Reason)
		}
	}
	// Do waits for the model's acknowledgement, so this elapsed time is the loop's
	// own. Against a 5s per-action timeout, twenty actions behind a blocked
	// subscriber finishing in well under a second is the assertion.
	if el := time.Since(start); el > settle {
		t.Errorf("%d actions took %s — the blocked subscriber back-pressured the loop", changes, el)
	}

	var newest Update
	for newest.Rev < first.Rev+changes {
		newest = recvFrame(t, live, "the reading subscriber to catch up")
	}
	if body := lastEntry(t, newest); !strings.Contains(body, string(rune('a'+changes-1))) {
		t.Errorf("newest entry = %q, want the last change", body)
	}
}

// Do hands back the model's own verdict, refusals included. A refusal is a
// normal outcome — a client describes a run it last saw a moment ago — so the
// reason has to survive the trip.
func TestDoReturnsTheActionResult(t *testing.T) {
	cases := []struct {
		name       string
		action     ui.Action
		accepted   bool
		wantReason string
	}{
		{"clear is always accepted", ui.Clear(), true, "transcript cleared"},
		{"setModel reports what it set", ui.SetModel("haiku"), true, "haiku"},
		{"an empty queue still clears", ui.QueueClear(), true, "0 queued messages"},
		// No driver and no session id: arming would have nothing to arm.
		{"arm is refused with no session", ui.Arm(), false, "no session id yet"},
		{"setModel with no name is refused", ui.SetModel("  "), false, "no model name"},
		{"an unknown kind is refused", ui.Action{Kind: "teleport"}, false, "unknown action"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := testHub(t, ui.Config{})
			res := h.Do(tc.action)
			if res.Accepted != tc.accepted {
				t.Errorf("accepted = %v, want %v (reason %q)", res.Accepted, tc.accepted, res.Reason)
			}
			if !strings.Contains(res.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", res.Reason, tc.wantReason)
			}
		})
	}
}

// A gate answered by id over the action seam resolves that gate and unblocks the
// hook — the same path the HTTP server will take, end to end through the Hub.
func TestDoAnswersAGate(t *testing.T) {
	gates := make(chan *gate.Pending, 1)
	h := testHub(t, ui.Config{GateReqs: gates, Countdown: time.Minute})

	frames, unsub := h.Subscribe()
	defer unsub()
	recvFrame(t, frames, "the opening frame")

	p, decision := bashPending("toolu_answer", "rm -rf /tmp/x")
	gates <- p
	recvFrame(t, frames, "the gate to reach the model")

	if res := h.Do(ui.GateDeny("toolu_answer")); !res.Accepted {
		t.Fatalf("GateDeny was refused: %s", res.Reason)
	}
	select {
	case d := <-decision:
		if d.Behavior != gate.Deny {
			t.Errorf("decision = %+v, want a veto", d)
		}
	case <-time.After(settle):
		t.Fatal("no decision — claude would still be blocked on the hook")
	}

	// A stale id resolves nothing, over this seam as over any other.
	if res := h.Do(ui.GateAllow("toolu_answer")); res.Accepted {
		t.Errorf("answering the same id twice was accepted: %s", res.Reason)
	}
}

// Unsubscribing twice is safe, and so is unsubscribing while the loop is busy
// publishing — the removal takes the same lock the fan-out holds.
func TestUnsubscribeIsIdempotentAndRaceFree(t *testing.T) {
	h := testHub(t, ui.Config{})

	frames, unsub := h.Subscribe()
	recvFrame(t, frames, "the opening frame")

	// Changes are still flowing while the unsubscribes land.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 10 {
			h.Do(ui.SetModel(string(rune('a' + i))))
		}
	}()

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(unsub)
	}
	wg.Wait()
	<-done

	// Nothing more is delivered to a stream nobody is subscribed to. (One frame
	// may already have been in the mailbox when the unsubscribe landed, so drain
	// that before watching.)
	select {
	case <-frames:
	default:
	}
	noFrame(t, frames, "an unsubscribed stream")
}

// Close is idempotent, and it waits: when it returns, nothing is still touching
// the model.
func TestCloseTwiceIsSafe(t *testing.T) {
	h := New(ui.New(nil, ui.Config{}))
	h.Close()
	h.Close()

	select {
	case <-h.Done():
	default:
		t.Fatal("Done is not closed after Close returned")
	}
	// A message sent to a stopped Hub is dropped, not a hang and not a panic.
	h.Send(ui.ActionMsg{Action: ui.Clear()})
	if res := h.Do(ui.Clear()); res.Accepted {
		t.Errorf("an action was accepted by a closed hub: %+v", res)
	}
}

// A subscriber parked on its channel when the Hub closes has to be able to get
// out. The channel is deliberately never closed — that would hand a mid-read
// subscriber a zero-valued Update it could not tell from a real one — so Done is
// the way out, and this is the shape every subscriber should be written in.
func TestCloseWhileSubscriberIsMidRead(t *testing.T) {
	h := New(ui.New(nil, ui.Config{}))

	frames, unsub := h.Subscribe()
	defer unsub()
	recvFrame(t, frames, "the opening frame")

	released := make(chan struct{})
	go func() {
		defer close(released)
		select {
		case <-frames:
		case <-h.Done():
		}
	}()

	h.Close()
	select {
	case <-released:
	case <-time.After(settle):
		t.Fatal("a subscriber blocked on its channel was never released by Close")
	}
}

// Done closes when the model quits, which is how a server learns the run is over
// without polling for it.
func TestDoneClosesOnQuit(t *testing.T) {
	h := testHub(t, ui.Config{}) // Cleanup calls Close, which must also be fine after a quit

	if res := h.Do(ui.Quit()); !res.Accepted {
		t.Fatalf("Quit was refused: %s", res.Reason)
	}
	select {
	case <-h.Done():
	case <-time.After(settle):
		t.Fatal("Done never closed after the model quit")
	}
}

// A subscriber that arrives mid-run is primed with the current frame, so a
// webview opened halfway through a run renders immediately instead of waiting
// for the run to do something next.
func TestSubscribeIsPrimedWithTheCurrentFrame(t *testing.T) {
	h := testHub(t, ui.Config{})

	early, unsubEarly := h.Subscribe()
	defer unsubEarly()
	recvFrame(t, early, "the opening frame")
	if res := h.Do(ui.SetModel("opus")); !res.Accepted {
		t.Fatalf("SetModel was refused: %s", res.Reason)
	}
	changed := recvFrame(t, early, "the frame the action changed")

	late, unsubLate := h.Subscribe()
	defer unsubLate()

	got := recvFrame(t, late, "the priming frame")
	if got.Rev != changed.Rev || string(got.JSON) != string(changed.JSON) {
		t.Errorf("late subscriber got rev %d, want the current frame (rev %d)", got.Rev, changed.Rev)
	}
}
