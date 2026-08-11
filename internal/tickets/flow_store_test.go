package tickets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPutWritesFlowMmd proves Put writes .acy/tickets/flow.mmd on every
// board change, with content that names the ticket just written.
func TestPutWritesFlowMmd(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	flowPath := filepath.Join(s.dir(), "flow.mmd")
	b, err := os.ReadFile(flowPath) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("reading flow.mmd: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("flow.mmd is empty, want mermaid content")
	}
	if !strings.Contains(string(b), "t1") {
		t.Fatalf("flow.mmd does not mention ticket t1:\n%s", b)
	}
}

// TestUpdateFieldsWritesFlowMmd proves the same for the UpdateFields path,
// which Put underlies.
func TestUpdateFieldsWritesFlowMmd(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.UpdateFields("t1", StatusInProgress, "", "", "", ""); err != nil {
		t.Fatalf("UpdateFields: %v", err)
	}

	flowPath := filepath.Join(s.dir(), "flow.mmd")
	b, err := os.ReadFile(flowPath) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("reading flow.mmd: %v", err)
	}
	if !strings.Contains(string(b), "in-progress") {
		t.Fatalf("flow.mmd does not reflect the updated status:\n%s", b)
	}
}

// TestPutWritesFlowMmdRegardlessOfMode proves the flow.mmd write happens for
// both Store.Mode values — it is a local file write, not a git operation, so
// Mode "none" must not suppress it.
func TestPutWritesFlowMmdRegardlessOfMode(t *testing.T) {
	for _, mode := range []string{"none", "direct"} {
		s := New(t.TempDir(), mode, failRunner(t))
		s.now = newTestStore(t).now
		if err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo}); err != nil {
			t.Fatalf("Put (mode=%s): %v", mode, err)
		}
		flowPath := filepath.Join(s.dir(), "flow.mmd")
		if _, err := os.Stat(flowPath); err != nil {
			t.Fatalf("flow.mmd missing (mode=%s): %v", mode, err)
		}
	}
}

// TestListIgnoresStrayMmdFile proves List() only ever reads files that end
// in ".md" — flow.mmd, which Put now writes alongside every ticket, does not
// end in ".md" and must not be parsed as one.
func TestListIgnoresStrayMmdFile(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Put already wrote flow.mmd; overwrite it with garbage that would fail
	// to parse as a ticket, to prove List() never even opens it.
	flowPath := filepath.Join(s.dir(), "flow.mmd")
	if err := os.WriteFile(flowPath, []byte("not a ticket file"), 0o644); err != nil {
		t.Fatalf("WriteFile(flow.mmd): %v", err)
	}

	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != "t1" {
		t.Fatalf("List() = %+v, want exactly [t1]", list)
	}
}
