package codex

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/gate"
)

// commandExecutionApprovalLine builds a synthetic item/commandExecution/
// requestApproval server request line, shaped like the live one captured in
// docs/codex-fixtures/app-server-session.ndjson.
func commandExecutionApprovalLine(id int64, threadID, itemID, cwd, command string) []byte {
	b, _ := json.Marshal(map[string]any{
		"method": methodCommandExecutionApproval,
		"id":     id,
		"params": map[string]any{
			"threadId": threadID,
			"itemId":   itemID,
			"cwd":      cwd,
			"command":  command,
			"reason":   "test",
		},
	})
	return b
}

// fileChangeApprovalLine builds a synthetic item/fileChange/requestApproval
// server request line. The patch/path field names are unconfirmed (see
// BuildPreToolUseInput's doc comment), so "path" here stands in for whatever
// codex actually sends, just to prove it survives the round trip.
func fileChangeApprovalLine(id int64, threadID, itemID, cwd string) []byte {
	b, _ := json.Marshal(map[string]any{
		"method": methodFileChangeApproval,
		"id":     id,
		"params": map[string]any{
			"threadId": threadID,
			"itemId":   itemID,
			"cwd":      cwd,
			"reason":   "test",
			"path":     "main.go",
		},
	})
	return b
}

// TestBridgeMapsCommandExecutionFields proves the exact field mapping the
// rest of acy depends on: ToolName "Bash" (not a codex-specific name),
// ToolUseID = itemId, SessionID = threadId, a "command" key in ToolInput.
func TestBridgeMapsCommandExecutionFields(t *testing.T) {
	w := newSyncWriter()
	d := NewWithWriter(Options{}, w)
	b := NewBridge()
	b.Attach(d)
	defer b.Close()

	d.handleLine(commandExecutionApprovalLine(0, "thread-1", "item-1", "/work", "echo hi"))

	select {
	case p := <-b.Requests():
		if p.Input.ToolName != "Bash" {
			t.Errorf("ToolName = %q, want Bash", p.Input.ToolName)
		}
		if p.Input.ToolUseID != "item-1" {
			t.Errorf("ToolUseID = %q, want item-1 (codex's itemId)", p.Input.ToolUseID)
		}
		if p.Input.SessionID != "thread-1" {
			t.Errorf("SessionID = %q, want thread-1 (codex's threadId)", p.Input.SessionID)
		}
		if p.Input.Cwd != "/work" {
			t.Errorf("Cwd = %q, want /work", p.Input.Cwd)
		}
		if p.Input.HookEventName != "PreToolUse" {
			t.Errorf("HookEventName = %q, want PreToolUse", p.Input.HookEventName)
		}
		var in struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(p.Input.ToolInput, &in); err != nil {
			t.Fatalf("ToolInput = %s, not valid JSON: %v", p.Input.ToolInput, err)
		}
		if in.Command != "echo hi" {
			t.Errorf("ToolInput command = %q, want %q", in.Command, "echo hi")
		}
		p.Resolve(gate.Decision{Behavior: gate.Allow})
	case <-time.After(2 * time.Second):
		t.Fatal("no request forwarded")
	}
	w.awaitWrite(t)
}

// TestBridgeMapsFileChangeFields proves the fileChange mapping: ToolName
// "Edit", and the raw params carried through under a "changes" key that the
// merge guard never inspects.
func TestBridgeMapsFileChangeFields(t *testing.T) {
	w := newSyncWriter()
	d := NewWithWriter(Options{}, w)
	b := NewBridge()
	b.Attach(d)
	defer b.Close()

	d.handleLine(fileChangeApprovalLine(0, "thread-2", "item-2", "/work"))

	select {
	case p := <-b.Requests():
		if p.Input.ToolName != "Edit" {
			t.Errorf("ToolName = %q, want Edit", p.Input.ToolName)
		}
		if p.Input.ToolUseID != "item-2" || p.Input.SessionID != "thread-2" {
			t.Errorf("ids = %q/%q, want item-2/thread-2", p.Input.ToolUseID, p.Input.SessionID)
		}
		var in map[string]json.RawMessage
		if err := json.Unmarshal(p.Input.ToolInput, &in); err != nil {
			t.Fatalf("ToolInput = %s, not valid JSON: %v", p.Input.ToolInput, err)
		}
		changes, ok := in["changes"]
		if !ok {
			t.Fatalf("ToolInput = %s, want a %q key", p.Input.ToolInput, "changes")
		}
		if !strings.Contains(string(changes), "main.go") {
			t.Errorf("changes = %s, want it to carry the raw params through verbatim", changes)
		}
		p.Resolve(gate.Decision{Behavior: gate.Allow})
	case <-time.After(2 * time.Second):
		t.Fatal("no request forwarded")
	}
	w.awaitWrite(t)
}

