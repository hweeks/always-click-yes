package engineerd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// attachHarness wires one Attach call to an io.Pipe on each side, so a test
// can write inbound lines and read outbound lines while Attach is still
// running, plus a fakeController wired to a real control socket so answer
// forwarding is exercised over the same net.Conn path production uses.
type attachHarness struct {
	dir  string
	ctrl *fakeController
	srv  *ControlServer

	inW  *io.PipeWriter
	outR *bufio.Scanner

	done chan error
}

func newAttachHarness(t *testing.T, fromSeq int64) *attachHarness {
	t.Helper()
	dir := shortTempDir(t)

	ctrl := &fakeController{}
	srv, err := ListenControl(dir, ctrl)
	if err != nil {
		t.Fatalf("ListenControl: %v", err)
	}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	h := &attachHarness{
		dir:  dir,
		ctrl: ctrl,
		srv:  srv,
		inW:  inW,
		outR: bufio.NewScanner(outR),
		done: make(chan error, 1),
	}
	h.outR.Buffer(make([]byte, 0, 64*1024), 1<<20)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	go func() {
		defer cancel()
		h.done <- Attach(ctx, dir, fromSeq, inR, outW)
		_ = outW.Close()
	}()

	t.Cleanup(func() {
		_ = inW.Close()
		_ = srv.Close()
	})
	return h
}

// nextMsg reads and decodes the next NDJSON line Attach wrote to out,
// bounded so a bug that stops delivery fails the test instead of hanging it.
func (h *attachHarness) nextMsg(t *testing.T) any {
	t.Helper()
	scanned := make(chan bool, 1)
	go func() { scanned <- h.outR.Scan() }()

	select {
	case ok := <-scanned:
		if !ok {
			t.Fatalf("out closed before delivering a message: %v", h.outR.Err())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the next message on out")
	}

	var env struct {
		Type engineerwire.Type `json:"type"`
	}
	line := h.outR.Bytes()
	if err := json.Unmarshal(line, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	switch env.Type {
	case engineerwire.TypeHello:
		var m engineerwire.Hello
		_ = json.Unmarshal(line, &m)
		return m
	case engineerwire.TypeEvent:
		var m engineerwire.Event
		_ = json.Unmarshal(line, &m)
		return m
	case engineerwire.TypeResult:
		var m engineerwire.Result
		_ = json.Unmarshal(line, &m)
		return m
	default:
		t.Fatalf("unexpected message type %q", env.Type)
		return nil
	}
}

func TestAttachReplaysFromSeq(t *testing.T) {
	dir := t.TempDir()
	j, err := engineerwire.Open(dir)
	if err != nil {
		t.Fatalf("Open journal: %v", err)
	}
	defer func() { _ = j.Close() }()
	if _, err := j.Append(engineerwire.Hello{EngineerID: "e1"}); err != nil {
		t.Fatalf("append hello: %v", err)
	}
	if _, err := j.Append(engineerwire.Event{Kind: engineerwire.EventPhase, Text: "PLAN"}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	if _, err := j.Append(engineerwire.Event{Kind: engineerwire.EventPhase, Text: "AUTO-RUN"}); err != nil {
		t.Fatalf("append event: %v", err)
	}

	// `in` is already at EOF and the journal holds no Result yet, so Attach
	// has no reason to return on its own; force it via ctx instead, purely to
	// bound the test — what's under test here is the replay content.
	in := bytes.NewReader(nil)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- Attach(ctx, dir, 2, in, &out) }()

	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not return after ctx expired")
	}

	sc := bufio.NewScanner(bytes.NewReader(out.Bytes()))
	var got []engineerwire.Event
	for sc.Scan() {
		var m engineerwire.Event
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("decode: %v (line: %s)", err, sc.Text())
		}
		got = append(got, m)
	}
	if len(got) != 2 {
		t.Fatalf("got %d replayed messages, want 2 (seq 2 and 3): %+v", len(got), got)
	}
	if got[0].Seq != 2 || got[1].Seq != 3 {
		t.Errorf("replayed seqs = %d,%d, want 2,3", got[0].Seq, got[1].Seq)
	}
}

