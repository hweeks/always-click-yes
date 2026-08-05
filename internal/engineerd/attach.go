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
// out, in reaches EOF and the journal already holds a Result (nothing more
// will ever be written, so there is nothing left to wait for), or ctx ends.
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
			// If the journal already holds a Result the engineer is done too
			// and nothing more is coming; otherwise it may still be running
			// with nobody left to answer it, so keep following.
			done, err := journalHasResult(j)
			if err != nil {
				alog.Printf("engineerd: attach: checking for a result: %v", err)
			}
			if done {
				return nil
			}
			inDone = nil // already closed: never select it again
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// journalHasResult reports whether dir's journal already contains a Result
// message, i.e. whether the engineer that wrote it has finished.
func journalHasResult(j *engineerwire.Journal) (bool, error) {
	msgs, err := j.ReplayFrom(1)
	if err != nil {
		return false, err
	}
	for _, m := range msgs {
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
