package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The report is the entire return value of a delegated task.
//
// A child process burns six figures of tokens reading files, running tests and
// editing code, and then all of it is thrown away — the process exits and its
// context goes with it. This struct is the only thing that survives into the
// parent's conversation, so it is the one place where being expressive is worth
// paying for and everything else is waste.
//
// The schema is written as an interface rather than a form: the enumerations
// tell the child which behaviours exist at all, which is what stops it from
// inventing a "completed" when the honest answer was "I could not do this".

// Outcome values. A child that has no legitimate way to say no will say yes.
const (
	OutcomeCompleted = "completed"
	OutcomePartial   = "partial"
	OutcomeBlocked   = "blocked"
	OutcomeRejected  = "rejected"
)

// ReportSchema is passed to the child as --json-schema for a claude child, and
// as turn/start's outputSchema for a codex child (internal/supervisor/codex.go).
// Both backends validate the child's final answer against it and hand back a
// parsed structured_output object.
//
// The codex path reaches OpenAI's STRICT structured-output validator, which is
// stricter than claude's: every object node must set "additionalProperties":
// false AND list every one of its properties in "required" — there is no such
// thing as an optional property, only a required one whose type includes
// "null". A schema that omits an optional key from "required" is rejected
// outright, before the child gets to run at all; a live codex child hit this
// on its very first turn:
//
//	Invalid schema for response_format 'codex_output_schema': In
//	context=('properties', 'changed', 'items'), 'required' is required to be
//	supplied and to be an array including every key in properties. Missing
//	'note'.
//
// So every optional field below is expressed as a nullable union
// (`["string", "null"]` or `["array", "null"]`) and listed in its object's
// "required", with the description telling the child that null (or an empty
// array) is the correct answer when it has nothing to say — otherwise a model
// reading "required" without reading this comment will assume it must invent
// something for every field. This is one schema for both backends: a
// codex-only fork would drift from what claude actually enforces.
const ReportSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["outcome", "summary", "changed", "verified", "followups", "needs_decision"],
  "properties": {
    "outcome": {
      "type": "string",
      "enum": ["completed", "partial", "blocked", "rejected"],
      "description": "completed — every acceptance criterion is met AND you verified it. partial — real progress, work remains, nothing is stopping you. blocked — an obstacle you cannot clear alone: a missing credential, a broken dependency, a choice only a human can make. rejected — the task as written was the wrong thing to do; say why in summary and do not do it."
    },
    "summary": {
      "type": "string",
      "description": "What you did and what it means for the caller, in 1-3 sentences. Written for someone who did NOT watch you work and will never read your transcript."
    },
    "changed": {
      "type": "array",
      "maxItems": 40,
      "description": "Every file you created, modified or deleted. An empty array if you changed nothing.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["path", "action", "note"],
        "properties": {
          "path": {"type": "string", "description": "Repo-relative path."},
          "action": {"type": "string", "enum": ["created", "modified", "deleted"]},
          "note": {"type": ["string", "null"], "maxLength": 300, "description": "Only when the path alone does not say what changed. Null otherwise — do not invent one."}
        }
      }
    },
    "verified": {
      "type": ["array", "null"],
      "maxItems": 10,
      "description": "Checks you actually ran, and what they said. A claim in summary with no check here is an unverified claim, and the caller will read it that way. Null or empty if you ran none.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["check", "result", "detail"],
        "properties": {
          "check": {"type": "string", "maxLength": 200, "description": "The exact command or check, for example: go test ./internal/ui/"},
          "result": {"type": "string", "enum": ["pass", "fail", "not_run"]},
          "detail": {"type": ["string", "null"], "maxLength": 500, "description": "Required when result is fail: the failing case, not the raw output. Null otherwise."}
        }
      }
    },
    "followups": {
      "type": ["array", "null"],
      "maxItems": 5,
      "description": "Work you deliberately did not do. Phrase each so it could be dispatched as its own task; if you cannot, it belongs in summary instead. Null or empty if there are none.",
      "items": {"type": "string", "maxLength": 300}
    },
    "needs_decision": {
      "type": ["string", "null"],
      "maxLength": 600,
      "description": "Only when outcome is blocked and a human must choose. State the choice and the options — not the background. Null otherwise."
    }
  }
}`

// FileChange is one file a task touched.
//
// Note is required by ReportSchema but nullable in meaning: a child sends
// explicit JSON null when it has nothing to add. encoding/json's documented
// behaviour for unmarshaling null into a non-pointer field is a no-op — the
// field is simply left at its zero value and no error is produced — so a
// plain string is enough to accept both "absent" and "null" without a pointer.
type FileChange struct {
	Path   string `json:"path"`
	Action string `json:"action"`
	Note   string `json:"note,omitempty"`
}

// Check is one verification the child actually ran. Detail is nullable for the
// same reason as FileChange.Note above.
type Check struct {
	Check  string `json:"check"`
	Result string `json:"result"`
	Detail string `json:"detail,omitempty"`
}

// Report is a child's structured account of its task. Verified, Followups and
// NeedsDecision are all nullable in ReportSchema — a JSON null for a slice
// field unmarshals to nil with no error, the same as an absent key, so these
// need no special-case handling either.
type Report struct {
	Outcome       string       `json:"outcome"`
	Summary       string       `json:"summary"`
	Changed       []FileChange `json:"changed"`
	Verified      []Check      `json:"verified,omitempty"`
	Followups     []string     `json:"followups,omitempty"`
	NeedsDecision string       `json:"needs_decision,omitempty"`
}

// ParseReport decodes a child's structured_output.
//
// A malformed or absent report is not an error the caller can act on — the
// child has already exited and the parent is blocked waiting — so this always
// produces a usable Report and reports separately whether it was genuine.
func ParseReport(raw json.RawMessage) (Report, bool) {
	if len(raw) == 0 {
		return Report{}, false
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return Report{}, false
	}
	if r.Outcome == "" || r.Summary == "" {
		return r, false
	}
	return r, true
}

// degraded builds a report for a child that finished without producing one:
// it crashed, ran out of budget, or ignored the schema. Saying so plainly beats
// a silent "completed", which is the one failure the parent cannot detect.
func degraded(reason string) Report {
	return Report{
		Outcome: OutcomeBlocked,
		Summary: "This task produced no structured report: " + reason +
			". Its work may be partially applied — check the repository state before dispatching anything that builds on it.",
	}
}

// Render is what the parent's conversation actually pays for, so it is a fixed
// compact shape rather than the child's prose. The summary is the part a human
// actually reads and is exempt from every cap here — a child's summary must
// reach the parent whole, however long. Everything after it is bounded: a
// pathological pile of changed files, checks or followups must not cost more
// than the work it describes.
func (r Report) Render(taskID, title string) string {
	var head strings.Builder

	outcome := strings.ToUpper(r.Outcome)
	if outcome == "" {
		outcome = "UNKNOWN"
	}
	fmt.Fprintf(&head, "%s %s — %s\n", taskID, title, outcome)
	head.WriteString(strings.TrimSpace(r.Summary))

	var tail strings.Builder

	if len(r.Changed) > 0 {
		tail.WriteString("\nchanged: ")
		parts := make([]string, 0, len(r.Changed))
		for i, c := range r.Changed {
			if i == maxRenderedFiles {
				parts = append(parts, fmt.Sprintf("… +%d more", len(r.Changed)-i))
				break
			}
			p := c.Path + " (" + actionGlyph(c.Action) + ")"
			if c.Note != "" {
				p += " " + clip(c.Note, maxNoteRunes)
			}
			parts = append(parts, p)
		}
		tail.WriteString(strings.Join(parts, " · "))
	}

	if len(r.Verified) > 0 {
		tail.WriteString("\nverified: ")
		parts := make([]string, 0, len(r.Verified))
		for i, c := range r.Verified {
			if i == maxRenderedChecks {
				parts = append(parts, fmt.Sprintf("… +%d more", len(r.Verified)-i))
				break
			}
			p := clip(c.Check, maxCheckRunes) + " " + c.Result
			if c.Result == "fail" && c.Detail != "" {
				p += " (" + clip(c.Detail, maxDetailRunes) + ")"
			}
			parts = append(parts, p)
		}
		tail.WriteString(strings.Join(parts, " · "))
	}

	if r.NeedsDecision != "" {
		tail.WriteString("\nneeds a decision: " + clip(r.NeedsDecision, maxDecisionRunes))
	}

	for i, f := range r.Followups {
		if i == maxRenderedFollowups {
			fmt.Fprintf(&tail, "\nfollowup: … +%d more", len(r.Followups)-i)
			break
		}
		tail.WriteString("\nfollowup: " + clip(f, maxFollowupRunes))
	}

	// Per-field clips bound each part of the tail but say nothing about the
	// whole of it, and it is the whole that lands in the parent's context and is
	// re-read on every turn thereafter. So the tail's total is capped outright,
	// same as before — what changed is that the summary is no longer part of
	// what gets clipped here. The header and summary are written first and
	// therefore always survive.
	out := head.String()
	if t := clip(tail.String(), maxRenderedRunes); t != "" {
		out += "\n" + t
	}
	return out
}

const (
	maxRenderedFiles     = 20
	maxRenderedChecks    = 8
	maxRenderedFollowups = 5

	// ~1250 tokens. A dozen of these is still a rounding error against the
	// hundreds of thousands of tokens the children spent producing them.
	// Counted in runes, because that is what clip bounds — splitting a rune to
	// hit a byte target would corrupt the text for no benefit. This bounds the
	// non-summary tail only; the summary itself carries no cap.
	maxRenderedRunes = 5000
)

// The per-field clip limits, one per bounded field of a Report. Each one is also
// the field's maxLength in ReportSchema, and TestSchemaCapsMatchTheClipLimits
// holds the two together: a schema cap above the clip silently truncates text
// the child was allowed to write, and a cap below it costs the child its whole
// report to a validation error over text the renderer would have carried.
//
// summary is deliberately absent from both this list and the schema's
// maxLength: a long summary must reach the parent whole, so it has no clip
// constant and ReportSchema's "summary" property carries no maxLength either.
//
// These are a safety net, not guidance. What the child actually writes is shaped
// by the descriptions in the schema — "in 1-3 sentences" and the like — so the
// caps sit far above them and should effectively never bind.
const (
	maxNoteRunes     = 300
	maxCheckRunes    = 200
	maxDetailRunes   = 500
	maxFollowupRunes = 300
	maxDecisionRunes = 600
)

func actionGlyph(action string) string {
	switch action {
	case "created":
		return "+"
	case "deleted":
		return "-"
	case "modified":
		return "M"
	default:
		return "?"
	}
}

// clip truncates on a rune boundary. Both backends enforce a field's schema
// maxLength on the child's structured output — claude directly, and codex via
// OpenAI's strict structured-output validator — and a violation costs the
// entire report rather than merely a long field, so the caps sit far above the
// guidance the descriptions give and the renderer is what actually bounds the
// parent's context. summary has no maxLength and is never passed through clip:
// see the const block above.
func clip(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return strings.TrimSpace(string(r[:limit])) + "…"
}
