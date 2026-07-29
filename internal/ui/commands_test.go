package ui

import (
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/session"
	"github.com/hweeks/always-click-yes/internal/state"
)

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in   string
		name string
		args string
		ok   bool
	}{
		{"/help", "help", "", true},
		{"  /resume  ", "resume", "", true},
		{"/resume abc123", "resume", "abc123", true},
		{"/model claude-opus-4-8", "model", "claude-opus-4-8", true},
		{"/RESUME Abc", "resume", "Abc", true}, // name lowercased, args preserved
		{"just text", "", "", false},
		{"", "", "", false},
		{"/", "", "", false},
		{"not/a/command", "", "", false},
	}
	for _, c := range cases {
		name, args, ok := parseCommand(c.in)
		if ok != c.ok || name != c.name || args != c.args {
			t.Errorf("parseCommand(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, name, args, ok, c.name, c.args, c.ok)
		}
	}
}

// The picker mixes sessions acy supervised with sessions it merely knows about
// (a bare `claude` run). The label is how you tell them apart, so an unsupervised
// session must render as nothing at all.
func TestSnapLabel(t *testing.T) {
	tests := []struct {
		name string
		snap state.Snapshot
		ok   bool
		want string
	}{
		{"no snapshot", state.Snapshot{}, false, ""},
		{"empty phase", state.Snapshot{}, true, ""},
		{"planning", state.Snapshot{Phase: "PLAN"}, true, "PLAN"},
		{"mid auto-run", state.Snapshot{Phase: "AUTO-RUN", Dispatches: 3, CostSettled: 1.234}, true, "AUTO-RUN · 3 tasks · $1.23"},
		{"complete", state.Snapshot{Phase: "COMPLETE", CostSettled: 4.1}, true, "COMPLETE · $4.10"},
		{"armed but not yet spent", state.Snapshot{Phase: "AUTO-RUN"}, true, "AUTO-RUN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := snapLabel(tt.snap, tt.ok); got != tt.want {
				t.Errorf("snapLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A dispatched child is a real claude session on disk, so it turns up in the
// picker's list — and a twenty-task run would bury the run itself under twenty
// rows nobody can usefully resume. The ledger says which ids were children.
func TestPickerHidesDispatchedChildren(t *testing.T) {
	list := []session.Info{
		{ID: "parent-1"}, {ID: "child-a"}, {ID: "unrelated"}, {ID: "child-b"},
	}
	snaps := map[string]state.Snapshot{
		"parent-1": {SessionID: "parent-1", Tasks: []state.Task{
			{ID: "t1", SessionID: "child-a"},
			{ID: "t2", SessionID: "child-b"},
		}},
	}

	got := hideChildSessions(list, snaps)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (the parent and the unrelated session): %+v", len(got), got)
	}
	for _, s := range got {
		if s.ID == "child-a" || s.ID == "child-b" {
			t.Errorf("child session %s should be hidden", s.ID)
		}
	}
}

// With no ledger there is nothing to hide, and the list must come back intact
// rather than empty.
func TestPickerKeepsEverythingWithoutALedger(t *testing.T) {
	list := []session.Info{{ID: "a"}, {ID: "b"}}
	if got := hideChildSessions(list, nil); len(got) != 2 {
		t.Errorf("got %d rows, want 2", len(got))
	}
}

// snapLoader is a state.Load stand-in over a fixed map, for tests that need the
// picker to be told which sessions acy supervised.
func snapLoader(snaps map[string]state.Snapshot) func(string) (state.Snapshot, bool, error) {
	return func(id string) (state.Snapshot, bool, error) {
		s, ok := snaps[id]
		return s, ok, nil
	}
}

// SessionRows is the one list both front ends show: same filtering, same labels.
// The HTTP server calls it directly and the /resume picker gets its rows from
// the same place, so a row that is hidden or labelled in one is hidden or
// labelled in the other.
func TestSessionRows(t *testing.T) {
	mod := time.Unix(1_700_000_000, 0)
	list := []session.Info{
		{ID: "parent-1", ModTime: mod, Summary: "port the parser"},
		{ID: "child-a", ModTime: mod, Summary: "a dispatched task"},
		{ID: "plain", ModTime: mod, Summary: "a plain claude chat"},
	}
	rows := SessionRows(list, snapLoader(map[string]state.Snapshot{
		"parent-1": {
			Phase: "AUTO-RUN", Dispatches: 3, CostSettled: 2.5,
			Tasks: []state.Task{{ID: "t1", SessionID: "child-a"}},
		},
	}))

	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (the child is hidden): %+v", len(rows), rows)
	}
	if rows[0].ID != "parent-1" || rows[0].Label != "AUTO-RUN · 3 tasks · $2.50" {
		t.Errorf("row 0 = %+v, want the supervised run with its label", rows[0])
	}
	// An empty label is how a plain claude session is told apart from a run acy
	// supervised — it is not a missing value.
	if rows[1].ID != "plain" || rows[1].Label != "" {
		t.Errorf("row 1 = %+v, want the unsupervised session with no label", rows[1])
	}
	if rows[0].ModTimeUnixMs != mod.UnixMilli() {
		t.Errorf("modTimeUnixMs = %d, want %d", rows[0].ModTimeUnixMs, mod.UnixMilli())
	}
	// Nothing is selected: the cursor belongs to whoever is showing the list.
	for _, r := range rows {
		if r.Selected {
			t.Errorf("row %s came back selected; the picker stamps that, not the list", r.ID)
		}
	}
}

// No state store at all (ui tests, and any front end that was given none) means
// no labels — never a dropped list.
func TestSessionRowsWithoutState(t *testing.T) {
	rows := SessionRows([]session.Info{{ID: "a"}, {ID: "b"}}, nil)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Label != "" {
		t.Errorf("label = %q, want empty with no snapshots", rows[0].Label)
	}
}
