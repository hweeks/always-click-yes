package ui

import (
	"fmt"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/state"
)

// maxReplayEntries caps how much of a restored transcript is put on screen.
//
// This is not cosmetic. rebuild() re-renders every entry — markdown, syntax
// highlighting, wrapping — on every message and every keystroke, so an uncapped
// replay of a long session would make typing crawl. Claude still has the whole
// conversation; only the view is trimmed, and the elided head says so.
const maxReplayEntries = 200

// resumeMsg carries a restored session back into the event loop.
type resumeMsg struct {
	id      string
	snap    state.Snapshot
	hasSnap bool
	events  []driver.Event
	err     error
}

// loadResumeCmd reads the two halves of a resumable run off the event loop:
// claude's transcript (the conversation) and acy's snapshot (everything about the
// run that claude does not record). A missing snapshot is not an error — it just
// means acy never supervised this session, and the resume degrades to replaying
// the transcript into a fresh plan session.
func loadResumeCmd(
	id string,
	loadState func(string) (state.Snapshot, bool, error),
	replay func(string) ([]driver.Event, error),
) tea.Cmd {
	return func() tea.Msg {
		msg := resumeMsg{id: id}
		if loadState != nil {
			snap, ok, err := loadState(id)
			if err != nil {
				return resumeMsg{id: id, err: err}
			}
			msg.snap, msg.hasSnap = snap, ok
		}
		if replay != nil {
			evs, err := replay(id)
			if err != nil {
				return resumeMsg{id: id, err: err}
			}
			msg.events = evs
		}
		return msg
	}
}

// applyResume rebuilds the run from a prior session and relaunches its driver.
// The transcript comes back on screen, acy's own state comes back from the
// snapshot, and the phase the run was in decides how the new driver comes up.
func (m *Model) applyResume(msg resumeMsg) tea.Cmd {
	if msg.err != nil {
		m.appendEntry(entry{kind: eWarn, body: "could not restore session: " + msg.err.Error()})
		m.status = "planning"
		// A failed restore must still leave a usable app, so fall back to the cold
		// start the user would otherwise have got.
		return launchCmd(m.ctx, m.launcher, LaunchSpec{Phase: PhasePlan})
	}

	// Abandon the session we were in before adopting the new one. /resume can be
	// typed mid-turn, and until this driver is stopped and the generation moved on,
	// its events still look current: a `result` arriving a second later would bank
	// the old session's cost into the restored run's tally and stamp the old phase
	// over the restored snapshot.
	if m.drv != nil {
		m.drv.Stop()
		m.drv = nil
	}
	m.gen++

	// A restored run is a different run. Everything the old one was in the middle of
	// — a turn in flight, a stream that had ended, a question on screen — belongs to
	// a session that no longer exists. Left set, `ended` or `processing` would make
	// the composer refuse to send for the rest of the run.
	m.ended = false
	m.processing = false
	m.interrupted = false
	m.planReady = false
	m.ask = nil
	m.pending = nil

	m.sessionID = msg.id
	m.entries = nil
	m.turnText = ""
	m.planBody = ""
	m.costSettled = 0
	m.costCurrent = 0
	m.parentTokens = state.Tokens{}
	m.childTokens = state.Tokens{}
	m.childCost = 0
	m.dispatches = 0
	m.tasks = nil
	m.lastContext = 0

	for _, ev := range msg.events {
		m.ingestReplay(ev)
	}
	replayed := len(m.entries)
	m.capReplay()

	phase := PhasePlan
	if msg.hasSnap {
		m.planBody = msg.snap.PlanBody
		m.costSettled = msg.snap.CostSettled // a resumed process restarts its own total at zero
		// Tokens carry over verbatim: they were counted per turn, so there is no
		// per-process figure to reconcile — only a tally to keep going.
		m.parentTokens = msg.snap.ParentTokens
		m.childTokens = msg.snap.ChildTokens
		m.childCost = msg.snap.ChildCost
		m.dispatches = msg.snap.Dispatches
		m.tasks = msg.snap.Tasks
		m.lineage = msg.snap.Lineage
		phase = parsePhase(msg.snap.Phase)

		// A run belongs to the project it was started in. Resuming it from somewhere
		// else would otherwise rewrite its Cwd on the next persist, and `--continue`
		// back in the original project would no longer find it — losing the very run
		// you were trying to get back to.
		if snapCwd := msg.snap.Cwd; snapCwd != "" && !sameDir(snapCwd, m.cwd) {
			m.appendEntry(entry{kind: eWarn, body: fmt.Sprintf(
				"this run belongs to %s — resuming it from here; it stays listed under its own project", snapCwd)})
			m.cwd = snapCwd
		}
	}

	// A run that already finished comes back as a plan session: there is nothing
	// left to auto-run, but you may well want to keep talking to it — and Ctrl+G
	// re-arms if you do.
	if phase == PhaseComplete {
		m.appendEntry(entry{kind: eMeta, body: fmt.Sprintf(
			"this run had already completed · $%.4f spent · continue chatting, Ctrl+G to arm again", m.totalCost())})
		phase = PhasePlan
	}

	m.appendEntry(entry{kind: eGood, body: fmt.Sprintf(
		"↩ resumed session %s · %s · %d entries replayed%s",
		short(msg.id), phase, replayed, costSuffix(m.totalCost()))})

	alog.Printf("resume: id=%s phase=%s entries=%d tasks=%d cost=%.4f snapshot=%v",
		msg.id, phase, replayed, m.dispatches, m.totalCost(), msg.hasSnap)

	m.noteInterruptedTasks()

	// Take the phase now, not when the driver lands. Launching claude takes a second
	// or two, and until the phase moves the run still looks like a plan session with
	// a session id — which is exactly the state Ctrl+G arms from. A keypress in that
	// window would launch a *second* process for the same session and kick off the
	// work it is already halfway through.
	m.phase = phase
	m.status = "resuming…"
	return launchCmd(m.ctx, m.launcher, LaunchSpec{
		Phase:    phase,
		ResumeID: msg.id,
		Model:    m.nextModel,
	})
}

