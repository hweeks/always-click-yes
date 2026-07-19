package mcp

import (
	"encoding/json"
	"testing"
	"time"
)

// TestBridgeRoundTrip is the whole blocking contract in one test: the mcp child
// asks, the supervisor answers, and the child's Ask call returns that answer. If
// this breaks, claude's turn hangs.
func TestBridgeRoundTrip(t *testing.T) {
	b, err := Listen(t.TempDir())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = b.Close() }()

	// The supervisor side: answer the first question that arrives.
	go func() {
		p := <-b.Requests()
		p.Resolve(Answer{Text: "Color: " + p.Req.ToolUseID})
	}()

	got, err := Ask(b.SocketPath(), Request{
		Tool:      ToolAsk,
		ToolUseID: "tu_1",
		Args:      json.RawMessage(`{"questions":[]}`),
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.Text != "Color: tu_1" {
		t.Errorf("answer = %q, want the supervisor's reply", got.Text)
	}
}

// The request must reach the supervisor intact — the args are what the panel
// renders, and the tool name is what it switches on.
func TestBridgeCarriesRequest(t *testing.T) {
	b, err := Listen(t.TempDir())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = b.Close() }()

	got := make(chan Request, 1)
	go func() {
		p := <-b.Requests()
		got <- p.Req
		p.Resolve(Answer{Text: "ok"})
	}()

	args := `{"questions":[{"header":"Color","options":[{"label":"red"}]}]}`
	if _, err := Ask(b.SocketPath(), Request{Tool: ToolAsk, ToolUseID: "tu_9", Args: json.RawMessage(args)}); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	select {
	case r := <-got:
		if r.Tool != ToolAsk || r.ToolUseID != "tu_9" {
			t.Errorf("request = %+v, want tool=%s id=tu_9", r, ToolAsk)
		}
		if string(r.Args) != args {
			t.Errorf("args = %s, want them verbatim: %s", r.Args, args)
		}
	case <-time.After(time.Second):
		t.Fatal("the request never reached the supervisor")
	}
}

// Resolve is answer-once. A double resolve (e.g. the countdown firing just as the
// user hits Enter) must not panic or send twice.
func TestResolveOnce(t *testing.T) {
	p, reply := NewPending(Request{Tool: ToolAsk})
	p.Resolve(Answer{Text: "first"})
	p.Resolve(Answer{Text: "second"})

	if got := <-reply; got.Text != "first" {
		t.Errorf("answer = %q, want the first one", got.Text)
	}
	select {
	case got := <-reply:
		t.Errorf("a second answer was sent: %q", got.Text)
	default:
	}
}

// Abandon is idempotent: the bridge marks a dropped connection, and the UI may
// abandon the same request when its driver is swapped out.
func TestAbandonTwiceIsSafe(t *testing.T) {
	p, _ := NewPending(Request{Tool: ToolAsk})
	p.Abandon()
	p.Abandon()
	select {
	case <-p.Done():
	default:
		t.Fatal("Done() was not closed by Abandon")
	}
}

// An unreachable supervisor must surface as an error, so the mcp child can fail
// open rather than blocking claude forever on a socket nobody is listening to.
func TestAskUnreachableSocketErrors(t *testing.T) {
	if _, err := Ask(t.TempDir()+"/nope.sock", Request{Tool: ToolAsk}); err == nil {
		t.Fatal("Ask on a dead socket returned no error; the turn would hang")
	}
}
