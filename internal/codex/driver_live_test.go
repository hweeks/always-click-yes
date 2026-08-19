package codex

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/gate"
)

// requireLiveCodex skips unless the caller has opted in to spending real
// usage against their ChatGPT plan (ACY_LIVE=1, the same switch
// internal/driver's own TestLiveDriver and internal/gate's TestGateE2E use for
// claude) and a real codex binary is on PATH.
func requireLiveCodex(t *testing.T) {
	t.Helper()
	if os.Getenv("ACY_LIVE") == "" {
		t.Skip("set ACY_LIVE=1 to run the live codex driver suite (spends real usage on your ChatGPT plan)")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("no codex binary on PATH")
	}
}

// liveScratchDir is a scratch project for codex to run in, anchored directly
// under /tmp rather than t.TempDir() (which nests under $TMPDIR) — AGENTS.md's
// arch-mode section documents macOS's ~104-byte sockaddr_un ceiling biting
// exactly this shape of path twice already for a different feature's own
// state dir; anchoring at /tmp is just hygiene here, not a fix for a bug this
// package has hit, since codex itself owns no unix socket under this
// directory — only its subprocess's cwd lives here.
func liveScratchDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "acy-codex-live-")
	if err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// openLiveLog points alog at a fresh file for the duration of one test, so a
// probe can read back the exact wire bytes codex sent — the only way to
// inspect a shape this package's decoder does not (yet) know about. alog is
// process-wide state, so these tests must never run in parallel (same
// constraint internal/alog's own tests document).
func openLiveLog(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "acy-codex-wire.log")
	if _, err := alog.Open(p); err != nil {
		t.Fatalf("open alog: %v", err)
	}
	t.Cleanup(alog.Close)
	return p
}

func readLiveLog(t *testing.T, p string) string {
	t.Helper()
	alog.Close()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(b)
}

