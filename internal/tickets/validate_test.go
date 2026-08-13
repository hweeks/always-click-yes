package tickets

import (
	"strings"
	"testing"
)

func rawTicket(id, title, status, dependsOn string) string {
	return rawTicketFull(id, title, status, dependsOn, "")
}

func rawTicketFull(id, title, status, dependsOn, stackOn string) string {
	dep := ""
	if dependsOn != "" {
		dep = "depends_on:\n  - " + dependsOn + "\n"
	}
	stack := ""
	if stackOn != "" {
		stack = "stack_on: " + stackOn + "\n"
	}
	return "---\n" +
		"id: " + id + "\n" +
		"title: " + title + "\n" +
		"status: " + status + "\n" +
		dep +
		stack +
		"updated: 2026-08-05T12:00:00Z\n" +
		"---\n\nBody.\n"
}

// A jira value containing a newline used to be written verbatim into the
// single "jira: <value>\n" frontmatter line, splitting into an extra line
// the hand-rolled parser reads back as a bogus or duplicate key. Put must
// refuse it before it ever reaches disk.
func TestPutRejectsNewlineInJira(t *testing.T) {
	s := newTestStore(t)
	err := s.Put(Ticket{ID: "a", Title: "A", Status: StatusTodo, Jira: "ENG-1\nstatus: merged"})
	if err == nil {
		t.Fatal("Put with a newline embedded in jira: want error, got nil")
	}
	if !strings.Contains(err.Error(), "jira") {
		t.Fatalf("error %q does not name the jira field", err)
	}
}

// Every field render.go writes on its own single frontmatter line has the
// same hole jira did: an embedded newline or carriage return splits it into
// extra lines. Put must reject all of them, not just jira.
func TestPutRejectsNewlineInEverySingleLineField(t *testing.T) {
	cases := []struct {
		name string
		t    Ticket
	}{
		{"id", Ticket{ID: "a\nb", Title: "T", Status: StatusTodo}},
		{"title", Ticket{ID: "a", Title: "T\nx: y", Status: StatusTodo}},
		{"status", Ticket{ID: "a", Title: "T", Status: "todo\nx: y"}},
		{"branch", Ticket{ID: "a", Title: "T", Status: StatusTodo, Branch: "b\nx: y"}},
		{"pr", Ticket{ID: "a", Title: "T", Status: StatusTodo, PR: "1\nx: y"}},
		{"jira", Ticket{ID: "a", Title: "T", Status: StatusTodo, Jira: "ENG-1\r\nx: y"}},
		{"stack_on", Ticket{ID: "a", Title: "T", Status: StatusTodo, StackOn: "p\nx: y"}},
		{"depends_on", Ticket{ID: "a", Title: "T", Status: StatusTodo, DependsOn: []string{"p\nx: y"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestStore(t)
			if err := s.Put(c.t); err == nil {
				t.Fatalf("Put with a newline embedded in %s: want error, got nil", c.name)
			}
		})
	}
}

// A legitimate value with ordinary punctuation — including a colon, which
// the frontmatter format itself uses as a delimiter — must still round-trip
// through Put/Get unchanged. The fix must reject newlines specifically, not
// tighten the format so far that real values stop working.
func TestPutRoundTripsLegitimateFieldValues(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(Ticket{ID: "base", Title: "Base ticket", Status: StatusTodo}); err != nil {
		t.Fatalf("Put(base): %v", err)
	}

	tk := Ticket{
		ID:        "a",
		Title:     "Fix: the thing, properly!",
		Status:    StatusInProgress,
		Branch:    "feature/fix-the-thing",
		PR:        "https://github.com/org/repo/pull/42",
		Jira:      "ENG-42",
		DependsOn: []string{"base"},
		StackOn:   "base",
	}
	if err := s.Put(tk); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get("a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != tk.Title {
		t.Errorf("Title = %q, want %q", got.Title, tk.Title)
	}
	if got.Branch != tk.Branch {
		t.Errorf("Branch = %q, want %q", got.Branch, tk.Branch)
	}
	if got.PR != tk.PR {
		t.Errorf("PR = %q, want %q", got.PR, tk.PR)
	}
	if got.Jira != tk.Jira {
		t.Errorf("Jira = %q, want %q", got.Jira, tk.Jira)
	}
	if got.StackOn != tk.StackOn {
		t.Errorf("StackOn = %q, want %q", got.StackOn, tk.StackOn)
	}
	if len(got.DependsOn) != 1 || got.DependsOn[0] != "base" {
		t.Errorf("DependsOn = %v, want [base]", got.DependsOn)
	}
}

func TestValidateOK(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(Ticket{ID: "a", Title: "A", Status: StatusTodo}); err != nil {
		t.Fatalf("Put(a): %v", err)
	}
	if err := s.Put(Ticket{ID: "b", Title: "B", Status: StatusTodo, DependsOn: []string{"a"}}); err != nil {
		t.Fatalf("Put(b): %v", err)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate on a clean store: %v", err)
	}
}

func TestValidateCatchesDuplicateID(t *testing.T) {
	s := newTestStore(t)
	// Two files, hand-written, that both claim the same id — a shape Put
	// itself can never produce, since it always reuses one ticket's existing
	// file. Validate exists precisely to catch this if it happens anyway.
	writeRaw(t, s, "a-one.md", rawTicket("a", "A one", "todo", ""))
	writeRaw(t, s, "a-two.md", rawTicket("a", "A two", "todo", ""))

	err := s.Validate()
	if err == nil {
		t.Fatal("Validate with two files claiming id \"a\": want error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error %q does not mention a duplicate id", err)
	}
}

func TestValidateCatchesDanglingDependsOn(t *testing.T) {
	s := newTestStore(t)
	// Written directly rather than via Put, which already refuses a
	// depends_on naming a ticket that doesn't exist.
	writeRaw(t, s, "a.md", rawTicket("a", "A", "todo", "ghost"))

	err := s.Validate()
	if err == nil {
		t.Fatal("Validate with a dangling depends_on: want error, got nil")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("error %q does not name the dangling ticket", err)
	}
}

func TestValidateCatchesCycle(t *testing.T) {
	s := newTestStore(t)
	// A depends on B and B depends on A. Put cannot construct this (each
	// call would fail the existence check on the other, not-yet-created
	// ticket), so the pair is written directly.
	writeRaw(t, s, "a.md", rawTicket("a", "A", "todo", "b"))
	writeRaw(t, s, "b.md", rawTicket("b", "B", "todo", "a"))

	err := s.Validate()
	if err == nil {
		t.Fatal("Validate with an A->B->A cycle: want error, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error %q does not mention a cycle", err)
	}
}

func TestValidateCatchesSelfCycle(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(Ticket{ID: "a", Title: "A", Status: StatusTodo, DependsOn: []string{"a"}}); err != nil {
		t.Fatalf("Put(a) with a self-reference: %v", err)
	}

	err := s.Validate()
	if err == nil {
		t.Fatal("Validate with a self-referencing ticket: want error, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error %q does not mention a cycle", err)
	}
}

func TestValidateCatchesDanglingStackOn(t *testing.T) {
	s := newTestStore(t)
	// Written directly rather than via Put, which already refuses a
	// stack_on naming a ticket that doesn't exist.
	writeRaw(t, s, "a.md", rawTicketFull("a", "A", "todo", "", "ghost"))

	err := s.Validate()
	if err == nil {
		t.Fatal("Validate with a dangling stack_on: want error, got nil")
	}
	if !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("error %q does not name both tickets", err)
	}
}

