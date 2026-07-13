package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hweeks/always-click-yes/internal/driver"
)

// bufCloser is an in-memory io.WriteCloser so a test can inspect the exact bytes
// the driver would write to claude's stdin.
type bufCloser struct{ *bytes.Buffer }

func (bufCloser) Close() error { return nil }

// errCloser fails every write, standing in for a dead claude stdin.
type errCloser struct{}

func (errCloser) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
func (errCloser) Close() error              { return nil }

// askFixture is the AskUserQuestion tool_use line as claude emits it, kept in
// testdata so the offline tests below are checked against a payload we can
// refresh from a live run rather than one we invented. TestLiveAskUserQuestion
// (internal/driver) re-captures it; see AGENTS.md on why it can't run in CI.
func askFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "ask_tool_use.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return bytes.TrimSpace(b)
}

// askModel builds a model whose driver writes into buf, with the fixture's
// question already open in the panel.
func askModel(t *testing.T, w interface {
	Write([]byte) (int, error)
	Close() error
}) *Model {
	t.Helper()
	ev, err := driver.Decode(askFixture(t))
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	m := &Model{drv: driver.NewWithWriter(driver.Options{}, w)}
	for _, b := range ev.Message.Blocks() {
		if b.Type == driver.BlockToolUse {
			m.ingestToolUse(b)
		}
	}
	if m.ask == nil {
		t.Fatal("fixture did not open the ask panel")
	}
	return m
}

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

// sentResult decodes the single tool_result the driver wrote back to claude.
func sentResult(t *testing.T, buf *bytes.Buffer) (toolUseID, content string) {
	t.Helper()
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
	b := sent.Message.Content[0]
	if b.Type != "tool_result" {
		t.Errorf("content type = %q, want tool_result", b.Type)
	}
	return b.ToolUseID, b.Content
}

// TestAskEndToEnd drives an AskUserQuestion tool call through acy's whole consumer
// path — the stream-json line claude emits, decoded by the driver, ingested into
// the panel, answered by keystroke — and asserts the precise tool_result acy
// writes back.
//
// Expectations are derived from the fixture rather than hardcoded, so refreshing
// testdata from a live capture (ACY_UPDATE_FIXTURE=1, see ask_live_test.go)
// cannot silently break this test.
func TestAskEndToEnd(t *testing.T) {
	buf := bufCloser{&bytes.Buffer{}}
	m := askModel(t, buf)

	wantID := m.ask.toolUseID
	if wantID == "" {
		t.Fatal("fixture carries no tool_use id; the tool_result could not be routed back")
	}

	// Answer every question, taking the second option where there is one so the
	// test proves the selection is actually read rather than defaulted.
	var picked []string
	for range len(m.ask.questions) {
		if m.ask == nil {
			break
		}
		q := &m.ask.questions[m.ask.qIdx]
		idx := 0
		if len(q.options) > 1 {
			idx = 1
			m.handleAskKey(tea.KeyMsg{Type: tea.KeyDown})
		}
		picked = append(picked, q.options[idx].label)
		m.handleAskKey(tea.KeyMsg{Type: tea.KeyEnter})
	}

	if m.ask != nil {
		t.Error("expected the panel to close after Enter on the last question")
	}

	id, content := sentResult(t, buf.Buffer)
	if id != wantID {
		t.Errorf("tool_use_id = %q, want %q — claude could not match the result to its call", id, wantID)
	}
	for _, label := range picked {
		if !strings.Contains(content, label) {
			t.Errorf("answer content = %q, want it to mention the chosen label %q", content, label)
		}
	}
}

// Esc skips the questions. It must still send a tool_result: the turn is blocked
// on this tool call, so a skip that sends nothing hangs claude forever.
func TestAskEscapeStillAnswers(t *testing.T) {
	buf := bufCloser{&bytes.Buffer{}}
	m := askModel(t, buf)

	m.handleAskKey(tea.KeyMsg{Type: tea.KeyEsc})

	if m.ask != nil {
		t.Error("expected the panel to close after Esc")
	}
	id, content := sentResult(t, buf.Buffer)
	if id != "toolu_ask1" {
		t.Errorf("tool_use_id = %q, want toolu_ask1", id)
	}
	if !strings.Contains(content, "skipped") {
		t.Errorf("skip answer = %q, want it to tell claude to proceed on its own judgment", content)
	}
}

// Multi-select: space toggles choices and every selected label ends up in the
// answer. Enter with nothing toggled falls back to the cursor row, so a
// multi-select question can never return an empty answer.
func TestAskMultiSelect(t *testing.T) {
	newModel := func() (*Model, *bytes.Buffer) {
		buf := bufCloser{&bytes.Buffer{}}
		m := &Model{
			drv: driver.NewWithWriter(driver.Options{}, buf),
			ask: &askState{
				toolUseID: "tu_multi",
				questions: []askQuestion{{
					header:      "Targets",
					multiSelect: true,
					options:     []askOption{{label: "alpha"}, {label: "beta"}, {label: "gamma"}},
					selected:    map[int]bool{},
				}},
			},
		}
		return m, buf.Buffer
	}

	// Toggle alpha and gamma, leave beta off.
	m, buf := newModel()
	m.handleAskKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}}) // alpha on
	m.handleAskKey(tea.KeyMsg{Type: tea.KeyDown})                      // -> beta
	m.handleAskKey(tea.KeyMsg{Type: tea.KeyDown})                      // -> gamma
	m.handleAskKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}}) // gamma on
	m.handleAskKey(tea.KeyMsg{Type: tea.KeyEnter})

	_, content := sentResult(t, buf)
	if !strings.Contains(content, "alpha") || !strings.Contains(content, "gamma") {
		t.Errorf("answer = %q, want both toggled labels", content)
	}
	if strings.Contains(content, "beta") {
		t.Errorf("answer = %q, want the untoggled label left out", content)
	}

	// Enter with nothing toggled: the cursor row is selected rather than sending
	// an empty answer.
	m, buf = newModel()
	m.handleAskKey(tea.KeyMsg{Type: tea.KeyDown}) // cursor on beta, nothing toggled
	m.handleAskKey(tea.KeyMsg{Type: tea.KeyEnter})

	_, content = sentResult(t, buf)
	if !strings.Contains(content, "beta") {
		t.Errorf("answer = %q, want Enter with no toggles to fall back to the cursor row", content)
	}
}

