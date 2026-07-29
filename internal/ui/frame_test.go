package ui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/gate"
	"github.com/hweeks/always-click-yes/internal/orchestrator"
	"github.com/hweeks/always-click-yes/internal/session"
	"github.com/hweeks/always-click-yes/internal/state"
)

// frameTime is the fixed clock these tests build against, so a deadline can be
// asserted as an exact number rather than a range.
var frameTime = time.Unix(1_700_000_000, 0).UTC()

// The header's three headline facts, projected. Status is passed through
// verbatim — it is prose the model already chose — and the hint comes from the
// same function the composer renders, so the two front ends can never disagree
// about what the user is being told.
func TestFramePhaseStatusAndHint(t *testing.T) {
	cases := []struct {
		name       string
		set        func(t *testing.T, m *Model)
		wantPhase  string
		wantStatus string
		wantHint   HintKind
	}{
		{"plan idle", func(*testing.T, *Model) {}, "PLAN", "planning", HintPlan},
		{"plan with a plan ready", func(_ *testing.T, m *Model) {
			m.planReady = true
			m.status = "idle"
		}, "PLAN", "idle", HintPlanReady},
		{"auto-run working", func(_ *testing.T, m *Model) {
			m.phase = PhaseAutoRun
			m.processing = true
			m.status = "working…"
		}, "AUTO-RUN", "working…", HintWorking},
		{"auto-run gated", func(t *testing.T, m *Model) {
			m.phase = PhaseAutoRun
			m.processing = true
			p, _ := bashPending("echo hi")
			m.enqueue(p)
		}, "AUTO-RUN", "planning", HintGate},
		{"auto-run waiting on a task", func(_ *testing.T, m *Model) {
			m.phase = PhaseAutoRun
			m.status = "waiting on a task"
			m.dispatcher = &busyDispatcher{fakeDispatcher: newFakeDispatcher(nil)}
		}, "AUTO-RUN", "waiting on a task", HintBusy},
		{"complete", func(_ *testing.T, m *Model) {
			m.phase = PhaseComplete
			m.status = "complete — vet the work below"
		}, "COMPLETE", "complete — vet the work below", HintComplete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(nil, Config{})
			m.now = frameTime
			tc.set(t, &m)

			f := m.Frame()
			if f.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", f.Phase, tc.wantPhase)
			}
			if f.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", f.Status, tc.wantStatus)
			}
			if f.Hint.Kind != tc.wantHint {
				t.Errorf("hint kind = %q, want %q", f.Hint.Kind, tc.wantHint)
			}
			// The kind is for styling; the text is the thing the user reads, and
			// it must be the same string the composer renders.
			if want := stripAnsi(m.inputView()); !strings.Contains(want, f.Hint.Text) {
				t.Errorf("hint text %q is not what the composer shows:\n%s", f.Hint.Text, want)
			}
		})
	}
}

// A seq is an identity. It is handed out once and never again — most sharply
// across /clear, which empties the transcript but must not rewind the counter,
// or a client would be handed two different entries wearing the same id.
func TestFrameEntrySeqIsMonotonicAcrossClear(t *testing.T) {
	m := New(nil, Config{})
	m.appendEntry(entry{kind: eYou, body: "first"})
	m.appendEntry(entry{kind: eClaude, body: "second"})

	before := m.Frame().Entries
	if len(before) < 3 {
		t.Fatalf("setup: %d entries, want the greeting plus two", len(before))
	}
	high := before[len(before)-1].Seq

	m.runCommand("clear", "")
	m.appendEntry(entry{kind: eYou, body: "after the clear"})

	after := m.Frame().Entries
	if len(after) != 2 {
		t.Fatalf("after /clear: %d entries, want the notice plus the new message", len(after))
	}
	for _, e := range after {
		if e.Seq <= high {
			t.Errorf("seq %d was reused after /clear (highest before was %d)", e.Seq, high)
		}
	}

	// And strictly increasing within a frame, so a client can diff on it.
	var prev int
	for _, e := range append(before, after...) {
		if e.Seq <= prev {
			t.Errorf("seq went %d -> %d; ids must only ever climb", prev, e.Seq)
		}
		prev = e.Seq
	}
}

