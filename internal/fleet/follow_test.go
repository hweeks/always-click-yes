package fleet

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// fakeTransport drives Follow's reattach loop through a scripted sequence of
// Attach calls, one step per call — no real process involved. Each step gets
// the fromSeq Follow asked for, the stdin reader Follow forwards answers on,
// and the onMsg callback to emit outbound messages through.
type fakeTransport struct {
	mu    sync.Mutex
	calls []int64 // fromSeq recorded per Attach call, in order
	steps []func(fromSeq int64, in io.Reader, onMsg func(any)) error
}

func (f *fakeTransport) Start(context.Context, engineerwire.Spec) (StartAck, error) {
	return StartAck{}, nil
}

func (f *fakeTransport) Attach(_ context.Context, _ string, fromSeq int64, in io.Reader, onMsg func(any)) error {
	f.mu.Lock()
	idx := len(f.calls)
	f.calls = append(f.calls, fromSeq)
	step := f.steps[idx]
	f.mu.Unlock()
	return step(fromSeq, in, onMsg)
}

func (f *fakeTransport) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// readOneAnswer blocks until one NDJSON line is available on in and decodes
// it as an engineerwire.Answer.
func readOneAnswer(t *testing.T, in io.Reader) engineerwire.Answer {
	t.Helper()
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		t.Fatalf("reading forwarded answer: %v", err)
	}
	var a engineerwire.Answer
	if err := json.Unmarshal([]byte(line), &a); err != nil {
		t.Fatalf("decoding forwarded answer %q: %v", line, err)
	}
	return a
}

func noopBackoff(ctx context.Context, d time.Duration) {}

func TestFollowResultEndsTheLoop(t *testing.T) {
	orig := backoffSleep
	backoffSleep = noopBackoff
	t.Cleanup(func() { backoffSleep = orig })

	ft := &fakeTransport{
		steps: []func(int64, io.Reader, func(any)) error{
			func(_ int64, _ io.Reader, onMsg func(any)) error {
				onMsg(engineerwire.Hello{Seq: 1, EngineerID: "e1"})
				onMsg(engineerwire.Result{Seq: 2, Outcome: "success", Summary: "done"})
				return nil
			},
		},
	}

	var got []any
	var reconnects int
	err := Follow(context.Background(), ft, "e1", 1, nil, func(m any) { got = append(got, m) },
		func(int64, int) { reconnects++ })
	if err != nil {
		t.Fatalf("Follow: %v", err)
	}
	if ft.callCount() != 1 {
		t.Errorf("Attach called %d times, want 1 (a clean Result must not reattach)", ft.callCount())
	}
	if reconnects != 0 {
		t.Errorf("onReconnect fired %d times, want 0", reconnects)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(got), got)
	}
}

func TestFollowMidStreamDropResumesAtRightSeq(t *testing.T) {
	orig := backoffSleep
	backoffSleep = noopBackoff
	t.Cleanup(func() { backoffSleep = orig })

	ft := &fakeTransport{
		steps: []func(int64, io.Reader, func(any)) error{
			func(_ int64, _ io.Reader, onMsg func(any)) error {
				onMsg(engineerwire.Hello{Seq: 1, EngineerID: "e1"})
				onMsg(engineerwire.Event{Seq: 2, Kind: engineerwire.EventPhase, Text: "working"})
				return errors.New("connection dropped")
			},
			func(fromSeq int64, _ io.Reader, onMsg func(any)) error {
				if fromSeq != 3 {
					t.Errorf("reattach fromSeq = %d, want 3 (lastSeq 2 + 1)", fromSeq)
				}
				onMsg(engineerwire.Event{Seq: 3, Kind: engineerwire.EventPhase, Text: "still working"})
				onMsg(engineerwire.Result{Seq: 4, Outcome: "success", Summary: "done"})
				return nil
			},
		},
	}

	var got []any
	var reconnectGap int64
	var reconnectAttempt int
	err := Follow(context.Background(), ft, "e1", 1, nil, func(m any) { got = append(got, m) },
		func(gap int64, attempt int) { reconnectGap = gap; reconnectAttempt = attempt })
	if err != nil {
		t.Fatalf("Follow: %v", err)
	}
	if ft.callCount() != 2 {
		t.Fatalf("Attach called %d times, want 2", ft.callCount())
	}
	if reconnectGap != 2 || reconnectAttempt != 1 {
		t.Errorf("onReconnect(gap=%d, attempt=%d), want (2, 1)", reconnectGap, reconnectAttempt)
	}
	if len(got) != 4 {
		t.Fatalf("got %d messages, want 4 across both attaches: %+v", len(got), got)
	}
	if r, ok := got[3].(engineerwire.Result); !ok || r.Outcome != "success" {
		t.Errorf("last message = %+v, want a successful Result", got[3])
	}
}