// TestLiveCodexHandshake is stage 1 of the live codex probe (see
// docs/codex-cli-findings.md and AGENTS.md): does a real `codex app-server`
// process reach a session id at all, with no user turn sent? Nothing can be
// armed without one, so this gates everything else.
//
//	ACY_LIVE=1 go test ./internal/codex/ -run TestLiveCodexHandshake -v
func TestLiveCodexHandshake(t *testing.T) {
	requireLiveCodex(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	d := New(Options{Cwd: liveScratchDir(t), Sandbox: "read-only", ApprovalPolicy: "never"})
	if err := d.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer d.Stop()

	if d.SessionID() == "" {
		t.Fatal("no session id after Start — nothing could arm from this")
	}
	t.Logf("session id: %s", d.SessionID())

	select {
	case ev := <-d.Events():
		if !ev.IsInit() {
			t.Fatalf("first event was not init: %+v", ev)
		}
		if ev.SessionID != d.SessionID() {
			t.Errorf("init event session id %q != Driver.SessionID() %q", ev.SessionID, d.SessionID())
		}
		t.Logf("init event: session=%s model=%s", ev.SessionID, ev.Model)
	case <-time.After(5 * time.Second):
		t.Fatal("no init event arrived on Events() despite a captured session id")
	}
}

// TestLiveCodexPlanTurnAndUsageSemantics is stages 2 and 3 of the live codex
// probe: a trivial turn renders as one whole assistant message (not
// fragments — proving item/agentMessage/delta really is safe to drop and
// item/completed really does carry the whole thing), exactly one
// driver.TypeResult ends the turn, and a second turn on the same thread (no
// relaunch — "arm in place") proves handleTokenUsage's add-vs-assign
// semantics: tokenUsage.last accumulates per turn (turn 2's Usage is a fresh,
// independent count, not turn 1's carried forward) while tokenUsage.total is
// a cumulative process total (turn 2's ModelUsage total is >= turn 1's).
//
//	ACY_LIVE=1 go test ./internal/codex/ -run TestLiveCodexPlanTurnAndUsageSemantics -v
func TestLiveCodexPlanTurnAndUsageSemantics(t *testing.T) {
	requireLiveCodex(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	d := New(Options{Cwd: liveScratchDir(t), Sandbox: "read-only", ApprovalPolicy: "never"})
	if err := d.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer d.Stop()
	<-d.Events() // init

	runTurn := func(prompt string) (driver.Event, []driver.Event) {
		if err := d.Send(prompt); err != nil {
			t.Fatalf("send: %v", err)
		}
		var assistantEvents []driver.Event
		for ev := range d.Events() {
			if ev.Type == driver.TypeAssistant {
				assistantEvents = append(assistantEvents, ev)
			}
			if ev.IsTurnEnd() {
				return ev, assistantEvents
			}
		}
		t.Fatalf("channel closed before a result event for %q", prompt)
		return driver.Event{}, nil
	}

	result1, asst1 := runTurn("Reply with exactly: PONG1")
	if len(asst1) != 1 {
		t.Fatalf("turn 1: got %d assistant events, want exactly 1 (a whole message, not fragments): %+v", len(asst1), asst1)
	}
	blocks := asst1[0].Message.Blocks()
	if len(blocks) != 1 || !strings.Contains(blocks[0].Text, "PONG1") {
		t.Fatalf("turn 1: assistant blocks = %+v, want one text block containing PONG1", blocks)
	}
	t.Logf("turn 1 assistant text: %q", blocks[0].Text)
	if result1.Usage == nil {
		t.Fatal("turn 1: result carried no Usage")
	}
	t.Logf("turn 1 usage: %+v", *result1.Usage)
	modelUsage1, ok := firstModelUsage(result1.ModelUsage)
	if !ok {
		t.Fatal("turn 1: result carried no ModelUsage")
	}
	t.Logf("turn 1 modelUsage (cumulative): %+v", modelUsage1)

	result2, asst2 := runTurn("Reply with exactly: PONG2")
	if len(asst2) != 1 {
		t.Fatalf("turn 2: got %d assistant events, want exactly 1: %+v", len(asst2), asst2)
	}
	if result2.Usage == nil {
		t.Fatal("turn 2: result carried no Usage")
	}
	t.Logf("turn 2 usage: %+v", *result2.Usage)
	modelUsage2, ok := firstModelUsage(result2.ModelUsage)
	if !ok {
		t.Fatal("turn 2: result carried no ModelUsage")
	}
	t.Logf("turn 2 modelUsage (cumulative): %+v", modelUsage2)

	// Cumulative (assigned) semantics: the process-wide total must not have
	// shrunk between turns.
	if modelUsage2.InputTokens < modelUsage1.InputTokens {
		t.Errorf("modelUsage.InputTokens went backwards (%d -> %d); tokenUsage.total should be a monotonic cumulative count, not per-turn",
			modelUsage1.InputTokens, modelUsage2.InputTokens)
	}
	// Per-turn (added, then reset) semantics: turn 2's own usage should be
	// its own small turn's worth, not the sum of turn 1 and turn 2 — which is
	// what it would look like if the accumulator were never reset in
	// handleTurnCompleted, or if last/total were swapped.
	if result2.Usage.InputTokens >= modelUsage2.InputTokens {
		t.Errorf("turn 2's per-turn Usage.InputTokens (%d) >= the cumulative modelUsage total (%d); "+
			"expected the per-turn figure to be much smaller than the cumulative one, which is the whole point of the distinction",
			result2.Usage.InputTokens, modelUsage2.InputTokens)
	}
}

func firstModelUsage(m map[string]driver.ModelUsage) (driver.ModelUsage, bool) {
	for _, v := range m {
		return v, true
	}
	return driver.ModelUsage{}, false
}

// TestLiveCodexReasoningItemShape probes unverified protocol detail #2: the
// JSON field(s) actually carrying a reasoning item's text. The recon
// fixture's reasoning items were always empty, so reasoningItem's
// summary/content shape (translate.go) is an educated guess never
// live-exercised. This asks for a reasoning effort high enough that a
// summary is likely to be non-empty, and prints the raw item/completed line
// alongside what the decoder actually extracted.
//
//	ACY_LIVE=1 go test ./internal/codex/ -run TestLiveCodexReasoningItemShape -v
func TestLiveCodexReasoningItemShape(t *testing.T) {
	requireLiveCodex(t)
	logPath := openLiveLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	d := New(Options{
		Cwd:            liveScratchDir(t),
		Sandbox:        "read-only",
		ApprovalPolicy: "never",
		Effort:         "high",
		Config:         map[string]any{"model_reasoning_summary": "detailed"},
	})
	if err := d.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer d.Stop()
	<-d.Events() // init

	if err := d.Send("Think step by step about whether 1237 is a prime number, then answer with just YES or NO."); err != nil {
		t.Fatalf("send: %v", err)
	}
	var sawThinking bool
	var thinkingTexts []string
	for ev := range d.Events() {
		if ev.Type == driver.TypeAssistant && ev.Message != nil {
			for _, b := range ev.Message.Blocks() {
				if b.Type == driver.BlockThinking {
					sawThinking = true
					thinkingTexts = append(thinkingTexts, b.Thinking)
				}
			}
		}
		if ev.IsTurnEnd() {
			d.Stop()
			break
		}
	}

	body := readLiveLog(t, logPath)
	for line := range strings.SplitSeq(body, "\n") {
		if strings.Contains(line, `"reasoning"`) {
			t.Logf("wire (reasoning item): %s", line)
		}
	}

	if !sawThinking {
		t.Log("no BlockThinking emitted at all — either codex sent no reasoning item, or it sent one this decoder produced empty text for (see wire lines above)")
		return
	}
	for _, txt := range thinkingTexts {
		t.Logf("decoded thinking text: %q", txt)
		if strings.TrimSpace(txt) == "" {
			t.Error("decoded a reasoning item into an EMPTY thinking block while the wire line above is non-trivial — reasoningItem's field shape is wrong, see the wire line logged above for the real one")
		}
	}
}

// TestLiveCodexApprovalGate is stage 4's positive case at the driver level
// (docs/codex-cli-findings.md §3): a real command-execution approval request
// arrives on Approvals(), BuildPreToolUseInput translates it, and Approve
// with "accept" lets the write through. Sandbox/approvalPolicy here match
// exactly what codexChildOptions (internal/supervisor/codex.go) actually
// configures for a dispatched child — workspace-write + untrusted — so this
// is the same shape of approval a real Dispatch would raise, just answered
// directly instead of through the full gate/UI stack.
//
//	ACY_LIVE=1 go test ./internal/codex/ -run TestLiveCodexApprovalGate -v
func TestLiveCodexApprovalGate(t *testing.T) {
	requireLiveCodex(t)
	dir := liveScratchDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	d := New(Options{Cwd: dir, Sandbox: "workspace-write", ApprovalPolicy: "untrusted"})
	if err := d.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer d.Stop()
	<-d.Events() // init

	if err := d.Send("Use the shell to create a file named shell.txt in the current directory containing exactly: hi"); err != nil {
		t.Fatalf("send: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range d.Events() {
			if ev.IsTurnEnd() {
				return
			}
		}
	}()

	// Looped, not a single select: the model may raise more than one approval
	// in this turn (e.g. a fileChange to create the file, and later a
	// commandExecution to verify it), and every one of them has to be
	// answered or the unanswered one hangs the turn until the test's context
	// deadline.
	approvalsSeen := 0
approvalLoop:
	for {
		select {
		case req := <-d.Approvals():
			approvalsSeen++
			t.Logf("approval request: method=%s id=%d params=%s", req.Method, req.ID, req.Params)
			in, ok := BuildPreToolUseInput(req)
			if !ok {
				t.Errorf("BuildPreToolUseInput did not recognize method %q", req.Method)
			} else {
				if in.SessionID != d.SessionID() {
					t.Errorf("PreToolUseInput.SessionID = %q, want the thread id %q", in.SessionID, d.SessionID())
				}
				// codex may satisfy "create a file" with either its shell tool
				// (item/commandExecution/requestApproval -> "Bash") or its native
				// file-edit tool (item/fileChange/requestApproval -> "Edit") —
				// which one it reaches for is the model's own choice, not
				// something this test can force, so both are accepted so long as
				// BuildPreToolUseInput mapped the method to the tool name the
				// gate actually recognizes (see its own doc comment on why that
				// mapping matters).
				switch req.Method {
				case methodCommandExecutionApproval:
					if in.ToolName != "Bash" {
						t.Errorf("commandExecution approval mapped to ToolName %q, want %q", in.ToolName, "Bash")
					}
				case methodFileChangeApproval:
					if in.ToolName != "Edit" {
						t.Errorf("fileChange approval mapped to ToolName %q, want %q", in.ToolName, "Edit")
					}
				default:
					t.Errorf("unexpected approval method %q", req.Method)
				}
				t.Logf("translated PreToolUseInput: %+v", in)
			}
			if err := d.Approve(req.ID, decisionFor(gate.Decision{Behavior: gate.Allow})); err != nil {
				t.Fatalf("approve: %v", err)
			}
		case <-done:
			break approvalLoop
		case <-time.After(60 * time.Second):
			t.Fatal("timed out waiting for an approval or the turn to end")
			break approvalLoop
		}
	}
	if approvalsSeen == 0 {
		t.Fatal("no approval request arrived at all")
	}
	d.Stop()

	got, err := os.ReadFile(filepath.Join(dir, "shell.txt"))
	if err != nil {
		t.Fatalf("shell.txt was never created after accepting the approval: %v", err)
	}
	if !strings.Contains(string(got), "hi") {
		t.Errorf("shell.txt = %q, want it to contain %q", got, "hi")
	}
}

// TestLiveCodexThreadResume probes unverified protocol detail #1:
// thread/resume's exact param field name. threadResumeParams
// (internal/codex/driver.go) sends "threadId", inferred from turn/start's own
// field name but never live-verified. If the field name is wrong, codex
// either errors the request outright or silently ignores it and starts a
// fresh thread with no memory of turn 1 — either way, asking the resumed
// thread to recall something only turn 1 said proves or disproves it.
//
//	ACY_LIVE=1 go test ./internal/codex/ -run TestLiveCodexThreadResume -v
func TestLiveCodexThreadResume(t *testing.T) {
	requireLiveCodex(t)
	dir := liveScratchDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	first := New(Options{Cwd: dir, Sandbox: "read-only", ApprovalPolicy: "never"})
	if err := first.Start(ctx); err != nil {
		t.Fatalf("start first: %v", err)
	}
	threadID := first.SessionID()
	t.Logf("first thread id: %s", threadID)
	<-first.Events() // init

	if err := first.Send("My favorite number is 8842. Reply with exactly: OK"); err != nil {
		t.Fatalf("send turn 1: %v", err)
	}
	for ev := range first.Events() {
		if ev.IsTurnEnd() {
			break
		}
	}
	first.Stop()
	_ = first.Wait()

	second := New(Options{Cwd: dir, Sandbox: "read-only", ApprovalPolicy: "never", ResumeID: threadID})
	if err := second.Start(ctx); err != nil {
		t.Fatalf("start second (thread/resume threadId=%s): %v", threadID, err)
	}
	defer second.Stop()
	t.Logf("resumed thread id: %s", second.SessionID())
	select {
	case ev := <-second.Events():
		if ev.IsInit() {
			t.Logf("resume init event: session=%s model=%s", ev.SessionID, ev.Model)
		}
	case <-time.After(5 * time.Second):
	}

	if err := second.Send("What is my favorite number? Reply with exactly the number, nothing else."); err != nil {
		t.Fatalf("send turn 2: %v", err)
	}
	var gotText string
	for ev := range second.Events() {
		if ev.Type == driver.TypeAssistant && ev.Message != nil {
			for _, b := range ev.Message.Blocks() {
				if b.Type == driver.BlockText {
					gotText += b.Text
				}
			}
		}
		if ev.IsTurnEnd() {
			second.Stop()
			break
		}
	}
	t.Logf("resumed thread's answer: %q", gotText)
	if !strings.Contains(gotText, "8842") {
		t.Errorf("resumed thread did not recall the secret number (got %q) — thread/resume's %q param name is likely wrong; "+
			"the resumed thread started with no memory of turn 1", gotText, "threadId")
	}
}

// TestLiveCodexStructuredOutputField probes unverified protocol detail #3,
// explicitly flagged as load-bearing: which field on turn completion carries
// the schema-validated result when TurnStartParams.outputSchema is set.
// handleTurnCompleted (internal/codex/translate.go) currently never sets
// driver.Event.StructuredOutput at all — this either proves that omission
// wrong by finding the real field and reporting it, or proves the
// alternative hypothesis that outputSchema constrains the final agentMessage
// item's own text (i.e., the model's last message IS the validated JSON, no
// separate field exists at all).
//
//	ACY_LIVE=1 go test ./internal/codex/ -run TestLiveCodexStructuredOutputField -v
func TestLiveCodexStructuredOutputField(t *testing.T) {
	requireLiveCodex(t)
	logPath := openLiveLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	schema := json.RawMessage(`{"type":"object","required":["outcome","note"],"additionalProperties":false,"properties":{"outcome":{"type":"string","enum":["ok"]},"note":{"type":"string"}}}`)
	d := New(Options{
		Cwd:            liveScratchDir(t),
		Sandbox:        "read-only",
		ApprovalPolicy: "never",
		OutputSchema:   schema,
	})
	if err := d.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer d.Stop()
	<-d.Events() // init

	if err := d.Send(`Respond with your structured report: outcome "ok", note "hello".`); err != nil {
		t.Fatalf("send: %v", err)
	}

	var lastAssistantText string
	var result driver.Event
	var gotResult bool
	for ev := range d.Events() {
		if ev.Type == driver.TypeAssistant && ev.Message != nil {
			for _, b := range ev.Message.Blocks() {
				if b.Type == driver.BlockText {
					lastAssistantText = b.Text
				}
			}
		}
		if ev.IsTurnEnd() {
			result = ev
			gotResult = true
			d.Stop()
			break
		}
	}
	if !gotResult {
		t.Fatal("no result event")
	}

	t.Logf("last assistant text: %q", lastAssistantText)
	t.Logf("result.StructuredOutput (as currently decoded): %s", result.Result)
	if len(result.StructuredOutput) > 0 {
		t.Logf("result.StructuredOutput: %s", result.StructuredOutput)
	} else {
		t.Log("result.StructuredOutput is empty — translate.go does not currently populate it from anything")
	}

	body := readLiveLog(t, logPath)
	for line := range strings.SplitSeq(body, "\n") {
		if strings.Contains(line, "RX ") && (strings.Contains(line, `"turn/completed"`) || strings.Contains(line, "outcome")) {
			t.Logf("wire: %s", line)
		}
	}

	if !strings.Contains(lastAssistantText, "outcome") {
		t.Error("the final assistant message does not contain the schema's own field names — if outputSchema constrains the final message, this should be exactly the validated JSON")
	}
	if json.Valid([]byte(lastAssistantText)) {
		t.Logf("the final assistant message IS valid JSON: %s", lastAssistantText)
	}
}
