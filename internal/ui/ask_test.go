package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hweeks/always-click-yes/internal/driver"
)

// bufCloser is an in-memory io.WriteCloser so a test can inspect the exact bytes
// the driver would write to claude's stdin.
type bufCloser struct{ *bytes.Buffer }

func (bufCloser) Close() error { return nil }

func TestParseAsk(t *testing.T) {
	in := `{"questions":[{"header":"Color","question":"Which color?","multiSelect":false,
		"options":[{"label":"red","description":"warm"},{"label":"blue","description":"cool"}]}]}`
	a, ok := parseAsk(json.RawMessage(in))
	if !ok {
		t.Fatal("expected parseAsk to succeed")
	}
	if len(a.questions) != 1 {
		t.Fatalf("want 1 question, got %d", len(a.questions))
	}
	q := a.questions[0]
	if q.header != "Color" || q.question != "Which color?" || q.multiSelect {
		t.Errorf("unexpected question fields: %+v", q)
	}
	if len(q.options) != 2 || q.options[0].label != "red" || q.options[1].label != "blue" {
		t.Errorf("unexpected options: %+v", q.options)
	}
}

func TestParseAskRejectsEmpty(t *testing.T) {
	for _, in := range []string{`{}`, `{"questions":[]}`, `{"questions":[{"question":"q","options":[]}]}`, `not json`} {
		if _, ok := parseAsk(json.RawMessage(in)); ok {
			t.Errorf("expected parseAsk(%q) to fail", in)
		}
	}
}

func TestSubmitAskBuildsAnswer(t *testing.T) {
	// A model with no driver: submitAsk should clear state and not panic.
	m := &Model{ask: &askState{
		toolUseID: "tu1",
		questions: []askQuestion{{
			header:   "Color",
			options:  []askOption{{label: "red"}, {label: "blue"}},
			selected: map[int]bool{1: true},
		}},
	}}
	m.submitAsk(false)
	if m.ask != nil {
		t.Error("expected ask cleared after submit")
	}
	// The answer echo should have been appended.
	found := false
	for _, e := range m.entries {
		if e.kind == eYou && strings.Contains(e.body, "blue") {
			found = true
		}
	}
	if !found {
		t.Error("expected an answer entry mentioning the selected label")
	}
}

// TestAskEndToEnd drives a real AskUserQuestion tool call through acy's whole
// consumer path — the exact stream-json line claude emits, decoded by the driver,
// ingested into the panel, answered via keystrokes — and asserts the precise
// tool_result acy writes back. This is the harness-independent proof that acy
// handles AskUserQuestion: it needs no live claude, only a faithful wire payload.
func TestAskEndToEnd(t *testing.T) {
	// The assistant event exactly as claude emits it for an AskUserQuestion call.
	raw := `{"type":"assistant","message":{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_ask1","name":"AskUserQuestion","input":` +
		`{"questions":[{"header":"Scope","question":"Which target?","multiSelect":false,` +
		`"options":[{"label":"acy handles it","description":"consumer side"},` +
		`{"label":"This harness","description":"producer side"}]}]}}]}}`

	ev, err := driver.Decode([]byte(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Capture whatever the driver would send back to claude.
	buf := bufCloser{&bytes.Buffer{}}
	drv := driver.NewWithWriter(driver.Options{}, buf)
	m := &Model{drv: drv}

	// Ingest the real tool_use block: the panel must open with the parsed question.
	var used bool
	for _, b := range ev.Message.Blocks() {
		if b.Type == driver.BlockToolUse {
			m.ingestToolUse(b)
			used = true
		}
	}
	if !used {
		t.Fatal("no tool_use block decoded from the event")
	}
	if m.ask == nil {
		t.Fatal("expected the AskUserQuestion panel to open")
	}
	if m.ask.toolUseID != "toolu_ask1" {
		t.Errorf("tool_use id = %q, want toolu_ask1", m.ask.toolUseID)
	}
	if len(m.ask.questions) != 1 || len(m.ask.questions[0].options) != 2 {
		t.Fatalf("unexpected parsed panel: %+v", m.ask.questions)
	}

	// Answer it: arrow down to the second option, Enter to confirm and submit.
	m.handleAskKey(tea.KeyMsg{Type: tea.KeyDown})
	m.handleAskKey(tea.KeyMsg{Type: tea.KeyEnter})

	if m.ask != nil {
		t.Error("expected the panel to close after Enter on the last question")
	}

	// The driver must have written a single tool_result referencing the call id
	// and carrying the chosen label.
	var sent struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
				Content   string `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &sent); err != nil {
		t.Fatalf("tool_result was not valid JSON: %v (raw=%q)", err, buf.String())
	}
	if sent.Type != "user" || sent.Message.Role != "user" {
		t.Errorf("wrong envelope: type=%q role=%q", sent.Type, sent.Message.Role)
	}
	if len(sent.Message.Content) != 1 {
		t.Fatalf("want 1 content block, got %d", len(sent.Message.Content))
	}
	block := sent.Message.Content[0]
	if block.Type != "tool_result" {
		t.Errorf("content type = %q, want tool_result", block.Type)
	}
	if block.ToolUseID != "toolu_ask1" {
		t.Errorf("tool_use_id = %q, want toolu_ask1", block.ToolUseID)
	}
	if !strings.Contains(block.Content, "This harness") {
		t.Errorf("answer content = %q, want it to mention the chosen label", block.Content)
	}
}
