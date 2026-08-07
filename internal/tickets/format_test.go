package tickets

import (
	"strings"
	"testing"
	"time"
)

func TestParseRenderRoundTripsStackOn(t *testing.T) {
	tk := Ticket{
		ID:      "child",
		Title:   "Child",
		Status:  StatusTodo,
		StackOn: "parent",
		Updated: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
		Body:    "Body.\n",
	}

	got, err := parse(render(tk))
	if err != nil {
		t.Fatalf("parse(render(tk)): %v", err)
	}
	if got.StackOn != "parent" {
		t.Fatalf("StackOn = %q, want %q", got.StackOn, "parent")
	}
}

func TestRenderOmitsEmptyStackOn(t *testing.T) {
	tk := Ticket{
		ID:      "solo",
		Title:   "Solo",
		Status:  StatusTodo,
		Updated: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
		Body:    "Body.\n",
	}

	out := string(render(tk))
	if strings.Contains(out, "stack_on") {
		t.Fatalf("render with no StackOn wrote a stack_on line:\n%s", out)
	}

	got, err := parse(render(tk))
	if err != nil {
		t.Fatalf("parse(render(tk)): %v", err)
	}
	if got.StackOn != "" {
		t.Fatalf("StackOn = %q, want empty", got.StackOn)
	}
}

// A file written before stack_on existed has no such key anywhere in its
// frontmatter. This proves boards from before this change still load.
func TestParseFrontmatterWithoutStackOnKey(t *testing.T) {
	raw := "---\n" +
		"id: t1\n" +
		"title: T1\n" +
		"status: todo\n" +
		"updated: 2026-08-05T12:00:00Z\n" +
		"---\n\nBody.\n"

	got, err := parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse with no stack_on key: %v", err)
	}
	if got.StackOn != "" {
		t.Fatalf("StackOn = %q, want empty", got.StackOn)
	}
}

func TestParseRejectsUnknownKeyWithStackOnAllowed(t *testing.T) {
	raw := "---\n" +
		"id: t1\n" +
		"title: T1\n" +
		"status: todo\n" +
		"assignee: bob\n" +
		"updated: 2026-08-05T12:00:00Z\n" +
		"---\n\nBody.\n"

	_, err := parse([]byte(raw))
	if err == nil {
		t.Fatal("parse with an unknown frontmatter key: want error, got nil")
	}
	if !strings.Contains(err.Error(), "assignee") {
		t.Fatalf("error %q does not name the unknown key", err)
	}
}

func TestParseStackOnAsListIsRejected(t *testing.T) {
	// stack_on is a scalar, not a list key — giving it list form ("key:"
	// with no value, followed by "  - " lines) must fail the same way any
	// other non-list key would.
	raw := "---\n" +
		"id: t1\n" +
		"title: T1\n" +
		"status: todo\n" +
		"stack_on:\n" +
		"  - parent\n" +
		"updated: 2026-08-05T12:00:00Z\n" +
		"---\n\nBody.\n"

	_, err := parse([]byte(raw))
	if err == nil {
		t.Fatal("parse with stack_on given list form: want error, got nil")
	}
}
