package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/driver"
)

const fixtureThreadID = "01a01547-26f7-7fd2-afeb-349415035aa2"
const fixtureModel = "gpt-5.6-terra"

func readFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "codex-fixtures", "app-server-session.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func firstBlock(t *testing.T, ev driver.Event) driver.ContentBlock {
	t.Helper()
	if ev.Message == nil {
		t.Fatalf("event has no message: %+v", ev)
	}
	blocks := ev.Message.Blocks()
	if len(blocks) != 1 {
		t.Fatalf("want 1 content block, got %d: %+v", len(blocks), blocks)
	}
	return blocks[0]
}

// TestFixtureReplay drives docs/codex-fixtures/app-server-session.ndjson
// through the Driver's read loop exactly as codex's stdout would, and asserts
// the full resulting driver.Event sequence. The fixture is a real two-turn
// session: a clean file read, then a shell write declined via the approval
// gate — captured live per docs/codex-cli-findings.md.
//
// The four outgoing requests the fixture answers (initialize=1, thread/start=2,
// turn/start=3, turn/start=4) are pre-registered in the pending map, standing
// in for a caller that already sent them — this is what lets the test replay
// the file as one deterministic pass instead of racing a live handshake
// against an unpaced io.Reader (see wire_test.go's TestHandshake... for a test
// that instead drives the real blocking call() path end to end).
func TestFixtureReplay(t *testing.T) {
	data := readFixture(t)

	var tx bytes.Buffer
	d := NewWithWriter(Options{}, nopWriteCloser{&tx})
	d.pendingMu.Lock()
	d.pending[1] = pendingRequest{kind: kindGeneric, ch: make(chan rpcResult, 1)}
	d.pending[2] = pendingRequest{kind: kindThreadStart, ch: make(chan rpcResult, 1)}
	d.pending[3] = pendingRequest{kind: kindTurnStart, ch: make(chan rpcResult, 1)}
	d.pending[4] = pendingRequest{kind: kindTurnStart, ch: make(chan rpcResult, 1)}
	d.pendingMu.Unlock()

	go d.readLoop(bytes.NewReader(data))

	var events []driver.Event
	for ev := range d.Events() {
		events = append(events, ev)
	}

	const wantCount = 12
	if len(events) != wantCount {
		t.Fatalf("want %d events, got %d: %+v", wantCount, len(events), events)
	}

	// --- init --------------------------------------------------------------
	init := events[0]
	if init.Type != driver.TypeSystem || init.Subtype != "init" {
		t.Fatalf("event 0 = %+v, want system/init", init)
	}
	if init.SessionID != fixtureThreadID {
		t.Errorf("init SessionID = %q, want %q", init.SessionID, fixtureThreadID)
	}
	if init.Model != fixtureModel {
		t.Errorf("init Model = %q, want %q", init.Model, fixtureModel)
	}

	// --- turn 1: reasoning, tool_use, tool_result, text, result -------------
	if b := firstBlock(t, events[1]); events[1].Type != driver.TypeAssistant || b.Type != driver.BlockThinking {
		t.Errorf("event 1 = %+v (block %+v), want assistant/thinking", events[1], b)
	}

	toolUse1 := firstBlock(t, events[2])
	if events[2].Type != driver.TypeAssistant || toolUse1.Type != driver.BlockToolUse {
		t.Fatalf("event 2 = %+v (block %+v), want assistant/tool_use", events[2], toolUse1)
	}
	if toolUse1.ID != "exec-f1518689-dde4-4887-8102-b710292228b1" {
		t.Errorf("turn1 tool_use ID = %q", toolUse1.ID)
	}
	if !strings.Contains(string(toolUse1.Input), "hello.txt") {
		t.Errorf("turn1 tool_use Input = %s, want it to mention hello.txt", toolUse1.Input)
	}

	toolResult1 := firstBlock(t, events[3])
	if events[3].Type != driver.TypeUser || toolResult1.Type != driver.BlockToolResult {
		t.Fatalf("event 3 = %+v (block %+v), want user/tool_result", events[3], toolResult1)
	}
	if toolResult1.ToolUseID != toolUse1.ID {
		t.Errorf("turn1 tool_result ToolUseID = %q, want %q", toolResult1.ToolUseID, toolUse1.ID)
	}
	if toolResult1.IsError {
		t.Error("turn1 tool_result should not be an error: the command succeeded")
	}
	var output1 string
	if err := json.Unmarshal(toolResult1.Content, &output1); err != nil {
		t.Fatalf("turn1 tool_result content not a JSON string: %v", err)
	}
	if output1 != "hello from acy recon\n" {
		t.Errorf("turn1 tool_result content = %q", output1)
	}

	text1 := firstBlock(t, events[4])
	if events[4].Type != driver.TypeAssistant || text1.Type != driver.BlockText || text1.Text != "hello from acy recon" {
		t.Errorf("event 4 = %+v (block %+v), want assistant text %q", events[4], text1, "hello from acy recon")
	}

	result1 := events[5]
	if result1.Type != driver.TypeResult {
		t.Fatalf("event 5 = %+v, want result", result1)
	}
	if result1.TotalCostUSD != 0 {
		t.Errorf("turn1 TotalCostUSD = %v, want 0 (codex reports no dollar figure)", result1.TotalCostUSD)
	}
	if result1.Usage == nil {
		t.Fatal("turn1 result has no Usage")
	}
	wantUsage1 := driver.Usage{InputTokens: 32133, OutputTokens: 127, CacheReadInputTokens: 26112, CacheCreationInputTokens: 0}
	if *result1.Usage != wantUsage1 {
		t.Errorf("turn1 Usage = %+v, want %+v", *result1.Usage, wantUsage1)
	}
	mu1, ok := result1.ModelUsage[fixtureModel]
	if !ok {
		t.Fatalf("turn1 ModelUsage missing key %q: %+v", fixtureModel, result1.ModelUsage)
	}
	wantModelUsage1 := driver.ModelUsage{InputTokens: 32133, OutputTokens: 127, CacheReadInputTokens: 26112, CacheCreationInputTokens: 0, ContextWindow: 258400}
	if mu1 != wantModelUsage1 {
		t.Errorf("turn1 ModelUsage = %+v, want %+v", mu1, wantModelUsage1)
	}

	// --- turn 2: reasoning, text, tool_use(declined), tool_result(error), text, result
	if b := firstBlock(t, events[6]); events[6].Type != driver.TypeAssistant || b.Type != driver.BlockThinking {
		t.Errorf("event 6 = %+v (block %+v), want assistant/thinking", events[6], b)
	}

	commentary := firstBlock(t, events[7])
	wantCommentary := "I’ll create `new.txt` with `hi` and verify it immediately."
	if events[7].Type != driver.TypeAssistant || commentary.Type != driver.BlockText || commentary.Text != wantCommentary {
		t.Errorf("event 7 = %+v (block %+v), want assistant text %q", events[7], commentary, wantCommentary)
	}

	toolUse2 := firstBlock(t, events[8])
	if events[8].Type != driver.TypeAssistant || toolUse2.Type != driver.BlockToolUse {
		t.Fatalf("event 8 = %+v (block %+v), want assistant/tool_use", events[8], toolUse2)
	}
	if toolUse2.ID != "exec-6d552fe0-3aa2-47e1-924c-3a3fb67ec101" {
		t.Errorf("turn2 tool_use ID = %q", toolUse2.ID)
	}
	if !strings.Contains(string(toolUse2.Input), "new.txt") {
		t.Errorf("turn2 tool_use Input = %s, want it to mention new.txt", toolUse2.Input)
	}

	toolResult2 := firstBlock(t, events[9])
	if events[9].Type != driver.TypeUser || toolResult2.Type != driver.BlockToolResult {
		t.Fatalf("event 9 = %+v (block %+v), want user/tool_result", events[9], toolResult2)
	}
	if toolResult2.ToolUseID != toolUse2.ID {
		t.Errorf("turn2 tool_result ToolUseID = %q, want %q", toolResult2.ToolUseID, toolUse2.ID)
	}
	if !toolResult2.IsError {
		t.Error("turn2 tool_result should be an error: the approval was declined and the command never ran")
	}

	text2 := firstBlock(t, events[10])
	wantText2 := "I couldn’t create the file because permission to write was declined."
	if events[10].Type != driver.TypeAssistant || text2.Type != driver.BlockText || text2.Text != wantText2 {
		t.Errorf("event 10 = %+v (block %+v), want assistant text %q", events[10], text2, wantText2)
	}

	result2 := events[11]
	if result2.Type != driver.TypeResult {
		t.Fatalf("event 11 = %+v, want result", result2)
	}
	if result2.TotalCostUSD != 0 {
		t.Errorf("turn2 TotalCostUSD = %v, want 0", result2.TotalCostUSD)
	}
	if result2.Usage == nil {
		t.Fatal("turn2 result has no Usage")
	}
	wantUsage2 := driver.Usage{InputTokens: 34892, OutputTokens: 168, CacheReadInputTokens: 28160, CacheCreationInputTokens: 0}
	if *result2.Usage != wantUsage2 {
		t.Errorf("turn2 Usage = %+v, want %+v", *result2.Usage, wantUsage2)
	}
	mu2, ok := result2.ModelUsage[fixtureModel]
	if !ok {
		t.Fatalf("turn2 ModelUsage missing key %q: %+v", fixtureModel, result2.ModelUsage)
	}
	wantModelUsage2 := driver.ModelUsage{InputTokens: 67025, OutputTokens: 295, CacheReadInputTokens: 54272, CacheCreationInputTokens: 0, ContextWindow: 258400}
	if mu2 != wantModelUsage2 {
		t.Errorf("turn2 ModelUsage = %+v, want %+v", mu2, wantModelUsage2)
	}

	// Exactly one driver.TypeResult per turn: the fixture has two turns, so
	// there must be exactly two — never zero (a dropped idle signal hangs the
	// UI forever) and never more than two (a duplicate double-banks usage).
	results := 0
	for _, ev := range events {
		if ev.Type == driver.TypeResult {
			results++
		}
	}
	if results != 2 {
		t.Errorf("got %d driver.TypeResult events, want exactly 2 (one per turn)", results)
	}
}

