// Bridge turns codex's in-band approval requests into the exact currency
// acy's countdown gate already speaks, so no second hook process or socket is
// needed on this path at all — see the package doc comment and approval.go.
package codex

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/gate"
)

// The two ServerRequest methods this bridge knows how to translate
// (docs/codex-cli-findings.md §3). item/permissions/requestApproval is a
// third real variant but was never exercised live in that recon and has no
// gate.PreToolUseInput mapping here — see BuildPreToolUseInput's default case.
const (
	methodCommandExecutionApproval = "item/commandExecution/requestApproval"
	methodFileChangeApproval       = "item/fileChange/requestApproval"
	// mcpServer/elicitation/request is Codex asking its client whether an MCP
	// server may proceed with an elicitation. acy's own MCP server raises this
	// for every tool call under Codex's untrusted policy. It is not a shell or
	// file operation: the eventual Dispatch/Finish/Ask call is still validated
	// by acy's MCP bridge and phase machine, so it must not be put through the
	// filesystem tool countdown. It does need an immediate {action:"accept"}
	// response or Codex rejects the MCP call before acy sees it.
	methodMCPServerElicitation = "mcpServer/elicitation/request"
)

// codex's own decision vocabulary (docs/codex-cli-findings.md §3). Only two
// of the six possible values are ever sent by this package — see decisionFor.
const (
	decisionAccept  = "accept"
	decisionDecline = "decline"
)

// approvalParams is the subset of a ServerRequest's params this bridge reads.
// threadId/itemId/cwd/reason are common to both approval methods observed
// live; command is commandExecution-only. Anything else — notably
// fileChange's patch/path fields, whose exact shape this recon never captured
// live (docs/codex-cli-findings.md §3 only exercised commandExecution) —
// is preserved by carrying req.Params through verbatim rather than guessed at
// here; see BuildPreToolUseInput.
type approvalParams struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Cwd      string `json:"cwd"`
	Command  string `json:"command"`
}

// BuildPreToolUseInput translates one codex ApprovalRequest into the
// PreToolUseInput acy's gate already knows how to countdown and, for a Bash
// tool, merge-guard. ok is false for a method this bridge has no mapping for
// (item/permissions/requestApproval, or any future ServerRequest variant);
// the caller must still answer it (declined) rather than silently drop it —
// see forward.
//
// Exported (rather than kept as a forward-only helper) because it is the one
// piece of this file callable without a live Driver: it is pure and
// side-effect-free, which is exactly what a test proving the "Bash"/"command"
// naming choice actually reaches internal/ui's merge guard needs — see
// internal/ui's codex bridge test.
func BuildPreToolUseInput(req ApprovalRequest) (gate.PreToolUseInput, bool) {
	var p approvalParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		alog.Printf("codex: gate bridge: decode approval params (method=%s id=%d): %v", req.Method, req.ID, err)
		return gate.PreToolUseInput{}, false
	}

	in := gate.PreToolUseInput{
		// SessionID carries codex's threadId, not req.ID or anything else
		// numeric: enqueue (internal/ui/gate.go) calls
		// dispatcher.TaskFor(p.Input.SessionID) to decide whether this
		// request came from the supervising session or a dispatched child.
		// Get this wrong and every child edit is recognized as a parent read
		// and waved through with no countdown at all.
		SessionID: p.ThreadID,
		// ToolUseID carries codex's itemId, not the server request's own id
		// (req.ID) — that id is reused across unrelated approval requests
		// within one process (it is the server's own small request counter,
		// not a stable per-item identity), while itemId is unique to this
		// command/file-change item. Gates are answered by identity, never
		// position (resolveByID's doc comment), so this has to be the value
		// that will not collide.
		ToolUseID: p.ItemID,
		Cwd:       p.Cwd,
		// HookEventName: there is no PreToolUse hook subprocess anywhere on
		// this path (see the package doc comment) — but enqueue and the
		// transcript both key off this exact string, so it travels through
		// unchanged rather than being left blank.
		HookEventName: "PreToolUse",
		// PermissionMode has no codex equivalent: no surface this recon
		// found (docs/codex-cli-findings.md) exposes a claude-style
		// --permission-mode value over app-server's protocol, so this is left
		// blank rather than inventing one.
	}

	switch req.Method {
	case methodCommandExecutionApproval:
		// "Bash", not a codex-specific name, deliberately: mergeGuardVerdict
		// (internal/ui/guard.go) only ever inspects a tool literally named
		// "Bash" and reads a "command" field off it, and readOnlyParentTools
		// (internal/ui/model.go) excludes exactly that name so it can never
		// bypass the countdown either. A codex-specific name here would leave
		// both checks inert — the merge guard would never fire on a codex
		// `gh pr merge`, and ParentNoExec would have nothing to recognize.
		in.ToolName = "Bash"
		inputJSON, err := json.Marshal(map[string]string{"command": p.Command})
		if err != nil {
			alog.Printf("codex: gate bridge: marshal command input (id=%d): %v", req.ID, err)
			return gate.PreToolUseInput{}, false
		}
		in.ToolInput = inputJSON
	case methodFileChangeApproval:
		in.ToolName = "Edit"
		// mergeGuardVerdict only ever reads a "command" field off a "Bash"
		// tool's input, so it never looks at this value regardless of what
		// key it lives under — a file change cannot merge a PR or push a
		// branch, so there is nothing here for the guard to catch. "changes"
		// carries req.Params verbatim (patch/path fields and all) precisely
		// because their real field names are unconfirmed; passing the whole
		// payload through loses nothing a firmer schema would have kept.
		inputJSON, err := json.Marshal(map[string]json.RawMessage{"changes": req.Params})
		if err != nil {
			alog.Printf("codex: gate bridge: marshal file change input (id=%d): %v", req.ID, err)
			return gate.PreToolUseInput{}, false
		}
		in.ToolInput = inputJSON
	default:
		return gate.PreToolUseInput{}, false
	}

	return in, true
}

