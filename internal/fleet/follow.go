package fleet

import (
	"context"
	"io"
	"math/rand"
	"sync"
	"time"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

const (
	initialBackoff = time.Second
	maxBackoff     = 60 * time.Second
)

// backoffSleep waits out one reattach backoff, jittered up to +20%, or
// returns early if ctx ends first. Replaced in tests so the reattach loop's
// backoff schedule can be exercised without waiting on it in real time.
var backoffSleep = func(ctx context.Context, d time.Duration) {
	jitter := time.Duration(rand.Int63n(int64(d)/5 + 1)) //nolint:gosec // jitter, not a security context
	timer := time.NewTimer(d + jitter)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// Follow attaches to engineer id via t and keeps onMsg fed with its journal
// forever: on anything short of a clean Result — the process dying, the
// connection dropping, an ssh session ending — it reattaches from the
// highest seq it has seen, with an exponential backoff (1s doubling, capped
// at 60s) between attempts. It returns nil once a Result message arrives, or
// ctx.Err() once ctx ends.
//
// answers carries inbound Answer/Cancel messages (engineerwire.Answer /
// engineerwire.Cancel, boxed) to forward to the engineer. One that arrives
// while no attach is live is buffered and delivered as soon as the next
// attach's stdin pipe is open, in the order it arrived.
//
// onReconnect fires once per reattach (never for the first attach), with
// the seq being resumed from and the attempt number, so a caller can surface
// "dropped, replayed N".
func Follow(
	ctx context.Context,
	t Transport,
	id string,
	fromSeq int64,
	answers <-chan any,
	onMsg func(any),
	onReconnect func(gap int64, attempt int),
) error {
	var mu sync.Mutex
	var pending []any
	go bufferAnswers(ctx, answers, &mu, &pending)

	lastSeq := fromSeq - 1
	backoff := initialBackoff
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		if attempt > 0 {
			backoffSleep(ctx, backoff)
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			onReconnect(lastSeq, attempt)
		}

		seen, gotResult, err := attachOnce(ctx, t, id, lastSeq+1, &mu, &pending, onMsg)
		if seen > lastSeq {
			lastSeq = seen
		}
		if gotResult {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			alog.Printf("fleet: follow %s: attach ended, reattaching from seq %d: %v", id, lastSeq+1, err)
		}
	}
}

// bufferAnswers drains answers into pending (guarded by mu) for the whole
// life of Follow, independent of whether an attach is currently live —
// that is what makes an answer sent while disconnected survive to the next
// attach.
func bufferAnswers(ctx context.Context, answers <-chan any, mu *sync.Mutex, pending *[]any) {
	defer alog.Recover("fleet.Follow.bufferAnswers")
	for {
		select {
		case a, ok := <-answers:
			if !ok {
				return
			}
			mu.Lock()
			*pending = append(*pending, a)
			mu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

// attachOnce runs one Transport.Attach call, forwarding buffered (and newly
// arriving) answers into its stdin pipe, and reports the highest seq
// observed and whether a Result was seen.
func attachOnce(
	ctx context.Context,
	t Transport,
	id string,
	fromSeq int64,
	mu *sync.Mutex,
	pending *[]any,
	onMsg func(any),
) (highestSeq int64, gotResult bool, err error) {
	pr, pw := io.Pipe()
	attachCtx, cancel := context.WithCancel(ctx)

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		forwardAnswers(attachCtx, pw, mu, pending)
	}()

	highestSeq = fromSeq - 1
	err = t.Attach(attachCtx, id, fromSeq, pr, func(msg any) {
		if s, ok := seqOf(msg); ok && s > highestSeq {
			highestSeq = s
		}
		onMsg(msg)
		if _, ok := msg.(engineerwire.Result); ok {
			gotResult = true
		}
	})

	cancel()
	_ = pw.Close()
	_ = pr.Close()
	<-writerDone
	return highestSeq, gotResult, err
}

// forwardAnswers drains pending into pw as marshaled NDJSON lines until ctx
// ends, polling for newly queued items. attachOnce tears its pipe down
// (unblocking any in-flight Write) the moment its Attach call returns, so
// this never outlives the attempt it belongs to.
func forwardAnswers(ctx context.Context, pw io.WriteCloser, mu *sync.Mutex, pending *[]any) {
	defer alog.Recover("fleet.Follow.forwardAnswers")
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		mu.Lock()
		items := *pending
		*pending = nil
		mu.Unlock()

		for _, item := range items {
			line, err := engineerwire.Marshal(item)
			if err != nil {
				alog.Printf("fleet: follow: marshal %T: %v", item, err)
				continue
			}
			if _, err := pw.Write(line); err != nil {
				alog.Printf("fleet: follow: forwarding inbound message: %v", err)
				return
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// seqOf extracts the seq field common to every outbound engineerwire
// message.
func seqOf(msg any) (int64, bool) {
	switch m := msg.(type) {
	case engineerwire.Hello:
		return m.Seq, true
	case engineerwire.Event:
		return m.Seq, true
	case engineerwire.Question:
		return m.Seq, true
	case engineerwire.Result:
		return m.Seq, true
	default:
		return 0, false
	}
}
