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
	ToolAsk      = "AskUserQuestion"
	ToolPlan     = "PresentPlan"
	ToolDispatch = "Dispatch"
	ToolFinish   = "Finish"
)

// Role says which side of the run an `acy mcp` process is serving. It is fixed
// at spawn time by the --role flag, because the two sides need different tools:
// a parent delegates, a child does the work.
//
// Without this a child would inherit --mcp-config from its parent, gain Dispatch
// along with everything else, and be able to spawn children of its own — an
// unbounded tree of processes with no supervision and no budget.
type Role string

const (
	RoleParent Role = "parent"
	RoleChild  Role = "child"
)

// ParseRole defaults to parent: an unrecognised or absent role should produce
// the supervised, fully-featured session rather than silently disarming it.
func ParseRole(s string) Role {
	if Role(s) == RoleChild {
		return RoleChild
	}
	return RoleParent
}

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

// DispatchNotArmed is returned when the parent calls Dispatch before the human
// has armed the run.
//
// It replaces about sixty words of "you cannot leave this phase, there is no
// tool for it, do not look for one" that used to sit in the plan-phase system
// prompt and be re-read on every single turn. The model now learns it at the
// one moment it is relevant — when it tries — and pays nothing for it otherwise.
const DispatchNotArmed = "Dispatch is not available yet: this run has not been armed.\n\n" +
	"A human reads your plan and presses Ctrl+G, and that keystroke is the only thing that starts " +
	"the work. Nothing you can do here will start it. Present your plan and stop."

// DispatchUnavailable is returned when the supervisor has no dispatcher wired
// at all. Like SupervisorGone this fails open, because the caller's turn is
// blocked on this reply.
const DispatchUnavailable = "(delegation is not available in this session, so this task was not run — " +
	"say so plainly rather than reporting it as done)"

const dispatchDescription = "Hand one task to a fresh engineer and block until they report back. " +
	"They have the full toolset — editing, shell, tests — and they begin with no memory of this " +
	"conversation: they cannot see the plan, the user's messages, or any earlier report, so the task " +
	"has to stand on its own. They work, verify, return a structured report, and their session ends. " +
	"Scope each task so its report can honestly say 'completed', and read that report before you " +
	"dispatch the next one."

func toolDefs(role Role) []toolDef {
	defs := []toolDef{
		{Name: ToolAsk, Description: askDescription, InputSchema: json.RawMessage(askSchema)},
	}
	if role == RoleChild {
		// A child asks questions and does work. It does not plan, and it very
		// deliberately does not delegate.
		return defs
	}
	return append(defs,
		toolDef{Name: ToolPlan, Description: planDescription, InputSchema: json.RawMessage(planSchema)},
		toolDef{Name: ToolDispatch, Description: dispatchDescription, InputSchema: json.RawMessage(DispatchSchema)},
		toolDef{Name: ToolFinish, Description: finishDescription, InputSchema: json.RawMessage(finishSchema)},
	)
}

const finishDescription = "End the run: the approved work is done and you have seen it verified. " +
	"This hands control back to the human, who reviews the work in this same session — so say what " +
	"you actually confirmed, not what you assume. If work remains, do not call this."

const finishSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["outcome", "summary"],
  "properties": {
    "outcome": {
      "type": "string",
      "enum": ["completed", "abandoned"],
      "description": "completed — the approved work is done and verified. abandoned — the run is stopping without finishing; say why in summary."
    },
    "summary": {
      "type": "string",
      "maxLength": 2000,
      "description": "What the run achieved and what the human should look at first. Name anything you could not verify."
    }
  }
}`

// FinishRecorded is what Finish returns to the parent. Answered locally by the
// `acy mcp` child: the supervisor reads the outcome out of the ordinary tool_use
// event, exactly as it does for PresentPlan, so there is no socket to wedge on.
//
// This replaces the STATUS sentinel — seven lines of the old auto-run prompt
// asking the model to end every reply with a magic string, read back by a
// substring match that would fire if the model merely *mentioned* it. A tool
// call cannot be accidentally matched, and cannot be missed; it can only not be
// made, and the answer to that is a human, not another billed turn.
const FinishRecorded = "Run marked finished. The human has been handed control and is reviewing " +
	"your work in this session. Stop here; if they want more, they will say so."

// DispatchSchema is the parameter shape the parent sees for Dispatch. It lives
// here, with the rest of the wire contract, and internal/orchestrator decodes
// against it.
//
// The descriptions carry the contract rather than the system prompt, because
// this is the one place the parent reads at the moment it matters — as against
// a prompt, which it re-reads on every turn whether relevant or not. "They begin
// with no memory of this conversation" belongs here in particular: it is the
// fact that makes a vague instruction fail, and this is where instructions get
// written.
const DispatchSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["title", "instruction", "success"],
  "properties": {
    "title": {
      "type": "string",
      "maxLength": 120,
      "description": "A few words naming the task, for the human watching. For example: add the token ledger"
    },
    "instruction": {
      "type": "string",
      "maxLength": 4000,
      "description": "One cohesive deliverable, written to stand alone. The engineer has the full toolset and no memory of your conversation: state the change, where it goes, and constraints. Do not bundle independent work; dispatch the next task only after this report returns."
    },
    "context": {
      "type": "array",
      "maxItems": 20,
      "items": {"type": "string", "maxLength": 300},
      "description": "Paths worth reading first. A shortcut, not a restriction — they can read anything."
    },
    "success": {
      "type": "string",
      "maxLength": 1000,
      "description": "A concrete narrow check proving this one deliverable. Without this they will decide for themselves what done means."
    },
    "budget_usd": {
      "type": "number",
      "description": "Optional spend ceiling for this one task. Omit unless you have a reason; the default is the supervisor's."
    }
  }
}`

func serverInfo() map[string]any {
	return map[string]any{"name": ServerName, "version": version.String()}
}

// --- the supervisor bridge (unix socket) ---

// Request is a tools/call that the `acy mcp` child forwards to the supervisor.
type Request struct {
	Tool      string          `json:"tool"`        // bare name, e.g. AskUserQuestion
	ToolUseID string          `json:"tool_use_id"` // correlates with the tool_use event
	Role      Role            `json:"role"`        // which side of the run is asking
	Args      json.RawMessage `json:"args"`
}

// Answer is the supervisor's reply: text handed back to the model verbatim.
type Answer struct {
	Text string `json:"text"`
}
