package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// handshake is the exact byte sequence claude 2.1.207 sends an MCP server, captured
// live from a probe server. Testing against a recording rather than against our own
// idea of the protocol is the whole point: the parts that bite here (a notification
// with no id, a bare tool name, the id-echo) are all things a from-memory
// implementation gets subtly wrong.
func handshake(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "claude_handshake.jsonl"))
	if err != nil {
		t.Fatalf("read handshake: %v", err)
	}
	return b
}

// replies runs Serve over the recorded handshake and decodes what came back.
func replies(t *testing.T, h Handler) []response {
	t.Helper()
	var out bytes.Buffer
	if err := Serve(bytes.NewReader(handshake(t)), &out, h); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var got []response
	dec := json.NewDecoder(&out)
	for dec.More() {
		var r response
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("decode reply: %v", err)
		}
		got = append(got, r)
	}
	return got
}

func noopHandler(string, json.RawMessage, string) (string, error) { return "ok", nil }

// The load-bearing assertion: claude sends notifications/initialized with no id,
// and a reply to it is a protocol violation. Three requests carry ids, so exactly
// three responses may come back.
func TestServeAnswersOnlyRequests(t *testing.T) {
	got := replies(t, noopHandler)
	if len(got) != 3 {
		t.Fatalf("got %d replies, want 3 — the id-less notification must not be answered", len(got))
	}
	for i, want := range []string{"0", "1", "2"} {
		if string(got[i].ID) != want {
			t.Errorf("reply %d has id %s, want %s — claude matches replies by id", i, got[i].ID, want)
		}
		if got[i].JSONRPC != "2.0" {
			t.Errorf("reply %d: jsonrpc = %q, want 2.0", i, got[i].JSONRPC)
		}
	}
}

// initialize must echo the client's protocol version back.
func TestServeInitializeEchoesVersion(t *testing.T) {
	got := replies(t, noopHandler)
	res, ok := got[0].Result.(map[string]any)
	if !ok {
		t.Fatalf("initialize result is not an object: %#v", got[0].Result)
	}
	if v := res["protocolVersion"]; v != "2025-11-25" {
		t.Errorf("protocolVersion = %v, want the client's 2025-11-25", v)
	}
	info, _ := res["serverInfo"].(map[string]any)
	if info["name"] != ServerName {
		t.Errorf("serverInfo.name = %v, want %q — it decides the mcp__<name>__ prefix", info["name"], ServerName)
	}
}

// tools/list must advertise both tools, and each schema must be valid JSON — an
// invalid one is rejected silently by claude and the tool just never appears.
func TestServeListsTools(t *testing.T) {
	got := replies(t, noopHandler)
	res, ok := got[1].Result.(map[string]any)
	if !ok {
		t.Fatalf("tools/list result is not an object: %#v", got[1].Result)
	}
	list, _ := res["tools"].([]any)
	if len(list) != 2 {
		t.Fatalf("advertised %d tools, want 2 (%s, %s)", len(list), ToolAsk, ToolPlan)
	}
	seen := map[string]bool{}
	for _, raw := range list {
		td, _ := raw.(map[string]any)
		name, _ := td["name"].(string)
		seen[name] = true
		if td["description"] == "" {
			t.Errorf("%s has no description; the model has nothing to decide from", name)
		}
		if _, ok := td["inputSchema"].(map[string]any); !ok {
			t.Errorf("%s inputSchema did not survive as an object: %#v", name, td["inputSchema"])
		}
	}
	if !seen[ToolAsk] || !seen[ToolPlan] {
		t.Errorf("advertised tools = %v, want both %s and %s", seen, ToolAsk, ToolPlan)
	}
}

