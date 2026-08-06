package engineerd

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// fakeController records every Answer/Cancel call it receives, safe for
// concurrent use since ControlServer dispatches each connection on its own
// goroutine.
type fakeController struct {
	mu      sync.Mutex
	answers []engineerwire.Answer
	cancels []string
}

func (f *fakeController) Answer(questionID, text string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, engineerwire.Answer{QuestionID: questionID, Text: text})
	return true
}

func (f *fakeController) Cancel(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, reason)
}

func (f *fakeController) answerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.answers)
}

func (f *fakeController) lastAnswer() engineerwire.Answer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.answers[len(f.answers)-1]
}

func (f *fakeController) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cancels)
}

func (f *fakeController) lastCancel() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancels[len(f.cancels)-1]
}

// waitFor polls cond until it is true or 2s pass, failing the test otherwise.
// The control socket routes over goroutines and a real net.Conn, so nothing
// here is synchronous from the caller's point of view.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func TestControlServerRoutesAnswer(t *testing.T) {
	dir := shortTempDir(t)
	ctrl := &fakeController{}
	srv, err := ListenControl(dir, ctrl)
	if err != nil {
		t.Fatalf("ListenControl: %v", err)
	}
	defer func() { _ = srv.Close() }()

	conn, err := net.Dial("unix", srv.SocketPath())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	line, err := engineerwire.Marshal(engineerwire.Answer{QuestionID: "q1", Text: "yes"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := conn.Write(line); err != nil {
		t.Fatalf("Write: %v", err)
	}

	waitFor(t, func() bool { return ctrl.answerCount() == 1 })
	if got := ctrl.lastAnswer(); got.QuestionID != "q1" || got.Text != "yes" {
		t.Errorf("routed answer = %+v, want question_id=q1 text=yes", got)
	}
}

func TestControlServerRoutesCancel(t *testing.T) {
	dir := shortTempDir(t)
	ctrl := &fakeController{}
	srv, err := ListenControl(dir, ctrl)
	if err != nil {
		t.Fatalf("ListenControl: %v", err)
	}
	defer func() { _ = srv.Close() }()

	if err := SendControl(dir, engineerwire.Cancel{Reason: "architect gave up"}); err != nil {
		t.Fatalf("SendControl: %v", err)
	}

	waitFor(t, func() bool { return ctrl.cancelCount() == 1 })
	if got := ctrl.lastCancel(); got != "architect gave up" {
		t.Errorf("routed cancel reason = %q, want %q", got, "architect gave up")
	}
}

// A malformed line must not kill the connection: the next well-formed line on
// the same connection has to still land.
func TestControlServerSkipsMalformedLines(t *testing.T) {
	dir := shortTempDir(t)
	ctrl := &fakeController{}
	srv, err := ListenControl(dir, ctrl)
	if err != nil {
		t.Fatalf("ListenControl: %v", err)
	}
	defer func() { _ = srv.Close() }()

	conn, err := net.Dial("unix", srv.SocketPath())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("not json at all\n")); err != nil {
		t.Fatalf("Write malformed: %v", err)
	}
	if _, err := conn.Write([]byte(`{"type":"spec","ticket":"T1"}` + "\n")); err != nil {
		t.Fatalf("Write unknown-type: %v", err)
	}
	line, err := engineerwire.Marshal(engineerwire.Answer{QuestionID: "q2", Text: "still works"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := conn.Write(line); err != nil {
		t.Fatalf("Write good line: %v", err)
	}

	waitFor(t, func() bool { return ctrl.answerCount() == 1 })
	if got := ctrl.lastAnswer(); got.QuestionID != "q2" {
		t.Errorf("routed answer = %+v, want question_id=q2", got)
	}
}

// Multiple concurrent connections must all be routed; nothing about the
// server serializes them.
func TestControlServerHandlesConcurrentConnections(t *testing.T) {
	dir := shortTempDir(t)
	ctrl := &fakeController{}
	srv, err := ListenControl(dir, ctrl)
	if err != nil {
		t.Fatalf("ListenControl: %v", err)
	}
	defer func() { _ = srv.Close() }()

	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = SendControl(dir, engineerwire.Answer{QuestionID: fmt.Sprintf("q%d", i), Text: "x"})
		}(i)
	}
	wg.Wait()

	waitFor(t, func() bool { return ctrl.answerCount() == n })
}
