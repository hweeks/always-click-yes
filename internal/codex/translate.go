package codex

import (
	"encoding/json"
	"strings"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/mcp"
)

// handleNotification routes a server->client notification (method+params, no
// id) to whatever acy translation exists for it. Most of codex's observed
// notification methods have none — thread/status/changed, mcpServer/*,
// account/rateLimits/updated, turn/started, serverRequest/resolved and the
// rest fall through untouched, the same way claude driver's Decode only
// populates the fields a given type/subtype actually carries.
func (d *Driver) handleNotification(method string, params json.RawMessage) {
	switch method {
	case "thread/started":
		// Carries the same thread id/model as the thread/start (or
		// thread/resume) response, which is what actually emits the one init
		// event — see applyResponseSideEffects in rpc.go. Acting on this
		// notification too would double-emit it.
	case "item/started":
		// Nothing to surface until the item completes.
	case "item/agentMessage/delta":
		// acy's UI ingests whole messages, not partial text; emitting an event
		// per delta would turn the transcript into a stream of fragments
		// instead of one message. Wait for item/completed instead.
	case "item/completed":
		d.handleItemCompleted(params)
	case "thread/tokenUsage/updated":
		d.handleTokenUsage(params)
	case "turn/completed":
		d.handleTurnCompleted(params)
	}
}

// itemHead reads only the discriminator every item shape shares.
type itemHead struct {
	Type string `json:"type"`
}

type itemCompletedParams struct {
	Item json.RawMessage `json:"item"`
}

type agentMessageItem struct {
	Text string `json:"text"`
}

// reasoningItem's summary is VERIFIED LIVE (docs/codex-fixtures/reasoning-summary.jsonl,
// captured with `-c model_reasoning_summary="detailed"` and effort "high" —
// the account's default config never populates it, which is why the original
// recon fixture's reasoning items were always empty): each summary entry is a
// bare JSON string, e.g. "summary":["**Verifying primality of 1237**"], not
// an object with a "text" field as this package originally guessed. content
// was still empty in that capture, so its element shape remains unconfirmed.
//
// reasoningChunk's UnmarshalJSON accepts either shape — a bare string (the
// confirmed one) or an object carrying "text" (the original guess, kept as a
// fallback for content or a future codex version) — and never itself returns
// an error. That matters beyond tolerance: a struct-typed
// []reasoningChunk{Text string `json:"text"`} failed json.Unmarshal outright
// on the real "summary":["..."] shape, which made handleItemCompleted drop
// the ENTIRE item silently (see its own error-logging branch) rather than
// degrade to the "empty thinking block" this comment used to promise — the
// promise was wrong in practice, not just in shape.
type reasoningItem struct {
	Summary []reasoningChunk `json:"summary"`
	Content []reasoningChunk `json:"content"`
}

type reasoningChunk struct {
	Text string
}

// UnmarshalJSON never fails: an unrecognized chunk shape decodes to an empty
// Text rather than aborting the whole reasoning item's decode, which is the
// property that matters most here — see reasoningItem's own comment.
func (c *reasoningChunk) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		c.Text = s
		return nil
	}
	var obj struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(b, &obj) // best-effort; an unrecognized shape just yields empty text
	c.Text = obj.Text
	return nil
}

func reasoningText(it reasoningItem) string {
	var b strings.Builder
	for _, c := range it.Content {
		b.WriteString(c.Text)
	}
	for _, c := range it.Summary {
		b.WriteString(c.Text)
	}
	return b.String()
}