// noteInterruptedTasks marks any task that was still running when the run died.
//
// A child process is killed with its supervisor, mid-edit and mid-thought, so
// its work may be half-applied to the working tree. That is the one situation
// where a tool built to run unattended must not guess: re-dispatching could
// redo work already done, and skipping it could leave the tree broken. So the
// tasks are named, and the decision is handed to the session with the resume
// prompt below.
func (m *Model) noteInterruptedTasks() {
	var stuck []string
	for i := range m.tasks {
		if !m.tasks[i].Unfinished() {
			continue
		}
		m.tasks[i].Outcome = "interrupted"
		m.tasks[i].EndedAt = m.tasks[i].StartedAt
		stuck = append(stuck, fmt.Sprintf("%s (%s)", m.tasks[i].ID, m.tasks[i].Title))
	}
	if len(stuck) == 0 {
		return
	}
	m.interruptedTasks = stuck
	m.appendEntry(entry{kind: eWarn, body: fmt.Sprintf(
		"⚠ %d task(s) were interrupted by the restart and never reported: %s\n"+
			"Their work may be partly applied — check before building on it.",
		len(stuck), strings.Join(stuck, ", "))})
}

// resumePrompt is the single message a restored auto-run is sent.
//
// This is not the nudge loop coming back. That loop fired after *every* idle
// turn, up to ten times, re-billing the whole conversation each time to ask "are
// you done yet?" — and it was the most expensive habit acy had. This fires once,
// in response to the human explicitly asking to resume an armed run, which is
// the difference between honouring a request and guessing.
func (m *Model) resumePrompt() string {
	var b strings.Builder
	b.WriteString("This run was interrupted and has been restored. ")
	if len(m.interruptedTasks) > 0 {
		b.WriteString("These tasks were still running when it died and never reported: ")
		b.WriteString(strings.Join(m.interruptedTasks, ", "))
		b.WriteString(". Their work may be partly applied, so check the current state of those files ")
		b.WriteString("before deciding whether to re-dispatch them. ")
	}
	b.WriteString("Take stock of what is actually done, then carry on with the approved plan. ")
	b.WriteString("Call Finish when it is all complete and verified.")
	return b.String()
}

// sameDir compares two project paths the way state.Latest does.
func sameDir(a, b string) bool {
	abs := func(p string) string {
		if r, err := filepath.Abs(p); err == nil {
			return r
		}
		return p
	}
	return abs(a) == abs(b)
}

