package ui

import (
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// benchModel builds a ready Model with ~500 mixed entries: prose, tool calls
// with real bodies, thinking blocks and notices — a stand-in for a long-running
// auto-run's transcript.
func benchModel(tb testing.TB) Model {
	tb.Helper()
	m := New(nil, Config{})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	for i := range 500 {
		switch i % 5 {
		case 0:
			m.appendEntry(entry{kind: eClaude, body: strings.Repeat(
				"Here is some prose about the current step of the task. ", 12)})
		case 1:
			m.appendEntry(entry{kind: eTool, title: "Bash", body: toolBody("Bash",
				[]byte(`{"command":"go build ./... && go test ./... -run TestSomething -v"}`))})
		case 2:
			m.appendEntry(entry{kind: eToolOK, body: strings.TrimRight(
				strings.Repeat("output line "+strconv.Itoa(i)+"\n", 15), "\n")})
		case 3:
			m.appendEntry(entry{kind: eThinking, body: strings.Repeat(
				"Thinking through the plan and what to do next. ", 10)})
		case 4:
			m.appendEntry(entry{kind: eMeta, body: "notice #" + strconv.Itoa(i)})
		}
	}
	return m
}

// BenchmarkRebuild times m.rebuild() called repeatedly with nothing changed in
// between — exactly what the 120ms tick does while a gate is pending or an ask
// is open.
func BenchmarkRebuild(b *testing.B) {
	m := benchModel(b)
	m.rebuild() // reach steady state
	b.ResetTimer()
	for range b.N {
		m.rebuild()
	}
}

// TestRebuildNoopSkipsSetContent proves the load-bearing half of the cache: a
// second rebuild() with nothing changed must not re-render or call SetContent,
// which is what keeps the 120ms tick free while a gate is pending or an ask is
// open.
func TestRebuildNoopSkipsSetContent(t *testing.T) {
	m := sizedModel(t)
	m.appendEntry(entry{kind: eClaude, body: "hello"})
	m.rebuild()

	calls := m.rc.setContentCalls
	renders := len(m.rc.renders)
	if calls == 0 {
		t.Fatal("setup: expected the first rebuild to call SetContent")
	}

	m.rebuild() // nothing changed

	if m.rc.setContentCalls != calls {
		t.Errorf("setContentCalls = %d, want unchanged %d (a no-op rebuild re-rendered)",
			m.rc.setContentCalls, calls)
	}
	if len(m.rc.renders) != renders {
		t.Errorf("len(renders) = %d, want unchanged %d", len(m.rc.renders), renders)
	}
}

// TestRebuildWidthChangeInvalidates proves a resize busts the cache: it must
// re-render (SetContent called again) and the new content must actually reflect
// the new width.
func TestRebuildWidthChangeInvalidates(t *testing.T) {
	m := sizedModel(t)
	m.appendEntry(entry{kind: eClaude, body: strings.Repeat("word ", 40)})
	m.rebuild()

	before := m.View().Content
	calls := m.rc.setContentCalls

	tm, _ := m.Update(tea.WindowSizeMsg{Width: 50, Height: 30})
	m = tm.(Model)

	if m.rc.setContentCalls <= calls {
		t.Errorf("setContentCalls = %d, want > %d after a width change", m.rc.setContentCalls, calls)
	}
	after := m.View().Content
	if before == after {
		t.Error("expected the rendered content to change after a width change")
	}
}

func TestRebuildPreservesScrollbackOnNewOutput(t *testing.T) {
	m := sizedModel(t)
	for i := range 40 {
		m.appendEntry(entry{kind: eClaude, body: "history line " + strconv.Itoa(i)})
	}
	m.rebuild()
	m.vp.GotoTop()
	m.vp.ScrollDown(5)
	before := m.vp.YOffset()
	if before == 0 || m.vp.AtBottom() {
		t.Fatalf("setup: offset=%d atBottom=%v, want scrolled into history", before, m.vp.AtBottom())
	}

	m.appendEntry(entry{kind: eClaude, body: "new output"})
	m.rebuild()

	if got := m.vp.YOffset(); got != before {
		t.Errorf("offset = %d, want preserved %d after new output", got, before)
	}
}

func TestRebuildFollowsNewOutputWhenAlreadyAtBottom(t *testing.T) {
	m := sizedModel(t)
	for i := range 40 {
		m.appendEntry(entry{kind: eClaude, body: "history line " + strconv.Itoa(i)})
	}
	m.rebuild()
	if !m.vp.AtBottom() {
		t.Fatal("setup: rebuild should initially follow the bottom")
	}

	m.appendEntry(entry{kind: eClaude, body: strings.Repeat("new output ", 20)})
	m.rebuild()

	if !m.vp.AtBottom() {
		t.Errorf("new output stopped following while viewport was pinned; offset=%d", m.vp.YOffset())
	}
}

func TestNoopRebuildDoesNotMoveScrollback(t *testing.T) {
	m := sizedModel(t)
	for i := range 40 {
		m.appendEntry(entry{kind: eClaude, body: "history line " + strconv.Itoa(i)})
	}
	m.rebuild()
	m.vp.GotoTop()
	m.vp.ScrollDown(4)
	before := m.vp.YOffset()

	m.rebuild()

	if got := m.vp.YOffset(); got != before {
		t.Errorf("offset = %d, want %d after no-op rebuild", got, before)
	}
}