// mcpToolCallItem is codex's representation of a completed MCP tool call —
// VERIFIED FROM SCHEMA, not a live capture: `codex app-server
// generate-json-schema --out <dir> --experimental` (codex-cli 0.147.0) is
// free — no model call, no cost (docs/codex-cli-findings.md:237) — and its
// ServerNotification.json defines ThreadItem as a oneOf union whose
// "mcpToolCall" variant (title McpToolCallThreadItem) is a THIRD, distinct
// item type alongside "agentMessage"/"reasoning"/"commandExecution": codex
// does not fold an MCP call into a commandExecution item, it emits its own.
// This is the piece the original recon never saw (docs/codex-cli-findings.md
// §5 says outright it "could not test with a real MCP server"), and it is why
// PresentPlan/Finish calls vanished before this file grew this case — they
// fell through handleItemCompleted's default branch under the wrong type
// string entirely.
//
// Every field below is the schema's own name, required-ness, and type:
// id/server/tool/status are required strings; arguments/result/error are
// typed `true` (arbitrary JSON) or nullable refs in the schema, not narrowed
// further by the generator, which is why arguments is decoded as raw JSON
// here rather than a typed struct — it's genuinely the call's argument
// object verbatim (e.g. {"plan": "..."} for PresentPlan), the same shape a
// tools/call request carries on acy's own MCP server (internal/mcp/stdio.go).
// status is McpToolCallStatus, an enum of exactly "inProgress"/"completed"/
// "failed" — but since this type only ever arrives via item/completed (whose
// own doc comment ties completedAtMs to "when this item lifecycle
// completed"), "inProgress" should never actually appear here.
type mcpToolCallItem struct {
	ID        string             `json:"id"`
	Server    string             `json:"server"`
	Tool      string             `json:"tool"`
	Arguments json.RawMessage    `json:"arguments"`
	Status    string             `json:"status"`
	Result    *mcpToolCallResult `json:"result"`
	Error     *mcpToolCallError  `json:"error"`
}

// mcpToolCallResult is McpToolCallResult from the schema. Content is left as
// raw JSON rather than a typed slice: the schema itself types it `items:
// true`, i.e. an unconstrained array — in practice the standard MCP
// content-block shape ([{"type":"text","text":"..."}]), which is exactly what
// ui.rawText already knows how to read (it tries a []ContentText decode
// before falling back to the raw bytes), and exactly what acy's own MCP
// server hands back (internal/mcp/stdio.go's toolResult).
type mcpToolCallResult struct {
	Content json.RawMessage `json:"content"`
}

// mcpToolCallError is McpToolCallError from the schema: a single required
// "message" string, nothing else.
type mcpToolCallError struct {
	Message string `json:"message"`
}

type commandExecutionItem struct {
	ID               string  `json:"id"`
	Command          string  `json:"command"`
	Cwd              string  `json:"cwd"`
	Status           string  `json:"status"`
	AggregatedOutput *string `json:"aggregatedOutput"`
	ExitCode         *int    `json:"exitCode"`
}

// handleItemCompleted translates one item/completed notification into zero,
// one, or two driver.Events depending on the item's type.
func (d *Driver) handleItemCompleted(raw json.RawMessage) {
	var p itemCompletedParams
	if err := json.Unmarshal(raw, &p); err != nil {
		alog.Printf("codex: decode item/completed: %v", err)
		return
	}
	var head itemHead
	if err := json.Unmarshal(p.Item, &head); err != nil {
		alog.Printf("codex: decode item/completed head: %v", err)
		return
	}

	switch head.Type {
	case "agentMessage":
		var it agentMessageItem
		if err := json.Unmarshal(p.Item, &it); err != nil {
			alog.Printf("codex: decode agentMessage item: %v", err)
			return
		}
		d.emitAssistantBlock(driver.ContentBlock{Type: driver.BlockText, Text: it.Text})
	case "reasoning":
		var it reasoningItem
		if err := json.Unmarshal(p.Item, &it); err != nil {
			alog.Printf("codex: decode reasoning item: %v", err)
			return
		}
		d.emitAssistantBlock(driver.ContentBlock{Type: driver.BlockThinking, Thinking: reasoningText(it)})
	case "commandExecution":
		var it commandExecutionItem
		if err := json.Unmarshal(p.Item, &it); err != nil {
			alog.Printf("codex: decode commandExecution item: %v", err)
			return
		}
		d.emitCommandExecution(it)
	case "mcpToolCall":
		var it mcpToolCallItem
		if err := json.Unmarshal(p.Item, &it); err != nil {
			alog.Printf("codex: decode mcpToolCall item: %v", err)
			return
		}
		d.emitMcpToolCall(it)
	case "userMessage":
		// Our own injected input, echoed back by codex. claude's driver never
		// echoes an injected turn either (AGENTS.md's stream-json notes) —
		// drop it so the transcript isn't doubled.
	default:
		alog.Printf("codex: unhandled item/completed type %q", head.Type)
	}
}

