package ui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/state"
)

// --- fixtures -----------------------------------------------------------------

func userEvent(text string) driver.Event {
	return driver.Event{
		Type:    driver.TypeUser,
		Message: &driver.Message{Role: "user", Content: json.RawMessage(mustJSON(text))},
	}
}

func assistantEvent(text string) driver.Event {
	blocks := []driver.ContentBlock{{Type: driver.BlockText, Text: text}}
	return driver.Event{
		Type:    driver.TypeAssistant,
		Message: &driver.Message{Role: "assistant", Content: json.RawMessage(mustJSON(blocks))},
	}
}

func toolResultEvent(text string) driver.Event {
	blocks := []driver.ContentBlock{{
		Type:      driver.BlockToolResult,
		ToolUseID: "toolu_1",
		Content:   json.RawMessage(mustJSON(text)),
	}}
	return driver.Event{
		Type:    driver.TypeUser,
		Message: &driver.Message{Role: "user", Content: json.RawMessage(mustJSON(blocks))},
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// resumeModel builds a model wired for restore, with both halves faked so the test
// never touches a disk — the same injection the rest of the ui tests use.
func resumeModel(t *testing.T, snap state.Snapshot, hasSnap bool, evs []driver.Event) (*Model, *[]state.Snapshot) {
	t.Helper()
	var saved []state.Snapshot
	m := New(nil, Config{
		Ctx:       context.Background(),
		Countdown: 30 * time.Second,
		Cwd:       "/proj",
		Launcher: func(context.Context, LaunchSpec) (*driver.Driver, error) {
			return driver.New(driver.Options{}), nil // never started; these tests don't run the cmd
		},
		LoadState: func(string) (state.Snapshot, bool, error) { return snap, hasSnap, nil },
		SaveState: func(s state.Snapshot) error { saved = append(saved, s); return nil },
		Replay:    func(string) ([]driver.Event, error) { return evs, nil },
	})
	return &m, &saved
}

// --- the transcript comes back ------------------------------------------------

// The live stream never echoes the prompts acy injects, so replaying a transcript
// through ingest alone would show Claude's answers with none of the questions.
func TestApplyResumeReplaysBothSidesOfTheConversation(t *testing.T) {
	evs := []driver.Event{
		userEvent("add a doc comment"),
		assistantEvent("here is the plan"),
		toolResultEvent("package main"),
		assistantEvent("done, the comment is added"),
	}
	m, _ := resumeModel(t, state.Snapshot{}, false, evs)

	m.applyResume(resumeMsg{id: "sess-1", events: evs})

	got := m.transcript()
	for _, want := range []string{"add a doc comment", "here is the plan", "done, the comment is added"} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript is missing %q\n---\n%s", want, got)
		}
	}
}

// turnText is what the resumed run's completion check reads for the STATUS
// sentinel. After a replay it must hold the *final* assistant turn — not the first,
// and not every turn concatenated. This is the assertion most likely to rot
// silently, because nothing else would visibly break if it did.
func TestApplyResumeLeavesOnlyTheFinalAssistantTurn(t *testing.T) {
	evs := []driver.Event{
		userEvent("first prompt"),
		assistantEvent("FIRST answer"),
		userEvent("second prompt"),
		assistantEvent("SECOND answer"),
	}
	m, _ := resumeModel(t, state.Snapshot{}, false, evs)

	m.applyResume(resumeMsg{id: "sess-1", events: evs})

	if strings.Contains(m.turnText, "FIRST") {
		t.Errorf("turnText carries an earlier turn; the completion check would read stale text:\n%q", m.turnText)
	}
	if !strings.Contains(m.turnText, "SECOND") {
		t.Errorf("turnText = %q, want the final assistant turn", m.turnText)
	}
}

// --- acy's own state comes back -----------------------------------------------

