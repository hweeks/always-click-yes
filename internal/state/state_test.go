package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stateDir points the package at a scratch directory. ACY_STATE_DIR exists for
// this: os.UserConfigDir is platform-specific, and a HOME-only override would not
// hold on every OS the tests run on.
func stateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(EnvDir, dir)
	return dir
}

func snap(id, cwd, phase string) Snapshot {
	return Snapshot{SessionID: id, Cwd: cwd, Phase: phase}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	stateDir(t)

	want := Snapshot{
		SessionID:   "abc-123",
		Cwd:         "/tmp/project",
		Phase:       "AUTO-RUN",
		Model:       "opus",
		PlanBody:    "step one\nstep two",
		Rounds:      3,
		CostSettled: 1.25,
		Lineage:     []string{"older-id"},
	}
	if err := Save(want); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, ok, err := Load("abc-123")
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if got.Phase != want.Phase || got.PlanBody != want.PlanBody ||
		got.Rounds != want.Rounds || got.CostSettled != want.CostSettled ||
		got.Model != want.Model || got.Cwd != want.Cwd {
		t.Fatalf("round trip lost fields:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Lineage) != 1 || got.Lineage[0] != "older-id" {
		t.Fatalf("lineage = %v", got.Lineage)
	}
	if got.Version != SchemaVersion {
		t.Fatalf("version = %d, want %d", got.Version, SchemaVersion)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("Save should stamp UpdatedAt")
	}
}

// A session acy never supervised has no snapshot. That is the common case, not an
// error: the caller falls back to a transcript-only resume.
func TestLoadMissingIsNotAnError(t *testing.T) {
	stateDir(t)

	got, ok, err := Load("nobody-home")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Fatalf("ok = true, want false (got %+v)", got)
	}
}

