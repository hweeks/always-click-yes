package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokensAdd(t *testing.T) {
	var got Tokens
	got.Add(Tokens{Input: 1, Output: 2, CacheCreate: 3, CacheRead: 4})
	got.Add(Tokens{Input: 10, Output: 20, CacheCreate: 30, CacheRead: 40})

	want := Tokens{Input: 11, Output: 22, CacheCreate: 33, CacheRead: 44}
	if got != want {
		t.Errorf("Add = %+v, want %+v", got, want)
	}
	if got, want := want.Volume(), int64(110); got != want {
		t.Errorf("Volume() = %d, want %d", got, want)
	}
}

// CacheRead reaches millions in a single run — one measured run read 8,697,690
// cached tokens — so the tally must not be sized for a small number.
func TestTokensHoldRealRunVolumes(t *testing.T) {
	var got Tokens
	for range 100 {
		got.Add(Tokens{CacheRead: 8_697_690})
	}
	if want := int64(869_769_000); got.CacheRead != want {
		t.Errorf("CacheRead = %d, want %d", got.CacheRead, want)
	}
}

func TestTokensRoundTrip(t *testing.T) {
	stateDir(t)

	want := Snapshot{
		SessionID:    "tok-1",
		Cwd:          "/tmp/p",
		Phase:        "AUTO-RUN",
		ParentTokens: Tokens{Input: 5, Output: 60, CacheCreate: 700, CacheRead: 8_000},
		ChildTokens:  Tokens{CacheRead: 900_000},
		ChildCost:    2.5,
		Dispatches:   7,
	}
	if err := Save(want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load("tok-1")
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if got.ParentTokens != want.ParentTokens || got.ChildTokens != want.ChildTokens {
		t.Errorf("tokens = %+v / %+v, want %+v / %+v",
			got.ParentTokens, got.ChildTokens, want.ParentTokens, want.ChildTokens)
	}
	if got.ChildCost != want.ChildCost || got.Dispatches != want.Dispatches {
		t.Errorf("childCost=%v dispatches=%d, want %v / %d",
			got.ChildCost, got.Dispatches, want.ChildCost, want.Dispatches)
	}
}

// The token fields were added without bumping SchemaVersion, on the grounds
// that they are purely additive. That is only true if a snapshot written by an
// older build still loads — so prove it against a literal old-format file
// rather than against a struct this build produced.
func TestPreTokenSnapshotStillLoads(t *testing.T) {
	dir := stateDir(t)

	const old = `{"version":1,"session_id":"old-1","cwd":"/tmp/p","phase":"AUTO-RUN",` +
		`"rounds":3,"cost_settled":1.25,"updated_at":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, "old-1.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok, err := Load("old-1")
	if err != nil || !ok {
		t.Fatalf("an older snapshot must still load: ok=%v err=%v", ok, err)
	}
	if got.Rounds != 3 || got.CostSettled != 1.25 {
		t.Errorf("pre-existing fields lost: %+v", got)
	}
	if !got.ParentTokens.IsZero() || !got.ChildTokens.IsZero() {
		t.Errorf("absent token fields should decode to zero, got %+v / %+v",
			got.ParentTokens, got.ChildTokens)
	}
}

// omitzero, not omitempty: omitempty has no effect on a struct field, so
// without it every snapshot would carry four zeroed token objects forever.
func TestZeroTokensAreOmittedFromJSON(t *testing.T) {
	b, err := json.Marshal(Snapshot{SessionID: "x", Phase: "PLAN"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"parent_tokens", "child_tokens", "child_cost", "dispatches"} {
		if got := string(b); strings.Contains(got, key) {
			t.Errorf("zero-valued %q should be omitted, got %s", key, got)
		}
	}
}
