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

	ToolLaunchEngineer = "LaunchEngineer"
	ToolAwait          = "Await"
	ToolAnswerEngineer = "AnswerEngineer"
	ToolFleetStatus    = "FleetStatus"

	ToolReadTickets  = "ReadTickets"
	ToolUpdateTicket = "UpdateTicket"
	ToolCreateTicket = "CreateTicket"
)

// Role says which side of the run an `acy mcp` process is serving. It is fixed
// at spawn time by the --role flag, because the different sides need different
// tools: a parent delegates to local children, an architect delegates to
// remote engineer instances as well as local children, and a child does the
// work.
//
// Without this a child would inherit --mcp-config from its parent, gain Dispatch
// along with everything else, and be able to spawn children of its own — an
// unbounded tree of processes with no supervision and no budget.
type Role string

const (
	RoleParent    Role = "parent"
	RoleChild     Role = "child"
	RoleArchitect Role = "architect"
)

// ParseRole defaults to parent: an unrecognised or absent role should produce
// the supervised, fully-featured session rather than silently disarming it.
func ParseRole(s string) Role {
	switch Role(s) {
	case RoleChild:
		return RoleChild
	case RoleArchitect:
		return RoleArchitect
	default:
		return RoleParent
	}
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

const dispatchDescription = "Hand one task to a fresh worker session and block until they report back. " +
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
	defs = append(defs,
		toolDef{Name: ToolPlan, Description: planDescription, InputSchema: json.RawMessage(planSchema)},
		toolDef{Name: ToolDispatch, Description: dispatchDescription, InputSchema: json.RawMessage(DispatchSchema)},
		toolDef{Name: ToolFinish, Description: finishDescription, InputSchema: json.RawMessage(finishSchema)},
	)
	if role != RoleArchitect {
		// A parent delegates to local children only; the fleet tools below are
		// the architect's alone.
		return defs
	}
	return append(defs,
		toolDef{Name: ToolLaunchEngineer, Description: launchEngineerDescription, InputSchema: json.RawMessage(launchEngineerSchema)},
		toolDef{Name: ToolAwait, Description: awaitDescription, InputSchema: json.RawMessage(awaitSchema)},
		toolDef{Name: ToolAnswerEngineer, Description: answerEngineerDescription, InputSchema: json.RawMessage(answerEngineerSchema)},
		toolDef{Name: ToolFleetStatus, Description: fleetStatusDescription, InputSchema: json.RawMessage(fleetStatusSchema)},
		toolDef{Name: ToolReadTickets, Description: readTicketsDescription, InputSchema: json.RawMessage(readTicketsSchema)},
		toolDef{Name: ToolUpdateTicket, Description: updateTicketDescription, InputSchema: json.RawMessage(updateTicketSchema)},
		toolDef{Name: ToolCreateTicket, Description: createTicketDescription, InputSchema: json.RawMessage(createTicketSchema)},
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

// --- the architect's fleet tools ---
//
// These four are advertised only for RoleArchitect. An architect does not edit
// code itself — it delegates whole tickets to remote engineer instances (each a
// fresh acy run on its own machine, in its own worktree) the same way a parent
// delegates a task to a local child, except an engineer is unattended, takes
// unbounded wall-clock, and reports back through Await rather than a blocking
// call.

const launchEngineerDescription = "Launch a remote engineer on a ticket. Non-blocking — returns " +
	"immediately with the engineer's id, host and branch; the engineer works unattended in its own " +
	"worktree and ends by opening a PR. Launch up to capacity, then Await. One ticket per engineer."

// launchEngineerSchema is the parameter shape the architect sees for
// LaunchEngineer. "brief" carries the same weight DispatchSchema's
// "instruction" does, for the same reason: the engineer is a fresh acy
// instance on another machine with no memory of this conversation, and plans
// its own subtasks from this brief alone — a vague one fails silently, days
// later, on a machine nobody is watching.
const launchEngineerSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["ticket", "title", "brief", "success"],
  "properties": {
    "ticket": {
      "type": "string",
      "maxLength": 64,
      "description": "A short identifier for this unit of work, e.g. a ticket key. One ticket per engineer."
    },
    "title": {
      "type": "string",
      "maxLength": 120,
      "description": "A few words naming the task, for the human watching. For example: add the token ledger"
    },
    "brief": {
      "type": "string",
      "maxLength": 8000,
      "description": "The full standalone work order. The engineer is a fresh acy instance on another machine with no memory of this conversation, and plans its own subtasks from this brief alone — state the change, where it goes, and every constraint that matters."
    },
    "success": {
      "type": "string",
      "maxLength": 1000,
      "description": "A concrete check proving the ticket is done. Without this the engineer decides for itself what done means."
    },
    "host": {
      "type": "string",
      "description": "Pin this engineer to a named fleet host. Omit to let the fleet auto-place it."
    },
    "budget_usd": {
      "type": "number",
      "description": "Optional spend ceiling for this engineer. Omit unless you have a reason; the default is the fleet's."
    }
  }
}`

const awaitDescription = "Block until the next fleet event and return it — an engineer's result " +
	"(with PR URL and cost), an escalated question (answer it with AnswerEngineer), a PR merge/close, " +
	"or a reconnection notice. This is your main loop: launch to capacity, Await, react, repeat. Do " +
	"not poll FleetStatus in a loop; Await is the cheap wait."

const awaitSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {}
}`

const answerEngineerDescription = "Answer an escalated question from the plan and tickets; the " +
	"engineer is blocked on it."

const answerEngineerSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["engineer_id", "question_id", "answer"],
  "properties": {
    "engineer_id": {
      "type": "string",
      "description": "The engineer this question came from."
    },
    "question_id": {
      "type": "string",
      "description": "The question being answered, from the escalation Await returned."
    },
    "answer": {
      "type": "string",
      "maxLength": 2000,
      "description": "The answer. The engineer is blocked on it and resumes as soon as it arrives."
    }
  }
}`

const fleetStatusDescription = "A snapshot of every engineer (state, host, branch, PR, cost) and " +
	"host capacity; for taking stock, not for waiting — use Await to wait."

const fleetStatusSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {}
}`