// Enter on a non-final question advances instead of submitting; only the last
// question sends the tool_result, and it carries every question's answer.
func TestAskAdvancesThroughQuestions(t *testing.T) {
	buf := bufCloser{&bytes.Buffer{}}
	m := &Model{
		drv: driver.NewWithWriter(driver.Options{}, buf),
		ask: &askState{
			toolUseID: "tu_multi_q",
			questions: []askQuestion{
				{header: "First", options: []askOption{{label: "one"}, {label: "two"}}, selected: map[int]bool{}},
				{header: "Second", options: []askOption{{label: "red"}, {label: "blue"}}, selected: map[int]bool{}},
			},
		},
	}

	m.handleAskKey(tea.KeyMsg{Type: tea.KeyEnter}) // answer "one", advance
	if m.ask == nil {
		t.Fatal("panel closed on the first of two questions")
	}
	if m.ask.qIdx != 1 {
		t.Fatalf("qIdx = %d, want 1 after answering the first question", m.ask.qIdx)
	}
	if buf.Len() != 0 {
		t.Fatalf("a tool_result was sent before the last question was answered: %q", buf.String())
	}

	m.handleAskKey(tea.KeyMsg{Type: tea.KeyDown})  // -> blue
	m.handleAskKey(tea.KeyMsg{Type: tea.KeyEnter}) // answer "blue", submit
	if m.ask != nil {
		t.Error("expected the panel to close after the last question")
	}

	_, content := sentResult(t, buf.Buffer)
	for _, want := range []string{"First", "one", "Second", "blue"} {
		if !strings.Contains(content, want) {
			t.Errorf("answer = %q, want it to contain %q", content, want)
		}
	}
}

// An AskUserQuestion whose input we can't parse must fall through to a plain tool
// entry. Opening an empty panel would trap the user: the panel swallows every key
// and there would be no option to select to get out of it.
func TestAskUnparsableFallsBackToToolEntry(t *testing.T) {
	m := &Model{}
	m.ingestToolUse(driver.ContentBlock{
		Type:  driver.BlockToolUse,
		ID:    "tu_bad",
		Name:  "AskUserQuestion",
		Input: json.RawMessage(`{"questions":[]}`),
	})

	if m.ask != nil {
		t.Fatal("an unparsable AskUserQuestion opened a panel with no options to pick")
	}
	if len(m.entries) != 1 || m.entries[0].kind != eTool || m.entries[0].title != "AskUserQuestion" {
		t.Fatalf("want a plain tool entry, got %+v", m.entries)
	}
}

// An MCP-provided AskUserQuestion arrives as mcp__<server>__AskUserQuestion. It
// must open the same panel — --plan-tools already accepts MCP-prefixed names, so
// ingestToolUse has to recognise them too.
func TestAskMCPPrefixedOpensPanel(t *testing.T) {
	m := &Model{}
	m.ingestToolUse(driver.ContentBlock{
		Type:  driver.BlockToolUse,
		ID:    "tu_mcp",
		Name:  "mcp__questions__AskUserQuestion",
		Input: json.RawMessage(`{"questions":[{"header":"H","question":"Q?","options":[{"label":"yes"}]}]}`),
	})

	if m.ask == nil {
		t.Fatal("an MCP-prefixed AskUserQuestion did not open the panel")
	}
	if m.ask.toolUseID != "tu_mcp" {
		t.Errorf("tool_use id = %q, want tu_mcp", m.ask.toolUseID)
	}
}

// If the tool_result can't be written, the turn is stuck — claude is blocked on a
// result that will never arrive. Say so, instead of clearing the panel and
// parking on "working…" forever with no explanation.
func TestAskSurfacesSendFailure(t *testing.T) {
	m := askModel(t, errCloser{})
	// Esc submits in one keystroke regardless of how many questions the fixture
	// carries, so this stays correct across a fixture refresh.
	m.handleAskKey(tea.KeyMsg{Type: tea.KeyEsc})

	if m.ask != nil {
		t.Error("expected the panel to close even when the send failed")
	}
	if m.processing {
		t.Error("processing = true after a failed send; the turn is not actually running")
	}
	var warned bool
	for _, e := range m.entries {
		if e.kind == eWarn && strings.Contains(e.body, "could not send") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning entry after a failed tool_result send; entries = %+v", m.entries)
	}
}

// A question left open when the session ends must be cleared. The panel captures
// every keystroke, so an orphaned one over a dead driver locks the UI: its only
// exits (Enter/Esc) both write to a closed pipe.
func TestAskClearedWhenStreamCloses(t *testing.T) {
	m := sizedModel(t)
	m.ask = &askState{
		toolUseID: "tu_orphan",
		questions: []askQuestion{{options: []askOption{{label: "x"}}, selected: map[int]bool{}}},
	}

	next, _ := m.Update(streamClosedMsg{gen: m.gen})
	if next.(Model).ask != nil {
		t.Error("the ask panel survived the session ending; the UI would be stuck")
	}
}