// TestFixtureReplaySurfacesApprovalRequest confirms the mid-turn approval
// request in the fixture (server id 0) is captured as a pending approval
// rather than translated into a driver.Event — approvals are surfaced, not
// decided (see approval.go).
func TestFixtureReplaySurfacesApprovalRequest(t *testing.T) {
	data := readFixture(t)

	var tx bytes.Buffer
	d := NewWithWriter(Options{}, nopWriteCloser{&tx})
	d.pendingMu.Lock()
	d.pending[1] = pendingRequest{kind: kindGeneric, ch: make(chan rpcResult, 1)}
	d.pending[2] = pendingRequest{kind: kindThreadStart, ch: make(chan rpcResult, 1)}
	d.pending[3] = pendingRequest{kind: kindTurnStart, ch: make(chan rpcResult, 1)}
	d.pending[4] = pendingRequest{kind: kindTurnStart, ch: make(chan rpcResult, 1)}
	d.pendingMu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range d.Events() {
		}
	}()
	d.readLoop(bytes.NewReader(data))
	<-done

	pending := d.PendingApprovals()
	if len(pending) != 1 {
		t.Fatalf("want 1 outstanding approval after replay (never answered), got %d: %+v", len(pending), pending)
	}
	if pending[0].ID != 0 || pending[0].Method != "item/commandExecution/requestApproval" {
		t.Errorf("outstanding approval = %+v", pending[0])
	}
	if !strings.Contains(string(pending[0].Params), "new.txt") {
		t.Errorf("approval Params = %s, want it to mention new.txt", pending[0].Params)
	}
}