// The TUI bakes syntax highlighting into a tool body at ingest, on purpose — but
// escape codes are a terminal's answer, not a webview's. So the projection ships
// the body stripped, and the unhighlighted source beside it with its language.
func TestFrameEntryStripsAnsiAndCarriesRaw(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		input    string
		wantRaw  string
		wantLang string
	}{
		{"bash", "Bash", `{"command":"go test ./..."}`, "go test ./...", "bash"},
		{"edit", "Edit", `{"file_path":"a.txt","old_string":"foo","new_string":"bar"}`,
			"a.txt\n- foo\n+ bar", "diff"},
		{"write", "Write", `{"file_path":"/tmp/x.go","content":"package main"}`,
			"/tmp/x.go\npackage main", "go"},
		{"read", "Read", `{"file_path":"big.log","offset":100}`, "big.log  (offset 100)", ""},
		{"anything else", "Grep", `{"pattern":"TODO"}`, "pattern: TODO", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(nil, Config{})
			m.ingestToolUse(driver.ContentBlock{
				Type: driver.BlockToolUse, Name: tc.tool, Input: json.RawMessage(tc.input),
			})

			e := m.Frame().Entries[len(m.Frame().Entries)-1]
			if e.Kind != "tool" || e.Title != tc.tool {
				t.Fatalf("entry = %+v, want the %s tool call", e, tc.tool)
			}
			if strings.Contains(e.Body, "\x1b[") {
				t.Errorf("body still carries ANSI: %q", e.Body)
			}
			if e.Raw != tc.wantRaw {
				t.Errorf("raw = %q, want %q", e.Raw, tc.wantRaw)
			}
			if e.Lang != tc.wantLang {
				t.Errorf("lang = %q, want %q", e.Lang, tc.wantLang)
			}
			// Stripping the body must land on the same text as raw; if the two
			// ever diverge, one of them is lying about what the tool did.
			if e.Body != e.Raw {
				t.Errorf("body %q and raw %q disagree", e.Body, e.Raw)
			}
		})
	}
}

// The HTML rendering is opt-in, and the default has to be off: `acy run` never
// reads it, and running goldmark and chroma over every ingested entry to build
// markup no terminal can display would be a cost the terminal front end pays for
// nothing. The HTTP server behind the webview is what turns it on.
func TestFrameHTMLIsOptIn(t *testing.T) {
	// The same transcript, ingested twice — once for each setting — so the only
	// difference between the two frames is the config bit.
	build := func(t *testing.T, renderHTML bool) []Entry {
		t.Helper()
		m := New(nil, Config{RenderHTML: renderHTML})
		m.appendEntry(entry{kind: eClaude, body: "Here is the **plan**."})
		m.appendEntry(entry{kind: eToolOK, body: "ok\t0.4s"})
		m.ingestToolUse(driver.ContentBlock{
			Type: driver.BlockToolUse, Name: "Bash",
			Input: json.RawMessage(`{"command":"go test ./..."}`),
		})
		return m.Frame().Entries
	}

	t.Run("off by default", func(t *testing.T) {
		for _, e := range build(t, false) {
			if e.HTML != "" {
				t.Errorf("entry %d (%s) carries html without being asked: %q", e.Seq, e.Kind, e.HTML)
			}
		}
	})

	t.Run("on when asked", func(t *testing.T) {
		entries := build(t, true)
		for _, e := range entries {
			if e.HTML == "" {
				t.Errorf("entry %d (%s) has no html", e.Seq, e.Kind)
			}
			// The kind travels as a class, which is how a client styles a
			// fragment it was handed rather than switching on the kind itself.
			if want := "acy-entry--" + e.Kind; !strings.Contains(e.HTML, want) {
				t.Errorf("entry %d html does not carry %s: %s", e.Seq, want, e.HTML)
			}
		}

		last := entries[len(entries)-1]
		if last.Kind != "tool" || last.Lang != "bash" {
			t.Fatalf("setup: last entry is %+v, want the Bash tool call", last)
		}
		// The tool body is highlighted from raw, with chroma's class-based
		// markup — no escape codes, and no inline styles for the webview's CSP
		// to drop.
		if !strings.Contains(last.HTML, `<pre class="chroma">`) {
			t.Errorf("the tool call was not highlighted: %s", last.HTML)
		}
		if strings.Contains(last.HTML, "\x1b[") || strings.Contains(last.HTML, "style=") {
			t.Errorf("the html carries terminal escapes or inline styles: %q", last.HTML)
		}

		// Claude's prose is markdown, which is the reason this exists: the
		// client renders none itself.
		var prose Entry
		for _, e := range entries {
			if e.Kind == "claude" {
				prose = e
			}
		}
		if !strings.Contains(prose.HTML, "<strong>plan</strong>") {
			t.Errorf("claude prose was not rendered as markdown: %s", prose.HTML)
		}
	})
}

