package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

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
	return strings.ToLower(name), strings.TrimSpace(rest), true
}

// handleEnter dispatches the input box on Enter: a "/command" runs a supervisor
// command, anything else is sent to claude. It returns a tea.Cmd for commands
// that need one (quit, launching a resume).
func (m *Model) handleEnter() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	if name, args, ok := parseCommand(text); ok {
		m.input.Reset()
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
	case "resume":
		return m.startResume(args)
	default:
		m.appendEntry(entry{kind: eWarn, body: "unknown command /" + name + " — type /help"})
	}
	return nil
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
	m.sessionList = list
	m.pickIdx = 0
	m.picking = true
	return nil
}

// resumeSession relaunches the driver resuming id in interactive plan/chat mode,
// so the user can keep talking and then Ctrl+G to arm.
func (m *Model) resumeSession(id string) tea.Cmd {
	m.appendEntry(entry{kind: eGood, body: "↩ resuming session " + short(id) + " — continue chatting, Ctrl+G to arm"})
	m.status = "resuming…"
	return launchCmd(m.ctx, m.launcher, LaunchSpec{Phase: PhasePlan, ResumeID: id, Model: m.nextModel})
}

// handlePickKey drives the /resume picker. Returns a tea.Cmd when a selection
// launches a resume.
func (m *Model) handlePickKey(msg tea.KeyMsg) tea.Cmd {
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
		m.picking = false
		m.appendEntry(entry{kind: eMeta, body: "resume cancelled"})
	case "enter":
		id := m.sessionList[m.pickIdx].ID
		m.picking = false
		return m.resumeSession(id)
	}
	return nil
}
