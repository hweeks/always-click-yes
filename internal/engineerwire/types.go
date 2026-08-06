// Package engineerwire is the wire contract for "arch mode": an architect acy
// process driving detached engineer acy processes over NDJSON — one JSON
// object per line, stdio locally and over ssh remotely alike. This package is
// deliberately just the contract and its journal: no CLI, no daemon, no git
// logic. See docs/engineer-protocol.md for the full protocol writeup.
//
// Every message carries a "type" field naming which of the seven structs
// below it is. Inbound messages (architect -> engineer) are Spec, Answer and
// Cancel. Outbound messages (engineer -> architect) are Hello, Event,
// Question and Result, and additionally carry Seq and At — see Journal.
package engineerwire

import "github.com/hweeks/always-click-yes/internal/state"

// Type names the kind of message on the wire.
type Type string

const (
	TypeSpec     Type = "spec"
	TypeAnswer   Type = "answer"
	TypeCancel   Type = "cancel"
	TypeHello    Type = "hello"
	TypeEvent    Type = "event"
	TypeQuestion Type = "question"
	TypeResult   Type = "result"
)

// ProtocolVersion is the wire protocol's major version, reported in every
// Hello. An architect refuses to attach to an engineer reporting a different
// one rather than guess at a shape it was not built to read.
const ProtocolVersion = 1

// --- inbound: architect -> engineer ---

// Spec is the one task assignment an engineer process is spawned with. It is
// the whole of what the engineer knows about its job; there is no earlier
// conversation to fall back on.
type Spec struct {
	Type Type `json:"type"`

	Ticket  string `json:"ticket"`
	Title   string `json:"title"`
	Brief   string `json:"brief"`
	Success string `json:"success"`

	BaseBranch string `json:"base_branch"`
	Branch     string `json:"branch"`

	Model       string `json:"model,omitempty"`
	ChildModel  string `json:"child_model,omitempty"`
	ChildEffort string `json:"child_effort,omitempty"`

	BudgetUSD    float64 `json:"budget_usd,omitempty"`
	DeadmanHours float64 `json:"deadman_hours,omitempty"`

	VerifyCommands       []string `json:"verify_commands,omitempty"`
	VerifyTimeoutSeconds int      `json:"verify_timeout_seconds,omitempty"`
}

// Answer resolves one Question, matched by QuestionID.
type Answer struct {
	Type Type `json:"type"`

	QuestionID string `json:"question_id"`
	Text       string `json:"text"`
}

// Cancel tells the engineer to stop.
type Cancel struct {
	Type Type `json:"type"`

	Reason string `json:"reason"`
}

// --- outbound: engineer -> architect ---

// Hello is the first message an engineer ever sends. It is always Seq 1 —
// Journal.Append assigns seq in order, and Hello is the first thing appended.
type Hello struct {
	Type Type   `json:"type"`
	Seq  int64  `json:"seq"`
	At   string `json:"at"` // RFC3339

	EngineerID      string `json:"engineer_id"`
	ProtocolVersion int    `json:"protocol_version"`
	ACYVersion      string `json:"acy_version"`
	Host            string `json:"host"`
	PID             int    `json:"pid"`
}

// EventKind narrows Event.Kind to the set the architect understands.
type EventKind string

const (
	EventPhase       EventKind = "phase"
	EventTaskStarted EventKind = "task_started"
	EventTaskReport  EventKind = "task_report"
	EventCost        EventKind = "cost"
	EventLog         EventKind = "log"
)

// Event is one narrated step of the engineer's progress.
type Event struct {
	Type Type   `json:"type"`
	Seq  int64  `json:"seq"`
	At   string `json:"at"` // RFC3339

	Kind    EventKind    `json:"kind"`
	Text    string       `json:"text,omitempty"`
	CostUSD float64      `json:"cost_usd,omitempty"`
	Tokens  state.Tokens `json:"tokens,omitzero"`
}

// AskOption is one choice offered by an AskQuestion.
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// AskQuestion is one question put to the architect. Its shape mirrors
// internal/mcp/protocol.go's askSchema field-for-field, so an architect can
// hand a Question straight to the same AskUserQuestion rendering and answer
// path it already has, with no translation layer in between.
type AskQuestion struct {
	Question    string      `json:"question"`
	Header      string      `json:"header"`
	MultiSelect bool        `json:"multiSelect,omitempty"`
	Options     []AskOption `json:"options"`
}

// Question is an engineer blocking on the architect for a decision.
type Question struct {
	Type Type   `json:"type"`
	Seq  int64  `json:"seq"`
	At   string `json:"at"` // RFC3339

	QuestionID string        `json:"question_id"`
	Questions  []AskQuestion `json:"questions"`
}

// VerifyStatus narrows VerifyCheck.Status to the set an architect understands.
type VerifyStatus string

const (
	VerifyPassed  VerifyStatus = "passed"  // ran, exit 0
	VerifyFailed  VerifyStatus = "failed"  // ran, non-zero exit
	VerifySkipped VerifyStatus = "skipped" // binary not installed on this host — a fact, never a failure
	VerifyTimeout VerifyStatus = "timeout" // per-command deadline elapsed
	VerifyError   VerifyStatus = "error"   // could not be launched/run for any other reason
)

// VerifyCheck is one command acy itself ran in the worktree to check the
// engineer's work, and what happened when it did.
type VerifyCheck struct {
	Name       string       `json:"name"`
	Argv       []string     `json:"argv"`
	Status     VerifyStatus `json:"status"`
	ExitCode   int          `json:"exit_code"`
	Output     string       `json:"output,omitempty"`
	Truncated  bool         `json:"truncated,omitempty"`
	DurationMS int64        `json:"duration_ms"`
}

// Result is the engineer's final report. Nothing follows it.
type Result struct {
	Type Type   `json:"type"`
	Seq  int64  `json:"seq"`
	At   string `json:"at"` // RFC3339

	Outcome string `json:"outcome"`
	Summary string `json:"summary"`
	Branch  string `json:"branch,omitempty"`
	PRURL   string `json:"pr_url,omitempty"`

	CostUSD float64      `json:"cost_usd,omitempty"`
	Tokens  state.Tokens `json:"tokens,omitzero"`
	Files   []string     `json:"files,omitempty"`

	// Verification is machine-collected evidence: the commands acy itself ran
	// in the worktree after the session's own verdict, never the model's
	// report of having run them.
	Verification []VerifyCheck `json:"verification,omitempty"`
}
