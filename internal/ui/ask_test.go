package ui

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/mcp"
)

// askFixture is the AskUserQuestion tool_use line as claude emits it, kept in
// testdata so the offline tests below are checked against a payload we can refresh
// from a live run rather than one we invented.
func askFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "ask_tool_use.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return bytes.TrimSpace(b)
}

// fixtureArgs pulls the tool input out of the fixture's tool_use block. That input
// is exactly what claude passes as a tools/call's `arguments`, so it is what
// arrives on the ask socket.
func fixtureArgs(t *testing.T) json.RawMessage {
	t.Helper()
	ev, err := driver.Decode(askFixture(t))
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	for _, b := range ev.Message.Blocks() {
		if b.Type == driver.BlockToolUse {
			return b.Input
		}
	}
	t.Fatal("fixture carries no tool_use block")
	return nil
}

// pendingFor builds an in-flight question the way the ask bridge would, and returns
// the channel its answer will land on.
func pendingFor(args string) (*mcp.Pending, <-chan mcp.Answer) {
	return mcp.NewPending(mcp.Request{
		Tool:      mcp.ToolAsk,
		ToolUseID: "tu_test",
		Args:      json.RawMessage(args),
	})
}

// answer reads the answer the UI resolved, failing if none arrived. Resolve is
// buffered, so a correctly-answered question is readable immediately.
func answer(t *testing.T, ch <-chan mcp.Answer) string {
	t.Helper()
	select {
	case a := <-ch:
		return a.Text
	case <-time.After(time.Second):
		t.Fatal("no answer was sent back — claude's turn would hang forever")
		return ""
	}
}

