package ui

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/session"
	"github.com/hweeks/always-click-yes/internal/state"
)

// Regression net for `acy run`'s appearance.
//
// A second front end is being grown off this model (a projection in frame.go, a
// shared presentation layer in present.go), and the one thing that must not
// change while that happens is what the terminal draws. So these tests pin the
// whole frame — every scene the TUI has — byte for byte.
//
// Full-frame goldens rather than substring assertions, because a substring
// assertion only catches the drift it was written to imagine. View() proved
// reproducible for this: lipgloss v2 renders ANSI into the *string* and leaves
// the color-profile downgrade to the writer, glamour is pinned to an explicit
// dark style rather than probing the terminal, and chroma's terminal256
// formatter is a pure function of its input. Measured byte-identical on
// darwin/arm64 and on linux/arm64 in a container, with TERM unset, TERM=dumb,
// NO_COLOR=1, CLICOLOR_FORCE=1 and TERM=xterm-256color COLORTERM=truecolor.
//
// # There is no color profile to pin, and that is the finding
//
// The obvious hardening — "pin the profile so CI's linux/amd64 can't disagree"
// — has nothing to pin. In lipgloss v2 the profile lives on a *writer*
// (lipgloss.Writer, built from os.Environ()) and is consulted only by
// Print/Fprint/Sprint, none of which acy calls: Style.Render bakes the full
// color into the string it returns, and View() is nothing but Render. glamour
// is constructed here with explicit styles, so it never runs its
// terminal-probing auto-style path, and its renderer defaults to
// termenv.TrueColor with no environment read at all. chroma's terminal256
// formatter takes its palette from an argument. So there is no supported knob,
// because there is no environment-dependent decision left to make — setting
// lipgloss.Writer.Profile in a TestMain would pin a value nothing in this path
// reads, which is worse than leaving it alone: it would read as a guarantee.
//
// Two things were checked rather than assumed. GOARCH=amd64 (the architecture
// CI actually runs, exercised here under Rosetta) reproduces these goldens
// byte for byte — so the float math inside the progress bar's gradient, the
// one place an arm64/amd64 FMA difference could have shown up, does not move
// them. And the *one* environment variable that does move them is
// RUNEWIDTH_EASTASIAN: charmbracelet/x/ansi latches it in package init, which
// runs before any TestMain could unset it, and it widens the box-drawing and
// emoji runes every panel here is built from. That is what requireStableWidths
// below refuses to run under — a legible skip rather than a screenful of
// escape codes blamed on a code change.
//
// If a change to the UI is intended, regenerate with:
//
//	ACY_UPDATE_GOLDEN=1 go test ./internal/ui/ -run TestTUIFrames
//
// and read the diff. Regenerating without reading it is the only way this file
// can lie to you.

// requireStableWidths skips a golden test when the environment has already
// changed how wide a rune is.
//
// RUNEWIDTH_EASTASIAN is read in charmbracelet/x/ansi's init(), so by the time
// any Go test code runs the decision is made and cannot be taken back. The
// goldens were recorded with it off, which is the default and what CI has; with
// it on every ⏳, ✔ and ─ measures differently and every frame here fails for a
// reason that has nothing to do with the code under test.
func requireStableWidths(t *testing.T) {
	t.Helper()
	if ea, err := strconv.ParseBool(os.Getenv("RUNEWIDTH_EASTASIAN")); err == nil && ea {
		t.Skip("RUNEWIDTH_EASTASIAN is set: rune widths differ from the recorded goldens, " +
			"and x/ansi latches it in init() before a test can unset it")
	}
}

// pinTime is the fixed clock every scene is built against. Countdowns and the
// elapsed-time readout are derived from it, so nothing here reads the wall clock.
var pinTime = time.Unix(1_700_000_000, 0).UTC()

// pinScene is one screen `acy run` can be in.
type pinScene struct {
	name  string
	build func(t *testing.T) Model
}

