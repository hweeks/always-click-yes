// Package hub runs a ui.Model without a terminal.
//
// It exists because acy has two front ends and only one state machine. The TUI
// gets its runtime from tea.NewProgram, which wants a TTY and owns its own
// goroutine; everything else — the live e2e suite, and the HTTP server that
// feeds the VS Code webview — needs to *interleave* with the model instead:
// send an action, wait for a phase, read what was written to disk.
//
// That runtime is small enough to own outright. Update is a pure function of
// (model, msg), so a plain loop over the commands it returns is the whole of
// it. The e2e harness had exactly that loop hand-rolled inside it; this package
// is that loop promoted, so there is one headless runtime rather than two.
//
// What the Hub adds over the loop is the frame stream. The model ticks at 120ms
// only while a countdown or working animation is live. Those cosmetic ticks do
// not build a Frame at all; semantic changes are marshalled once and identical
// bytes are still suppressed as the final guard. Change detection and the exact
// bytes an HTTP server writes therefore come out of the same step.
package hub

import (
	"bytes"
	"encoding/json"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/ui"
)

const (
	// msgBuffer is the inbox depth. Deep enough that a burst of driver events or
	// keystrokes never blocks the goroutine producing them, shallow enough that a
	// wedged loop is noticed rather than silently queued against.
	msgBuffer = 256

	// actionTimeout bounds Do. The model answers an action synchronously inside
	// one Update, so reaching this means the loop is wedged — and an HTTP handler
	// waiting on a wedged model must give up rather than hold its connection (and
	// its server's goroutine) forever.
	actionTimeout = 5 * time.Second

	// The synthetic terminal size. Bubble Tea's first message is always the
	// window size and the model renders nothing until one arrives, so a headless
	// run has to invent one. It only affects wrapping — no headless caller reads
	// View() — but it must be plausible: a one-column terminal would wrap every
	// transcript line to nothing.
	defaultWidth  = 100
	defaultHeight = 40
)

// Update is one frame, already encoded.
//
// The JSON is carried rather than the ui.Frame value because the Hub had to
// marshal it anyway to tell whether anything changed, and re-marshalling per
// subscriber would be the same work done twice for identical bytes.
//
// Rev counts *distinct* frames, from 1. It is not a message counter: an idle
// run holds its Rev steady for as long as it stays idle. A subscriber that
// missed frames sees Rev jump, which is the only way it can tell.
type Update struct {
	Rev int

	// JSON is shared by every subscriber and must not be modified. It is
	// immutable by construction — the Hub never reuses a buffer — so a reader
	// may hold it for as long as it likes.
	JSON []byte
}

// Hub owns one ui.Model and the goroutine that drives it.
type Hub struct {
	msgs chan tea.Msg

	// mu guards the model and the frame derived from it: dirty, last and rev all
	// describe whether `last` still is what `model` would marshal to, so they are
	// one piece of state and take one lock. Only the loop writes the model; Read
	// borrows it.
	mu    sync.Mutex
	model ui.Model
	rev   int
	last  []byte

	// dirty says the model has moved since last was built. Building is deferred
	// until someone actually wants a frame — see frame().
	dirty bool
	// frameBuilds is a test seam and a useful diagnostic counter: cosmetic ticks
	// must not increment it merely to rediscover byte equality.
	frameBuilds int

	// subsMu guards the subscriber set alone. It is always taken *without* mu
	// held, never nested inside it, so the two can never deadlock against each
	// other.
	subsMu sync.Mutex
	subs   map[*subscriber]struct{}

	stop     chan struct{} // closed by Close
	stopOnce sync.Once
	done     chan struct{} // closed when the loop exits
}

// subscriber is one frame stream: a one-deep mailbox, never a queue.
//
// A slow client must not be able to slow the model down, and it must not be
// able to make the model hold *old* state for it either. So a frame that is
// still undelivered is replaced by the newer one: a stalled subscriber may miss
// intermediate frames, and can never miss the current one.
type subscriber struct {
	ch chan Update

	// lastRev is the newest revision this subscriber has been offered. It is
	// guarded by the Hub's subsMu, which every offer is made under.
	lastRev int
}

// offer delivers u, displacing an undelivered frame if there is one, and skips
// a revision this subscriber has already been offered.
//
// The skip matters at exactly one moment: Subscribe registers a subscriber and
// *then* primes it, so a frame published in the gap reaches it by both routes.
// Without this a client's first two events would be the same frame under the
// same id — harmless to render, but it would make "the id advanced" stop
// meaning "something happened".
//
// It cannot block: the Hub is the only sender, so after the drain below the
// buffer is empty and the send succeeds. (A reader taking the value in between
// leaves it empty too — either way the next send lands.)
func (s *subscriber) offer(u Update) {
	if u.Rev <= s.lastRev {
		return
	}
	s.lastRev = u.Rev
	for {
		select {
		case s.ch <- u:
			return
		default:
		}
		select {
		case <-s.ch: // drop the frame nobody read; it is already stale
		default:
		}
	}
}

