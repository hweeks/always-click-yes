package ui

import (
	"testing"

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
