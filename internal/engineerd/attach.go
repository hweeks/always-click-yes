package engineerd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// Attach streams dir's journal to out as NDJSON — replay from fromSeq, then
// live follow — while concurrently reading Answer/Cancel lines from in and
// forwarding them to dir's control socket. It is the plumbing behind
// `acy engineer attach`: an architect's one connection to a detached
// engineer, in both directions at once.
//
// It returns once any of three things happens: a Result message is streamed
// out (only once the drain that produced it has actually flushed to out),
// in reaches EOF and no Result can ever reach out from fromSeq (nothing more
// will ever be written that this attach hasn't already skipped past, so
// there is nothing left to wait for), or ctx ends. Stdin EOF never races the
// journal drain to decide which one wins: it only disarms forwarding.
func Attach(ctx context.Context, dir string, fromSeq int64, in io.Reader, out io.Writer) error {
	j, err := engineerwire.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = j.Close() }()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	inDone := make(chan struct{})
	go func() {
		defer close(inDone)
		forwardInbound(dir, in)
	}()

	bw := bufio.NewWriter(out)
	defer func() { _ = bw.Flush() }()

	// The journal stream is the primary loop: everything up to and including
	// a streamed Result must reach out before Attach can return on that
	// account. Stdin EOF never short-circuits that drain — it only stops
	// forwarding, and arms an exit *once nothing more can ever reach ch*.
	ch := j.Follow(ctx, fromSeq)
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			line, err := engineerwire.Marshal(msg)
			if err != nil {
				alog.Printf("engineerd: attach: marshal %T: %v", msg, err)
				continue
			}
			if _, err := bw.Write(line); err != nil {
				return err
			}
			if err := bw.Flush(); err != nil {
				return err
			}
			if _, ok := msg.(engineerwire.Result); ok {
				return nil
			}
		case <-inDone:
			// The input side is done — `in` hit EOF or a read past recovery.
			// That only ever stops forwarding. If a Result is unreachable
			// from fromSeq — journaled, but strictly before fromSeq, so
			// Follow will never re-deliver it — there is nothing left this
			// attach could ever produce, so exit now. Otherwise a Result is
			// either still to be drained off ch or hasn't been written yet
			// (the engineer may still be running with nobody left to
			// answer it): either way, disarm this case and keep following;
			// the case above is what returns once a streamed Result actually
			// reaches out.
			unreachable, err := resultUnreachable(j, fromSeq)
			if err != nil {
				alog.Printf("engineerd: attach: checking for a result: %v", err)
			}
			if unreachable {
				return nil
			}
			inDone = nil // already closed: never select it again
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// resultUnreachable reports whether dir's journal will never deliver a
// Result to this Follow session: one was journaled, but at a seq strictly
// before fromSeq, so Follow — which only ever emits seq >= fromSeq — has
// already skipped it and will not produce it now or later. It returns false
// both when a Result is still due (its seq is >= fromSeq) and when none has
// been journaled yet (the engineer may still be running).
func resultUnreachable(j *engineerwire.Journal, fromSeq int64) (bool, error) {
	fromHere, err := j.ReplayFrom(fromSeq)
	if err != nil {
		return false, err
	}
	for _, m := range fromHere {
		if _, ok := m.(engineerwire.Result); ok {
			return false, nil
		}
	}
	all, err := j.ReplayFrom(1)
	if err != nil {
		return false, err
	}
	for _, m := range all {
		if _, ok := m.(engineerwire.Result); ok {
			return true, nil
		}
	}
	return false, nil
}

// forwardInbound reads NDJSON lines from in until it hits EOF, forwarding
// each Answer/Cancel to dir's control socket. A malformed line, or a line
// naming a message type this side does not forward, is logged and skipped.
// A failed send (the engineer has already finished and torn its socket down)
// is also just logged: that is not this loop's problem to solve, and there
// may be more input still queued behind it.
func forwardInbound(dir string, in io.Reader) {
	defer alog.Recover("engineerd.attach.forwardInbound")

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var env struct {
			Type engineerwire.Type `json:"type"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			alog.Printf("engineerd: attach: malformed inbound line: %v", err)
			continue
		}

		var msg any
		switch env.Type {
		case engineerwire.TypeAnswer:
			var m engineerwire.Answer
			if err := json.Unmarshal(line, &m); err != nil {
				alog.Printf("engineerd: attach: malformed answer: %v", err)
				continue
			}
			msg = m
		case engineerwire.TypeCancel:
			var m engineerwire.Cancel
			if err := json.Unmarshal(line, &m); err != nil {
				alog.Printf("engineerd: attach: malformed cancel: %v", err)
				continue
			}
			msg = m
		default:
			alog.Printf("engineerd: attach: ignoring inbound message type %q", env.Type)
			continue
		}

		if err := SendControl(dir, msg); err != nil {
			alog.Printf("engineerd: attach: forwarding to control socket: %v", err)
		}
	}
}