// A buffered answer — one that arrives while no attach is live — must be
// delivered on the next attach's stdin, in order, once reconnected.
func TestFollowBufferedAnswerDeliveredAfterReconnect(t *testing.T) {
	answers := make(chan any, 1)

	orig := backoffSleep
	backoffSleep = func(ctx context.Context, d time.Duration) {
		// The reattach backoff is exactly the window between "no attach is
		// live" and "the next attach opens its stdin pipe" — send the
		// buffered answer here to pin it to that window deterministically.
		answers <- engineerwire.Answer{QuestionID: "q1", Text: "go ahead"}
	}
	t.Cleanup(func() { backoffSleep = orig })

	var gotAnswer engineerwire.Answer
	ft := &fakeTransport{
		steps: []func(int64, io.Reader, func(any)) error{
			func(_ int64, _ io.Reader, onMsg func(any)) error {
				onMsg(engineerwire.Hello{Seq: 1, EngineerID: "e1"})
				return errors.New("connection dropped")
			},
			func(_ int64, in io.Reader, onMsg func(any)) error {
				gotAnswer = readOneAnswer(t, in) // blocks until forwardAnswers delivers it
				onMsg(engineerwire.Result{Seq: 2, Outcome: "success", Summary: "done"})
				return nil
			},
		},
	}

	err := Follow(context.Background(), ft, "e1", 1, answers, func(any) {}, func(int64, int) {})
	if err != nil {
		t.Fatalf("Follow: %v", err)
	}
	if gotAnswer.QuestionID != "q1" || gotAnswer.Text != "go ahead" {
		t.Errorf("forwarded answer = %+v, want {q1, go ahead}", gotAnswer)
	}
}

// The backoff schedule doubles from 1s and never exceeds the 60s cap, for as
// many reattach attempts as it takes.
func TestFollowBackoffCaps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var mu sync.Mutex
	var recorded []time.Duration
	orig := backoffSleep
	backoffSleep = func(_ context.Context, d time.Duration) {
		mu.Lock()
		recorded = append(recorded, d)
		n := len(recorded)
		mu.Unlock()
		if n >= 8 {
			cancel() // Follow checks ctx right after this call and stops — no 9th attach needed
		}
	}
	t.Cleanup(func() { backoffSleep = orig })

	alwaysDrop := func(_ int64, _ io.Reader, onMsg func(any)) error {
		return errors.New("dropped")
	}
	steps := make([]func(int64, io.Reader, func(any)) error, 8)
	for i := range steps {
		steps[i] = alwaysDrop
	}
	ft := &fakeTransport{steps: steps}

	err := Follow(ctx, ft, "e1", 1, nil, func(any) {}, func(int64, int) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Follow error = %v, want context.Canceled", err)
	}

	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, 60 * time.Second, 60 * time.Second,
	}
	mu.Lock()
	defer mu.Unlock()
	if len(recorded) != len(want) {
		t.Fatalf("recorded %v backoffs, want %v", recorded, want)
	}
	for i, w := range want {
		if recorded[i] != w {
			t.Errorf("backoff[%d] = %v, want %v", i, recorded[i], w)
		}
	}
}

// Follow must return promptly once ctx ends outside of a backoff wait too —
// e.g. while an attach is actually live.
func TestFollowCtxEndsDuringAttach(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})

	ft := &fakeTransport{
		steps: []func(int64, io.Reader, func(any)) error{
			func(_ int64, _ io.Reader, _ func(any)) error {
				cancel()
				<-release
				return errors.New("torn down by ctx")
			},
		},
	}

	done := make(chan error, 1)
	go func() { done <- Follow(ctx, ft, "e1", 1, nil, func(any) {}, func(int64, int) {}) }()
	close(release)

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Follow error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Follow did not return after ctx was canceled")
	}
}