func TestAttachFollowsLiveAppendsAndForwardsAnswers(t *testing.T) {
	h := newAttachHarness(t, 1)

	j, err := engineerwire.Open(h.dir)
	if err != nil {
		t.Fatalf("Open journal: %v", err)
	}
	defer func() { _ = j.Close() }()

	if _, err := j.Append(engineerwire.Hello{EngineerID: "e1"}); err != nil {
		t.Fatalf("append hello: %v", err)
	}
	hello, ok := h.nextMsg(t).(engineerwire.Hello)
	if !ok || hello.EngineerID != "e1" {
		t.Fatalf("first message = %+v, want the Hello", hello)
	}

	// A live append, appended after Attach was already following, must show
	// up on out without a reconnect.
	if _, err := j.Append(engineerwire.Event{Kind: engineerwire.EventLog, Text: "live"}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	ev, ok := h.nextMsg(t).(engineerwire.Event)
	if !ok || ev.Text != "live" {
		t.Fatalf("second message = %+v, want the live-appended event", ev)
	}

	// An inbound Answer line must be forwarded to the control socket.
	line, err := engineerwire.Marshal(engineerwire.Answer{QuestionID: "q1", Text: "go ahead"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := h.inW.Write(line); err != nil {
		t.Fatalf("write inbound: %v", err)
	}
	waitFor(t, func() bool { return h.ctrl.answerCount() == 1 })
	if got := h.ctrl.lastAnswer(); got.QuestionID != "q1" || got.Text != "go ahead" {
		t.Errorf("forwarded answer = %+v, want question_id=q1 text=%q", got, "go ahead")
	}

	// A Result ends the attach even though `in` is still open.
	if _, err := j.Append(engineerwire.Result{Outcome: "completed", Summary: "done"}); err != nil {
		t.Fatalf("append result: %v", err)
	}
	res, ok := h.nextMsg(t).(engineerwire.Result)
	if !ok || res.Outcome != "completed" {
		t.Fatalf("third message = %+v, want the Result", res)
	}

	select {
	case err := <-h.done:
		if err != nil {
			t.Errorf("Attach returned error %v, want nil after streaming a Result", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not return after streaming a Result")
	}
}

func TestAttachExitsOnCtxEnd(t *testing.T) {
	dir := t.TempDir()
	j, err := engineerwire.Open(dir)
	if err != nil {
		t.Fatalf("Open journal: %v", err)
	}
	defer func() { _ = j.Close() }()
	if _, err := j.Append(engineerwire.Hello{EngineerID: "e1"}); err != nil {
		t.Fatalf("append hello: %v", err)
	}

	inR, inW := io.Pipe()
	defer func() { _ = inW.Close() }()
	var out bytes.Buffer

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Attach(ctx, dir, 1, inR, &out) }()

	// Give Attach a moment to stream the backlog before ending ctx.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Error("Attach returned nil after ctx was cancelled, want context.Canceled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not return after ctx was cancelled")
	}
}

// This is the regression case for the real-hardware race: when `in` is
// already at EOF (e.g. `acy engineer attach <id> --from 1 < /dev/null`
// against a finished engineer), Attach's stdin-EOF exit condition must never
// win against draining the journal to `out`. Pre-fix, this flaked heavily —
// see the fix commit message for the observed rate — because the select
// could see `inDone` closed before the Follow goroutine had pushed the
// backlog onto its channel, and returned without ever writing the replay.
func TestAttachDrainsFullJournalWhenStdinIsAlreadyAtEOF(t *testing.T) {
	dir := t.TempDir()
	j, err := engineerwire.Open(dir)
	if err != nil {
		t.Fatalf("Open journal: %v", err)
	}
	if _, err := j.Append(engineerwire.Hello{EngineerID: "e1"}); err != nil {
		t.Fatalf("append hello: %v", err)
	}
	if _, err := j.Append(engineerwire.Event{Kind: engineerwire.EventPhase, Text: "AUTO-RUN"}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	if _, err := j.Append(engineerwire.Result{Outcome: "completed", Summary: "done"}); err != nil {
		t.Fatalf("append result: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	// `in` is already at EOF before Attach ever reads it — exactly what
	// `< /dev/null` looks like from Attach's side.
	in := bytes.NewReader(nil)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := Attach(ctx, dir, 1, in, &out); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	sc := bufio.NewScanner(bytes.NewReader(out.Bytes()))
	var types []engineerwire.Type
	for sc.Scan() {
		var env struct {
			Type engineerwire.Type `json:"type"`
		}
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v (line: %s)", err, sc.Text())
		}
		types = append(types, env.Type)
	}
	want := []engineerwire.Type{engineerwire.TypeHello, engineerwire.TypeEvent, engineerwire.TypeResult}
	if len(types) != len(want) {
		t.Fatalf("out contained %d messages %v, want the full journal %v", len(types), types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("message %d type = %q, want %q", i, types[i], want[i])
		}
	}
}

// When `in` hits EOF and the journal already holds a Result, Attach must
// return promptly on its own — no ctx expiry needed — since nothing more
// will ever be written or read. fromSeq is set past the Result's own seq so
// Follow's replay/live-follow side never re-delivers it: this isolates the
// "in EOF, already resulted" exit path from the ordinary "Result streamed"
// one, which a fromSeq covering the Result would otherwise satisfy first.
func TestAttachExitsWhenInputEOFsAndResultAlreadyJournaled(t *testing.T) {
	dir := t.TempDir()
	j, err := engineerwire.Open(dir)
	if err != nil {
		t.Fatalf("Open journal: %v", err)
	}
	if _, err := j.Append(engineerwire.Hello{EngineerID: "e1"}); err != nil {
		t.Fatalf("append hello: %v", err)
	}
	final, err := j.Append(engineerwire.Result{Outcome: "completed", Summary: "done"})
	if err != nil {
		t.Fatalf("append result: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	resultSeq := final.(engineerwire.Result).Seq

	// `in` reaches EOF as soon as Attach starts reading it.
	in := bytes.NewReader(nil)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- Attach(ctx, dir, resultSeq+1, in, &out) }()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Attach returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not return promptly with `in` at EOF and a Result already journaled")
	}
	if out.Len() != 0 {
		t.Errorf("out = %q, want nothing written (fromSeq was past the Result)", out.String())
	}
}