// emitAssistantBlock wraps a single content block in a one-element array (the
// shape driver.Message.Blocks expects) and emits it as an assistant event.
func (d *Driver) emitAssistantBlock(block driver.ContentBlock) {
	content, err := json.Marshal([]driver.ContentBlock{block})
	if err != nil {
		alog.Printf("codex: marshal content block: %v", err)
		return
	}
	d.mu.Lock()
	threadID := d.threadID
	d.mu.Unlock()
	d.events <- driver.Event{
		Type:      driver.TypeAssistant,
		SessionID: threadID,
		Message:   &driver.Message{Role: "assistant", Content: content},
	}
}

// emitCommandExecution emits the tool_use/tool_result pair a single completed
// commandExecution item implies: unlike claude (whose tool_use and tool_result
// arrive as separate stream events), codex reports a shell command's
// invocation and its outcome in the same item/completed notification.
func (d *Driver) emitCommandExecution(it commandExecutionItem) {
	inputJSON, err := json.Marshal(map[string]string{"command": it.Command, "cwd": it.Cwd})
	if err != nil {
		alog.Printf("codex: marshal commandExecution input: %v", err)
		return
	}
	d.emitAssistantBlock(driver.ContentBlock{
		Type:  driver.BlockToolUse,
		ID:    it.ID,
		Name:  "shell",
		Input: inputJSON,
	})

	output := ""
	if it.AggregatedOutput != nil {
		output = *it.AggregatedOutput
	}
	// A declined/cancelled/failed execution never reaches "completed" and
	// never gets a real exit code (the fixture's declined case has both
	// aggregatedOutput and exitCode null) — either signal alone is enough to
	// mark the tool_result an error.
	isError := it.Status != "completed" || (it.ExitCode != nil && *it.ExitCode != 0)

	contentJSON, err := json.Marshal(output)
	if err != nil {
		alog.Printf("codex: marshal commandExecution output: %v", err)
		return
	}
	resultContent, err := json.Marshal([]driver.ContentBlock{{
		Type:      driver.BlockToolResult,
		ToolUseID: it.ID,
		Content:   contentJSON,
		IsError:   isError,
	}})
	if err != nil {
		alog.Printf("codex: marshal tool_result block: %v", err)
		return
	}

	d.mu.Lock()
	threadID := d.threadID
	d.mu.Unlock()
	d.events <- driver.Event{
		Type:      driver.TypeUser,
		SessionID: threadID,
		Message:   &driver.Message{Role: "user", Content: resultContent},
	}
}