// New starts a Hub around m.
//
// The ordering here is load-bearing and is the one the e2e harness established:
// Init reads the model, so its command is taken *before* the loop goroutine can
// start writing to it; the synthetic window size goes in first, because the
// model stays unready (and renders nothing) until one arrives; and the init
// command is executed last, so the events it subscribes to arrive at a model
// that is already laid out.
func New(m ui.Model) *Hub {
	h := &Hub{
		model: m,
		msgs:  make(chan tea.Msg, msgBuffer),
		subs:  make(map[*subscriber]struct{}),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
		// Nothing has been built yet, so the very first ask must build: a
		// subscriber that connects before the model has processed a single message
		// still has to be handed a frame.
		dirty: true,
	}
	init := m.Init()
	go h.loop()

	h.Send(tea.WindowSizeMsg{Width: defaultWidth, Height: defaultHeight})
	h.exec(init)
	return h
}

// loop is the event loop: apply a message, publish the frame if it changed, run
// whatever commands came back.
func (h *Hub) loop() {
	defer close(h.done)
	alog.Printf("hub: started")
	for {
		select {
		case <-h.stop:
			alog.Printf("hub: stopped")
			return
		case msg := <-h.msgs:
			// A QuitMsg is the model saying the run is over — /quit, ctrl+c, or a
			// Quit action. tea.Program would tear the terminal down here; the Hub
			// simply stops, and Done tells everyone waiting.
			if _, ok := msg.(tea.QuitMsg); ok {
				alog.Printf("hub: quit")
				return
			}
			h.mu.Lock()
			before := h.model
			next, cmd := h.model.Update(msg)
			h.model = next.(ui.Model)
			// Marked, not built. Building a Frame and marshalling it is real work —
			// a transcript's worth of entries, every one copied — and cosmetic clock
			// ticks are filtered before they can mark it.
			if ui.FrameChangedByUpdate(before, h.model, msg) {
				h.dirty = true
			}
			h.mu.Unlock()

			h.publish()
			h.exec(cmd)
		}
	}
}

// publish hands the current frame to every subscriber, and does nothing at all
// when the bytes have not changed. Idle models schedule no clock at all; while
// active, cosmetic ticks are filtered before this point and Frame carries no
// clock as a second defence.
//
// With nobody subscribed it does not even build. That is the other half of the
// same bargain: `acy run` drives its model through tea.Program, but the live
// e2e harness and any future headless caller drive it through here, and a run
// nobody is watching should cost what the terminal's does.
func (h *Hub) publish() {
	if !h.hasSubs() {
		return
	}
	u, changed := h.frame()
	if !changed {
		return
	}
	// Taken after mu was released, never inside it: subsMu is the inner lock
	// nowhere, so there is no ordering to get wrong.
	h.subsMu.Lock()
	defer h.subsMu.Unlock()
	for s := range h.subs {
		s.offer(u)
	}
}

func (h *Hub) hasSubs() bool {
	h.subsMu.Lock()
	defer h.subsMu.Unlock()
	return len(h.subs) > 0
}

// frame returns the current frame, building it if the model has moved since the
// last one, and says whether these are bytes nobody has seen before.
//
// Rev advances here and only here, so it still counts *distinct frames* even
// though frames are now built lazily: a change that happened while nobody was
// subscribed is folded into the next build rather than counted on its own. A
// subscriber therefore never sees a rev it missed the frame for.
func (h *Hub) frame() (Update, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.dirty {
		return Update{Rev: h.rev, JSON: h.last}, false
	}
	h.frameBuilds++
	blob, err := json.Marshal(h.model.Frame())
	if err != nil {
		// A frame that will not marshal is a bug in Frame, not a reason to stop
		// supervising a run — the terminal front end is unaffected. Stay dirty and
		// keep serving the last frame that did marshal.
		alog.Printf("hub: frame marshal failed: %v", err)
		return Update{Rev: h.rev, JSON: h.last}, false
	}
	h.dirty = false
	if bytes.Equal(blob, h.last) {
		return Update{Rev: h.rev, JSON: h.last}, false
	}
	h.last = blob
	h.rev++
	return Update{Rev: h.rev, JSON: blob}, true
}

// Current is the frame as of right now, built on demand.
//
// It is what a one-shot reader (GET /api/frame) asks for, and what a new
// subscriber is primed with. The JSON may be nil on a Hub that has not processed
// its first message yet — there is genuinely no frame to give — which a caller
// should treat as "not ready", not as an error.
func (h *Hub) Current() Update {
	u, _ := h.frame()
	return u
}

// exec runs a command off the loop and feeds its message back in, which is
// exactly what tea.Program does. Batched commands fan out; nil commands are
// no-ops.
func (h *Hub) exec(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		switch m := msg.(type) {
		case nil:
			return
		case tea.BatchMsg:
			for _, c := range m {
				h.exec(c)
			}
		default:
			h.Send(msg)
		}
	}()
}

