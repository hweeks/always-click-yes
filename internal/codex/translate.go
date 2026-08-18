package codex

import (
	"encoding/json"
	"strings"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/driver"
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

// reasoningItem's field shapes beyond "type" were never populated in any
// captured fixture (docs/codex-fixtures/app-server-session.ndjson's reasoning
// items all have empty summary/content arrays), so the exact schema is
// unconfirmed. summary/content are read defensively on the working
// hypothesis that, like agentMessage, a reasoning item is a sequence of
// text-bearing chunks; if that guess is wrong the result is an empty thinking
// block, not a decode error.
type reasoningItem struct {
	Summary []reasoningChunk `json:"summary"`
	Content []reasoningChunk `json:"content"`
}

type reasoningChunk struct {
	Text string `json:"text"`
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
		ID string `json:"id"`
	} `json:"turn"`
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
		Type:       driver.TypeResult,
		SessionID:  p.ThreadID,
		Usage:      &usage,
		ModelUsage: modelUsageMap,
		// TotalCostUSD is deliberately left at its zero value: codex reports no
		// dollar figure anywhere in its protocol — docs/codex-cli-findings.md §9
		// greps every captured fixture and every generated schema for one and
		// finds none, only percentage-of-plan and token counts. Showing no
		// number is more honest than inventing one from a price table; do not
		// "fix" this zero later by adding a per-model cost estimate.
	}
}