func TestApplyResumeRestoresRunState(t *testing.T) {
	snap := state.Snapshot{
		SessionID:   "sess-1",
		Cwd:         "/proj",
		Phase:       "AUTO-RUN",
		PlanBody:    "the approved plan",
		Rounds:      3,
		CostSettled: 1.25,
	}
	m, _ := resumeModel(t, snap, true, nil)

	cmd := m.applyResume(resumeMsg{id: "sess-1", snap: snap, hasSnap: true})
	if cmd == nil {
		t.Fatal("applyResume should relaunch the driver")
	}

	if m.planBody != "the approved plan" {
		t.Errorf("planBody = %q", m.planBody)
	}
	if m.rounds != 3 {
		t.Errorf("rounds = %d, want 3 — the auto-round budget must not reset on resume", m.rounds)
	}
	if m.totalCost() != 1.25 {
		t.Errorf("totalCost = %.2f, want 1.25 — the tally must survive the restart", m.totalCost())
	}
	if m.costCurrent != 0 {
		t.Errorf("costCurrent = %.2f, want 0 — a resumed process restarts its own total", m.costCurrent)
	}
	if m.sessionID != "sess-1" {
		t.Errorf("sessionID = %q", m.sessionID)
	}
}

// A session acy never supervised still resumes — you just get the conversation
// back, in a plan session, with no state to restore.
func TestApplyResumeWithoutSnapshotDegradesToPlan(t *testing.T) {
	evs := []driver.Event{userEvent("hi"), assistantEvent("hello")}
	m, _ := resumeModel(t, state.Snapshot{}, false, evs)

	m.applyResume(resumeMsg{id: "bare-claude-session", events: evs})

	if m.phase != PhasePlan {
		t.Errorf("phase = %s, want PLAN", m.phase)
	}
	if m.totalCost() != 0 {
		t.Errorf("totalCost = %.2f, want 0", m.totalCost())
	}
	if !strings.Contains(m.transcript(), "hello") {
		t.Error("the transcript should still be replayed without a snapshot")
	}
}

// A finished run comes back as a chat, not an auto-run: there is nothing left to
// drive, but the cost it already incurred must not vanish.
func TestApplyResumeOfCompleteRunLandsInPlan(t *testing.T) {
	snap := state.Snapshot{SessionID: "s", Phase: "COMPLETE", CostSettled: 4.10}
	m, _ := resumeModel(t, snap, true, nil)

	m.applyResume(resumeMsg{id: "s", snap: snap, hasSnap: true})

	if m.totalCost() != 4.10 {
		t.Errorf("totalCost = %.2f, want 4.10", m.totalCost())
	}
	if !strings.Contains(m.transcript(), "already completed") {
		t.Errorf("a completed run should say so on resume:\n%s", m.transcript())
	}
}

// A restore that fails must still leave a usable app.
func TestApplyResumeErrorFallsBackToAColdStart(t *testing.T) {
	m, _ := resumeModel(t, state.Snapshot{}, false, nil)

	cmd := m.applyResume(resumeMsg{id: "s", err: errBoom})
	if cmd == nil {
		t.Fatal("a failed restore should still cold-start a plan session")
	}
	if !strings.Contains(m.transcript(), "could not restore session") {
		t.Errorf("the failure should be visible:\n%s", m.transcript())
	}
}

var errBoom = errTest("boom")

type errTest string

func (e errTest) Error() string { return string(e) }

// --- the view stays responsive -------------------------------------------------

// rebuild() re-renders every entry on every keystroke, so a long transcript is
// capped. The cap must say what it hid — a silently truncated history reads as a
// lost one.
func TestApplyResumeCapsTheReplayedTranscript(t *testing.T) {
	var evs []driver.Event
	for i := range maxReplayEntries + 50 {
		evs = append(evs, assistantEvent(strings.Repeat("x", 3)+string(rune('a'+i%26))))
	}
	m, _ := resumeModel(t, state.Snapshot{}, false, evs)

	m.applyResume(resumeMsg{id: "s", events: evs})

	// maxReplayEntries kept, plus the elided-head note, plus the "resumed" line.
	if len(m.entries) > maxReplayEntries+2 {
		t.Fatalf("entries = %d, want the replay capped near %d", len(m.entries), maxReplayEntries)
	}
	if !strings.Contains(m.transcript(), "earlier entries elided") {
		t.Error("the cap must tell the user what it hid")
	}
}

// --- persistence ---------------------------------------------------------------

