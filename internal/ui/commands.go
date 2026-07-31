package ui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/hweeks/always-click-yes/internal/session"
	"github.com/hweeks/always-click-yes/internal/state"
)

// snapLabel is the acy state shown against a session in the resume picker: which
// phase it stopped in, how many tasks it delegated, what it had cost. A
// session acy never supervised has no snapshot and shows nothing, which is how you
// tell the two apart in the list.
func snapLabel(s state.Snapshot, ok bool) string {
	if !ok || s.Phase == "" {
		return ""
	}
	parts := []string{s.Phase}
	if s.Dispatches > 0 {
		parts = append(parts, fmt.Sprintf("%d tasks", s.Dispatches))
	}
	if s.CostSettled > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", s.CostSettled))
	}
	return strings.Join(parts, " · ")
}

// parseCommand splits a leading-slash input into a lowercase command name and
// its argument string. ok is false for anything that is not a "/command".
func parseCommand(s string) (name, args string, ok bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "/") {
		return "", "", false
	}
	s = strings.TrimPrefix(s, "/")
	if s == "" {
		return "", "", false
	}
	name, rest, _ := strings.Cut(s, " ")
	// An absolute path is not a command. Every supervisor command is a single bare
	// word, so a separator in the name means the leading slash was the root of a
	// path — which is exactly what a message opening with an attached file looks
	// like, and it would otherwise be eaten as an unknown command and never sent.
	if strings.ContainsRune(name, '/') {
		return "", "", false
	}
	return strings.ToLower(name), strings.TrimSpace(rest), true
}

// handleEnter dispatches the input box on Enter: a "/command" runs a supervisor
// command, anything else is sent to claude. It returns a tea.Cmd for commands
// that need one (quit, launching a resume).
func (m *Model) handleEnter() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	if name, args, ok := parseCommand(text); ok {
		m.clearComposer()
		return m.runCommand(name, args)
	}
	m.sendInput()
	return nil
}

// runCommand executes an app-level slash command. Unknown commands surface a
// warning rather than being forwarded to claude.
func (m *Model) runCommand(name, args string) tea.Cmd {
	switch name {
	case "help", "?":
		m.showHelp = true
	case "clear":
		m.entries = nil
		m.appendEntry(entry{kind: eMeta, body: "(transcript cleared) · /help for commands"})
	case "quit", "exit", "q":
		if m.drv != nil {
			m.drv.Stop()
		}
		return tea.Quit
	case "model":
		if args == "" {
			cur := m.nextModel
			if cur == "" {
				cur = "(launcher default)"
			}
			m.appendEntry(entry{kind: eMeta, body: "model for the next session: " + cur})
			break
		}
		m.nextModel = args
		m.appendEntry(entry{kind: eMeta, body: "model set to " + args + " — applies to the next launched/resumed session"})
	case "log":
		p := m.logPath
		if p == "" {
			p = "(disabled)"
		}
		m.appendEntry(entry{kind: eMeta, body: "debug log: " + p})
	case "tokens", "cost":
		m.appendEntry(entry{kind: eMeta, body: m.tokenReport()})
	case "tasks":
		// Deliberately reads the model's own ledger rather than re-syncing from the
		// orchestrator: after a /resume the rows come from the snapshot while the
		// orchestrator has never run a task, so syncing here would erase them.
		m.appendEntry(entry{kind: eMeta, body: m.taskReport()})
	case "done", "finish":
		// The manual counterpart to the Finish tool, for when the session stops
		// without calling it. There is deliberately no automatic version: a run
		// that goes quiet is a question for a human, not a reason to spend
		// another full-context turn asking "are you done yet?".
		if m.phase == PhaseComplete {
			m.appendEntry(entry{kind: eMeta, body: "this run is already finished"})
			break
		}
		m.cancelDispatches("the run was finished by hand")
		m.finish("completed", strings.TrimSpace(args))
	case "queue":
		switch strings.ToLower(args) {
		case "":
			m.appendEntry(entry{kind: eMeta, body: m.queueReport()})
		case "clear":
			n := len(m.queued)
			m.queued = nil
			body := "queue cleared — " + plural(n, "message") + " dropped, unsent"
			if n == 0 {
				body = "the queue was already empty"
			}
			m.appendEntry(entry{kind: eMeta, body: body})
		default:
			m.appendEntry(entry{kind: eWarn, body: "unknown argument " + args + " — /queue or /queue clear"})
		}
	case "retry":
		m.raise(RetryCooldown())
	case "resume":
		return m.startResume(args)
	default:
		m.appendEntry(entry{kind: eWarn, body: "unknown command /" + name + " — type /help"})
	}
	return nil
}

// queueReport lists the messages waiting to go out. In full, not truncated: this
// is the command you type when you want to read back — or copy out — what you
// wrote while the model was working.
func (m Model) queueReport() string {
	if len(m.queued) == 0 {
		return "nothing queued — with the session idle, Enter sends straight away"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s · goes out as one turn when the session next falls idle",
		plural(len(m.queued), "queued message"))
	for i, q := range m.queued {
		fmt.Fprintf(&b, "\n%2d. %s", i+1, q)
	}
	return b.String()
}

