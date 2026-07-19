// Package mcp exposes acy's interactive UI to claude as tools.
//
// claude -p's built-in tool registry contains neither AskUserQuestion nor
// ExitPlanMode (see AGENTS.md), so the model has no way to ask the human a
// question and no way to hand a finished plan over. MCP tools *are* added to the
// registry, so acy ships its own MCP server and claude spawns it — the binary is
// its own MCP server, exactly as it is already its own PreToolUse hook.
//
// Two halves, mirroring package gate:
//
//   - stdio.go is the server claude spawns (`acy mcp --socket <p>`): a newline-
//     delimited JSON-RPC loop on stdin/stdout.
//   - server.go/client.go are the bridge back to the supervisor. AskUserQuestion
//     blocks on a unix socket until the TUI's picker produces an answer.
//
// The blocking direction is the whole point, and it is why an answer cannot be
// injected on claude's stdin: a tools/call blocks claude on *this process's*
// JSON-RPC response, not on its own input stream.
package mcp

import (
	"encoding/json"

	"github.com/hweeks/always-click-yes/internal/version"
)

// ServerName is the MCP server name claude registers us under. It determines the
// qualified tool names the model sees: mcp__acy__AskUserQuestion.
const ServerName = "acy"

// Bare tool names, as they appear in a tools/call. claude qualifies them with an
// "mcp__<server>__" prefix in the assistant event stream; ui.baseToolName strips
// it back off.
const (
	ToolAsk  = "AskUserQuestion"
	ToolPlan = "PresentPlan"
)

// Qualified returns the name claude uses for one of our tools in the event stream
// and in --allowedTools.
func Qualified(tool string) string { return "mcp__" + ServerName + "__" + tool }

// defaultProtocolVersion is used only if the client sends none. Normally we echo
// whatever claude asked for; negotiation is lenient (a probe server that answered
// a different version than claude offered was still accepted), but echoing is the
// honest answer and costs nothing.
const defaultProtocolVersion = "2025-06-18"

// JSON-RPC 2.0 error codes we actually use.
const codeMethodNotFound = -32601

// request is an incoming JSON-RPC message. ID is a RawMessage because it may be a
// number or a string, and because *absent* must stay distinguishable from zero: a
// message with no id is a notification and must never be answered.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// textContent is one block of a tool result.
type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolResult is the result payload of a tools/call.
type toolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError"`
}

func okResult(text string) toolResult {
	return toolResult{Content: []textContent{{Type: "text", Text: text}}}
}

func errResult(text string) toolResult {
	return toolResult{Content: []textContent{{Type: "text", Text: text}}, IsError: true}
}

// toolDef is one entry in tools/list.
type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// askSchema must stay in lockstep with ui.parseAsk, which decodes exactly this
// shape — TestAskSchemaMatchesParser holds the two together. A field the model is
// told to send but parseAsk cannot read would silently drop the panel back to a
// plain tool entry, which looks like the tool simply not working.
const askSchema = `{
  "type": "object",
  "properties": {
    "questions": {
      "type": "array",
      "description": "The questions to put to the human, asked one at a time.",
      "items": {
        "type": "object",
        "properties": {
          "question": {"type": "string", "description": "The question, in full."},
          "header": {"type": "string", "description": "A 1-3 word label for the answer, e.g. \"Storage\"."},
          "multiSelect": {"type": "boolean", "description": "Allow more than one option to be chosen."},
          "options": {
            "type": "array",
            "description": "The choices. Two to four works best.",
            "items": {
              "type": "object",
              "properties": {
                "label": {"type": "string", "description": "The choice itself, kept short."},
                "description": {"type": "string", "description": "What picking this would mean."}
              },
              "required": ["label"]
            }
          }
        },
        "required": ["question", "header", "options"]
      }
    }
  },
  "required": ["questions"]
}`

const planSchema = `{
  "type": "object",
  "properties": {
    "plan": {"type": "string", "description": "The finished plan, as markdown."}
  },
  "required": ["plan"]
}`

const askDescription = "Ask the human a multiple-choice question and block until they answer. " +
	"Use this for any genuine fork in the road — an ambiguous requirement, a choice between two " +
	"approaches — rather than asking in prose, which surfaces no prompt and which nobody may be " +
	"watching for. In AUTO-RUN the human is likely away: a question auto-skips after the countdown " +
	"and you are told to use your best judgment, so do not ask what you could reasonably decide."

const planDescription = "Hand a finished plan to the human for approval. This does NOT start the " +
	"work and does NOT exit the planning phase — only the human can do that. Call it once, when the " +
	"plan is complete, and then stop."

// PlanRecorded is what PresentPlan returns to the model. It is answered locally by
// the `acy mcp` child: the supervisor reads the plan text out of the ordinary
// tool_use event, so there is nothing to wait for and no dead socket can wedge the
// turn.
//
// This string carries the weight of the whole plan phase. The model has no
// ExitPlanMode and cannot arm the run, and unless it is told so plainly it will
// either keep reaching for a tool that does not exist or ask "shall I proceed?"
// into a void.
const PlanRecorded = "Plan recorded and shown to the human.\n\n" +
	"You have NOT exited the planning phase — you cannot; only the human can, by pressing Ctrl+G to " +
	"arm the run. Stop here. Do not begin implementing, do not call this tool again, and do not ask " +
	"whether to proceed: no reply is coming. If they want changes, they will say so."

// SupervisorGone is returned when the ask bridge is unreachable. The gate fails
// *closed* (deny), because a tool would otherwise run unapproved. This fails
// *open*, because claude is blocked on our reply and the alternative to a useless
// answer is a permanently hung turn.
const SupervisorGone = "(the supervisor is unavailable, so this question could not be put to " +
	"anyone — proceed with your best judgment)"

func toolDefs() []toolDef {
	return []toolDef{
		{Name: ToolAsk, Description: askDescription, InputSchema: json.RawMessage(askSchema)},
		{Name: ToolPlan, Description: planDescription, InputSchema: json.RawMessage(planSchema)},
	}
}

func serverInfo() map[string]any {
	return map[string]any{"name": ServerName, "version": version.String()}
}

// --- the supervisor bridge (unix socket) ---

// Request is a tools/call that the `acy mcp` child forwards to the supervisor.
type Request struct {
	Tool      string          `json:"tool"`        // bare name, e.g. AskUserQuestion
	ToolUseID string          `json:"tool_use_id"` // correlates with the tool_use event
	Args      json.RawMessage `json:"args"`
}

// Answer is the supervisor's reply: text handed back to the model verbatim.
type Answer struct {
	Text string `json:"text"`
}