// decisionFor translates acy's own allow/deny into codex's wire vocabulary.
// Deny maps to "decline", never "cancel": both are valid codex decisions, but
// "decline" means "permission refused, continue the turn and try something
// else" — exactly what claude's deny has always meant to acy's veto. "cancel"
// additionally interrupts the whole turn immediately; it exists in codex's
// protocol for a possible future "stop the run" action but is deliberately
// not sent here — a per-tool veto has never ended a claude turn either, and
// codex should not become harsher than claude on the same decision.
func decisionFor(d gate.Decision) string {
	if d.Behavior == gate.Allow {
		return decisionAccept
	}
	return decisionDecline
}

// Bridge fans one or more Driver's Approvals() streams into a single
// gate.Pending stream — exactly the type ui.Config.GateReqs already expects —
// so codex's in-band approval requests reach the same countdown and merge
// guard claude's PreToolUse hook does.
type Bridge struct {
	out chan *gate.Pending

	mu     sync.Mutex
	closed bool
	done   chan struct{}  // closed once, by Close, to abandon in-flight requests
	wg     sync.WaitGroup // in-flight forward() calls only — never Attach's own loop, which can outlive a live driver indefinitely

	attached atomic.Int64 // drivers currently attached — see Attached
}

// NewBridge creates a Bridge ready to Attach drivers to.
func NewBridge() *Bridge {
	return &Bridge{
		out:  make(chan *gate.Pending, 16),
		done: make(chan struct{}),
	}
}

// Requests is the stream to hand to ui.Config.GateReqs.
func (b *Bridge) Requests() <-chan *gate.Pending { return b.out }

// Attach starts forwarding one driver's approval requests onto the shared
// channel. Dynamic membership is the point, not an afterthought: the parent
// session attaches at startup and each dispatched child attaches when it
// spawns and goes away when it exits, so Attach must tolerate drivers joining
// after others are already forwarding, and one driver's own Approvals()
// channel closing must never close the shared channel out from under every
// other attached driver — see forward and Close for how that is kept true.
func (b *Bridge) Attach(d *Driver) {
	b.attached.Add(1)
	go func() {
		defer alog.Recover("codex.gate.Attach")
		defer b.attached.Add(-1)
		for req := range d.Approvals() {
			b.forward(d, req)
		}
	}()
}