// startResume opens the session picker (no arg) or resumes a specific id.
func (m *Model) startResume(arg string) tea.Cmd {
	if arg != "" {
		return m.resumeSession(arg)
	}
	if m.sessionLister == nil {
		m.appendEntry(entry{kind: eWarn, body: "session listing is unavailable"})
		return nil
	}
	list, err := m.sessionLister()
	if err != nil {
		m.appendEntry(entry{kind: eWarn, body: "could not list sessions: " + err.Error()})
		return nil
	}
	if len(list) == 0 {
		m.appendEntry(entry{kind: eMeta, body: "no past sessions found for this project"})
		return nil
	}
	rows := pickRows(list, m.loadState)
	if len(rows) == 0 {
		m.appendEntry(entry{kind: eMeta, body: "no past sessions found for this project"})
		return nil
	}
	m.sessionList = rows
	m.pickIdx = 0
	m.picking = true
	return nil
}

// pickRow is one picker row as the model holds it: the row both front ends show,
// plus the transcript's modification time.
//
// The time is kept as a time.Time rather than read back out of the row's
// ModTimeUnixMs because the two front ends want different things from it. A
// client formats the instant itself, in whatever locale and zone the editor is
// in, so the wire carries milliseconds; the terminal prints it here, and
// re-deriving it from unix milliseconds would render every row in the machine's
// local zone even when the listing knew better.
type pickRow struct {
	SessionRow
	modTime time.Time
}

// SessionRows builds the /resume picker's rows for a session list: acy's own
// snapshot paired with each session, dispatched children dropped, and one label
// per row saying what acy remembers of the run.
//
// Exported because the HTTP server serves the same picker to a webview, and a
// second implementation of "which sessions are resumable and what do they say"
// is exactly the pair that drifts — the terminal and the webview would then
// disagree about a list a person is choosing from. The picker path below builds
// its rows through pickRows, which this is the wire-shaped half of.
//
// load is state.Load in production and a fake in tests; a nil load means no
// snapshots, so every row comes back unlabelled rather than the call failing.
func SessionRows(list []session.Info, load func(string) (state.Snapshot, bool, error)) []SessionRow {
	rows := pickRows(list, load)
	out := make([]SessionRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.SessionRow)
	}
	return out
}

// pickRows is the one implementation: load each session's snapshot, hide the
// dispatched children, label the rest.
func pickRows(list []session.Info, load func(string) (state.Snapshot, bool, error)) []pickRow {
	snaps := loadSnaps(list, load)
	list = hideChildSessions(list, snaps)
	out := make([]pickRow, 0, len(list))
	for _, s := range list {
		snap, ok := snaps[s.ID]
		out = append(out, pickRow{
			SessionRow: SessionRow{
				ID:            s.ID,
				ModTimeUnixMs: unixMs(s.ModTime),
				Summary:       s.Summary,
				Label:         snapLabel(snap, ok),
			},
			modTime: s.ModTime,
		})
	}
	return out
}

// hideChildSessions drops dispatched children from the picker.
//
// Children are persisted like any other session — deliberately, because when one
// does something unexplained its transcript is the only record of why — but a
// twenty-task run would otherwise bury the run itself under twenty rows you can
// never usefully resume. The ledger in each snapshot says which ids were
// children, so this needs no naming convention and no second source of truth.
func hideChildSessions(list []session.Info, snaps map[string]state.Snapshot) []session.Info {
	children := map[string]bool{}
	for _, snap := range snaps {
		for _, t := range snap.Tasks {
			if t.SessionID != "" {
				children[t.SessionID] = true
			}
		}
	}
	if len(children) == 0 {
		return list
	}
	out := make([]session.Info, 0, len(list))
	for _, s := range list {
		if !children[s.ID] {
			out = append(out, s)
		}
	}
	return out
}

// loadSnaps pairs each listed session with acy's state for it, where there is any.
// The list is claude's — it includes sessions acy never drove (a bare `claude`
// run), and those simply have no snapshot.
func loadSnaps(list []session.Info, load func(string) (state.Snapshot, bool, error)) map[string]state.Snapshot {
	if load == nil {
		return nil
	}
	snaps := make(map[string]state.Snapshot, len(list))
	for _, s := range list {
		if snap, ok, err := load(s.ID); err == nil && ok {
			snaps[s.ID] = snap
		}
	}
	return snaps
}

// resumeSession restores a prior session: claude's transcript comes back on screen
// and acy's own state (phase, plan, rounds, cost) comes back with it, so an armed
// run picks up where it stopped rather than restarting as a fresh chat.
func (m *Model) resumeSession(id string) tea.Cmd {
	m.status = "resuming…"
	return loadResumeCmd(id, m.loadState, m.replay)
}

// handlePickKey drives the /resume picker. Returns a tea.Cmd when a selection
// launches a resume.
func (m *Model) handlePickKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "up", "k":
		if m.pickIdx > 0 {
			m.pickIdx--
		}
	case "down", "j":
		if m.pickIdx < len(m.sessionList)-1 {
			m.pickIdx++
		}
	case "esc":
		// The keyboard is a client: closing the picker lives in applyAction, and
		// the terminal reads the outcome off the screen rather than off the result.
		_ = m.raise(PickerClose())
	case "enter":
		// Not routed through raise, and it cannot be: raise discards the tea.Cmd
		// applyAction returns, so a resume raised that way would never launch.
		// pickerClose is safe to route precisely because it produces no command.
		id := m.sessionList[m.pickIdx].ID
		m.picking = false
		return m.resumeSession(id)
	}
	return nil
}