func costSuffix(total float64) string {
	if total <= 0 {
		return ""
	}
	return fmt.Sprintf(" · $%.4f so far", total)
}

// ingestReplay is ingest for a record read back from claude's transcript.
//
// It differs in exactly one place, and the difference is load-bearing: the live
// stream never echoes the prompts acy injects — claude only ever sends user events
// carrying tool_results — so ingest has no case for user *text*. The transcript,
// though, records every prompt. Replaying it through ingest alone would put
// Claude's answers on screen with none of the questions.
func (m *Model) ingestReplay(ev driver.Event) {
	if ev.Type == driver.TypeUser && ev.Message != nil {
		if text := userText(ev); text != "" {
			// A new prompt means the turn that follows is a new one. Resetting here
			// leaves turnText holding exactly the final assistant turn once the replay
			// ends — which is precisely what the resumed run's completion check reads
			// for the STATUS sentinel.
			m.turnText = ""
			m.appendEntry(entry{kind: eYou, body: text})
			return
		}
	}
	m.ingest(ev)
}

// userText is the prose of a user record, or "" if the record is a tool_result
// (which ingest already knows how to render).
func userText(ev driver.Event) string {
	var parts []string
	for _, b := range ev.Message.Blocks() {
		if b.Type == driver.BlockToolResult {
			return "" // a tool result, not something a human typed
		}
		if b.Type == driver.BlockText {
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// capReplay trims a restored transcript to the newest maxReplayEntries, noting
// what it dropped. The full conversation is still claude's — this only bounds what
// acy re-renders.
func (m *Model) capReplay() {
	if len(m.entries) <= maxReplayEntries {
		return
	}
	dropped := len(m.entries) - maxReplayEntries
	kept := append([]entry{m.stamp(entry{kind: eMeta, body: fmt.Sprintf(
		"… %d earlier entries elided from the view · Claude still has the full context", dropped)})},
		m.entries[dropped:]...)
	m.entries = kept
}

// parsePhase turns a persisted phase name back into a Phase. An unknown or empty
// name resumes into PLAN, which is the phase that can do the least harm.
func parsePhase(s string) Phase {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "AUTO-RUN":
		return PhaseAutoRun
	case "COMPLETE":
		return PhaseComplete
	default:
		return PhasePlan
	}
}

// snapshot is acy's current state, in the form that survives a restart.
func (m *Model) snapshot() state.Snapshot {
	return state.Snapshot{
		SessionID:   m.sessionID,
		Cwd:         m.cwd,
		Phase:       m.phase.String(),
		Model:       m.model,
		PlanBody:    m.planBody,
		CostSettled: m.totalCost(), // bank the running session too: on resume it restarts at zero

		ParentTokens: m.parentTokens,
		ChildTokens:  m.childTokens,
		ChildCost:    m.childCost,
		Dispatches:   m.dispatches,
		Tasks:        state.TrimTasks(m.tasks),

		Lineage: m.lineage,
	}
}

// persist records acy's state for the current session. It runs at every transition
// rather than on a timer — the file is a few hundred bytes and a rename, and the
// payoff is that a crash at any moment resumes to the last thing that actually
// happened. A persistence failure is logged, never fatal: it costs you a resume,
// not a run.
func (m *Model) persist() {
	if m.saveState == nil || m.sessionID == "" {
		return
	}
	if err := m.saveState(m.snapshot()); err != nil {
		alog.Printf("state: save failed: %v", err)
	}
}

// adoptSession records the session id claude reports at init.
//
// claude 2.1.207 keeps the id across --resume, so this is normally a no-op restate
// of the id we asked for. If a future version forks instead, the id we resumed is
// now a dead end: tombstone it so a later --continue or --resume follows it forward
// to the run that is actually live, rather than reviving the fork's ancestor.
func (m *Model) adoptSession(id string) {
	if id == "" || id == m.sessionID {
		return // nothing to adopt; never let an empty id erase the one we have
	}
	if prev := m.sessionID; prev != "" && m.saveState != nil {
		alog.Printf("resume: session forked %s -> %s", prev, id)
		old := m.snapshot()
		old.SupersededBy = id
		if err := m.saveState(old); err != nil {
			alog.Printf("state: tombstone %s failed: %v", prev, err)
		}
		m.lineage = append(m.lineage, prev)
	}
	m.sessionID = id
}