// pinBase is a ready model at a fixed size with the wall clock replaced. Every
// scene starts here so a golden can never move because a test ran at a different
// second.
func pinBase(t *testing.T) Model {
	t.Helper()
	m := sizedModel(t)
	m.now = pinTime
	m.sessionID = "0123456789abcdef"
	m.model = "claude-opus-5"
	m.apiKeySource = "none"
	m.costCurrent = 0.4213
	m.parentTokens = state.Tokens{Input: 120, Output: 3400, CacheCreate: 51_000, CacheRead: 812_000}
	m.lastContext = 38_000
	m.contextWindow = 200_000
	return m
}

// pinWorking marks a turn in flight at a fixed elapsed time and animation frame.
func pinWorking(m *Model) {
	m.processing = true
	m.status = "working…"
	m.turnStart = pinTime.Add(-42 * time.Second)
	m.spinFrame = 3
}

// pinGate raises one Bash gate against the fixed clock.
func pinGate(t *testing.T, m *Model) {
	t.Helper()
	p, _ := bashPending("go test ./...")
	m.enqueue(p)
	if len(m.pending) != 1 {
		t.Fatalf("setup: want 1 pending gate, got %d", len(m.pending))
	}
}

func pinScenes() []pinScene {
	return []pinScene{
		{"plan-idle", func(t *testing.T) Model {
			m := pinBase(t)
			m.rebuild()
			return m
		}},

		{"plan-ready", func(t *testing.T) Model {
			m := pinBase(t)
			m.planReady = true
			m.planBody = "1. move the parser\n2. add a test"
			m.status = "idle"
			m.appendEntry(entry{kind: eClaude, body: "Here is what I propose."})
			m.appendEntry(entry{kind: ePlan, body: "1. move the parser\n2. add a test"})
			m.rebuild()
			return m
		}},

		{"autorun-working", func(t *testing.T) Model {
			m := pinBase(t)
			m.phase = PhaseAutoRun
			pinWorking(&m)
			m.dispatches = 2
			m.childCost = 1.25
			m.childTokens = state.Tokens{Input: 40, Output: 9000, CacheRead: 2_100_000}
			m.appendEntry(entry{kind: eGood, body: "▶ armed — delegating from here; Esc stops a running task"})
			m.appendEntry(entry{kind: eTool, task: "t2", title: "Bash",
				body: toolBody("Bash", []byte(`{"command":"go build ./..."}`))})
			m.rebuild()
			return m
		}},

		{"autorun-gate-pending", func(t *testing.T) Model {
			m := pinBase(t)
			m.phase = PhaseAutoRun
			pinWorking(&m)
			pinGate(t, &m)
			m.rebuild()
			return m
		}},

		{"autorun-gate-paused", func(t *testing.T) Model {
			m := pinBase(t)
			m.phase = PhaseAutoRun
			pinWorking(&m)
			pinGate(t, &m)
			// Ten seconds in, so the frozen remainder is a number and not the
			// full countdown — a paused gate that still reads 30s would hide a
			// bug in togglePause.
			m.now = pinTime.Add(10 * time.Second)
			m.togglePause()
			m.rebuild()
			return m
		}},

		{"queue-nonempty", func(t *testing.T) Model {
			m := pinBase(t)
			m.phase = PhaseAutoRun
			pinWorking(&m)
			// Set directly rather than through sendInput: that path stamps
			// turnStart from the wall clock.
			for i, q := range []string{"alpha", "beta", "gamma", "delta", "epsilon"} {
				m.queued = append(m.queued, queuedMsg{id: i + 1, text: q})
				m.appendEntry(entry{kind: eQueued, body: q})
			}
			m.rebuild()
			return m
		}},

		{"ask-overlay", func(t *testing.T) Model {
			m := pinBase(t)
			m.phase = PhaseAutoRun
			m.ask = &askState{
				questions: []askQuestion{{
					header:      "Storage",
					question:    "Where should the cache live?",
					multiSelect: true,
					options: []askOption{
						{label: "in memory", description: "fastest, lost on restart"},
						{label: "on disk", description: "survives a crash"},
					},
					selected: map[int]bool{1: true},
					cursor:   1,
				}, {
					header:   "Second",
					question: "And the eviction policy?",
					options:  []askOption{{label: "LRU"}, {label: "FIFO"}},
					selected: map[int]bool{},
				}},
				deadline: pinTime.Add(18 * time.Second),
			}
			m.appendEntry(entry{kind: eMeta, body: "❓ Claude is asking a question — answer below"})
			m.rebuild()
			return m
		}},

		{"resume-picker", func(t *testing.T) Model {
			m := pinBase(t)
			m.picking = true
			mod := pinTime.UTC()
			m.sessionList = pickRows([]session.Info{
				{ID: "aaaabbbbcccc", ModTime: mod, Summary: "port the parser"},
				{ID: "ddddeeeeffff", ModTime: mod.Add(-time.Hour), Summary: "a plain claude chat"},
			}, snapLoader(map[string]state.Snapshot{
				"aaaabbbbcccc": {Phase: "AUTO-RUN", Dispatches: 3, CostSettled: 2.5},
			}))
			m.pickIdx = 1
			m.rebuild()
			return m
		}},

		{"help-overlay", func(t *testing.T) Model {
			m := pinBase(t)
			m.showHelp = true
			m.rebuild()
			return m
		}},

		{"complete", func(t *testing.T) Model {
			m := pinBase(t)
			m.phase = PhaseComplete
			m.status = "complete — vet the work below"
			m.dispatches = 3
			m.childCost = 1.25
			m.appendEntry(entry{kind: eComplete,
				body: "✅ run completed · $1.6713 total · subscription (not billed)\n\nall three tasks landed\n\nchat below to vet the work"})
			m.rebuild()
			return m
		}},
	}
}

