package server

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/hweeks/always-click-yes/internal/alog"
)

// Server-Sent Events, chosen over a websocket because the traffic is one-way.
//
// Frames go out; actions come back over POST /api/action, which gets to be an
// ordinary request with an ordinary response instead of a correlation id and a
// reply the client has to match up. SSE is also plain HTTP, so it inherits the
// bearer token, the CORS rules and the loopback bind rather than needing its own
// version of each.
//
// The framing is the standard one:
//
//	id: <hub rev>
//	event: frame
//	data: <the frame JSON, one line>
//
// with a blank line ending each event and a flush after every write — without
// the flush a frame sits in Go's buffer until the next one arrives, which for an
// idle run is forever.
//
// A frame's JSON comes from encoding/json and therefore contains no literal
// newline, so `data:` is always exactly one line. The id is the hub's rev, which
// counts distinct frames rather than messages: a client that sees it jump missed
// frames, and there is deliberately no replay — the newest frame is the whole
// state, so the cure for a gap is the frame that follows it.

const (
	// heartbeat keeps the connection warm. An idle acy run emits no frames at all
	// — that property is the point of the hub — so without this a proxy or a
	// sleeping laptop's NAT would drop a connection that looks dead but is
	// working exactly as designed. A comment line is not an event and no client
	// handler ever sees it.
	heartbeat = 20 * time.Second

	// eventFrame carries a new frame; eventDone says the run itself is over —
	// the model quit — as opposed to this connection ending, which a client
	// learns from the socket closing. They mean different things: one is "reopen
	// the stream", the other is "there is nothing left to reopen it to".
	eventFrame = "frame"
	eventDone  = "done"
)

// handleEvents streams frames until the client goes away, the run ends, or the
// server shuts down.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Nothing sits in front of this server today, but an SSE stream through a
	// buffering proxy is a stream that arrives all at once at the end.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		alog.Printf("server: events: cannot flush, giving up: %v", err)
		return
	}

	// Subscribed *after* the headers are out, so the client is already reading by
	// the time the priming frame is written. Subscribe primes the channel with the
	// current frame, which is why a webview opened halfway through a run renders
	// immediately instead of waiting for the run to do something next.
	frames, unsub := s.hub.Subscribe()
	defer unsub()

	beat := time.NewTicker(heartbeat)
	defer beat.Stop()

	for {
		select {
		case <-r.Context().Done():
			// The client hung up — a closed webview, a reload, a cancelled fetch.
			return
		case <-s.closing:
			// Shutdown. Without this the server's grace period would be spent
			// waiting on a handler whose entire job is to not return.
			return
		case <-s.hub.Done():
			// The run is over. Say so before leaving: a client that only saw the
			// socket close could not tell this from a crash, and would reconnect
			// forever against a supervisor that has quit.
			_ = writeEvent(w, eventDone, 0, []byte(`{"reason":"the run has ended"}`))
			_ = rc.Flush()
			return
		case u := <-frames:
			if err := writeEvent(w, eventFrame, u.Rev, u.JSON); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case <-beat.C:
			// A comment. The client's EventSource/parser ignores it; the socket
			// does not.
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// writeEvent writes one SSE event. A zero rev omits the id line, which is what
// an event that is not a frame — and so has no revision — should carry.
func writeEvent(w io.Writer, event string, rev int, data []byte) error {
	var err error
	if rev > 0 {
		_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", rev, event, data)
	} else {
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	}
	return err
}