// tools/call passes the BARE tool name (not mcp__acy__-prefixed) and carries the
// tool_use id in _meta. Both matter: the name is what the handler switches on, and
// the id is what correlates the call with the event the UI already rendered.
func TestServeCallPassesBareNameAndToolUseID(t *testing.T) {
	var gotName, gotID string
	var gotArgs json.RawMessage
	replies(t, func(name string, args json.RawMessage, toolUseID string) (string, error) {
		gotName, gotArgs, gotID = name, args, toolUseID
		return "Color: blue", nil
	})

	if gotName != ToolAsk {
		t.Errorf("handler saw name %q, want the bare %q", gotName, ToolAsk)
	}
	if gotID != "toolu_01TaxdQjZ8r5BPPYxZjD8usU" {
		t.Errorf("toolUseID = %q, want the id from _meta[\"claudecode/toolUseId\"]", gotID)
	}
	// The arguments must be exactly what parseAsk expects to decode.
	var in struct {
		Questions []struct {
			Header  string `json:"header"`
			Options []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(gotArgs, &in); err != nil {
		t.Fatalf("arguments were not the shape the ask panel decodes: %v", err)
	}
	if len(in.Questions) != 1 || in.Questions[0].Header != "Color" || len(in.Questions[0].Options) != 2 {
		t.Errorf("arguments did not survive the hop: %+v", in)
	}
}

// The handler's text comes back as the tool result the model reads.
func TestServeCallReturnsHandlerText(t *testing.T) {
	got := replies(t, func(string, json.RawMessage, string) (string, error) {
		return "Color: blue", nil
	})
	res, ok := got[2].Result.(map[string]any)
	if !ok {
		t.Fatalf("tools/call result is not an object: %#v", got[2].Result)
	}
	if res["isError"] != false {
		t.Errorf("isError = %v, want false", res["isError"])
	}
	content, _ := res["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("want 1 content block, got %d", len(content))
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "Color: blue" {
		t.Errorf("content block = %#v, want the handler's text", block)
	}
}

// A handler error becomes an isError *result*, not a JSON-RPC error: the model can
// read a result and recover, whereas a protocol error just kills the turn.
func TestServeHandlerErrorBecomesToolError(t *testing.T) {
	got := replies(t, func(string, json.RawMessage, string) (string, error) {
		return "", errors.New("no such tool")
	})
	if got[2].Error != nil {
		t.Fatalf("handler error surfaced as a JSON-RPC error: %+v", got[2].Error)
	}
	res, _ := got[2].Result.(map[string]any)
	if res["isError"] != true {
		t.Errorf("isError = %v, want true", res["isError"])
	}
	content, _ := res["content"].([]any)
	block, _ := content[0].(map[string]any)
	if !strings.Contains(block["text"].(string), "no such tool") {
		t.Errorf("error text = %v, want the handler's message", block["text"])
	}
}

// An unknown method with an id gets a proper method-not-found; one without an id is
// still a notification and is still ignored.
func TestServeUnknownMethod(t *testing.T) {
	in := "{\"jsonrpc\":\"2.0\",\"id\":9,\"method\":\"resources/list\"}\n" +
		"{\"jsonrpc\":\"2.0\",\"method\":\"notifications/cancelled\"}\n"
	var out bytes.Buffer
	if err := Serve(strings.NewReader(in), &out, noopHandler); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var got []response
	dec := json.NewDecoder(&out)
	for dec.More() {
		var r response
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 1 {
		t.Fatalf("got %d replies, want 1 — the unknown notification must be ignored", len(got))
	}
	if got[0].Error == nil || got[0].Error.Code != codeMethodNotFound {
		t.Errorf("error = %+v, want code %d", got[0].Error, codeMethodNotFound)
	}
}

// Garbage on the wire must not take the server down: claude would see the pipe die
// and report the whole MCP server as failed.
func TestServeSurvivesGarbage(t *testing.T) {
	in := "not json at all\n" + "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}\n"
	var out bytes.Buffer
	if err := Serve(strings.NewReader(in), &out, noopHandler); err != nil {
		t.Fatalf("Serve died on a malformed line: %v", err)
	}
	if !strings.Contains(out.String(), ToolAsk) {
		t.Error("the request after the garbage line was never answered")
	}
}