// An entry a child produced has to say so. Without the tag a delegated task's
// tool calls read as though the supervisor made them, which is exactly the
// confusion delegating was meant to remove.
func TestFrameTagsAChildEntryWithItsTask(t *testing.T) {
	m := New(nil, Config{})
	blocks := []driver.ContentBlock{{
		Type: driver.BlockToolUse, Name: "Bash", Input: json.RawMessage(`{"command":"go build ./..."}`),
	}}
	m.ingestChild(orchestrator.Event{
		Kind:   orchestrator.KindStream,
		TaskID: "t7",
		Ev: driver.Event{
			Type:    driver.TypeAssistant,
			Message: &driver.Message{Role: "assistant", Content: json.RawMessage(mustJSON(blocks))},
		},
	})

	entries := m.Frame().Entries
	last := entries[len(entries)-1]
	if last.Task != "t7" {
		t.Errorf("task = %q, want t7", last.Task)
	}
	if last.Raw != "go build ./..." {
		t.Errorf("raw = %q, want the command", last.Raw)
	}
	// The parent's own entries stay untagged, or the tag says nothing.
	if entries[0].Task != "" {
		t.Errorf("a parent entry was tagged %q", entries[0].Task)
	}
}

// Every ekind must have a wire name. A new one that forgot to add itself here
// would project as "" and a client would silently render it as nothing.
func TestFrameEntryKindsAreAllNamed(t *testing.T) {
	m := New(nil, Config{})
	for k := eMeta; k <= eQueued; k++ {
		name, ok := entryKinds[k]
		if !ok || name == "" {
			t.Errorf("ekind %d has no wire name", k)
		}
	}
	m.appendEntry(entry{kind: eQueued, body: "held"})
	if got := m.Frame().Entries[len(m.entries)-1].Kind; got != "queued" {
		t.Errorf("queued entry projected as %q", got)
	}
}

// Gates are keyed by tool-use id and carry an absolute deadline. Both are
// load-bearing: an action that answered "gate 0" would answer the wrong tool
// after an auto-approve, and a countdown expressed as "23s left" would make
// every 120ms tick look like a state change.
func TestFrameGates(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.now = frameTime

	in := gate.PreToolUseInput{ToolName: "Bash", ToolUseID: "toolu_abc", SessionID: "child-sess"}
	in.ToolInput = json.RawMessage(`{"command":"rm -rf /tmp/x\nsecond line"}`)
	p, _ := gate.NewPending(in)
	m.dispatcher = newFakeDispatcher(map[string]string{"child-sess": "t3"})
	m.enqueue(p)

	gates := m.Frame().Gates
	if len(gates) != 1 {
		t.Fatalf("gates = %d, want 1", len(gates))
	}
	g := gates[0]
	if g.ToolUseID != "toolu_abc" {
		t.Errorf("toolUseId = %q, want the id from the hook", g.ToolUseID)
	}
	if g.Tool != "Bash" || g.Task != "t3" {
		t.Errorf("tool/task = %q/%q, want Bash/t3", g.Tool, g.Task)
	}
	if strings.Contains(g.Args, "\n") {
		t.Errorf("args must be a one-line preview, got %q", g.Args)
	}
	if want := frameTime.Add(30 * time.Second).UnixMilli(); g.DeadlineUnixMs != want {
		t.Errorf("deadlineUnixMs = %d, want %d", g.DeadlineUnixMs, want)
	}
	if g.RemainingMs != 0 {
		t.Errorf("remainingMs = %d, want 0 while the countdown is live", g.RemainingMs)
	}

	// Paused: the deadline is gone (there isn't one any more) and the frozen
	// remainder takes over, so a client shows a stopped clock rather than
	// animating towards a deadline that will never arrive.
	m.now = frameTime.Add(10 * time.Second)
	m.togglePause()
	g = m.Frame().Gates[0]
	if !m.Frame().Paused {
		t.Error("frame does not report the pause")
	}
	if g.DeadlineUnixMs != 0 {
		t.Errorf("deadlineUnixMs = %d, want 0 while paused", g.DeadlineUnixMs)
	}
	if g.RemainingMs != (20 * time.Second).Milliseconds() {
		t.Errorf("remainingMs = %d, want 20000", g.RemainingMs)
	}
}

// A frame must be byte-stable between real state changes: a later milestone
// detects change by comparing frames, and anything clock-shaped in here would
// make every tick look like news.
func TestFrameIsStableAcrossTicks(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.now = frameTime
	p, _ := bashPending("echo hi")
	m.enqueue(p)
	m.processing = true
	m.turnStart = frameTime

	first := mustMarshal(t, m.Frame())
	for i := 1; i <= 5; i++ {
		m.now = frameTime.Add(time.Duration(i) * tickInterval)
		m.spinFrame++
		if got := mustMarshal(t, m.Frame()); got != first {
			t.Fatalf("tick %d changed the frame:\n got %s\nwant %s", i, got, first)
		}
	}
}