// Send injects a raw tea.Msg.
//
// This is the low-level seam, and it is deliberately not the one a front end
// should reach for: a synthesised keystroke means "whatever the terminal was
// looking at", and a client is not looking at the terminal. Do is the semantic
// door. Send exists for the runtime itself (the commands the model returns) and
// for tests that need to press an actual key — the e2e suite drives Ctrl+G and
// Ctrl+X through here precisely because it is testing the key routing.
//
// It blocks only until the message is queued, and gives up if the Hub has
// stopped: a message sent to a hub that is gone is dropped, not an error.
func (h *Hub) Send(msg tea.Msg) {
	select {
	case h.msgs <- msg:
	case <-h.stop:
	case <-h.done:
	}
}

// Do performs one semantic action and waits for the model's verdict.
//
// The acknowledgement travels on the message rather than in the model (see
// ui.ActionMsg) and the model's side of the send never blocks, so the only way
// not to get an answer is for the loop itself to have stopped or wedged — both
// of which come back as a rejection with a reason, never as a hang.
func (h *Hub) Do(a ui.Action) ui.ActionResult {
	ack := make(chan ui.ActionResult, 1)
	h.Send(ui.ActionMsg{Action: a, Ack: ack})

	timer := time.NewTimer(actionTimeout)
	defer timer.Stop()
	select {
	case res := <-ack:
		return res
	case <-h.done:
		// Quit acknowledges itself and *then* stops the loop, so both cases can be
		// ready at once; prefer the real answer over the shutdown notice.
		return lastWord(ack, "the run has stopped")
	case <-timer.C:
		alog.Printf("hub: action %s timed out after %s", a.Kind, actionTimeout)
		return lastWord(ack, "the model did not answer within "+actionTimeout.String())
	}
}

// lastWord takes an answer that landed while we were giving up, and falls back
// to a refusal carrying reason.
func lastWord(ack <-chan ui.ActionResult, reason string) ui.ActionResult {
	select {
	case res := <-ack:
		return res
	default:
		return ui.ActionResult{Reason: reason}
	}
}

// Read borrows the model under the lock. Every read of run state goes through
// here: the fields are unexported, so a caller works through ui's accessors
// (Phase, Status, Transcript, …) or Frame, on a model that cannot change under
// it while fn runs.
//
// fn must not block for long and must not call back into the Hub — the loop
// takes the same lock on every message, so a slow reader stalls the run.
func (h *Hub) Read(fn func(ui.Model)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(h.model)
}

// Subscribe opens a frame stream and returns it with its unsubscribe.
//
// The channel is primed with the current frame, so a client that connects
// mid-run renders immediately rather than waiting for the run to do something.
//
// It is never closed. Closing it would hand a mid-read subscriber a zero-valued
// Update that is indistinguishable from a real one, and would put every
// unsubscribe in a race with the loop's fan-out. Shutdown travels on Done
// instead, which a subscriber selects on alongside its channel.
//
// The unsubscribe is idempotent and safe to call concurrently with a send.
func (h *Hub) Subscribe() (<-chan Update, func()) {
	s := &subscriber{ch: make(chan Update, 1)}

	h.subsMu.Lock()
	h.subs[s] = struct{}{}
	h.subsMu.Unlock()

	// Registered first, primed second, and never the other way round: a frame
	// published in between reaches a subscriber that is already in the set, and
	// the priming that follows can only re-offer the newest frame over itself. Do
	// it in the other order and a change landing in the gap is one this
	// subscriber never hears about at all.
	//
	// This is also where a run nobody was watching pays for its first frame: the
	// model may have been ticking for minutes with publish building nothing, and
	// Current builds from the model as it stands now.
	//
	// Current is called *outside* subsMu — it takes the model lock, and taking
	// that one inside subsMu is the one ordering that could deadlock against the
	// loop. offer is then made under subsMu like every other, which is what makes
	// its already-seen check safe.
	u := h.Current()
	h.subsMu.Lock()
	if u.JSON != nil {
		s.offer(u)
	}
	h.subsMu.Unlock()

	return s.ch, func() {
		// Under the same lock the fan-out holds, so an unsubscribe can never
		// interleave with an offer to this subscriber; delete on an absent key is a
		// no-op, which is what makes a second call free.
		h.subsMu.Lock()
		delete(h.subs, s)
		h.subsMu.Unlock()
	}
}

// Close stops the loop and waits for it to finish, so that when Close returns
// nothing is still touching the model. It is safe to call more than once, and
// safe to call on a Hub whose model has already quit.
//
// It deliberately does not stop the driver or the gate: the Hub did not start
// them, and a supervisor that owns those closes them itself.
func (h *Hub) Close() {
	h.stopOnce.Do(func() { close(h.stop) })
	<-h.done
}

// Done is closed once the loop has stopped — because the model quit (a
// tea.QuitMsg came back from /quit, Ctrl+C or a Quit action) or because Close
// was called. It is how a subscriber learns there will be no more frames.
func (h *Hub) Done() <-chan struct{} { return h.done }
