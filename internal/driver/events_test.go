package driver

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDecodeTurn walks a captured real turn and asserts each line decodes into
// the expected typed shape.
func TestDecodeTurn(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "turn.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	for _, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		ev, err := Decode(line)
		if err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		events = append(events, ev)
	}
	if len(events) != 6 {
		t.Fatalf("want 6 events, got %d", len(events))
	}

	// init
	if !events[0].IsInit() {
		t.Errorf("event 0 should be init")
	}
	if events[0].SessionID == "" || events[0].Model != "claude-sonnet-5" || events[0].PermissionMode != "default" {
		t.Errorf("init fields wrong: %+v", events[0])
	}

	// thinking block
	tb := events[1].Message.Blocks()
	if len(tb) != 1 || tb[0].Type != BlockThinking || tb[0].Thinking == "" {
		t.Errorf("thinking block wrong: %+v", tb)
	}

	// tool_use block
	ub := events[2].Message.Blocks()
	if len(ub) != 1 || ub[0].Type != BlockToolUse || ub[0].Name != "Bash" {
		t.Errorf("tool_use block wrong: %+v", ub)
	}
	if string(ub[0].Input) == "" {
		t.Errorf("tool_use input missing")
	}

	// tool_result block
	rb := events[3].Message.Blocks()
	if len(rb) != 1 || rb[0].Type != BlockToolResult || rb[0].ToolUseID != "toolu_01URPAqstoCXFcfhT2Pg5HMY" {
		t.Errorf("tool_result block wrong: %+v", rb)
	}

	// text block
	xb := events[4].Message.Blocks()
	if len(xb) != 1 || xb[0].Type != BlockText || xb[0].Text != "probe-ok" {
		t.Errorf("text block wrong: %+v", xb)
	}

	// result / turn end
	r := events[5]
	if !r.IsTurnEnd() {
		t.Errorf("event 5 should be turn end")
	}
	if r.StopReason != "end_turn" || r.TerminalReason != "completed" {
		t.Errorf("result stop fields wrong: %+v", r)
	}
	if r.TotalCostUSD <= 0 {
		t.Errorf("cost not parsed: %v", r.TotalCostUSD)
	}
}

// TestBlocksStringContent confirms a bare-string content (our own user injection
// shape) normalizes to a single text block.
func TestBlocksStringContent(t *testing.T) {
	ev, err := Decode([]byte(`{"type":"user","message":{"role":"user","content":"hello there"}}`))
	if err != nil {
		t.Fatal(err)
	}
	b := ev.Message.Blocks()
	if len(b) != 1 || b[0].Type != BlockText || b[0].Text != "hello there" {
		t.Fatalf("string content not normalized: %+v", b)
	}
}

func splitLines(data []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := range data {
		if data[i] == '\n' {
			out = append(out, trimNewline(data[start:i]))
			start = i + 1
		}
	}
	if start < len(data) {
		out = append(out, trimNewline(data[start:]))
	}
	return out
}