func TestFrameQueue(t *testing.T) {
	m := New(nil, Config{})
	if q := m.Frame().Queue; len(q) != 0 {
		t.Errorf("queue = %v, want empty", q)
	}
	m.queued = []string{"alpha", "beta"}
	if got := m.Frame().Queue; len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("queue = %v, want [alpha beta]", got)
	}
	// A copy, not the model's own slice: a projection a caller can append to is
	// a projection that can corrupt the run it describes.
	f := m.Frame()
	f.Queue[0] = "mutated"
	if m.queued[0] != "alpha" {
		t.Error("Frame handed out the model's own queue slice")
	}
}

// Only the question being asked travels — the earlier ones are answered and the
// later ones are not being asked yet — but its position does, so a client can
// say "2 of 3".
func TestFrameAsk(t *testing.T) {
	m := New(nil, Config{})
	m.now = frameTime
	if m.Frame().Ask != nil {
		t.Fatal("ask should be null with no question open")
	}

	m.ask = &askState{
		qIdx: 1,
		questions: []askQuestion{
			{header: "First", question: "already answered", options: []askOption{{label: "x"}}, selected: map[int]bool{}},
			{
				header: "Storage", question: "Where should the cache live?",
				multiSelect: true, cursor: 1,
				options: []askOption{
					{label: "in memory", description: "fastest, lost on restart"},
					{label: "on disk", description: "survives a crash"},
				},
				selected: map[int]bool{1: true},
			},
		},
		deadline: frameTime.Add(18 * time.Second),
	}

	a := m.Frame().Ask
	if a == nil {
		t.Fatal("ask is null with a question open")
	}
	if a.Header != "Storage" || a.Question != "Where should the cache live?" {
		t.Errorf("ask = %+v, want the current question", a)
	}
	if a.Index != 1 || a.Total != 2 {
		t.Errorf("index/total = %d/%d, want 1/2", a.Index, a.Total)
	}
	if !a.MultiSelect || a.Cursor != 1 {
		t.Errorf("multiSelect/cursor = %v/%d, want true/1", a.MultiSelect, a.Cursor)
	}
	if len(a.Options) != 2 || a.Options[0].Selected || !a.Options[1].Selected {
		t.Errorf("options = %+v, want only the second selected", a.Options)
	}
	if a.Options[0].Description != "fastest, lost on restart" {
		t.Errorf("option description = %q", a.Options[0].Description)
	}
	if want := frameTime.Add(18 * time.Second).UnixMilli(); a.DeadlineUnixMs != want {
		t.Errorf("deadlineUnixMs = %d, want %d", a.DeadlineUnixMs, want)
	}

	// PLAN has no deadline, and 0 has to mean "never" rather than the epoch.
	m.ask.deadline = time.Time{}
	if got := m.Frame().Ask.DeadlineUnixMs; got != 0 {
		t.Errorf("deadlineUnixMs = %d, want 0 for a question with no clock", got)
	}
}

// A task with no end time is running, not finished badly. Its blank outcome and
// zero cost are "not in yet", and the frame has to carry the difference.
func TestFrameTasks(t *testing.T) {
	m := New(nil, Config{})
	m.dispatches = 141 // more than the ledger holds: it is trimmed, the count is not
	m.tasks = []state.Task{
		finishedTask("t1", "rewrite the gate", "completed", 0.25, 5000),
		{ID: "t2", Title: "port the parser", StartedAt: frameTime},
	}

	f := m.Frame()
	if f.Dispatches != 141 {
		t.Errorf("dispatches = %d, want the full count even with a trimmed ledger", f.Dispatches)
	}
	if len(f.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(f.Tasks))
	}
	done, running := f.Tasks[0], f.Tasks[1]
	if done.Running {
		t.Error("a finished task is reported as running")
	}
	if done.Outcome != "completed" || done.Cost != 0.25 || done.Tokens.CacheRead != 5000 {
		t.Errorf("finished task = %+v", done)
	}
	if !running.Running {
		t.Error("a task with no end time must report as running")
	}
	if running.Outcome != "" {
		t.Errorf("running task outcome = %q, want empty — the frame says running, not '—'", running.Outcome)
	}
}

