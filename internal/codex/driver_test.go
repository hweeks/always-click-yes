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

// TestReasoningItemSummaryIsPlainStrings replays a real captured
// item/completed line (docs/codex-fixtures/reasoning-summary.jsonl) whose
// reasoning item has a non-empty summary — every earlier fixture's reasoning
// items were empty, so this is the first live proof of the array element's
// actual shape: a bare string, not an object with a "text" field. Before
// reasoningChunk grew a tolerant UnmarshalJSON, decoding this exact line
// failed outright and silently dropped the whole item (see translate.go's
// own comment on reasoningItem).
func TestReasoningItemSummaryIsPlainStrings(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "codex-fixtures", "reasoning-summary.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("fixture has %d lines, want 2 (item/started, item/completed)", len(lines))
	}

	var tx bytes.Buffer
	d := NewWithWriter(Options{}, nopWriteCloser{&tx})
	for _, line := range lines {
		d.handleLine([]byte(line))
	}
	close(d.events)

	var got driver.Event
	var n int
	for ev := range d.Events() {
		got = ev
		n++
	}
	if n != 1 {
		t.Fatalf("want exactly 1 event (item/started produces none), got %d", n)
	}
	if got.Type != driver.TypeAssistant {
		t.Fatalf("event = %+v, want assistant", got)
	}
	b := firstBlock(t, got)
	if b.Type != driver.BlockThinking {
		t.Fatalf("block = %+v, want thinking", b)
	}
	want := "**Verifying primality of 1237**"
	if b.Thinking != want {
		t.Errorf("Thinking = %q, want %q", b.Thinking, want)
	}
}

// TestTurnCompletedExtractsStructuredOutputFromFinalAgentMessage replays a
// real captured item/completed + turn/completed pair
// (docs/codex-fixtures/structured-output-turn-completed.jsonl) from a driver
// started with an OutputSchema. There is no claude-style dedicated
// "structured_output" field on turn/completed — outputSchema constrains the
// final agentMessage item itself, so its own "text" IS the validated JSON,
// both on item/completed and echoed again in turn/completed's turn.items.
func TestTurnCompletedExtractsStructuredOutputFromFinalAgentMessage(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "codex-fixtures", "structured-output-turn-completed.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("fixture has %d lines, want 2 (item/completed, turn/completed)", len(lines))
	}

	var tx bytes.Buffer
	d := NewWithWriter(Options{OutputSchema: json.RawMessage(`{"type":"object"}`)}, nopWriteCloser{&tx})
	for _, line := range lines {
		d.handleLine([]byte(line))
	}
	close(d.events)

	var result driver.Event
	var gotResult bool
	for ev := range d.Events() {
		if ev.Type == driver.TypeResult {
			result = ev
			gotResult = true
		}
	}
	if !gotResult {
		t.Fatal("no result event")
	}
	want := `{"outcome":"ok","note":"hello"}`
	if string(result.StructuredOutput) != want {
		t.Errorf("StructuredOutput = %s, want %s", result.StructuredOutput, want)
	}
}

// TestTurnCompletedOmitsStructuredOutputWithoutOutputSchema confirms
// extractStructuredOutput only ever fires when this driver was actually
// started with an OutputSchema — an ordinary conversational final message is
// prose, not a report, and must never be handed to ParseReport as one.
func TestTurnCompletedOmitsStructuredOutputWithoutOutputSchema(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "codex-fixtures", "structured-output-turn-completed.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	var tx bytes.Buffer
	d := NewWithWriter(Options{}, nopWriteCloser{&tx})
	for _, line := range lines {
		d.handleLine([]byte(line))
	}
	close(d.events)

	for ev := range d.Events() {
		if ev.Type == driver.TypeResult && len(ev.StructuredOutput) != 0 {
			t.Errorf("StructuredOutput = %s, want empty when no OutputSchema was configured", ev.StructuredOutput)
		}
	}
}