// Attached reports how many drivers are currently attached — Attach called
// and their Approvals() stream not yet closed. It exists so a caller that
// cannot launch a real codex process (internal/supervisor's own offline
// tests, chiefly) can still assert that its wiring actually calls Attach,
// without needing a live approval round trip to observe the effect.
func (b *Bridge) Attached() int64 { return b.attached.Load() }

// forward turns one approval request into a Pending, pushes it onto the
// shared channel, waits for its decision, and answers the driver. Every exit
// path calls approve exactly once — an unanswered codex approval hangs that
// turn forever server-side, so there is no path out of this function that
// leaves req unanswered, including a Bridge shutdown that catches it
// mid-flight.
func (b *Bridge) forward(d *Driver, req ApprovalRequest) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		alog.Printf("codex: gate bridge: declining %s id=%d (bridge closed)", req.Method, req.ID)
		b.approve(d, req.ID, decisionDecline)
		return
	}
	b.wg.Add(1)
	b.mu.Unlock()
	defer b.wg.Done()
	if req.Method == methodMCPServerElicitation {
		alog.Printf("codex: gate bridge: accepting MCP elicitation id=%d", req.ID)
		b.approve(d, req.ID, decisionAccept)
		return
	}

	in, ok := BuildPreToolUseInput(req)
	if !ok {
		alog.Printf("codex: gate bridge: declining unrecognized approval method %q (id=%d)", req.Method, req.ID)
		b.approve(d, req.ID, decisionDecline)
		return
	}

	p, decisions := gate.NewPending(in)

	select {
	case b.out <- p:
		alog.Printf("codex: gate bridge: forwarded %s id=%d item=%s thread=%s", req.Method, req.ID, in.ToolUseID, in.SessionID)
	case <-b.done:
		// Never reached the UI at all. Resolve it anyway so nothing that
		// might still be holding this pointer blocks on it, then answer the
		// driver directly.
		p.Resolve(gate.Decision{Behavior: gate.Deny, Reason: "acy shutting down"})
		b.approve(d, req.ID, decisionDecline)
		return
	}

	select {
	case dec := <-decisions:
		b.approve(d, req.ID, decisionFor(dec))
	case <-b.done:
		// The request reached the UI (or whatever reads Requests()) and was
		// never resolved — abandoned rather than answered. gate.Pending's own
		// Done() exists for the mirror image of this on the server side (a
		// hook connection dying before an answer arrives), but Bridge is this
		// request's producer, not a connection the reader can signal back
		// through, so Bridge's own shutdown is what stands in for that here.
		// Resolve is idempotent (sync.Once), so if the reader does still
		// answer after this, it is a harmless no-op.
		p.Resolve(gate.Decision{Behavior: gate.Deny, Reason: "acy shutting down"})
		b.approve(d, req.ID, decisionDecline)
	}
}

// approve calls the driver's Approve and logs, rather than swallows, a write
// failure. A dead stdin pipe (the process already exited) is the only
// realistic cause, and it means the answer was moot anyway.
func (b *Bridge) approve(d *Driver, id int64, decision string) {
	if err := d.Approve(id, decision); err != nil {
		alog.Printf("codex: gate bridge: approve id=%d decision=%s: %v", id, decision, err)
	}
}

// Close stops forwarding and closes the shared channel exactly once. Every
// request already in flight — pushed to the shared channel and awaiting a
// decision, or not yet pushed at all — is resolved and declined rather than
// left outstanding.
//
// It waits only for in-flight forward() calls (b.wg), never for Attach's own
// per-driver loop: that loop blocks on a live driver's Approvals() channel,
// which may not close for as long as that process keeps running, and Close
// must not hang on a driver nobody has told to stop. Once every in-flight
// forward() has observed done and returned, no goroutine can still attempt to
// send on out, so closing it here cannot race a send on a closed channel.
func (b *Bridge) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	close(b.done)
	b.mu.Unlock()

	b.wg.Wait()
	close(b.out)
}
