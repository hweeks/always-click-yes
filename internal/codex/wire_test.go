package codex

import (
	"bytes"
	"encoding/json"
	"testing"
)

// nopWriteCloser adapts a *bytes.Buffer to io.WriteCloser for tests that never
// launch a real process — mirrors driver.NewWithWriter's own test seam.
type nopWriteCloser struct{ *bytes.Buffer }

func (nopWriteCloser) Close() error { return nil }

// decodeWire unmarshals a single written line into a generic map so tests can
// assert on shape without caring about key order.
func decodeWire(t *testing.T, b []byte) map[string]any {
	t.Helper()
	b = bytes.TrimRight(b, "\n")
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("invalid JSON written: %v (%s)", err, b)
	}
	return got
}

func TestWireDoesNotIncludeJSONRPCField(t *testing.T) {
	// codex's app-server is JSON-RPC-*shaped* but does not carry a
	// "jsonrpc":"2.0" field (docs/codex-cli-findings.md §2) — every request
	// this package sends must not add one that was never there.
	var tx bytes.Buffer
	d := NewWithWriter(Options{}, nopWriteCloser{&tx})
	if _, _, err := d.sendRequest("initialize", d.initializeParams(), kindGeneric); err != nil {
		t.Fatal(err)
	}
	got := decodeWire(t, tx.Bytes())
	if _, ok := got["jsonrpc"]; ok {
		t.Errorf("must not include a jsonrpc field, got: %s", tx.String())
	}
}

func TestWireInitialize(t *testing.T) {
	var tx bytes.Buffer
	d := NewWithWriter(Options{}, nopWriteCloser{&tx})
	if _, _, err := d.sendRequest("initialize", d.initializeParams(), kindGeneric); err != nil {
		t.Fatal(err)
	}

	got := decodeWire(t, tx.Bytes())
	if got["id"] != float64(1) {
		t.Errorf("id = %v, want 1", got["id"])
	}
	if got["method"] != "initialize" {
		t.Errorf("method = %v, want initialize", got["method"])
	}
	params, ok := got["params"].(map[string]any)
	if !ok {
		t.Fatalf("params missing or wrong shape: %v", got["params"])
	}
	clientInfo, ok := params["clientInfo"].(map[string]any)
	if !ok {
		t.Fatalf("clientInfo missing or wrong shape: %v", params["clientInfo"])
	}
	if name, _ := clientInfo["name"].(string); name == "" {
		t.Errorf("clientInfo.name is empty: %v", clientInfo)
	}
}

func TestWireThreadStart(t *testing.T) {
	var tx bytes.Buffer
	d := NewWithWriter(Options{
		Cwd:                   "/tmp/codex-scratch",
		Model:                 "gpt-5.6-terra",
		Sandbox:               "readOnly",
		ApprovalPolicy:        "on-request",
		DeveloperInstructions: "be terse",
		Config:                map[string]any{"mcp_servers": map[string]any{"acy": map[string]any{"command": "/bin/acy"}}},
	}, nopWriteCloser{&tx})
	if _, _, err := d.sendRequest("thread/start", d.threadStartParams(), kindThreadStart); err != nil {
		t.Fatal(err)
	}

	got := decodeWire(t, tx.Bytes())
	if got["method"] != "thread/start" {
		t.Errorf("method = %v, want thread/start", got["method"])
	}
	if _, ok := got["id"]; !ok {
		t.Error("missing id")
	}
	params, ok := got["params"].(map[string]any)
	if !ok {
		t.Fatalf("params missing or wrong shape: %v", got["params"])
	}
	for key, want := range map[string]string{
		"cwd":                   "/tmp/codex-scratch",
		"model":                 "gpt-5.6-terra",
		"sandbox":               "readOnly",
		"approvalPolicy":        "on-request",
		"developerInstructions": "be terse",
	} {
		if got := params[key]; got != want {
			t.Errorf("params[%q] = %v, want %q", key, got, want)
		}
	}
	config, ok := params["config"].(map[string]any)
	if !ok {
		t.Fatalf("config missing or wrong shape: %v", params["config"])
	}
	if _, ok := config["mcp_servers"]; !ok {
		t.Errorf("config.mcp_servers missing: %v", config)
	}
}