// TestMcpToolCallItemEmitsQualifiedToolUseAndResult replays a SCHEMA-DERIVED
// fixture (docs/codex-fixtures/mcp-tool-call-schema-derived.jsonl) — NOT a
// live capture. Provoking a real "mcpToolCall" item requires a codex session
// that actually calls one of acy's own MCP tools, which spends real usage
// against the account's ChatGPT plan; this task was explicitly told not to
// spend that (ACY_LIVE=1 tests are off limits here). Instead the fixture's
// shape is read directly off `codex app-server generate-json-schema --out
// <dir> --experimental` (costs nothing, no model call —
// docs/codex-cli-findings.md:237): ServerNotification.json's ThreadItem union
// has an "mcpToolCall" variant (McpToolCallThreadItem) with required
// id/server/tool/status/arguments and nullable result/error — see
// translate.go's own comment on mcpToolCallItem for the full field-by-field
// account.
//
// This is the fixture that proves the actual defect this task fixes: before
// handleItemCompleted grew the "mcpToolCall" case, this exact item fell
// through to its default branch ("unhandled item/completed type") and was
// dropped — meaning PresentPlan (and Finish) never reached the UI even though
// acy's MCP server had already answered them correctly.
func TestMcpToolCallItemEmitsQualifiedToolUseAndResult(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "codex-fixtures", "mcp-tool-call-schema-derived.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("fixture has %d lines, want 1 (item/completed)", len(lines))
	}

	var tx bytes.Buffer
	d := NewWithWriter(Options{}, nopWriteCloser{&tx})
	for _, line := range lines {
		d.handleLine([]byte(line))
	}
	close(d.events)

	var events []driver.Event
	for ev := range d.Events() {
		events = append(events, ev)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 events (tool_use, tool_result), got %d: %+v", len(events), events)
	}

	toolUse := firstBlock(t, events[0])
	if events[0].Type != driver.TypeAssistant || toolUse.Type != driver.BlockToolUse {
		t.Fatalf("event 0 = %+v (block %+v), want assistant/tool_use", events[0], toolUse)
	}
	const wantName = "mcp__acy__PresentPlan"
	if toolUse.Name != wantName {
		t.Errorf("tool_use Name = %q, want %q", toolUse.Name, wantName)
	}
	if toolUse.ID != "mcptool-f1518689-dde4-4887-8102-b710292228b1" {
		t.Errorf("tool_use ID = %q", toolUse.ID)
	}
	var args struct {
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal(toolUse.Input, &args); err != nil {
		t.Fatalf("tool_use Input not JSON: %v (%s)", err, toolUse.Input)
	}
	if !strings.Contains(args.Plan, "Translate codex's mcpToolCall item") {
		t.Errorf("tool_use Input plan = %q, want it to contain the fixture's plan text", args.Plan)
	}

	toolResult := firstBlock(t, events[1])
	if events[1].Type != driver.TypeUser || toolResult.Type != driver.BlockToolResult {
		t.Fatalf("event 1 = %+v (block %+v), want user/tool_result", events[1], toolResult)
	}
	if toolResult.ToolUseID != toolUse.ID {
		t.Errorf("tool_result ToolUseID = %q, want %q", toolResult.ToolUseID, toolUse.ID)
	}
	if toolResult.IsError {
		t.Error("tool_result should not be an error: the fixture's status is \"completed\"")
	}
	if !strings.Contains(string(toolResult.Content), "Plan recorded and shown to the human") {
		t.Errorf("tool_result Content = %s, want it to contain the MCP server's own PlanRecorded text", toolResult.Content)
	}
}

// TestMcpToolCallItemQualifiesByItsOwnServerNotAcys is the discrimination
// test for the defect this task fixes: emitMcpToolCall used to build its
// tool_use Name with mcp.Qualified(it.Tool), which hardcodes acy's own
// ServerName and ignores the item's actual "server" field entirely. A codex
// thread's configured MCP servers are not limited to acy's own — `codex mcp
// add` writes into the user's ~/.codex/config.toml, merged with acy's inline
// config rather than replaced by it — so a tool call from any other server
// was silently relabelled as acy's. That is not cosmetic: a foreign server's
// tool named "Finish" would then read as acy's own end-the-run tool_use to
// anything downstream that trusts the name (ui.ingestToolUse). This drives a
// synthesized item/completed with server "some-other-server" and tool
// "Finish" through the same handleLine path TestMcpToolCallItemEmits... uses,
// and asserts the emitted Name carries that real server, not "acy" — the ui
// side of this same discrimination (a Model must not end the run on it) is
// TestIngestForeignMcpServerFinishDoesNotEndTheRun in internal/ui/ingest_test.go.
func TestMcpToolCallItemQualifiesByItsOwnServerNotAcys(t *testing.T) {
	const line = `{"method":"item/completed","params":{"item":{"type":"mcpToolCall","id":"mcptool-foreign-1","server":"some-other-server","tool":"Finish","arguments":{"outcome":"completed","summary":"not acy's"},"status":"completed","result":{"content":[{"type":"text","text":"ok"}]},"error":null},"threadId":"` + fixtureThreadID + `"}}`

	var tx bytes.Buffer
	d := NewWithWriter(Options{}, nopWriteCloser{&tx})
	d.handleLine([]byte(line))
	close(d.events)

	var events []driver.Event
	for ev := range d.Events() {
		events = append(events, ev)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 events (tool_use, tool_result), got %d: %+v", len(events), events)
	}

	toolUse := firstBlock(t, events[0])
	const wantName = "mcp__some-other-server__Finish"
	if toolUse.Name != wantName {
		t.Errorf("tool_use Name = %q, want %q", toolUse.Name, wantName)
	}
	if unwanted := "mcp__acy__Finish"; toolUse.Name == unwanted {
		t.Errorf("tool_use Name = %q, must not be relabelled as acy's own Finish", toolUse.Name)
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
