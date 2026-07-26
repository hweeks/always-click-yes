package orchestrator

import (
	"encoding/json"

	"github.com/hweeks/always-click-yes/internal/mcp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseReportRejectsIncompleteReports(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"absent", ``, false},
		{"not json", `garbage`, false},
		{"no outcome", `{"summary":"did a thing"}`, false},
		{"no summary", `{"outcome":"completed"}`, false},
		{"minimal valid", `{"outcome":"completed","summary":"did a thing"}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ok := ParseReport(json.RawMessage(c.raw))
			if ok != c.ok {
				t.Errorf("ParseReport(%q) ok = %v, want %v", c.raw, ok, c.ok)
			}
		})
	}
}

func TestRenderIncludesEverythingThatMatters(t *testing.T) {
	r := Report{
		Outcome: OutcomeBlocked,
		Summary: "Could not add the ledger.",
		Changed: []FileChange{
			{Path: "a.go", Action: "created"},
			{Path: "b.go", Action: "modified", Note: "renamed the field"},
			{Path: "c.go", Action: "deleted"},
		},
		Verified: []Check{
			{Check: "go build ./...", Result: "pass"},
			{Check: "go test ./...", Result: "fail", Detail: "TestLedger: want 3 got 0"},
		},
		Followups:     []string{"wire it into the header"},
		NeedsDecision: "store tokens per turn or per process?",
	}

	got := r.Render("t3", "add the ledger")
	for _, want := range []string{
		"t3", "add the ledger", "BLOCKED", "Could not add the ledger.",
		"a.go (+)", "b.go (M) renamed the field", "c.go (-)",
		"go test ./... fail", "TestLedger: want 3 got 0",
		"needs a decision:", "store tokens per turn",
		"followup: wire it into the header",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
}

// The schema states maxLength and maxItems, but whether claude enforces them is
// not something acy can rely on — and the parent pays for every byte of this on
// every subsequent turn. So the renderer has to be the thing that bounds it.
func TestRenderBoundsAPathologicalReport(t *testing.T) {
	r := Report{
		Outcome: OutcomeCompleted,
		Summary: strings.Repeat("a very long summary. ", 500),
	}
	for range 200 {
		r.Changed = append(r.Changed, FileChange{
			Path:   strings.Repeat("deep/", 20) + "file.go",
			Action: "modified",
			Note:   strings.Repeat("note ", 200),
		})
		r.Verified = append(r.Verified, Check{
			Check: strings.Repeat("check ", 100), Result: "fail",
			Detail: strings.Repeat("detail ", 200),
		})
		r.Followups = append(r.Followups, strings.Repeat("followup ", 100))
	}

	got := r.Render("t1", "big")

	// Roughly 4 chars per token: this has to stay small enough that a dozen of
	// them in the parent's context is still cheaper than one child's transcript.
	if n := utf8.RuneCountInString(got); n > maxRenderedRunes+1 {
		t.Errorf("rendered report is %d runes, cap is %d; it must stay bounded",
			n, maxRenderedRunes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("truncation should be visible, not silent")
	}
	if !strings.Contains(got, "COMPLETED") {
		t.Error("the outcome must survive truncation — it is the only part that is always read")
	}
}

// A report that fits must come through untouched: the cap is a backstop, not a
// routine haircut.
func TestRenderLeavesAnOrdinaryReportAlone(t *testing.T) {
	r := Report{
		Outcome: OutcomeCompleted,
		Summary: "Added the token ledger and wired it into the header.",
		Changed: []FileChange{{Path: "internal/state/state.go", Action: "modified"}},
		Verified: []Check{
			{Check: "go test ./...", Result: "pass"},
			{Check: "go test -race ./...", Result: "pass"},
		},
	}
	got := r.Render("t2", "add the ledger")
	if strings.Contains(got, "…") {
		t.Errorf("an ordinary report should not be truncated:\n%s", got)
	}
	if len(got) > 400 {
		t.Errorf("a typical report is %d bytes; it should be far smaller than the cap", len(got))
	}
}

func TestDegradedReportIsHonest(t *testing.T) {
	r := degraded("it ran out of budget")
	if r.Outcome != OutcomeBlocked {
		t.Errorf("outcome = %q, want %q — a child that vanished has not completed anything",
			r.Outcome, OutcomeBlocked)
	}
	got := r.Render("t9", "something")
	for _, want := range []string{"no structured report", "it ran out of budget", "check the repository state"} {
		if !strings.Contains(got, want) {
			t.Errorf("degraded report missing %q:\n%s", want, got)
		}
	}
}

func TestClipIsRuneSafe(t *testing.T) {
	got := clip(strings.Repeat("é", 100), 10)
	if strings.Contains(got, "\uFFFD") {
		t.Errorf("clip split a multi-byte rune: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("clip should mark truncation, got %q", got)
	}
}

// The schemas ship as string constants and are handed straight to claude, so a
// typo in one is a runtime failure in a place that is awkward to reach.
func TestSchemasAreValidJSON(t *testing.T) {
	for name, schema := range map[string]string{
		"ReportSchema":       ReportSchema,
		"mcp.DispatchSchema": mcp.DispatchSchema,
	} {
		var v map[string]any
		if err := json.Unmarshal([]byte(schema), &v); err != nil {
			t.Errorf("%s is not valid JSON: %v", name, err)
			continue
		}
		if v["type"] != "object" {
			t.Errorf("%s should describe an object", name)
		}
		if _, ok := v["properties"]; !ok {
			t.Errorf("%s has no properties", name)
		}
	}
}

// Outcome is the one field the parent branches on, so the enum in the schema
// and the constants in Go have to agree.
func TestOutcomeConstantsMatchTheSchema(t *testing.T) {
	var v struct {
		Properties struct {
			Outcome struct {
				Enum []string `json:"enum"`
			} `json:"outcome"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(ReportSchema), &v); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		OutcomeCompleted: false, OutcomePartial: false,
		OutcomeBlocked: false, OutcomeRejected: false,
	}
	for _, e := range v.Properties.Outcome.Enum {
		if _, ok := want[e]; !ok {
			t.Errorf("schema allows outcome %q with no matching Go constant", e)
		}
		want[e] = true
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("Go constant %q is not in the schema's enum", k)
		}
	}
}
