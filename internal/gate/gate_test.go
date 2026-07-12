package gate

import (
	"testing"
	"time"
)

// TestRoundTrip exercises the hook client <-> server path over a real socket.
func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	srv, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// Supervisor side: approve whatever arrives.
	go func() {
		for p := range srv.Requests() {
			if p.Input.ToolName != "Bash" {
				t.Errorf("tool_name = %q", p.Input.ToolName)
			}
			p.Resolve(Decision{Behavior: Allow, Reason: "ok"})
		}
	}()

	raw := []byte(`{"tool_name":"Bash","tool_input":{"command":"echo hi"},"tool_use_id":"t1","session_id":"s"}`)
	d, err := Ask(srv.SocketPath(), raw)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if d.Behavior != Allow || d.Reason != "ok" {
		t.Fatalf("decision = %+v", d)
	}
}

// TestDeny confirms the deny decision propagates.
func TestDeny(t *testing.T) {
	dir := t.TempDir()
	srv, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	go func() {
		for p := range srv.Requests() {
			p.Resolve(Decision{Behavior: Deny, Reason: "nope"})
		}
	}()

	d, err := Ask(srv.SocketPath(), []byte(`{"tool_name":"Edit"}`))
	if err != nil {
		t.Fatal(err)
	}
	if d.Behavior != Deny {
		t.Fatalf("want deny, got %+v", d)
	}
}

// TestResolveOnce confirms only the first decision wins.
func TestResolveOnce(t *testing.T) {
	p := &Pending{reply: make(chan Decision, 1), done: make(chan struct{})}
	p.Resolve(Decision{Behavior: Allow})
	p.Resolve(Decision{Behavior: Deny}) // must be a no-op, must not block
	select {
	case d := <-p.reply:
		if d.Behavior != Allow {
			t.Fatalf("want first decision Allow, got %+v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("no decision delivered")
	}
}