// isRule reports whether a line is one of the composer's horizontal rules.
func isRule(s string) bool {
	return s != "" && strings.Trim(s, "─") == ""
}

// TestTUIFrames pins every scene's whole frame against a golden file.
func TestTUIFrames(t *testing.T) {
	requireStableWidths(t)
	for _, sc := range pinScenes() {
		t.Run(sc.name, func(t *testing.T) {
			got := sc.build(t).View().Content
			path := filepath.Join("testdata", "frames", sc.name+".txt")

			if os.Getenv("ACY_UPDATE_GOLDEN") != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v — regenerate with ACY_UPDATE_GOLDEN=1", err)
			}
			if got != string(want) {
				t.Errorf("frame changed.\n--- got (ANSI stripped) ---\n%s\n--- want (ANSI stripped) ---\n%s",
					stripAnsi(got), stripAnsi(string(want)))
			}
		})
	}
}

// The goldens above catch *that* something moved; these say *what* the frame is
// made of, so a diff is readable rather than a wall of escape codes. They are
// also the part that survives a deliberate restyle.
func TestTUIStructure(t *testing.T) {
	t.Run("header composition", func(t *testing.T) {
		m := pinBase(t)
		m.phase = PhaseAutoRun
		m.queued = []queuedMsg{{id: 1, text: "one"}, {id: 2, text: "two"}}
		// Wider than the 100-column default: the meta strip is truncated from
		// the tail to fit one row, and at 100 columns the tail is exactly the
		// part this test is about.
		m.width = 160
		head := stripAnsi(m.headerView())
		// Order matters: phase chip, product name, then the meta strip —
		// status, queue, session, cost, tokens, billing.
		want := []string{
			"AUTO-RUN", "always-click-yes",
			"planning", "2 queued", "session 01234567", "$0.4213",
			"ctx 38k/200k", "⇣812k", "subscription",
		}
		var last int
		for _, w := range want {
			i := strings.Index(head, w)
			if i < 0 {
				t.Fatalf("header is missing %q:\n%s", w, head)
			}
			if i < last {
				t.Errorf("header has %q out of order:\n%s", w, head)
			}
			last = i
		}
	})

	t.Run("footer stacking order", func(t *testing.T) {
		m := pinBase(t)
		pinWorking(&m)
		pinGate(t, &m)
		m.queued = []queuedMsg{{id: 1, text: "and add a test"}}

		foot := stripAnsi(m.footerView())
		gate := strings.Index(foot, "auto-approve in")
		queue := strings.Index(foot, "1 queued · sends when this turn ends")
		composer := strings.Index(foot, m.input.Placeholder)
		if gate < 0 || queue < 0 || composer < 0 {
			t.Fatalf("footer is missing a panel (gate=%d queue=%d composer=%d):\n%s", gate, queue, composer, foot)
		}
		if gate >= queue || queue >= composer {
			t.Errorf("footer order is gate→queue→composer; got %d/%d/%d:\n%s", gate, queue, composer, foot)
		}
	})

	t.Run("overlays replace the transcript and take their own footer hint", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			set  func(m *Model)
			want string
		}{
			{"help", func(m *Model) { m.showHelp = true }, "press any key to close"},
			{"picker", func(m *Model) { m.picking = true }, "↑/↓ move · Enter resume · Esc cancel"},
			{"ask", func(m *Model) {
				m.ask = &askState{questions: []askQuestion{{
					question: "which?", options: []askOption{{label: "a"}}, selected: map[int]bool{},
				}}}
			}, "↑/↓ move · Enter confirm · Esc skip"},
			{"ask multi-select", func(m *Model) {
				m.ask = &askState{questions: []askQuestion{{
					question: "which?", multiSelect: true,
					options: []askOption{{label: "a"}}, selected: map[int]bool{},
				}}}
			}, "↑/↓ move · Space toggle · Enter confirm · Esc skip"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				m := pinBase(t)
				tc.set(&m)
				foot := stripAnsi(m.footerView())
				if !strings.Contains(foot, tc.want) {
					t.Errorf("footer hint = %q, want it to contain %q", foot, tc.want)
				}
				if strings.Contains(foot, m.input.Placeholder) {
					t.Errorf("an overlay's footer must not carry the composer:\n%s", foot)
				}
			})
		}
	})

	// The ask countdown only exists in AUTO-RUN, and a countdown nobody can see is
	// how the gate bug happened — so it has to be spelled out.
	t.Run("ask auto-skip suffix", func(t *testing.T) {
		m := pinBase(t)
		m.ask = &askState{
			questions: []askQuestion{{question: "which?", options: []askOption{{label: "a"}}, selected: map[int]bool{}}},
			deadline:  pinTime.Add(18 * time.Second),
		}
		if got := stripAnsi(m.footerView()); !strings.Contains(got, "auto-skip in 18s") {
			t.Errorf("footer should count the question down:\n%s", got)
		}
		m.ask.deadline = time.Time{}
		if got := stripAnsi(m.footerView()); strings.Contains(got, "auto-skip") {
			t.Errorf("a question with no deadline must not claim one:\n%s", got)
		}
	})

	t.Run("hint text selection", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			set  func(t *testing.T, m *Model)
			want string
		}{
			{"gate pending", func(t *testing.T, m *Model) { pinWorking(m); pinGate(t, m) },
				"working… · Enter queues your message · ^Y allow / ^X stop first · Ctrl+C to quit"},
			{"processing", func(_ *testing.T, m *Model) { pinWorking(m) },
				"working… · Esc to interject · Enter queues your message · Ctrl+C to quit"},
			{"busy on a task", func(_ *testing.T, m *Model) {
				m.phase = PhaseAutoRun
				m.dispatcher = &busyDispatcher{fakeDispatcher: newFakeDispatcher(nil)}
			}, "waiting on a task · Enter queues your message · Ctrl+C to quit"},
			{"plan ready", func(_ *testing.T, m *Model) { m.planReady = true },
				"📋 plan ready above · Ctrl+G to arm & run · or keep chatting to refine"},
			{"plan", func(_ *testing.T, m *Model) {},
				"Enter to send · ^J newline · Ctrl+G to arm (start auto-run) · Ctrl+C to quit"},
			{"complete", func(_ *testing.T, m *Model) { m.phase = PhaseComplete },
				"plan complete · Enter to send a follow-up · ^J newline · Ctrl+C to quit"},
			{"auto-run idle", func(_ *testing.T, m *Model) { m.phase = PhaseAutoRun },
				"Enter to send · ^J newline · Ctrl+C to quit"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				m := pinBase(t)
				tc.set(t, &m)
				if got := stripAnsi(m.inputView()); !strings.Contains(got, tc.want) {
					t.Errorf("composer hint should be %q, got:\n%s", tc.want, got)
				}
			})
		}
	})

	t.Run("gate panel contents", func(t *testing.T) {
		m := pinBase(t)
		m.phase = PhaseAutoRun
		p, _ := bashPending("rm -rf /tmp/x")
		m.enqueue(p)
		m.pending[0].task = "t7"
		p2, _ := bashPending("echo second")
		m.enqueue(p2)

		panel := stripAnsi(m.gateView())
		for _, want := range []string{
			// 31, not 30: the panel rounds up so a fresh 30s gate never opens
			// already reading 29.
			"⏳ auto-approve in 31s",
			"[t7]",          // who is asking
			"⚙ Bash",        // what it wants
			"rm -rf /tmp/x", // the one-line argument preview
			"(+1 queued)",   // and how many are behind it
			"^Y allow  ^X stop  ^R pause  ^C quit  ·  or just keep typing",
		} {
			if !strings.Contains(panel, want) {
				t.Errorf("gate panel is missing %q:\n%s", want, panel)
			}
		}

		m.togglePause()
		paused := stripAnsi(m.gateView())
		if !strings.Contains(paused, "⏸") || !strings.Contains(paused, "PAUSED") {
			t.Errorf("a paused gate must say so rather than showing a frozen number:\n%s", paused)
		}
		if strings.Contains(paused, "auto-approve in") {
			t.Errorf("a paused gate still advertised its countdown:\n%s", paused)
		}
	})

	t.Run("composer framing", func(t *testing.T) {
		m := pinBase(t)
		lines := strings.Split(stripAnsi(m.inputView()), "\n")
		// A top and bottom rule and no side borders: the composer is framed
		// with Border(RoundedBorder(), true, false), so the rules run edge to
		// edge and there are no corner or vertical runes at all.
		// rule, composer, rule, hint — the hint is the only line below the box
		// when nothing is attached.
		if len(lines) != 4 || !isRule(lines[0]) || !isRule(lines[2]) {
			t.Errorf("the composer keeps a top and bottom rule:\n%s", strings.Join(lines, "\n"))
		}
		for _, line := range lines {
			if strings.ContainsAny(line, "│╭╮╰╯") {
				t.Errorf("the composer must have no side borders:\n%s", line)
			}
		}
		// The working indicator sits above the box, not below it.
		pinWorking(&m)
		out := stripAnsi(m.inputView())
		if i, j := strings.Index(out, "WORKING"), strings.Index(out, "─"); i < 0 || j < 0 || i > j {
			t.Errorf("the WORKING indicator belongs above the composer:\n%s", out)
		}
	})

	// An attached paste says so directly under the box, because the paths are
	// sitting in the box as editable text and this is the confirmation acy read
	// the drag as files.
	t.Run("attachment note sits under the box", func(t *testing.T) {
		m := pinBase(t)
		m.attached = []string{"/tmp/a.go"}
		lines := strings.Split(stripAnsi(m.inputView()), "\n")
		// box top, composer, box bottom, the note, then the hint.
		if len(lines) != 5 || !isRule(lines[2]) || !strings.Contains(lines[3], "a.go") {
			t.Errorf("the attachment note belongs on its own line under the box:\n%s", strings.Join(lines, "\n"))
		}
	})
}