// LaunchNotArmed is returned when the architect calls LaunchEngineer before the
// human has armed the run. Mirrors DispatchNotArmed: launching an engineer
// starts real unattended work on a real machine, and only a human's Ctrl+G
// starts it.
const LaunchNotArmed = "LaunchEngineer is not available yet: this run has not been armed.\n\n" +
	"Launching engineers starts real, unattended work on real machines. A human reads your plan and " +
	"presses Ctrl+G, and that keystroke is the only thing that starts it. Present your plan and stop."

// AwaitNothingRunning is returned when the architect calls Await with nothing
// that could ever produce an event: no engineer running and no PR open.
// Without this the call blocks forever on a fleet that will never speak.
const AwaitNothingRunning = "Await has nothing to wait for: no engineer is running and no PR is " +
	"open.\n\nBlocking here would wait forever. Launch an engineer first, or call Finish if there is " +
	"nothing left to do."

// FleetUnavailable is returned when the session has no fleet wired at all —
// no fleet section in .acy.json, or acy was not started in architect mode.
// Like SupervisorGone and DispatchUnavailable this fails open: the caller's
// turn is blocked on this reply, and the honest answer is to say plainly that
// no engineers exist rather than let the model believe a fleet is out there.
const FleetUnavailable = "(this session has no fleet configured — .acy.json has no fleet section, " +
	"or acy was not started in architect mode — so no engineers exist in this session; say so " +
	"plainly rather than pretending they do)"

// --- the architect's ticket board ---
//
// The ticket board is the run's memory: a markdown file per ticket under
// .acy/tickets, in the repo itself rather than in acy's own state directory,
// so it survives a resumed run and travels with a clone or a PR diff. These
// three tools are the architect's only way to read or change it — advertised
// for RoleArchitect alone, the same as the fleet tools above.

const createTicketDescription = "Turn the approved plan into the board: one ticket per PR-sized unit of " +
	"work, called before launching any engineers. The brief becomes the engineer's whole work order — a " +
	"fresh instance with no memory of this conversation plans its own subtasks from it alone, so write it " +
	"to stand completely on its own. A new ticket starts as todo."

const createTicketSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["id", "title", "brief"],
  "properties": {
    "id": {
      "type": "string",
      "maxLength": 32,
      "pattern": "^[a-z0-9-]+$",
      "description": "A short identifier for this ticket, lowercase letters, digits and dashes only, e.g. \"add-token-ledger\"."
    },
    "title": {
      "type": "string",
      "maxLength": 120,
      "description": "A few words naming the task, for the human watching. For example: add the token ledger"
    },
    "brief": {
      "type": "string",
      "maxLength": 8000,
      "description": "The full standalone work order. The engineer that eventually takes this ticket has no memory of this conversation and plans its own subtasks from this brief alone — state the change, where it goes, and every constraint that matters."
    },
    "depends_on": {
      "type": "array",
      "maxItems": 10,
      "items": {"type": "string"},
      "description": "Ids of tickets that must merge before this one can start. Optional."
    },
    "stack_on": {
      "type": "string",
      "description": "The id of the single ticket this one's branch stacks on — not an array like depends_on, since only one ticket may claim a given parent. Use this when this ticket's branch can sit on top of that ticket's still-open PR and land together in the same stack; use depends_on instead when it must wait for that ticket to merge first. Optional."
    }
  }
}`

const readTicketsDescription = "The ticket board under .acy/tickets: every ticket with id, title, " +
	"status, branch, PR, dependencies, and brief. Read it at the start of a run and after every merge."

const readTicketsSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {}
}`

const updateTicketDescription = "Record a ticket's new state the moment it changes — in-progress when " +
	"its engineer launches (record the branch), in-review when its PR opens (record the PR url), merged " +
	"when the human merges, blocked with a note when stuck. Writes and commits the ticket file " +
	"deterministically; you never edit tickets by hand."

// updateTicketSchema deliberately has no stack_on: a stack's shape is decided
// once, when the board is written by CreateTicket, not renegotiated mid-run —
// letting it move later would let the board disagree with branches that
// already exist on top of each other.
const updateTicketSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["id", "status"],
  "properties": {
    "id": {
      "type": "string",
      "description": "The ticket's id, as ReadTickets reported it."
    },
    "status": {
      "type": "string",
      "enum": ["todo", "in-progress", "in-review", "merged", "blocked"],
      "description": "The ticket's new status."
    },
    "note": {
      "type": "string",
      "maxLength": 1000,
      "description": "Optional. Appended to the ticket's log, timestamped — say why, especially for blocked."
    },
    "branch": {
      "type": "string",
      "maxLength": 100,
      "description": "Optional. The branch the engineer is working on — record it when the engineer launches. Omit to leave it unchanged."
    },
    "pr": {
      "type": "string",
      "maxLength": 300,
      "description": "Optional. The PR URL — record it when the PR opens. Omit to leave it unchanged."
    }
  }
}`

// TicketsUnavailable is returned when the session has no ticket store wired at
// all — this is not an arch run. Like FleetUnavailable this fails open: the
// caller's turn is blocked on this reply.
const TicketsUnavailable = "(this session has no ticket store — it is not an arch run — so there is " +
	"no board to read or update; say so plainly rather than pretending one exists)"

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
