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

// ReportSchema's strict-mode "required" now names every optional field too —
// note, detail, verified, followups, needs_decision — so a codex child sends
// them as explicit JSON null rather than omitting them. ParseReport must
// decode that the same as an absent field, with no error and no false
// negative on "is this genuine".
func TestParseReportAcceptsExplicitJSONNulls(t *testing.T) {
	raw := `{
		"outcome": "completed",
		"summary": "Did the thing.",
		"changed": [{"path": "a.go", "action": "modified", "note": null}],
		"verified": null,
		"followups": null,
		"needs_decision": null
	}`
	r, ok := ParseReport(json.RawMessage(raw))
	if !ok {
		t.Fatal("ParseReport rejected a report whose optional fields were explicit JSON nulls")
	}
	if r.Outcome != OutcomeCompleted || r.Summary != "Did the thing." {
		t.Errorf("required fields did not decode: %+v", r)
	}
	if len(r.Changed) != 1 || r.Changed[0].Note != "" {
		t.Errorf("changed[0].note should decode a JSON null to the empty string, got %+v", r.Changed)
	}
	if r.Verified != nil {
		t.Errorf("verified should decode a JSON null to nil, got %+v", r.Verified)
	}
	if r.Followups != nil {
		t.Errorf("followups should decode a JSON null to nil, got %+v", r.Followups)
	}
	if r.NeedsDecision != "" {
		t.Errorf("needs_decision should decode a JSON null to the empty string, got %q", r.NeedsDecision)
	}

	// A report this sparse must still render without panicking on the nil slices.
	_ = r.Render("t8", "nulls")
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

// claude does enforce the schema's maxLength on the child's structured output
// for every field that still carries one, and a violation costs the entire
// report — so those caps are set far above the guidance in the descriptions
// and cannot be what keeps this small. The parent pays for every byte of a
// rendered report on every subsequent turn, so the renderer has to be the
// thing that bounds it. summary is the one exception: it has no cap, so this
// test keeps it short and piles the pathology onto changed/verified/followups
// instead — the fields the renderer's tail cap actually governs.
func TestRenderBoundsAPathologicalReport(t *testing.T) {
	r := Report{
		Outcome: OutcomeCompleted,
		Summary: "Rewired every handler in the dispatch table.",
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

	// Roughly 4 chars per token: the tail has to stay small enough that a dozen
	// of them in the parent's context is still cheaper than one child's
	// transcript. Slack covers the header and the short, uncapped summary.
	if n := utf8.RuneCountInString(got); n > maxRenderedRunes+len(r.Summary)+100 {
		t.Errorf("rendered report is %d runes, tail cap is %d; it must stay bounded",
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

// Every cap in the schema has to be exactly the clip limit the renderer applies
// to the same field, and the two live far apart — a string constant handed to
// claude, and an argument to clip — so nothing but a test keeps them together.
// summary is excluded on both sides: it carries no schema maxLength and no clip
// constant, by design — see the comment on the clip-limit const block.
func TestSchemaCapsMatchTheClipLimits(t *testing.T) {
	type stringField struct {
		MaxLength int `json:"maxLength"`
	}
	var v struct {
		Properties struct {
			Changed struct {
				Items struct {
					Properties struct {
						Note stringField `json:"note"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"changed"`
			Verified struct {
				Items struct {
					Properties struct {
						Check  stringField `json:"check"`
						Detail stringField `json:"detail"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"verified"`
			Followups struct {
				Items stringField `json:"items"`
			} `json:"followups"`
			NeedsDecision stringField `json:"needs_decision"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(ReportSchema), &v); err != nil {
		t.Fatal(err)
	}

	p := v.Properties
	for _, c := range []struct {
		field  string
		schema int
		clip   int
	}{
		{"changed[].note", p.Changed.Items.Properties.Note.MaxLength, maxNoteRunes},
		{"verified[].check", p.Verified.Items.Properties.Check.MaxLength, maxCheckRunes},
		{"verified[].detail", p.Verified.Items.Properties.Detail.MaxLength, maxDetailRunes},
		{"followups[]", p.Followups.Items.MaxLength, maxFollowupRunes},
		{"needs_decision", p.NeedsDecision.MaxLength, maxDecisionRunes},
	} {
		switch {
		case c.schema > c.clip:
			t.Errorf("%s: schema allows %d runes but the renderer clips at %d — text the child "+
				"was told it could write is silently truncated in the parent's context",
				c.field, c.schema, c.clip)
		case c.schema < c.clip:
			t.Errorf("%s: schema allows only %d runes while the renderer clips at %d — the child "+
				"loses its entire report to a validation error over text the renderer would "+
				"have carried in full", c.field, c.schema, c.clip)
		}
	}
}

// summary carries no cap at all — "its fine for a big summary to come upstream"
// — so a summary far bigger than maxRenderedRunes still has to arrive whole,
// header and following lines included. Multi-byte runes, so byte length is far
// past both caps while rune length is exactly 10000 — clip (were it wrongly
// still applied to the summary) would have to be counting the latter.
func TestRenderCarriesAHugeSummaryIntact(t *testing.T) {
	const summaryRunes = 10000
	summary := strings.Repeat("é", summaryRunes)
	r := Report{
		Outcome:  OutcomeCompleted,
		Summary:  summary,
		Changed:  []FileChange{{Path: "internal/orchestrator/report.go", Action: "modified"}},
		Verified: []Check{{Check: "go test ./internal/orchestrator/", Result: "pass"}},
	}

	got := r.Render("t7", "raise the caps")
	if strings.Contains(got, "…") {
		t.Errorf("a %d-rune summary was truncated; summary must never be clipped", summaryRunes)
	}
	if !strings.Contains(got, summary) {
		t.Error("the summary did not survive intact")
	}
	for _, want := range []string{"t7", "COMPLETED", "changed: ", "verified: "} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q — a huge summary crowded it out", want)
		}
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

// This is the regression guard for the whole schema, not just the fields
// spot-checked above: OpenAI's strict structured-output validator (the path a
// codex child hits) requires every object node to set "additionalProperties":
// false and to list EVERY key in "properties" inside "required" — an optional
// field is expressed by widening its type to include "null", never by leaving
// it out of "required". A live codex child hit exactly this on its first turn
// with a 400 ("'required' is required to be supplied and to be an array
// including every key in properties. Missing 'note'.") — see the comment on
// ReportSchema. Without this test, the only thing that catches a future
// property added without a matching "required" entry is another such 400.
func TestReportSchemaIsStrictModeLegalAtEveryObjectNode(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal([]byte(ReportSchema), &root); err != nil {
		t.Fatal(err)
	}

	isObjectType := func(typ any) bool {
		switch v := typ.(type) {
		case string:
			return v == "object"
		case []any:
			for _, e := range v {
				if s, _ := e.(string); s == "object" {
					return true
				}
			}
		}
		return false
	}

	var checked int
	var walk func(path string, node map[string]any)
	walk = func(path string, node map[string]any) {
		props, _ := node["properties"].(map[string]any)

		if isObjectType(node["type"]) {
			checked++
			if ap, ok := node["additionalProperties"].(bool); !ok || ap {
				t.Errorf("%s: additionalProperties must be `false`, got %#v", path, node["additionalProperties"])
			}

			required, _ := node["required"].([]any)
			requiredSet := make(map[string]bool, len(required))
			for _, r := range required {
				s, _ := r.(string)
				requiredSet[s] = true
			}
			if len(requiredSet) != len(required) {
				t.Errorf("%s: required has duplicate or non-string entries: %v", path, required)
			}

			for name := range props {
				if !requiredSet[name] {
					t.Errorf("%s: property %q is not in required — an optional property must still be "+
						"required and instead widen its type to include \"null\"", path, name)
				}
			}
			for name := range requiredSet {
				if _, ok := props[name]; !ok {
					t.Errorf("%s: required lists %q, which is not a property", path, name)
				}
			}
			if len(requiredSet) != len(props) {
				t.Errorf("%s: required (%d entries) and properties (%d entries) must be the same set",
					path, len(requiredSet), len(props))
			}
		}

		for name, v := range props {
			if child, ok := v.(map[string]any); ok {
				walk(path+"."+name, child)
			}
		}
		if items, ok := node["items"].(map[string]any); ok {
			walk(path+"[]", items)
		}
	}

	walk("$", root)

	if checked < 3 {
		t.Fatalf("only walked %d object node(s) — expected at least 3 (root, changed[], verified[]); "+
			"the walk may not be descending into \"items\"/\"properties\" correctly", checked)
	}
}