func TestPersistWritesWhatResumeNeeds(t *testing.T) {
	m, saved := resumeModel(t, state.Snapshot{}, false, nil)
	m.sessionID = "sess-9"
	m.phase = PhaseAutoRun
	m.planBody = "plan text"
	m.rounds = 2
	m.costSettled = 1.0
	m.costCurrent = 0.5

	m.persist()

	if len(*saved) != 1 {
		t.Fatalf("persist wrote %d snapshots, want 1", len(*saved))
	}
	got := (*saved)[0]
	if got.Phase != "AUTO-RUN" {
		t.Errorf("phase = %q", got.Phase)
	}
	if got.Rounds != 2 || got.PlanBody != "plan text" || got.Cwd != "/proj" {
		t.Errorf("snapshot = %+v", got)
	}
	// The running session's spend must be banked: on resume its process starts over
	// at zero, so anything not settled here is lost.
	if got.CostSettled != 1.5 {
		t.Errorf("cost_settled = %.2f, want 1.5 (settled + current)", got.CostSettled)
	}
}

// A session id we never learned means there is nothing to key a snapshot on.
func TestPersistIsANoOpBeforeTheSessionIDIsKnown(t *testing.T) {
	m, saved := resumeModel(t, state.Snapshot{}, false, nil)
	m.persist()
	if len(*saved) != 0 {
		t.Fatalf("persist wrote %d snapshots before the session id was known", len(*saved))
	}
}

// Every phase name must survive the round trip through the snapshot, or a resume
// silently lands in the wrong phase.
func TestPhaseRoundTrip(t *testing.T) {
	for _, p := range []Phase{PhasePlan, PhaseAutoRun, PhaseComplete} {
		if got := parsePhase(p.String()); got != p {
			t.Errorf("parsePhase(%q) = %s, want %s", p.String(), got, p)
		}
	}
	// Anything we don't recognise resumes into the phase that can do least harm.
	if got := parsePhase("nonsense"); got != PhasePlan {
		t.Errorf("parsePhase(nonsense) = %s, want PLAN", got)
	}
}

// If a future claude forks the session on --resume, the id we resumed becomes a
// dead end. Tombstone it, so a later --continue follows it to the live run instead
// of reviving its ancestor.
func TestAdoptSessionTombstonesAForkedID(t *testing.T) {
	m, saved := resumeModel(t, state.Snapshot{}, false, nil)
	m.sessionID = "old-id"

	m.adoptSession("new-id")

	if m.sessionID != "new-id" {
		t.Fatalf("sessionID = %q, want the id claude reported", m.sessionID)
	}
	if len(*saved) != 1 {
		t.Fatalf("expected the old id to be tombstoned, got %d writes", len(*saved))
	}
	if got := (*saved)[0]; got.SessionID != "old-id" || got.SupersededBy != "new-id" {
		t.Errorf("tombstone = %+v, want old-id superseded by new-id", got)
	}
	if len(m.lineage) != 1 || m.lineage[0] != "old-id" {
		t.Errorf("lineage = %v", m.lineage)
	}
}

// The ordinary case: claude keeps the id across --resume, so nothing is tombstoned.
func TestAdoptSessionIsANoOpWhenTheIDIsUnchanged(t *testing.T) {
	m, saved := resumeModel(t, state.Snapshot{}, false, nil)
	m.sessionID = "same"

	m.adoptSession("same")

	if len(*saved) != 0 {
		t.Fatalf("nothing should be tombstoned when the id is unchanged, got %d writes", len(*saved))
	}
}

// --- regressions ---------------------------------------------------------------

// The completion check's only evidence of what happened is the working session's
// last message. onDriverReady clears turnText on every launch — but a resumed
// auto-run is the one launch where the replay deliberately put the final assistant
// turn there. Clearing it would erase a STATUS: DONE the session had already said
// and nudge a finished run back into motion.
func TestResumedAutoRunReadsTheReplayedLastMessage(t *testing.T) {
	var sent strings.Builder
	m := New(nil, Config{Ctx: context.Background(), Countdown: 30 * time.Second})
	m.planBody = "the approved plan"
	m.turnText = "I finished both files.\nSTATUS: DONE" // what the replay recovered

	drv := driver.NewWithWriter(driver.Options{}, nopCloser{&sent})
	m.onDriverReady(driverReadyMsg{drv: drv, phase: PhaseAutoRun, kickoff: false})

	if m.phase != PhaseComplete {
		t.Fatalf("phase = %s, want COMPLETE — the launch ate the evidence on its way past", m.phase)
	}
	if sent.String() != "" {
		t.Errorf("a run that already said DONE must not be prompted; stdin got:\n%s", sent.String())
	}
}

