package driver

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveDriver drives a real `claude` process end-to-end. It costs a few cents
// and needs auth, so it is opt-in via ACY_LIVE=1.
//
//	ACY_LIVE=1 go test ./internal/driver/ -run TestLiveDriver -v
func TestLiveDriver(t *testing.T) {
	if os.Getenv("ACY_LIVE") == "" {
		t.Skip("set ACY_LIVE=1 to run the live driver test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	drv := New(Options{
		Model:          "sonnet",
		PermissionMode: "plan",
		ExtraArgs:      []string{"--tools", ""}, // no tools -> no side effects
	})
	if err := drv.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer drv.Stop()

	if err := drv.Send("Reply with exactly the word: pong"); err != nil {
		t.Fatalf("send: %v", err)
	}

	var gotInit, gotText, gotResult bool
	for ev := range drv.Events() {
		switch {
		case ev.IsInit():
			gotInit = true
			t.Logf("init: session=%s model=%s mode=%s", ev.SessionID, ev.Model, ev.PermissionMode)
		case ev.Type == TypeAssistant:
			for _, b := range ev.Message.Blocks() {
				if b.Type == BlockText && b.Text != "" {
					gotText = true
					t.Logf("assistant text: %q", b.Text)
				}
			}
		case ev.IsTurnEnd():
			gotResult = true
			t.Logf("result: stop=%s cost=$%.4f", ev.StopReason, ev.TotalCostUSD)
			drv.Stop() // one turn is enough; closes the stream
		}
	}

	if !gotInit || !gotText || !gotResult {
		t.Fatalf("missing events: init=%v text=%v result=%v", gotInit, gotText, gotResult)
	}
}

// TestLiveInterrupt confirms Driver.Interrupt aborts an in-flight turn. Opt-in.
//
//	ACY_LIVE=1 go test ./internal/driver/ -run TestLiveInterrupt -v
func TestLiveInterrupt(t *testing.T) {
	if os.Getenv("ACY_LIVE") == "" {
		t.Skip("set ACY_LIVE=1 to run the live interrupt test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	work := t.TempDir()
	drv := New(Options{
		Model:     "sonnet",
		Cwd:       work,
		ExtraArgs: []string{"--dangerously-skip-permissions", "--tools", "Bash"},
	})
	if err := drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer drv.Stop()

	if err := drv.Send("Using the Bash tool, create files b1.txt through b30.txt one at a time, one command per file, slowly."); err != nil {
		t.Fatal(err)
	}

	sentInterrupt := false
	var aborted bool
	for ev := range drv.Events() {
		// After the first tool result comes back, interrupt.
		if !sentInterrupt && ev.Type == TypeUser {
			for _, b := range ev.Message.Blocks() {
				if b.Type == BlockToolResult {
					sentInterrupt = true
					if err := drv.Interrupt(); err != nil {
						t.Fatalf("interrupt: %v", err)
					}
				}
			}
		}
		if ev.IsTurnEnd() {
			t.Logf("result: terminal_reason=%s stop=%s", ev.TerminalReason, ev.StopReason)
			aborted = ev.TerminalReason == "aborted_streaming"
			drv.Stop()
		}
	}

	if !sentInterrupt {
		t.Fatal("never saw a tool result to interrupt after")
	}
	if !aborted {
		t.Fatal("turn did not abort after interrupt")
	}
	// The interrupt should have stopped it well before all 30 files were made.
	entries, _ := os.ReadDir(work)
	t.Logf("files created before interrupt took effect: %d", len(entries))
	if len(entries) >= 30 {
		t.Fatalf("interrupt had no effect: %d files created", len(entries))
	}
}