func TestWireThreadStartOmitsUnsetFields(t *testing.T) {
	var tx bytes.Buffer
	d := NewWithWriter(Options{}, nopWriteCloser{&tx})
	if _, _, err := d.sendRequest("thread/start", d.threadStartParams(), kindThreadStart); err != nil {
		t.Fatal(err)
	}

	got := decodeWire(t, tx.Bytes())
	params, ok := got["params"].(map[string]any)
	if !ok {
		t.Fatalf("params missing or wrong shape: %v", got["params"])
	}
	for _, key := range []string{"cwd", "model", "sandbox", "approvalPolicy", "developerInstructions", "effort", "config"} {
		if _, present := params[key]; present {
			t.Errorf("params[%q] should be omitted when unset, got: %v", key, params[key])
		}
	}
}

func TestIsolateMCPConfigDisablesOnlyInheritedServers(t *testing.T) {
	config := map[string]any{"mcp_servers": map[string]any{
		"acy":  map[string]any{"command": "/bin/acy"},
		"jira": map[string]any{"command": "/bin/jira"},
	}}
	got := isolateMCPConfig(config, map[string]json.RawMessage{
		"personal": json.RawMessage(`{"command":"personal"}`),
		"jira":     json.RawMessage(`{"command":"old-jira"}`),
	})
	servers := got["mcp_servers"].(map[string]any)
	if disabled := servers["personal"].(map[string]any)["enabled"]; disabled != false {
		t.Errorf("inherited personal server enabled = %v, want false", disabled)
	}
	if gotJira := servers["jira"].(map[string]any)["command"]; gotJira != "/bin/jira" {
		t.Errorf("selected Jira config = %v, want acy's explicit server", servers["jira"])
	}
	if gotAcy := servers["acy"].(map[string]any)["command"]; gotAcy != "/bin/acy" {
		t.Errorf("selected acy config = %v", servers["acy"])
	}
}

func TestReadLoopClosesEventsAndApprovals(t *testing.T) {
	d := New(Options{})
	d.readLoop(bytes.NewReader(nil))
	if _, ok := <-d.Events(); ok {
		t.Error("Events remained open after stdout EOF")
	}
	if _, ok := <-d.Approvals(); ok {
		t.Error("Approvals remained open after stdout EOF")
	}
}

func TestWireTurnStart(t *testing.T) {
	var tx bytes.Buffer
	d := NewWithWriter(Options{OutputSchema: json.RawMessage(`{"type":"object"}`)}, nopWriteCloser{&tx})
	d.mu.Lock()
	d.threadID = "01a01547-26f7-7fd2-afeb-349415035aa2"
	d.mu.Unlock()

	if _, _, err := d.sendRequest("turn/start", d.turnStartParams("hello there"), kindTurnStart); err != nil {
		t.Fatal(err)
	}

	got := decodeWire(t, tx.Bytes())
	if got["method"] != "turn/start" {
		t.Errorf("method = %v, want turn/start", got["method"])
	}
	params, ok := got["params"].(map[string]any)
	if !ok {
		t.Fatalf("params missing or wrong shape: %v", got["params"])
	}
	if params["threadId"] != "01a01547-26f7-7fd2-afeb-349415035aa2" {
		t.Errorf("threadId = %v", params["threadId"])
	}
	input, ok := params["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input wrong shape: %v", params["input"])
	}
	block, ok := input[0].(map[string]any)
	if !ok || block["type"] != "text" || block["text"] != "hello there" {
		t.Errorf("input[0] = %v, want {type:text, text:%q}", block, "hello there")
	}
	if _, ok := params["outputSchema"]; !ok {
		t.Error("outputSchema missing despite Options.OutputSchema being set")
	}
}

func TestWireTurnInterrupt(t *testing.T) {
	var tx bytes.Buffer
	d := NewWithWriter(Options{}, nopWriteCloser{&tx})
	d.mu.Lock()
	d.threadID = "thread-1"
	d.activeTurnID = "turn-1"
	d.mu.Unlock()

	if _, _, err := d.sendRequest("turn/interrupt", d.turnInterruptParams(), kindGeneric); err != nil {
		t.Fatal(err)
	}

	got := decodeWire(t, tx.Bytes())
	if got["method"] != "turn/interrupt" {
		t.Errorf("method = %v, want turn/interrupt", got["method"])
	}
	params, ok := got["params"].(map[string]any)
	if !ok {
		t.Fatalf("params missing or wrong shape: %v", got["params"])
	}
	if params["threadId"] != "thread-1" || params["turnId"] != "turn-1" {
		t.Errorf("params = %v, want threadId=thread-1 turnId=turn-1", params)
	}
}