// emitMcpToolCall emits the tool_use/tool_result pair a completed mcpToolCall
// item implies, mirroring emitCommandExecution's shape: both arrive as one
// item/completed notification, so both are translated into a call and its
// outcome together rather than as two separately-timed events the way
// claude's driver reports a tool_use/tool_result pair.
//
// This is the fix for the defect this package existed to close: PresentPlan
// and Finish (internal/cli/mcp.go) are answered locally by the `acy mcp`
// child rather than over the supervisor socket, precisely because the
// supervisor/UI reads their content out of the ordinary tool_use event
// instead (see ui.ingestToolUse's own comment) — so unless codex's
// "mcpToolCall" item type gets translated into that same tool_use/tool_result
// shape, those two tools are answered correctly by the MCP server and then
// vanish, which is exactly what was observed live: PresentPlan calls
// succeeding (isError false, "Plan recorded...") while the human saw no plan
// at all.
//
// The emitted Name MUST carry codex's server name mcp__<server>__<tool>
// qualification the way claude's own event stream does — ui.baseToolName
// strips exactly that prefix back off to match PresentPlan/Finish/etc. by
// their bare name. It is built from the item's own "server" field, NOT
// hardcoded to acy's name: acy is not necessarily the only MCP server on a
// codex thread (`codex mcp add` writes servers into the user's own
// ~/.codex/config.toml, and thread/start's inline config overlays that
// rather than replacing it), so a tool call from some other configured
// server must keep that server's name rather than being relabelled as acy's
// — ui.ingestToolUse dispatches on the bare name baseToolName strips this
// prefix down to, so mislabeling would let a third-party tool named e.g.
// "Finish" silently drive acy's own end-the-run path. internal/mcp imports
// only alog and version — nothing that reaches back into internal/codex — so
// importing it here for mcp.QualifiedServer is not a cycle.
func (d *Driver) emitMcpToolCall(it mcpToolCallItem) {
	input := it.Arguments
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	// server is required by the schema, but an empty value here would format
	// as the malformed "mcp____tool" rather than a real qualification — fall
	// back to acy's own name, today's (pre-fix) behavior, instead.
	server := it.Server
	if server == "" {
		server = mcp.ServerName
	}
	d.emitAssistantBlock(driver.ContentBlock{
		Type:  driver.BlockToolUse,
		ID:    it.ID,
		Name:  mcp.QualifiedServer(server, it.Tool),
		Input: input,
	})

	// A failed call still gets a result — the model needs to see it failed —
	// so it falls back to the error message when there is no result content,
	// the same "either signal alone is enough" logic emitCommandExecution
	// uses for a declined/failed shell command.
	isError := it.Status != "completed" || it.Error != nil
	var content json.RawMessage
	switch {
	case it.Result != nil && len(it.Result.Content) > 0:
		content = it.Result.Content
	case it.Error != nil:
		msg, err := json.Marshal(it.Error.Message)
		if err != nil {
			alog.Printf("codex: marshal mcpToolCall error message: %v", err)
			return
		}
		content = msg
	default:
		content = json.RawMessage(`""`)
	}

	resultContent, err := json.Marshal([]driver.ContentBlock{{
		Type:      driver.BlockToolResult,
		ToolUseID: it.ID,
		Content:   content,
		IsError:   isError,
	}})
	if err != nil {
		alog.Printf("codex: marshal mcpToolCall tool_result block: %v", err)
		return
	}

	d.mu.Lock()
	threadID := d.threadID
	d.mu.Unlock()
	d.events <- driver.Event{
		Type:      driver.TypeUser,
		SessionID: threadID,
		Message:   &driver.Message{Role: "user", Content: resultContent},
	}
}