// A snapshot from a newer acy is refused rather than half-applied: restoring a
// run from a format we only partly understand is worse than not restoring it.
func TestLoadRefusesFutureSchema(t *testing.T) {
	dir := stateDir(t)

	path := filepath.Join(dir, "future.json")
	body := `{"version":999,"session_id":"future","phase":"AUTO-RUN"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	_, ok, err := Load("future")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Fatal("ok = true, want false for a newer schema version")
	}
}

// Save must be atomic, and must not leave litter that a later listing mistakes
// for a snapshot.
func TestSaveLeavesNoTempFiles(t *testing.T) {
	dir := stateDir(t)

	if err := Save(snap("a", "/p", "PLAN")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("temp file survived Save: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one file, got %d", len(entries))
	}
}

// A crashed Save leaves a dot-prefixed temp file behind. The *.json glob must not
// see it — otherwise a torn write would show up in the resume picker.
func TestAllIgnoresTempFiles(t *testing.T) {
	dir := stateDir(t)

	if err := Save(snap("real", "/p", "PLAN")); err != nil {
		t.Fatal(err)
	}
	litter := filepath.Join(dir, ".real.1234.tmp")
	if err := os.WriteFile(litter, []byte("{ truncated"), 0o600); err != nil {
		t.Fatal(err)
	}

	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].SessionID != "real" {
		t.Fatalf("All() = %+v, want just the real snapshot", all)
	}
}

func TestLatestPicksNewestForCwd(t *testing.T) {
	stateDir(t)

	// Same project, different ages.
	mustSave(t, Snapshot{SessionID: "old", Cwd: "/proj", Phase: "PLAN"})
	time.Sleep(2 * time.Millisecond) // UpdatedAt is the sort key
	mustSave(t, Snapshot{SessionID: "new", Cwd: "/proj", Phase: "AUTO-RUN"})
	// A different project must never be picked up.
	time.Sleep(2 * time.Millisecond)
	mustSave(t, Snapshot{SessionID: "elsewhere", Cwd: "/other", Phase: "AUTO-RUN"})

	got, ok, err := Latest("/proj")
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	if got.SessionID != "new" {
		t.Fatalf("Latest = %q, want %q", got.SessionID, "new")
	}

	if _, ok, _ := Latest("/nowhere"); ok {
		t.Fatal("Latest on an unknown project should report ok=false")
	}
}

// A superseded snapshot is a tombstone pointing at the live run; --continue must
// skip it and Resolve must follow it.
func TestSupersededIsSkippedAndResolved(t *testing.T) {
	stateDir(t)

	mustSave(t, Snapshot{SessionID: "forked", Cwd: "/proj", Phase: "AUTO-RUN", SupersededBy: "live"})
	time.Sleep(2 * time.Millisecond)
	mustSave(t, Snapshot{SessionID: "live", Cwd: "/proj", Phase: "AUTO-RUN"})

	got, ok, err := Latest("/proj")
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	if got.SessionID != "live" {
		t.Fatalf("Latest = %q, want the live run", got.SessionID)
	}

	id, err := Resolve("forked")
	if err != nil {
		t.Fatal(err)
	}
	if id != "live" {
		t.Fatalf("Resolve(forked) = %q, want %q", id, "live")
	}
}

// Resolve must terminate on a cycle rather than spinning the event loop forever.
func TestResolveTerminatesOnCycle(t *testing.T) {
	stateDir(t)

	mustSave(t, Snapshot{SessionID: "a", SupersededBy: "b"})
	mustSave(t, Snapshot{SessionID: "b", SupersededBy: "a"})

	done := make(chan string, 1)
	go func() {
		id, _ := Resolve("a")
		done <- id
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Resolve did not terminate on a cycle")
	}
}

// A session id reaches us from a --resume flag and from claude's init event.
// Neither is ours to hand to a path join unchecked.
func TestPathRejectsUnsafeIDs(t *testing.T) {
	stateDir(t)

	for _, id := range []string{"", "../escape", "a/b", `a\b`, ".hidden"} {
		if _, err := Path(id); err == nil {
			t.Errorf("Path(%q) should have been rejected", id)
		}
	}
}

func TestPrune(t *testing.T) {
	stateDir(t)

	for _, id := range []string{"one", "two", "three"} {
		mustSave(t, Snapshot{SessionID: id, Cwd: "/proj"})
		time.Sleep(2 * time.Millisecond)
	}
	if err := Prune(2); err != nil {
		t.Fatal(err)
	}
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("after Prune(2), %d snapshots remain", len(all))
	}
	// The newest survive.
	if all[0].SessionID != "three" || all[1].SessionID != "two" {
		t.Fatalf("Prune kept the wrong snapshots: %+v", all)
	}
}

// Engineers is additive: it round-trips like Tasks, and Unfinished tells a
// still-running entry from a terminal one the same way Task.Unfinished does.
func TestEngineerRoundTripAndUnfinished(t *testing.T) {
	stateDir(t)

	want := Snapshot{
		SessionID: "arch-1",
		Cwd:       "/tmp/project",
		Phase:     "AUTO-RUN",
		Engineers: []Engineer{
			{
				EngineerID: "e1", WireID: "w1", Ticket: "T1", Title: "fix it",
				Host: "host-a", Branch: "acy/t1", State: "running",
				LastSeq: 7, Tokens: Tokens{Input: 10, Output: 20},
				StartedAt: time.Now().UTC().Truncate(time.Second),
			},
			{
				EngineerID: "e2", WireID: "w2", Ticket: "T2", Title: "ship it",
				Host: "host-b", Branch: "acy/t2", State: "done",
				Outcome: "completed", PRURL: "https://example/pr/2", CostUSD: 1.5,
				StartedAt: time.Now().UTC().Truncate(time.Second),
				EndedAt:   time.Now().UTC().Truncate(time.Second),
			},
		},
	}
	if err := Save(want); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, ok, err := Load("arch-1")
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if len(got.Engineers) != 2 {
		t.Fatalf("Engineers = %+v, want 2 entries", got.Engineers)
	}
	e1, e2 := got.Engineers[0], got.Engineers[1]
	if e1.EngineerID != "e1" || e1.WireID != "w1" || e1.Ticket != "T1" || e1.Host != "host-a" ||
		e1.Branch != "acy/t1" || e1.State != "running" || e1.LastSeq != 7 || e1.Tokens.Input != 10 {
		t.Fatalf("e1 round trip lost fields: %+v", e1)
	}
	if !e1.Unfinished() {
		t.Error("e1.Unfinished() = false, want true (state running)")
	}
	if e2.State != "done" || e2.Outcome != "completed" || e2.PRURL != "https://example/pr/2" || e2.CostUSD != 1.5 {
		t.Fatalf("e2 round trip lost fields: %+v", e2)
	}
	if e2.Unfinished() {
		t.Error("e2.Unfinished() = true, want false (state done)")
	}
}

// A snapshot with no fleet must round-trip with no Engineers at all — the
// additive-field guarantee non-arch runs depend on.
func TestEngineersOmittedWhenNil(t *testing.T) {
	stateDir(t)

	if err := Save(snap("plain", "/p", "PLAN")); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load("plain")
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if len(got.Engineers) != 0 {
		t.Fatalf("Engineers = %+v, want none", got.Engineers)
	}
}

func mustSave(t *testing.T, s Snapshot) {
	t.Helper()
	if err := Save(s); err != nil {
		t.Fatalf("save %s: %v", s.SessionID, err)
	}
}