// TestBridgeWritesAcceptAndDecline proves the decision translation:
// gate.Allow -> "accept", gate.Deny -> "decline" (never "cancel" — see
// decisionFor's doc comment).
func TestBridgeWritesAcceptAndDecline(t *testing.T) {
	cases := []struct {
		name     string
		behavior string
		want     string
	}{
		{"allow", gate.Allow, `"decision":"accept"`},
		{"deny", gate.Deny, `"decision":"decline"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newSyncWriter()
			d := NewWithWriter(Options{}, w)
			b := NewBridge()
			b.Attach(d)
			defer b.Close()

			d.handleLine(commandExecutionApprovalLine(7, "thread", "item", "/work", "echo hi"))
			var p *gate.Pending
			select {
			case p = <-b.Requests():
			case <-time.After(2 * time.Second):
				t.Fatal("no request forwarded")
			}
			p.Resolve(gate.Decision{Behavior: tc.behavior})
			w.awaitWrite(t)

			w.mu.Lock()
			got := w.buf.String()
			w.mu.Unlock()
			if !strings.Contains(got, tc.want) {
				t.Errorf("wire output = %s, want it to contain %q", got, tc.want)
			}
		})
	}
}

// TestBridgeAbandonedPendingStillAnswersDriver proves an unanswered gate.
// Pending — received off Requests() and never resolved by anything — still
// gets an answer written to the driver once the Bridge is closed. An
// unanswered codex approval hangs that turn forever server-side, so this is
// not optional.
func TestBridgeAbandonedPendingStillAnswersDriver(t *testing.T) {
	w := newSyncWriter()
	d := NewWithWriter(Options{}, w)
	b := NewBridge()
	b.Attach(d)

	d.handleLine(commandExecutionApprovalLine(3, "thread", "item", "/work", "echo hi"))

	select {
	case <-b.Requests():
		// received, deliberately never resolved: the request is abandoned.
	case <-time.After(2 * time.Second):
		t.Fatal("no request forwarded")
	}

	// Close's own wg.Wait() only returns once every in-flight forward() call
	// has already written its answer (see Close's doc comment), so the write
	// below is guaranteed to have happened by the time Close returns — no
	// extra synchronization needed.
	b.Close()

	w.mu.Lock()
	got := w.buf.String()
	w.mu.Unlock()
	if !strings.Contains(got, `"id":3`) || !strings.Contains(got, `"decision":"decline"`) {
		t.Errorf("wire output = %s, want id=3 declined even though the Pending was never resolved", got)
	}
}

// TestBridgeAcceptsMCPElicitationWithoutQueueing proves an acy MCP call can
// reach acy's own phase/dispatch rules. Codex wraps each such call in an MCP
// elicitation under its untrusted approval policy; it is not a filesystem tool
// and must neither wait for nor consume the ordinary tool countdown.
func TestBridgeAcceptsMCPElicitationWithoutQueueing(t *testing.T) {
	w := newSyncWriter()
	d := NewWithWriter(Options{}, w)
	b := NewBridge()
	b.Attach(d)
	defer b.Close()

	d.handleLine([]byte(`{"method":"mcpServer/elicitation/request","id":4,"params":{"mode":"form","requestedSchema":{"type":"object"}}}`))
	w.awaitWrite(t)

	select {
	case p := <-b.Requests():
		t.Fatalf("MCP elicitation was incorrectly queued as tool %q", p.Input.ToolName)
	case <-time.After(100 * time.Millisecond):
	}
	w.mu.Lock()
	got := w.buf.String()
	w.mu.Unlock()
	if !strings.Contains(got, `"id":4`) || !strings.Contains(got, `"action":"accept"`) {
		t.Errorf("wire output = %s, want MCP elicitation id=4 accepted with action", got)
	}
}

// TestBridgeTwoDriversBothForward proves the fan-in: two independently
// attached drivers both land their approval requests on the one shared
// channel.
func TestBridgeTwoDriversBothForward(t *testing.T) {
	w1, w2 := newSyncWriter(), newSyncWriter()
	d1 := NewWithWriter(Options{}, w1)
	d2 := NewWithWriter(Options{}, w2)

	b := NewBridge()
	b.Attach(d1)
	b.Attach(d2)
	defer b.Close()

	d1.handleLine(commandExecutionApprovalLine(1, "thread-1", "item-a", "/work", "echo a"))
	d2.handleLine(commandExecutionApprovalLine(1, "thread-2", "item-b", "/work", "echo b"))

	seen := map[string]bool{}
	for i := range 2 {
		select {
		case p := <-b.Requests():
			seen[p.Input.ToolUseID] = true
			p.Resolve(gate.Decision{Behavior: gate.Allow})
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for request %d of 2", i+1)
		}
	}
	if !seen["item-a"] || !seen["item-b"] {
		t.Errorf("seen = %+v, want both item-a and item-b forwarded", seen)
	}
	w1.awaitWrite(t)
	w2.awaitWrite(t)
}

// TestBridgeDriverStreamClosingLeavesChannelOpen proves that one attached
// driver's Approvals() stream closing (it exited) does not close the shared
// channel out from under every other attached driver — dynamic membership
// (children attaching and leaving) depends on this.
func TestBridgeDriverStreamClosingLeavesChannelOpen(t *testing.T) {
	w1, w2 := newSyncWriter(), newSyncWriter()
	d1 := NewWithWriter(Options{}, w1)
	d2 := NewWithWriter(Options{}, w2)

	b := NewBridge()
	b.Attach(d1)

	// Simulate d1 exiting: its Approvals() stream closes. White-box access
	// (this file is package codex) stands in for what a real process exit
	// would eventually do to that channel.
	close(d1.approvals)

	b.Attach(d2)
	d2.handleLine(commandExecutionApprovalLine(1, "thread-2", "item-c", "/work", "echo c"))

	select {
	case p := <-b.Requests():
		if p.Input.ToolUseID != "item-c" {
			t.Errorf("ToolUseID = %q, want item-c", p.Input.ToolUseID)
		}
		p.Resolve(gate.Decision{Behavior: gate.Allow})
	case <-time.After(2 * time.Second):
		t.Fatal("shared channel appears closed after one driver's stream closed")
	}
	w2.awaitWrite(t)
	b.Close()
}