// The split by spender is the whole thesis of the orchestrator: a run that
// delegates shows the parent flat while the children climb. Totals are derived
// here so both front ends add up the same way.
func TestFrameCostAndTokenSplit(t *testing.T) {
	m := New(nil, Config{})
	m.costSettled = 0.5
	m.costCurrent = 0.25
	m.childCost = 2.0
	m.parentTokens = state.Tokens{Input: 10, Output: 20, CacheCreate: 30, CacheRead: 40}
	m.childTokens = state.Tokens{Input: 1, Output: 2, CacheCreate: 3, CacheRead: 4}
	m.lastContext = 38_000
	m.contextWindow = 200_000
	m.apiKeySource = "none"

	f := m.Frame()
	if f.Cost.Parent != 0.75 || f.Cost.Child != 2.0 || f.Cost.Total != 2.75 {
		t.Errorf("cost = %+v, want parent 0.75 child 2 total 2.75", f.Cost)
	}
	if f.Tokens.Parent.CacheRead != 40 || f.Tokens.Child.CacheRead != 4 || f.Tokens.Total.CacheRead != 44 {
		t.Errorf("cache reads = %+v, want 40/4/44", f.Tokens)
	}
	if f.Tokens.Total.Input != 11 || f.Tokens.Total.Output != 22 || f.Tokens.Total.CacheCreate != 33 {
		t.Errorf("token totals = %+v", f.Tokens.Total)
	}
	if f.Tokens.Context != 38_000 || f.Tokens.ContextWindow != 200_000 {
		t.Errorf("context = %d/%d, want 38000/200000", f.Tokens.Context, f.Tokens.ContextWindow)
	}
	if f.Billing != "subscription" {
		t.Errorf("billing = %q, want subscription", f.Billing)
	}
}

// The picker rows say which sessions acy supervised (they have a label) and
// which row the cursor is on, so a client renders the same list without keeping
// its own cursor.
func TestFramePicker(t *testing.T) {
	m := New(nil, Config{})
	m.picking = true
	m.sessionList = pickRows([]session.Info{
		{ID: "aaaabbbbcccc", ModTime: frameTime, Summary: "port the parser"},
		{ID: "ddddeeeeffff", ModTime: frameTime, Summary: "a plain claude chat"},
	}, snapLoader(map[string]state.Snapshot{
		"aaaabbbbcccc": {Phase: "AUTO-RUN", Dispatches: 3, CostSettled: 2.5},
	}))
	m.pickIdx = 1

	f := m.Frame()
	if !f.Picking {
		t.Error("frame does not report the open picker")
	}
	if len(f.Picker) != 2 {
		t.Fatalf("picker rows = %d, want 2", len(f.Picker))
	}
	if f.Picker[0].Label != "AUTO-RUN · 3 tasks · $2.50" {
		t.Errorf("label = %q, want the snapshot summary", f.Picker[0].Label)
	}
	if f.Picker[1].Label != "" {
		t.Errorf("a session acy never supervised must have no label, got %q", f.Picker[1].Label)
	}
	if f.Picker[0].Selected || !f.Picker[1].Selected {
		t.Error("selected row should follow pickIdx")
	}
	if f.Picker[0].ModTimeUnixMs != frameTime.UnixMilli() {
		t.Errorf("modTimeUnixMs = %d", f.Picker[0].ModTimeUnixMs)
	}
}

// The frame is the wire format, so it has to survive a round trip through
// encoding/json — no channels, no funcs, no pointers into the model.
func TestFrameMarshalsCleanly(t *testing.T) {
	m := New(nil, Config{LogPath: "/tmp/acy.log", ConfigPath: "/p/.acy.json", Cwd: "/p"})
	m.now = frameTime
	m.sessionID = "0123456789ab"
	m.interruptedTasks = []string{"t4"}
	p, _ := bashPending("echo hi")
	m.enqueue(p)

	blob := mustMarshal(t, m.Frame())

	var back Frame
	if err := json.Unmarshal([]byte(blob), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.LogPath != "/tmp/acy.log" || back.ConfigPath != "/p/.acy.json" || back.Cwd != "/p" {
		t.Errorf("paths did not survive the round trip: %+v", back)
	}
	if len(back.InterruptedTasks) != 1 || back.InterruptedTasks[0] != "t4" {
		t.Errorf("interruptedTasks = %v", back.InterruptedTasks)
	}
	if len(back.Gates) != 1 || back.Gates[0].Tool != "Bash" {
		t.Errorf("gates did not survive the round trip: %+v", back.Gates)
	}
	// No "now" anywhere. The absolute deadline is the only time the client gets,
	// and it is what keeps the frame identical between ticks.
	for _, banned := range []string{`"now"`, `"nowUnixMs"`, `"remaining"`} {
		if strings.Contains(blob, banned) {
			t.Errorf("frame carries %s — a client animates from its own clock", banned)
		}
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