func TestValidateCatchesStackOnCycle(t *testing.T) {
	s := newTestStore(t)
	// A stacks on B and B stacks on A. Put cannot construct this (each call
	// would fail the existence check on the other, not-yet-created ticket),
	// so the pair is written directly.
	writeRaw(t, s, "a.md", rawTicketFull("a", "A", "todo", "", "b"))
	writeRaw(t, s, "b.md", rawTicketFull("b", "B", "todo", "", "a"))

	err := s.Validate()
	if err == nil {
		t.Fatal("Validate with an A->B->A stack_on cycle: want error, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error %q does not mention a cycle", err)
	}
	if !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Fatalf("error %q does not name both tickets", err)
	}
}

func TestValidateCatchesMultipleClaimantsOfSameStackOn(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(Ticket{ID: "base", Title: "Base", Status: StatusTodo}); err != nil {
		t.Fatalf("Put(base): %v", err)
	}
	if err := s.Put(Ticket{ID: "child-one", Title: "Child one", Status: StatusTodo, StackOn: "base"}); err != nil {
		t.Fatalf("Put(child-one): %v", err)
	}
	if err := s.Put(Ticket{ID: "child-two", Title: "Child two", Status: StatusTodo, StackOn: "base"}); err != nil {
		t.Fatalf("Put(child-two): %v", err)
	}

	err := s.Validate()
	if err == nil {
		t.Fatal("Validate with two tickets stacking on the same parent: want error, got nil")
	}
	if !strings.Contains(err.Error(), "base") {
		t.Fatalf("error %q does not name the shared parent", err)
	}
	if !strings.Contains(err.Error(), "child-one") || !strings.Contains(err.Error(), "child-two") {
		t.Fatalf("error %q does not name both claiming tickets", err)
	}
}
