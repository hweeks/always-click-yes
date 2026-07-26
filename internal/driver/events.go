// Package driver manages a long-lived `claude` subprocess in stream-json mode:
// it injects user messages on stdin and decodes the newline-delimited JSON event
// stream on stdout into typed Go values.
//
// The event shapes here were derived from live output of claude 2.1.207
// (`-p --input-format stream-json --output-format stream-json --verbose`).
package driver

import (
	"bytes"
	"encoding/json"
)

// EventType values seen on the top-level "type" field.
const (
	TypeSystem    = "system"    // subtype: "init", "hook_started", "hook_response", ...
	TypeAssistant = "assistant" // a model message (thinking / text / tool_use blocks)
	TypeUser      = "user"      // a user message (our injections, or tool_result echoes)
	TypeResult    = "result"    // end of a turn — the idle/done signal
)

// Content block types inside a Message.
const (
	BlockText       = "text"
	BlockThinking   = "thinking"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
)

// ContentBlock is one element of a message's content array.
type ContentBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`
	// thinking
	Thinking string `json:"thinking,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// Message is the "message" object on assistant/user events. Its Content field is
// polymorphic: a plain string (for our simple user injections) or an array of
// ContentBlock (everything the model produces). Use Blocks to normalize.
type Message struct {
	Role    string          `json:"role,omitempty"`
	Model   string          `json:"model,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
}

// Blocks returns the message content normalized to a slice of ContentBlock.
// A string content becomes a single text block.
func (m *Message) Blocks() []ContentBlock {
	if m == nil || len(m.Content) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(m.Content)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var blocks []ContentBlock
		if err := json.Unmarshal(m.Content, &blocks); err == nil {
			return blocks
		}
		return nil
	}
	// Fall back to a bare string.
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return []ContentBlock{{Type: BlockText, Text: s}}
	}
	return nil
}

// Event is a single decoded line of the stream. Only fields relevant to a given
// Type/Subtype are populated; the rest are zero. Raw preserves the original line.
type Event struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	SessionID      string `json:"session_id"`
	Model          string `json:"model"`
	PermissionMode string `json:"permissionMode"`

	// init-only: which credential claude billed this session to. "none" means
	// the claude.ai login (a subscription); anything else names the API key's
	// origin, e.g. "ANTHROPIC_API_KEY".
	APIKeySource string `json:"apiKeySource"`

	Message *Message `json:"message"`

	// result-only fields
	StopReason     string  `json:"stop_reason"`
	TerminalReason string  `json:"terminal_reason"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	IsError        bool    `json:"is_error"`
	Result         string  `json:"result"`
	NumTurns       int     `json:"num_turns"`

	// Usage is this turn's token count. ModelUsage is the process's running
	// total, per model. They mean opposite things — see the Usage doc comment.
	Usage      *Usage                `json:"usage"`
	ModelUsage map[string]ModelUsage `json:"modelUsage"`

	// StructuredOutput is the validated object claude produces when the process
	// was started with --json-schema. Absent otherwise. The string Result holds
	// the same JSON, but this is already parsed and is what callers should read.
	StructuredOutput json.RawMessage `json:"structured_output"`

	Raw json.RawMessage `json:"-"`
}

// Usage is the token count for a single turn.
//
// It is *per turn*, so a running tally must ADD each result's Usage. This is the
// opposite of TotalCostUSD, which is the process's cumulative spend and must be
// assigned. Verified against real transcripts: three turns of one process
// reported cache_read 474322, 306083, 251393 while the process-cumulative
// modelUsage reported 474322, 780405, 1031798.
//
// CacheReadInputTokens dominates every long session and is what acy exists to
// drive down: re-reading an ever-growing context is ~98% of a run's token volume.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// ModelUsage is one model's cumulative usage within a single claude process,
// keyed in Event.ModelUsage by model id. A turn may touch more than one model
// (a small model does some internal work), so this is the only place a
// per-model breakdown exists.
//
// Cumulative, so it is ASSIGNED, never added — and it resets to zero when a
// --resume starts a new process, exactly like TotalCostUSD. Its value here is
// ContextWindow (available nowhere else) and as a cross-check on the per-turn
// accumulator.
type ModelUsage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
	CostUSD                  float64 `json:"costUSD"`
	ContextWindow            int     `json:"contextWindow"`
}

// ContextSize is how much context this turn actually carried: everything the
// request had to present to the model, cached or not. It is a point-in-time
// reading, not a total, and it is the number that reveals a context growing
// without bound.
func (u *Usage) ContextSize() int {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// Decode parses one NDJSON line into an Event, preserving the raw bytes.
func Decode(line []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(line, &e); err != nil {
		return Event{Raw: append(json.RawMessage(nil), line...)}, err
	}
	e.Raw = append(json.RawMessage(nil), line...)
	return e, nil
}

// IsInit reports whether this is the session init event.
func (e Event) IsInit() bool { return e.Type == TypeSystem && e.Subtype == "init" }

// IsTurnEnd reports whether this event marks the end of a turn (idle).
func (e Event) IsTurnEnd() bool { return e.Type == TypeResult }