// Arming still starts a fresh turn, so it must still clear the last turn's text.
func TestArmingClearsTheLastTurn(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.turnText = "chatter from the planning conversation"

	drv := driver.NewWithWriter(driver.Options{}, nopCloser{&strings.Builder{}})
	m.onDriverReady(driverReadyMsg{drv: drv, phase: PhaseAutoRun, kickoff: true})

	if m.turnText != "" {
		t.Errorf("turnText = %q, want it cleared for the new turn", m.turnText)
	}
}

// /resume can be typed mid-turn. Until the old driver is stopped and the generation
// moves on, its events still look current — and a `result` landing a second later
// would bank the abandoned session's cost into the restored run's tally.
func TestApplyResumeAbandonsTheOldSession(t *testing.T) {
	snap := state.Snapshot{SessionID: "sess-B", Phase: "AUTO-RUN", CostSettled: 1.00}
	m, _ := resumeModel(t, snap, true, nil)

	// A live session A, mid-turn, having spent something.
	m.drv = driver.NewWithWriter(driver.Options{}, nopCloser{&strings.Builder{}})
	m.costCurrent = 0.35
	m.processing = true
	genBefore := m.gen

	m.applyResume(resumeMsg{id: "sess-B", snap: snap, hasSnap: true})

	if m.gen == genBefore {
		t.Error("the generation must move on, or the old driver's events still look current")
	}
	if m.totalCost() != 1.00 {
		t.Errorf("cost = %.2f, want the restored run's 1.00 — the old session's spend must not be banked into it", m.totalCost())
	}
	if m.processing {
		t.Error("the abandoned session's in-flight turn must not leave the restored run 'working…' forever")
	}
}

// A stream that ended sets m.ended, which makes sendInput refuse forever. Resuming
// is the obvious thing to do next, so it must clear it — otherwise the composer is
// dead for the rest of the run.
func TestApplyResumeRevivesAnEndedSession(t *testing.T) {
	m, _ := resumeModel(t, state.Snapshot{}, false, nil)
	m.ended = true
	m.processing = true

	m.applyResume(resumeMsg{id: "sess-1"})

	if m.ended || m.processing {
		t.Fatalf("ended=%v processing=%v — a resumed run must come back usable", m.ended, m.processing)
	}
}

// The phase has to land immediately. Launching claude takes a second or two, and
// until the phase moves, a resumed run still looks like a plan session with a
// session id — which is exactly what Ctrl+G arms from. Arming there would spawn a
// second process for the same session and kick off work already half done.
func TestApplyResumeTakesThePhaseBeforeTheDriverLands(t *testing.T) {
	snap := state.Snapshot{SessionID: "s", Phase: "AUTO-RUN"}
	m, _ := resumeModel(t, snap, true, nil)

	m.applyResume(resumeMsg{id: "s", snap: snap, hasSnap: true})

	if m.phase != PhaseAutoRun {
		t.Fatalf("phase = %s, want AUTO-RUN immediately — not once the driver arrives", m.phase)
	}
	if m.HasDriver() {
		t.Fatal("precondition: the driver should not exist yet")
	}
	// Ctrl+G must not be able to fire in this window.
	if m.phase == PhasePlan && m.sessionID != "" && m.drv != nil {
		t.Fatal("the arm guard would fire during the restore window")
	}
}

// A run belongs to the project it started in. Resuming it from elsewhere must not
// rewrite that, or --continue back in the original project stops finding it.
func TestApplyResumeKeepsARunWithItsOwnProject(t *testing.T) {
	snap := state.Snapshot{SessionID: "s", Phase: "AUTO-RUN", Cwd: "/original/project"}
	m, _ := resumeModel(t, snap, true, nil) // the model's cwd is /proj

	m.applyResume(resumeMsg{id: "s", snap: snap, hasSnap: true})

	if got := m.snapshot().Cwd; got != "/original/project" {
		t.Errorf("snapshot Cwd = %q, want the run's own project — otherwise --continue there loses it", got)
	}
	if !strings.Contains(m.transcript(), "belongs to /original/project") {
		t.Errorf("resuming a run from another project should say so:\n%s", m.transcript())
	}
}