// TestInterruptNoOpWithoutActiveTurn pins the required no-op behavior: with no
// turn in flight, Interrupt must return nil and write nothing at all — not an
// error, and not a request codex has no turn to apply it to.
func TestInterruptNoOpWithoutActiveTurn(t *testing.T) {
	var tx bytes.Buffer
	d := NewWithWriter(Options{}, nopWriteCloser{&tx})
	if err := d.Interrupt(); err != nil {
		t.Fatalf("Interrupt() with no active turn returned %v, want nil", err)
	}
	if tx.Len() != 0 {
		t.Errorf("Interrupt() with no active turn wrote %q, want nothing", tx.String())
	}
}

// TestWireApprovalReply pins the exact reply shape the fixture captures live
// (docs/codex-cli-findings.md §3): {"id": 0, "result": {"decision": "decline"}}.
func TestWireApprovalReply(t *testing.T) {
	var tx bytes.Buffer
	d := NewWithWriter(Options{}, nopWriteCloser{&tx})
	if err := d.Approve(0, "decline"); err != nil {
		t.Fatal(err)
	}

	got := decodeWire(t, tx.Bytes())
	if got["id"] != float64(0) {
		t.Errorf("id = %v, want 0", got["id"])
	}
	if _, ok := got["method"]; ok {
		t.Error("an approval reply is a response, not a request; must not carry a method")
	}
	result, ok := got["result"].(map[string]any)
	if !ok {
		t.Fatalf("result missing or wrong shape: %v", got["result"])
	}
	if result["decision"] != "decline" {
		t.Errorf("result.decision = %v, want decline", result["decision"])
	}
}

// TestWireMCPElicitationReply uses the other response envelope Codex expects
// for mcpServer/elicitation/request: {action:"accept"}, not the item-tool
// approval's {decision:"accept"}. This was verified by the first dogfood
// supervisor run: the wrong field makes Codex refuse Dispatch before acy's MCP
// server receives it.
func TestWireMCPElicitationReply(t *testing.T) {
	var tx bytes.Buffer
	d := NewWithWriter(Options{}, nopWriteCloser{&tx})
	d.handleServerRequest(wireEnvelope{
		ID:     json.RawMessage(`7`),
		Method: methodMCPServerElicitation,
		Params: json.RawMessage(`{"mode":"form","requestedSchema":{"type":"object"}}`),
	})
	if err := d.Approve(7, "accept"); err != nil {
		t.Fatal(err)
	}

	result, ok := decodeWire(t, tx.Bytes())["result"].(map[string]any)
	if !ok {
		t.Fatalf("result missing or wrong shape: %s", tx.String())
	}
	if result["action"] != "accept" {
		t.Errorf("result.action = %v, want accept", result["action"])
	}
	if _, ok := result["decision"]; ok {
		t.Errorf("MCP elicitation response must use action, not decision: %s", tx.String())
	}
}

// TestApproveClearsOutstanding confirms answering a request removes it from
// PendingApprovals, so a caller can trust that list as "still blocked."
func TestApproveClearsOutstanding(t *testing.T) {
	var tx bytes.Buffer
	d := NewWithWriter(Options{}, nopWriteCloser{&tx})
	d.handleServerRequest(wireEnvelope{
		ID:     json.RawMessage(`0`),
		Method: "item/commandExecution/requestApproval",
		Params: json.RawMessage(`{"reason":"write a file?"}`),
	})
	if len(d.PendingApprovals()) != 1 {
		t.Fatalf("want 1 pending approval before Approve, got %d", len(d.PendingApprovals()))
	}
	if err := d.Approve(0, "decline"); err != nil {
		t.Fatal(err)
	}
	if len(d.PendingApprovals()) != 0 {
		t.Errorf("want 0 pending approvals after Approve, got %d", len(d.PendingApprovals()))
	}
}
