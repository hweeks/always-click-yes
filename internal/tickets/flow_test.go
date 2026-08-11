package tickets

import (
	"strings"
	"testing"
)

// fullBoard exercises all five statuses, a depends_on edge, and a
// three-ticket stack chain, in the fixed order Mermaid and ASCII must
// preserve (both packages are told never to sort by map iteration).
func fullBoard() []Ticket {
	return []Ticket{
		{ID: "t-todo", Title: "Todo Ticket", Status: StatusTodo},
		{ID: "t-progress", Title: "Progress Ticket", Status: StatusInProgress, DependsOn: []string{"t-todo"}},
		{ID: "t-review", Title: "Review Ticket", Status: StatusInReview},
		{ID: "t-merged", Title: "Merged Ticket", Status: StatusMerged},
		{ID: "t-blocked", Title: "Blocked Ticket", Status: StatusBlocked, Jira: "ENG-1"},
		{ID: "s1", Title: "Stack Root", Status: StatusTodo},
		{ID: "s2", Title: "Stack Middle", Status: StatusTodo, StackOn: "s1"},
		{ID: "s3", Title: "Stack Leaf", Status: StatusTodo, StackOn: "s2"},
	}
}

const wantFullBoardMermaid = `flowchart TD
    t-todo["t-todo: Todo Ticket [todo]"]:::todo
    t-progress["t-progress: Progress Ticket [in-progress]"]:::in-progress
    t-review["t-review: Review Ticket [in-review]"]:::in-review
    t-merged["t-merged: Merged Ticket [merged]"]:::merged
    t-blocked["t-blocked: Blocked Ticket [blocked] (ENG-1)"]:::blocked
    s1["s1: Stack Root [todo]"]:::todo
    s2["s2: Stack Middle [todo]"]:::todo
    s3["s3: Stack Leaf [todo]"]:::todo
    t-todo --> t-progress
    s1 -.->|stacking| s2
    s2 -.->|stacking| s3
    classDef todo fill:#e0e0e0
    classDef in-progress fill:#fff3b0
    classDef in-review fill:#bde0fe
    classDef merged fill:#b7e4c7
    classDef blocked fill:#f8b4b4
`

const wantFullBoardASCII = `[todo] (4)
  - t-todo
  - s1
  - s2
  - s3
[in-progress] (1)
  - t-progress
[in-review] (1)
  - t-review
[merged] (1)
  - t-merged
[blocked] (1)
  - t-blocked

stacks:
  s1 -> s2 -> s3
`

func TestMermaidGoldenFullBoard(t *testing.T) {
	got := Mermaid(fullBoard())
	if got != wantFullBoardMermaid {
		t.Fatalf("Mermaid(fullBoard) mismatch\ngot:\n%s\nwant:\n%s", got, wantFullBoardMermaid)
	}
}

func TestMermaidGoldenFullBoardStable(t *testing.T) {
	// Run it twice — no map iteration in the emission path means the same
	// board must produce byte-identical output every time, not just once.
	first := Mermaid(fullBoard())
	second := Mermaid(fullBoard())
	if first != second {
		t.Fatalf("Mermaid(fullBoard) not stable across runs:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestASCIIGoldenFullBoard(t *testing.T) {
	got := ASCII(fullBoard())
	if got != wantFullBoardASCII {
		t.Fatalf("ASCII(fullBoard) mismatch\ngot:\n%s\nwant:\n%s", got, wantFullBoardASCII)
	}
}

func TestASCIIGoldenFullBoardStable(t *testing.T) {
	first := ASCII(fullBoard())
	second := ASCII(fullBoard())
	if first != second {
		t.Fatalf("ASCII(fullBoard) not stable across runs:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

const wantEmptyBoardMermaid = `flowchart TD
    empty["no tickets yet"]
`

const wantEmptyBoardASCII = "no tickets yet\n"

func TestMermaidEmptyBoard(t *testing.T) {
	got := Mermaid(nil)
	if got != wantEmptyBoardMermaid {
		t.Fatalf("Mermaid(nil) = %q, want %q", got, wantEmptyBoardMermaid)
	}
}

func TestASCIIEmptyBoard(t *testing.T) {
	got := ASCII(nil)
	if got != wantEmptyBoardASCII {
		t.Fatalf("ASCII(nil) = %q, want %q", got, wantEmptyBoardASCII)
	}
}

func TestMermaidEmptyBoardNonEmptyOutput(t *testing.T) {
	if got := Mermaid([]Ticket{}); got == "" {
		t.Fatal("Mermaid(empty slice) returned empty output, want a deterministic placeholder")
	}
}

func TestASCIIEmptyBoardNonEmptyOutput(t *testing.T) {
	if got := ASCII([]Ticket{}); got == "" {
		t.Fatal("ASCII(empty slice) returned empty output, want a deterministic placeholder")
	}
}

// escapedTitleBoard has a single ticket whose title carries a double quote,
// square brackets, and an embedded newline — everything Mermaid's label
// escaping must handle so the generated .mmd stays parseable.
func escapedTitleBoard() []Ticket {
	return []Ticket{
		{ID: "esc-1", Title: "Weird \"Title\" [brackets]\nSecond line", Status: StatusTodo},
	}
}

const wantEscapedTitleMermaid = `flowchart TD
    esc-1["esc-1: Weird &quot;Title&quot; [brackets]<br/>Second line [todo]"]:::todo
    classDef todo fill:#e0e0e0
    classDef in-progress fill:#fff3b0
    classDef in-review fill:#bde0fe
    classDef merged fill:#b7e4c7
    classDef blocked fill:#f8b4b4
`

func TestMermaidEscapesQuotesAndNewlines(t *testing.T) {
	got := Mermaid(escapedTitleBoard())
	if got != wantEscapedTitleMermaid {
		t.Fatalf("Mermaid(escapedTitleBoard) mismatch\ngot:\n%s\nwant:\n%s", got, wantEscapedTitleMermaid)
	}
	if !strings.Contains(got, "&quot;Title&quot;") {
		t.Fatal("Mermaid output does not contain the escaped quote &quot;Title&quot;")
	}
	if strings.Contains(got, `"Title"`) {
		t.Fatal("Mermaid output contains a bare, unescaped double quote inside the label")
	}
	if strings.Contains(got, "brackets]\nSecond") {
		t.Fatal("Mermaid output contains a literal newline inside the label, want <br/>")
	}
	if !strings.Contains(got, "brackets]<br/>Second") {
		t.Fatal("Mermaid output does not contain the escaped newline <br/> inside the label")
	}
}