type tokenUsageHalf struct {
	InputTokens           int `json:"inputTokens"`
	OutputTokens          int `json:"outputTokens"`
	CachedInputTokens     int `json:"cachedInputTokens"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
}

type tokenUsageParams struct {
	TokenUsage struct {
		Total              tokenUsageHalf `json:"total"`
		Last               tokenUsageHalf `json:"last"`
		ModelContextWindow int            `json:"modelContextWindow"`
	} `json:"tokenUsage"`
}

// handleTokenUsage accumulates one thread/tokenUsage/updated notification.
//
// tokenUsage.last is this turn's *incremental* usage since the previous
// update within the same turn (verified against the fixture: two updates in
// turn 1 report last=16111 then last=16149, and 16111+16149=32260 exactly
// matches that update's own cumulative total — the very first usage ever
// reported on the thread). So it accumulates by ADDING every update seen
// since the last turn/completed. tokenUsage.total is the whole thread's
// cumulative count as of this update, so it is ASSIGNED (overwritten)
// outright. Getting these two backwards is silent — both keep producing
// plausible numbers — which is exactly the mixup driver/events.go's own scar
// comment describes for claude's usage/modelUsage pair.
func (d *Driver) handleTokenUsage(raw json.RawMessage) {
	var p tokenUsageParams
	if err := json.Unmarshal(raw, &p); err != nil {
		alog.Printf("codex: decode thread/tokenUsage/updated: %v", err)
		return
	}

	d.usageMu.Lock()
	d.turnUsage.InputTokens += p.TokenUsage.Last.InputTokens
	d.turnUsage.OutputTokens += p.TokenUsage.Last.OutputTokens
	d.turnUsage.CacheReadInputTokens += p.TokenUsage.Last.CachedInputTokens
	d.turnUsage.CacheCreationInputTokens += p.TokenUsage.Last.CacheWriteInputTokens
	d.latestModelUsage = driver.ModelUsage{
		InputTokens:              p.TokenUsage.Total.InputTokens,
		OutputTokens:             p.TokenUsage.Total.OutputTokens,
		CacheReadInputTokens:     p.TokenUsage.Total.CachedInputTokens,
		CacheCreationInputTokens: p.TokenUsage.Total.CacheWriteInputTokens,
		ContextWindow:            p.TokenUsage.ModelContextWindow,
		// CostUSD deliberately left zero — see handleTurnCompleted.
	}
	d.usageMu.Unlock()
}

type turnCompletedParams struct {
	ThreadID string `json:"threadId"`
	Turn     struct {
		ID    string         `json:"id"`
		Items []turnItemHead `json:"items"`
	} `json:"turn"`
}

// turnItemHead reads just enough of one of turn/completed's own echoed
// items to find the final agentMessage — see extractStructuredOutput.
type turnItemHead struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// extractStructuredOutput is protocol detail #10 from
// docs/codex-cli-findings.md, VERIFIED LIVE
// (docs/codex-fixtures/structured-output-turn-completed.jsonl): codex has no
// separate field analogous to claude's result event structured_output.
// TurnStartParams.outputSchema instead constrains the final assistant
// MESSAGE itself — the last agentMessage item's own "text" IS the
// schema-validated JSON, verbatim, both inline in item/completed and echoed
// again in turn/completed's own turn.items. This only ever returns non-nil
// when this driver was actually started with an OutputSchema — a plain
// conversational turn's final agentMessage is prose, not JSON, and forcing
// it through json.Valid would just silently produce nil anyway, but gating
// on OutputSchema up front means that is by design, not by accident.
func (d *Driver) extractStructuredOutput(p turnCompletedParams) json.RawMessage {
	if len(d.opts.OutputSchema) == 0 {
		return nil
	}
	var text string
	for _, it := range p.Turn.Items {
		if it.Type == "agentMessage" {
			text = it.Text // last one wins if a turn ever has more than one
		}
	}
	if text == "" || !json.Valid([]byte(text)) {
		return nil
	}
	return json.RawMessage(text)
}

// handleTurnCompleted is acy's idle signal: it ends the turn, banking this
// turn's accumulated usage and the thread's latest cumulative model usage,
// then resets the per-turn accumulator so the next turn starts clean.
func (d *Driver) handleTurnCompleted(raw json.RawMessage) {
	var p turnCompletedParams
	if err := json.Unmarshal(raw, &p); err != nil {
		alog.Printf("codex: decode turn/completed: %v", err)
		return
	}

	structuredOutput := d.extractStructuredOutput(p)

	d.usageMu.Lock()
	usage := d.turnUsage
	modelUsage := d.latestModelUsage
	d.turnUsage = driver.Usage{}
	d.usageMu.Unlock()

	d.mu.Lock()
	if d.activeTurnID == p.Turn.ID {
		d.activeTurnID = ""
	}
	model := d.model
	d.mu.Unlock()

	var modelUsageMap map[string]driver.ModelUsage
	if model != "" {
		modelUsageMap = map[string]driver.ModelUsage{model: modelUsage}
	}

	d.events <- driver.Event{
		Type:             driver.TypeResult,
		SessionID:        p.ThreadID,
		Usage:            &usage,
		ModelUsage:       modelUsageMap,
		StructuredOutput: structuredOutput,
		// TotalCostUSD is deliberately left at its zero value: codex reports no
		// dollar figure anywhere in its protocol — docs/codex-cli-findings.md §9
		// greps every captured fixture and every generated schema for one and
		// finds none, only percentage-of-plan and token counts. Showing no
		// number is more honest than inventing one from a price table; do not
		// "fix" this zero later by adding a per-model cost estimate.
	}
}