// syncWriter is a thread-safe io.WriteCloser that also notifies a channel per
// write, so a test can drive a goroutine blocked in Driver.call and know
// exactly when its request has been written — without racing a plain
// *bytes.Buffer between the writer and the observing goroutine.
type syncWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	wrote chan struct{}
}

func newSyncWriter() *syncWriter {
	return &syncWriter{wrote: make(chan struct{}, 16)}
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.buf.Write(p)
	w.mu.Unlock()
	w.wrote <- struct{}{}
	return n, err
}

func (w *syncWriter) Close() error { return nil }

func (w *syncWriter) awaitWrite(t *testing.T) {
	t.Helper()
	select {
	case <-w.wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a write")
	}
}

// TestHandshakeSendsInitializeThenThreadStartAndEmitsInit drives the real
// blocking call()/sendRequest()/deliverResponse() path end to end — the part
// TestFixtureReplay deliberately bypasses by pre-registering pending entries.
// It proves Start's handshake actually blocks for each response in turn and
// unblocks correctly once one arrives, with no live process involved.
func TestHandshakeSendsInitializeThenThreadStartAndEmitsInit(t *testing.T) {
	w := newSyncWriter()
	d := NewWithWriter(Options{Cwd: "/work"}, w)

	done := make(chan error, 1)
	go func() { done <- d.handshake(context.Background()) }()

	w.awaitWrite(t) // "initialize" request
	d.handleLine([]byte(`{"id":1,"result":{"userAgent":"test"}}`))

	w.awaitWrite(t) // "thread/start" request
	d.handleLine([]byte(`{"id":2,"result":{"thread":{"id":"thread-abc"},"model":"gpt-5.6-terra"}}`))

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handshake never returned")
	}

	select {
	case ev := <-d.Events():
		if ev.Type != driver.TypeSystem || ev.Subtype != "init" {
			t.Fatalf("event = %+v, want system/init", ev)
		}
		if ev.SessionID != "thread-abc" || ev.Model != "gpt-5.6-terra" {
			t.Errorf("init event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no init event emitted")
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.threadID != "thread-abc" {
		t.Errorf("d.threadID = %q, want thread-abc", d.threadID)
	}
}

// TestHandshakePropagatesError confirms an RPC error on the initialize
// response fails the handshake instead of hanging or silently continuing to
// thread/start with no thread to speak of.
func TestHandshakePropagatesError(t *testing.T) {
	w := newSyncWriter()
	d := NewWithWriter(Options{}, w)

	done := make(chan error, 1)
	go func() { done <- d.handshake(context.Background()) }()

	w.awaitWrite(t)
	d.handleLine([]byte(`{"id":1,"error":{"code":-32000,"message":"nope"}}`))

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("handshake returned nil error despite an RPC error response")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handshake never returned")
	}
}