// askModel opens the fixture's question in the panel, exactly as the ask socket
// would, and returns the model plus the channel the answer must arrive on.
func askModel(t *testing.T, phase Phase) (*Model, <-chan mcp.Answer) {
	t.Helper()
	p, reply := mcp.NewPending(mcp.Request{
		Tool: mcp.ToolAsk, ToolUseID: "tu_fixture", Args: fixtureArgs(t),
	})
	m := &Model{phase: phase, countdown: 30 * time.Second, now: time.Now()}
	m.openAsk(p)
	if m.ask == nil {
		t.Fatal("fixture did not open the ask panel")
	}
	return m, reply
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

// TestAskSchemaMatchesParser holds mcp's advertised input schema and parseAsk
// together. They are declared in different packages, and if the schema ever
// describes a shape parseAsk cannot read, the panel silently degrades to a plain
// tool entry — which looks exactly like the tool not working at all.
func TestAskSchemaMatchesParser(t *testing.T) {
	// A payload using every field the schema advertises.
	in := `{"questions":[{
		"question":"Which store?","header":"Storage","multiSelect":true,
		"options":[{"label":"postgres","description":"durable"},{"label":"redis","description":"fast"}]}]}`
	a, ok := parseAsk(json.RawMessage(in))
	if !ok {
		t.Fatal("parseAsk rejected a payload that mcp.askSchema says is valid")
	}
	q := a.questions[0]
	if q.header != "Storage" || !q.multiSelect || len(q.options) != 2 {
		t.Errorf("schema fields did not survive the parse: %+v", q)
	}
	if q.options[0].description != "durable" {
		t.Errorf("option description dropped: %+v", q.options[0])
	}
}

// TestAskEndToEnd drives a question through acy's whole consumer path — the
// arguments claude sends over the ask socket, the panel, the keystrokes — and
// asserts the answer that goes back. That answer is the only thing that unblocks
// the turn.
func TestAskEndToEnd(t *testing.T) {
	m, reply := askModel(t, PhasePlan)

	// Answer every question, taking the second option where there is one so the test
	// proves the selection is actually read rather than defaulted.
	var picked []string
	for range len(m.ask.questions) {
		if m.ask == nil {
			break
		}
		q := &m.ask.questions[m.ask.qIdx]
		idx := 0
		if len(q.options) > 1 {
			idx = 1
			m.handleAskKey(tea.KeyPressMsg{Code: tea.KeyDown})
		}
		picked = append(picked, q.options[idx].label)
		m.handleAskKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	}

	if m.ask != nil {
		t.Error("expected the panel to close after Enter on the last question")
	}

	got := answer(t, reply)
	for _, label := range picked {
		if !strings.Contains(got, label) {
			t.Errorf("answer = %q, want it to mention the chosen label %q", got, label)
		}
	}
}

// Esc skips the questions. It must still answer: the turn is blocked on the MCP
// server's reply, so a skip that resolves nothing hangs claude forever.
func TestAskEscapeStillAnswers(t *testing.T) {
	m, reply := askModel(t, PhasePlan)

	m.handleAskKey(tea.KeyPressMsg{Code: tea.KeyEscape})

	if m.ask != nil {
		t.Error("expected the panel to close after Esc")
	}
	if got := answer(t, reply); !strings.Contains(got, "skipped") {
		t.Errorf("skip answer = %q, want it to tell claude to proceed on its own judgment", got)
	}
}

// Multi-select: space toggles choices and every selected label ends up in the
// answer. Enter with nothing toggled falls back to the cursor row, so a
// multi-select question can never return an empty answer.
func TestAskMultiSelect(t *testing.T) {
	const args = `{"questions":[{"header":"Targets","multiSelect":true,
		"options":[{"label":"alpha"},{"label":"beta"},{"label":"gamma"}]}]}`

	newModel := func() (*Model, <-chan mcp.Answer) {
		p, reply := pendingFor(args)
		m := &Model{phase: PhasePlan, now: time.Now()}
		m.openAsk(p)
		return m, reply
	}

	// Toggle alpha and gamma, leave beta off.
	m, reply := newModel()
	m.handleAskKey(tea.KeyPressMsg{Code: ' ', Text: " "}) // alpha on
	m.handleAskKey(tea.KeyPressMsg{Code: tea.KeyDown})    // -> beta
	m.handleAskKey(tea.KeyPressMsg{Code: tea.KeyDown})    // -> gamma
	m.handleAskKey(tea.KeyPressMsg{Code: ' ', Text: " "}) // gamma on
	m.handleAskKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	got := answer(t, reply)
	if !strings.Contains(got, "alpha") || !strings.Contains(got, "gamma") {
		t.Errorf("answer = %q, want both toggled labels", got)
	}
	if strings.Contains(got, "beta") {
		t.Errorf("answer = %q, want the untoggled label left out", got)
	}

	// Enter with nothing toggled: the cursor row is selected rather than sending an
	// empty answer.
	m, reply = newModel()
	m.handleAskKey(tea.KeyPressMsg{Code: tea.KeyDown}) // cursor on beta, nothing toggled
	m.handleAskKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	if got := answer(t, reply); !strings.Contains(got, "beta") {
		t.Errorf("answer = %q, want Enter with no toggles to fall back to the cursor row", got)
	}
}

// Enter on a non-final question advances instead of submitting; only the last
// question resolves the pending, and it carries every question's answer.
func TestAskAdvancesThroughQuestions(t *testing.T) {
	p, reply := pendingFor(`{"questions":[
		{"header":"First","options":[{"label":"one"},{"label":"two"}]},
		{"header":"Second","options":[{"label":"red"},{"label":"blue"}]}]}`)
	m := &Model{phase: PhasePlan, now: time.Now()}
	m.openAsk(p)

	m.handleAskKey(tea.KeyPressMsg{Code: tea.KeyEnter}) // answer "one", advance
	if m.ask == nil {
		t.Fatal("panel closed on the first of two questions")
	}
	if m.ask.qIdx != 1 {
		t.Fatalf("qIdx = %d, want 1 after answering the first question", m.ask.qIdx)
	}
	select {
	case a := <-reply:
		t.Fatalf("the question was answered before its last part: %q", a.Text)
	default:
	}

	m.handleAskKey(tea.KeyPressMsg{Code: tea.KeyDown})  // -> blue
	m.handleAskKey(tea.KeyPressMsg{Code: tea.KeyEnter}) // answer "blue", submit
	if m.ask != nil {
		t.Error("expected the panel to close after the last question")
	}

	got := answer(t, reply)
	for _, want := range []string{"First", "one", "Second", "blue"} {
		if !strings.Contains(got, want) {
			t.Errorf("answer = %q, want it to contain %q", got, want)
		}
	}
}

// A question whose arguments we can't parse must still be answered. Opening an
// empty panel would trap the user (it swallows every key and has no option to
// select), and answering nothing would hang the turn — so it self-answers.
func TestAskUnparsableStillAnswers(t *testing.T) {
	p, reply := pendingFor(`{"questions":[]}`)
	m := &Model{phase: PhasePlan, now: time.Now()}
	m.openAsk(p)

	if m.ask != nil {
		t.Fatal("an unparsable question opened a panel with no options to pick")
	}
	if got := answer(t, reply); got == "" {
		t.Error("an unparsable question was left unanswered; claude's turn would hang")
	}
}

// The tool_use event renders the question but must NOT open the panel: only the
// socket request carries the handle that can answer it, and claude's turn is
// blocked on that answer. Opening from both would race and leave a second panel
// with no way to reply.
func TestAskToolUseDoesNotOpenPanel(t *testing.T) {
	m := &Model{}
	m.ingestToolUse(driver.ContentBlock{
		Type:  driver.BlockToolUse,
		ID:    "tu_mcp",
		Name:  mcp.Qualified(mcp.ToolAsk),
		Input: json.RawMessage(`{"questions":[{"header":"H","question":"Q?","options":[{"label":"yes"}]}]}`),
	})

	if m.ask != nil {
		t.Fatal("the tool_use event opened a panel; it would race the socket request that can actually answer")
	}
}

// In AUTO-RUN the human has walked away by assumption. A question that blocks
// forever would strand the run — the exact failure acy exists to prevent — so it
// auto-skips on the same countdown as a gated tool.
func TestAskAutoSkipsInAutoRun(t *testing.T) {
	p, reply := pendingFor(`{"questions":[{"header":"H","options":[{"label":"yes"}]}]}`)
	start := time.Now()
	m := &Model{phase: PhaseAutoRun, countdown: 30 * time.Second, now: start}
	m.openAsk(p)

	if m.ask.deadline.IsZero() {
		t.Fatal("a question raised in AUTO-RUN got no deadline; nobody is there to answer it")
	}

	// Not yet due: it must still be waiting.
	m.now = start.Add(29 * time.Second)
	m.expireAsk()
	if m.ask == nil {
		t.Fatal("the question expired before its countdown ran out")
	}

	m.now = start.Add(31 * time.Second)
	m.expireAsk()
	if m.ask != nil {
		t.Error("the question did not auto-skip after its countdown")
	}
	if got := answer(t, reply); !strings.Contains(got, "skipped") {
		t.Errorf("auto-skip answer = %q, want it to tell claude to use its best judgment", got)
	}
}

// In PLAN a human is sitting right there, so a question waits as long as it takes.
func TestAskHasNoDeadlineInPlan(t *testing.T) {
	p, _ := pendingFor(`{"questions":[{"header":"H","options":[{"label":"yes"}]}]}`)
	start := time.Now()
	m := &Model{phase: PhasePlan, countdown: 30 * time.Second, now: start}
	m.openAsk(p)

	if !m.ask.deadline.IsZero() {
		t.Fatal("a question raised in PLAN got a deadline; it would time out under a human who is right there")
	}
	m.now = start.Add(time.Hour)
	m.expireAsk()
	if m.ask == nil {
		t.Error("a PLAN question expired; it should wait for the human indefinitely")
	}
}

// If the question's session is already gone there is nothing left to unblock. Say
// so, rather than parking on "working…" forever with no explanation.
func TestAskSurfacesDeadSession(t *testing.T) {
	m, _ := askModel(t, PhasePlan)

	// Stand in for the mcp child dying: the bridge abandons the request, and the
	// answer has nowhere left to go.
	p, _ := pendingFor(`{"questions":[{"header":"H","options":[{"label":"y"}]}]}`)
	m.ask.pending = p
	p.Abandon()

	m.handleAskKey(tea.KeyPressMsg{Code: tea.KeyEscape})

	if m.ask != nil {
		t.Error("expected the panel to close even when the session was gone")
	}
	if m.processing {
		t.Error("processing = true after answering a dead session; the turn is not actually running")
	}
	var warned bool
	for _, e := range m.entries {
		if e.kind == eWarn && strings.Contains(e.body, "could not deliver") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning after answering a dead session; entries = %+v", m.entries)
	}
}

// A question left open when the session ends must be cleared AND answered. The
// panel captures every keystroke, so an orphan locks the UI; and the mcp child, if
// somehow still alive, would hold claude blocked forever.
func TestAskClearedWhenStreamCloses(t *testing.T) {
	m := sizedModel(t)
	p, reply := pendingFor(`{"questions":[{"header":"H","options":[{"label":"x"}]}]}`)
	m.now = time.Now()
	m.openAsk(p)

	next, _ := m.Update(streamClosedMsg{gen: m.gen})
	if next.(Model).ask != nil {
		t.Error("the ask panel survived the session ending; the UI would be stuck")
	}
	if got := answer(t, reply); got == "" {
		t.Error("the orphaned question was never answered")
	}
}
