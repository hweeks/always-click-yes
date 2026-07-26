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

// ReportSchema is passed to the child as --json-schema, which makes claude
// validate its own final answer against it and hand back a parsed
// structured_output object on the result event.
const ReportSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["outcome", "summary", "changed"],
  "properties": {
    "outcome": {
      "type": "string",
      "enum": ["completed", "partial", "blocked", "rejected"],
      "description": "completed — every acceptance criterion is met AND you verified it. partial — real progress, work remains, nothing is stopping you. blocked — an obstacle you cannot clear alone: a missing credential, a broken dependency, a choice only a human can make. rejected — the task as written was the wrong thing to do; say why in summary and do not do it."
    },
    "summary": {
      "type": "string",
      "maxLength": 600,
      "description": "What you did and what it means for the caller, in 1-3 sentences. Written for someone who did NOT watch you work and will never read your transcript."
    },
    "changed": {
      "type": "array",
      "maxItems": 40,
      "description": "Every file you created, modified or deleted. An empty array if you changed nothing.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["path", "action"],
        "properties": {
          "path": {"type": "string", "description": "Repo-relative path."},
          "action": {"type": "string", "enum": ["created", "modified", "deleted"]},
          "note": {"type": "string", "maxLength": 120, "description": "Only when the path alone does not say what changed."}
        }
      }
    },
    "verified": {
      "type": "array",
      "maxItems": 10,
      "description": "Checks you actually ran, and what they said. A claim in summary with no check here is an unverified claim, and the caller will read it that way.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["check", "result"],
        "properties": {
          "check": {"type": "string", "maxLength": 120, "description": "The exact command or check, for example: go test ./internal/ui/"},
          "result": {"type": "string", "enum": ["pass", "fail", "not_run"]},
          "detail": {"type": "string", "maxLength": 200, "description": "Required when result is fail: the failing case, not the raw output."}
        }
      }
    },
    "followups": {
      "type": "array",
      "maxItems": 5,
      "description": "Work you deliberately did not do. Phrase each so it could be dispatched as its own task; if you cannot, it belongs in summary instead.",
      "items": {"type": "string", "maxLength": 200}
    },
    "needs_decision": {
      "type": "string",
      "maxLength": 300,
      "description": "Only when outcome is blocked and a human must choose. State the choice and the options — not the background."
    }
  }
}`

// FileChange is one file a task touched.
type FileChange struct {
	Path   string `json:"path"`
	Action string `json:"action"`
	Note   string `json:"note,omitempty"`
}

// Check is one verification the child actually ran.
type Check struct {
	Check  string `json:"check"`
	Result string `json:"result"`
	Detail string `json:"detail,omitempty"`
}

// Report is a child's structured account of its task.
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
// compact shape rather than the child's prose. Everything here is bounded: a
// pathological report must not cost more than the work it describes.
func (r Report) Render(taskID, title string) string {
	var b strings.Builder

	head := strings.ToUpper(r.Outcome)
	if head == "" {
		head = "UNKNOWN"
	}
	fmt.Fprintf(&b, "%s %s — %s\n", taskID, title, head)
	b.WriteString(clip(r.Summary, 700))

	if len(r.Changed) > 0 {
		b.WriteString("\nchanged: ")
		parts := make([]string, 0, len(r.Changed))
		for i, c := range r.Changed {
			if i == maxRenderedFiles {
				parts = append(parts, fmt.Sprintf("… +%d more", len(r.Changed)-i))
				break
			}
			p := c.Path + " (" + actionGlyph(c.Action) + ")"
			if c.Note != "" {
				p += " " + clip(c.Note, 120)
			}
			parts = append(parts, p)
		}
		b.WriteString(strings.Join(parts, " · "))
	}

	if len(r.Verified) > 0 {
		b.WriteString("\nverified: ")
		parts := make([]string, 0, len(r.Verified))
		for i, c := range r.Verified {
			if i == maxRenderedChecks {
				parts = append(parts, fmt.Sprintf("… +%d more", len(r.Verified)-i))
				break
			}
			p := clip(c.Check, 120) + " " + c.Result
			if c.Result == "fail" && c.Detail != "" {
				p += " (" + clip(c.Detail, 200) + ")"
			}
			parts = append(parts, p)
		}
		b.WriteString(strings.Join(parts, " · "))
	}

	if r.NeedsDecision != "" {
		b.WriteString("\nneeds a decision: " + clip(r.NeedsDecision, 300))
	}

	for i, f := range r.Followups {
		if i == maxRenderedFollowups {
			fmt.Fprintf(&b, "\nfollowup: … +%d more", len(r.Followups)-i)
			break
		}
		b.WriteString("\nfollowup: " + clip(f, 200))
	}

	// Per-field clips bound each part but say nothing about the whole, and it is
	// the whole that lands in the parent's context and is re-read on every turn
	// thereafter. So the total is capped outright. The first line — id, title,
	// outcome — is written first and therefore always survives.
	return clip(b.String(), maxRenderedRunes)
}

const (
	maxRenderedFiles     = 20
	maxRenderedChecks    = 8
	maxRenderedFollowups = 5

	// ~750 tokens. A dozen of these is still a rounding error against the
	// hundreds of thousands of tokens the children spent producing them.
	// Counted in runes, because that is what clip bounds — splitting a rune to
	// hit a byte target would corrupt the text for no benefit.
	maxRenderedRunes = 3000
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

// clip truncates on a rune boundary. --json-schema's maxLength is advisory
// enough that the renderer, not the schema, has to be what actually bounds the
// parent's context.
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
