// Package codex owns a long-lived `codex app-server` subprocess and translates
// its protocol into acy's single internal event vocabulary, driver.Event — the
// same vocabulary internal/driver produces for claude. internal/ui and every
// other consumer stay ignorant of which vendor's CLI is actually running; see
// internal/ui/agent.go.
//
// docs/codex-cli-findings.md is the recon this package implements against, and
// docs/codex-fixtures/app-server-session.ndjson is a captured real session used
// by this package's own offline tests — no test here launches a real codex
// process or makes a network call.
//
// The wire format is JSON-RPC-*shaped* — id/method/params, and id/result or
// id/error — but carries no "jsonrpc":"2.0" field, and a request the server
// sends (an approval request, chiefly) lives in a numeric id namespace
// completely separate from the client's own outgoing ids. See protocol.go's
// classifyLine for how that is told apart without ever comparing id values.
//
// This package decides nothing about approvals. A server-initiated request
// like item/commandExecution/requestApproval blocks codex's turn until
// answered; this package only surfaces it (Approvals / PendingApprovals) and
// lets a caller answer it (Approve). Wiring that to acy's countdown gate is a
// separate task — see approval.go.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/version"
)

// Options configures how the codex subprocess is launched and how its thread
// is started. Unlike claude, codex takes almost none of this as CLI flags —
// `codex app-server` takes no session configuration at all; it all travels as
// params on the initialize/thread-start JSON-RPC calls instead.
type Options struct {
	Bin string // path/name of the codex binary (default "codex")
	Cwd string // working directory for both the process and the thread

	Model                 string          // ThreadStartParams.model
	Sandbox               string          // ThreadStartParams.sandbox: readOnly, workspaceWrite, dangerFullAccess
	ApprovalPolicy        string          // ThreadStartParams.approvalPolicy: untrusted, on-request, never
	DeveloperInstructions string          // ThreadStartParams.developerInstructions (developer-role, additive)
	Config                map[string]any  // ThreadStartParams.config: raw per-thread overlay (e.g. mcp_servers)
	OutputSchema          json.RawMessage // TurnStartParams.outputSchema, attached to every turn/start
	Effort                string          // ThreadStartParams.effort (model-specific; no fixed enum)
	ResumeID              string          // when set, thread/resume is used instead of thread/start

	// Env overlays variables for the codex process. Env keys present here win
	// over an inherited value with the same name.
	Env map[string]string
	// StripEnv removes inherited variables from the child's environment
	// regardless of Env.
	StripEnv []string
}

// Args returns the CLI arguments the driver will pass to codex. Session
// configuration is not among them — see the Options doc comment — so this is
// just the subcommand. codex app-server defaults to --listen stdio://, which is
// exactly the transport this package speaks.
func (o Options) Args() []string {
	return []string{"app-server"}
}

// childEnv returns the environment for the codex subprocess, or nil to inherit
// the parent verbatim when there is nothing to overlay or strip.
func (o Options) childEnv() []string {
	if len(o.Env) == 0 && len(o.StripEnv) == 0 {
		return nil
	}
	parent := os.Environ()
	strip := make(map[string]bool, len(o.StripEnv))
	for _, name := range o.StripEnv {
		strip[name] = true
	}
	env := make([]string, 0, len(parent)+len(o.Env))
	for _, kv := range parent {
		name, _, _ := strings.Cut(kv, "=")
		_, overridden := o.Env[name]
		if strip[name] || overridden {
			continue
		}
		env = append(env, kv)
	}
	for name, value := range o.Env {
		env = append(env, name+"="+value)
	}
	return env
}

// requestKind tags a pending outgoing request with what response-side effect,
// if any, should run once its response arrives — see applyResponseSideEffects.
type requestKind int

const (
	kindGeneric requestKind = iota
	kindThreadStart
	kindThreadResume
	kindTurnStart
)

// rpcError is a JSON-RPC-shaped error object on a response line.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// rpcResult is what a pending request's channel receives once its response
// line arrives.
type rpcResult struct {
	Result json.RawMessage
	Err    *rpcError
}

type pendingRequest struct {
	kind requestKind
	ch   chan rpcResult
}

// Driver owns a running codex app-server process and surfaces its translated
// event stream. Call Start to launch it.
type Driver struct {
	opts   Options
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan driver.Event
	ctx    context.Context

	exited  chan struct{} // closed once the process is reaped
	waitErr error         // result of cmd.Wait; read only after exited is closed

	writeMu sync.Mutex
	once    sync.Once
	reqSeq  atomic.Int64

	pendingMu sync.Mutex
	pending   map[int64]pendingRequest

	mu           sync.Mutex // guards threadID, model, activeTurnID
	threadID     string
	model        string
	activeTurnID string

	usageMu          sync.Mutex // guards turnUsage, latestModelUsage
	turnUsage        driver.Usage
	latestModelUsage driver.ModelUsage

	approvalsMu  sync.Mutex
	approvalsOut map[int64]ApprovalRequest
	approvals    chan ApprovalRequest
}

// New creates a Driver. Call Start to launch the process.
func New(opts Options) *Driver {
	if opts.Bin == "" {
		opts.Bin = "codex"
	}
	return &Driver{
		opts:         opts,
		events:       make(chan driver.Event, 64),
		pending:      make(map[int64]pendingRequest),
		approvalsOut: make(map[int64]ApprovalRequest),
		approvals:    make(chan ApprovalRequest, 8),
	}
}

// NewWithWriter builds a Driver whose outgoing JSON-RPC requests go to w
// instead of a real process's stdin. It never launches codex, so the request
// side (sendRequest, Send, Interrupt, Approve) can be exercised and its exact
// wire format inspected in tests, and the response side can be driven directly
// by feeding readLoop an arbitrary reader. Test-only; production code uses
// New + Start.
func NewWithWriter(opts Options, w io.WriteCloser) *Driver {
	d := New(opts)
	d.stdin = w
	return d
}

// Start launches the codex subprocess, begins streaming events, and performs
// the initialize / thread-start (or thread-resume) handshake. It returns once
// the handshake completes and the init event has been emitted.
func (d *Driver) Start(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, d.opts.Bin, d.opts.Args()...)
	if d.opts.Cwd != "" {
		cmd.Dir = d.opts.Cwd
	}
	cmd.Env = d.opts.childEnv()
	cmd.SysProcAttr = detachedSysProcAttr()
	// Cancelling ctx must take down codex's whole tree, not just codex: it
	// spawns shell subprocesses and MCP servers that would otherwise be
	// orphaned.
	cmd.Cancel = func() error { return terminateGroup(cmd.Process) }
	cmd.WaitDelay = 5 * time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start codex: %w", err)
	}
	d.cmd = cmd
	d.stdin = stdin
	d.ctx = ctx
	d.exited = make(chan struct{})
	alog.Printf("codex: start %s %v (cwd=%q)", d.opts.Bin, d.opts.Args(), cmd.Dir)

	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); d.readLoop(stdout) }()
	go func() { defer readers.Done(); d.readStderr(stderr) }()

	// Reap the process, but only once both pipes have hit EOF: cmd.Wait closes
	// them, and closing a pipe out from under a reader loses its output.
	go func() {
		readers.Wait()
		d.waitErr = cmd.Wait()
		close(d.exited)
	}()

	return d.handshake(ctx)
}

// handshake sends initialize, then thread/start (or thread/resume when
// Options.ResumeID is set), and waits for each response in turn. The thread
// response's side effects (capturing the thread id/model and emitting the init
// event) run in applyResponseSideEffects as the response is decoded, not here
// — see that comment for why.
func (d *Driver) handshake(ctx context.Context) error {
	if _, err := d.call(ctx, "initialize", d.initializeParams(), kindGeneric); err != nil {
		return fmt.Errorf("codex: initialize: %w", err)
	}

	method := "thread/start"
	kind := kindThreadStart
	params := d.threadStartParams()
	if d.opts.ResumeID != "" {
		method = "thread/resume"
		kind = kindThreadResume
		params = d.threadResumeParams()
	}
	if _, err := d.call(ctx, method, params, kind); err != nil {
		return fmt.Errorf("codex: %s: %w", method, err)
	}

	d.mu.Lock()
	ready := d.threadID != ""
	d.mu.Unlock()
	if !ready {
		return fmt.Errorf("codex: %s response carried no thread id", method)
	}
	return nil
}

// initializeParams builds the "initialize" request's params.
func (d *Driver) initializeParams() map[string]any {
	return map[string]any{
		"clientInfo": map[string]any{
			"name":    "always-click-yes",
			"version": version.String(),
		},
	}
}

// threadStartParams builds thread/start's params from Options.
func (d *Driver) threadStartParams() map[string]any {
	p := map[string]any{}
	if d.opts.Cwd != "" {
		p["cwd"] = d.opts.Cwd
	}
	if d.opts.Model != "" {
		p["model"] = d.opts.Model
	}
	if d.opts.Sandbox != "" {
		p["sandbox"] = d.opts.Sandbox
	}
	if d.opts.ApprovalPolicy != "" {
		p["approvalPolicy"] = d.opts.ApprovalPolicy
	}
	if d.opts.DeveloperInstructions != "" {
		p["developerInstructions"] = d.opts.DeveloperInstructions
	}
	if d.opts.Effort != "" {
		p["effort"] = d.opts.Effort
	}
	if len(d.opts.Config) > 0 {
		p["config"] = d.opts.Config
	}
	return p
}

// threadResumeParams builds thread/resume's params. Only the by-id form is
// implemented. docs/codex-cli-findings.md §7 documents thread/resume accepting
// a history array, an on-disk path, or a thread id, with history > path > id
// precedence — but no live session in this recon's fixtures ever captured an
// actual thread/resume call, so the exact field name used here (threadId,
// mirroring turn/start's own field) is inferred from the protocol's otherwise
// consistent camelCase naming, not live-verified.
func (d *Driver) threadResumeParams() map[string]any {
	p := d.threadStartParams()
	p["threadId"] = d.opts.ResumeID
	return p
}

// context returns the context handshake/Send/Interrupt calls should honor:
// the one Start was given, or a background context for a Driver built with
// NewWithWriter (tests), which never calls Start.
func (d *Driver) context() context.Context {
	if d.ctx != nil {
		return d.ctx
	}
	return context.Background()
}

// Events returns the translated event stream. It is closed when the process's
// stdout hits EOF.
func (d *Driver) Events() <-chan driver.Event { return d.events }

// SessionID returns the thread id codex assigned in its thread/start (or
// thread/resume) response, "" before that handshake completes. Named to
// match internal/orchestrator's optional re-keying interface rather than
// this package's own "thread" vocabulary: a dispatched child has no way to
// make codex adopt a caller-chosen id the way claude's --session-id does
// (docs/codex-cli-findings.md §7 — thread.id is assigned by the server, not
// settable by the caller), so orchestrator reads this back once Start
// returns and re-keys its gate-attribution map to codex's real id instead
// of the placeholder it had to hand the task before a process existed.
func (d *Driver) SessionID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.threadID
}

// Send starts a new turn on the current thread by issuing turn/start, and
// waits for its acknowledgement (which carries the new turn id, captured for
// Interrupt — see applyResponseSideEffects). It does not wait for the turn to
// finish; that arrives later as a driver.TypeResult event via turn/completed.
func (d *Driver) Send(text string) error {
	d.mu.Lock()
	threadID := d.threadID
	d.mu.Unlock()
	if threadID == "" {
		return fmt.Errorf("codex: no active thread; Start must complete before Send")
	}

	_, err := d.call(d.context(), "turn/start", d.turnStartParams(text), kindTurnStart)
	return err
}

// turnStartParams builds turn/start's params: the single text input variant
// (findings.md §2 — UserInput also has image/localImage/localAudio variants
// this package does not use) plus, when configured, the per-turn output
// schema.
func (d *Driver) turnStartParams(text string) map[string]any {
	d.mu.Lock()
	threadID := d.threadID
	d.mu.Unlock()

	params := map[string]any{
		"threadId": threadID,
		"input": []map[string]any{
			{"type": "text", "text": text},
		},
	}
	if len(d.opts.OutputSchema) > 0 {
		params["outputSchema"] = d.opts.OutputSchema
	}
	return params
}

// Interrupt asks the active turn to abort via turn/interrupt. If no turn is
// active — Send has not been called, or the last one already completed — this
// is a no-op that returns nil, not an error: there is nothing to interrupt.
func (d *Driver) Interrupt() error {
	d.mu.Lock()
	turnID := d.activeTurnID
	d.mu.Unlock()
	if turnID == "" {
		return nil
	}

	alog.Printf("codex: interrupt turn %s", turnID)
	_, err := d.call(d.context(), "turn/interrupt", d.turnInterruptParams(), kindGeneric)
	return err
}

// turnInterruptParams builds turn/interrupt's params from the current thread
// and active turn.
func (d *Driver) turnInterruptParams() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return map[string]any{"threadId": d.threadID, "turnId": d.activeTurnID}
}

// CloseInput closes stdin, signaling the process that no more input is coming.
func (d *Driver) CloseInput() error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	if d.stdin == nil {
		return nil
	}
	return d.stdin.Close()
}

// Wait blocks until the process has exited and its output has been drained.
func (d *Driver) Wait() error {
	if d.cmd == nil {
		return nil
	}
	<-d.exited
	return d.waitErr
}

// Stop closes stdin and kills the process — and everything it spawned — if
// still running. It does not block: the goroutine started by Start reaps the
// process once its pipes drain.
func (d *Driver) Stop() {
	d.once.Do(func() {
		_ = d.CloseInput()
		if d.cmd != nil {
			_ = killGroup(d.cmd.Process)
		}
	})
}

// readLoop decodes NDJSON lines from codex's stdout. It uses a bufio.Reader
// (not Scanner) because individual lines can exceed Scanner's default token
// size (large tool payloads, e.g. aggregatedOutput on a busy commandExecution).
func (d *Driver) readLoop(r io.Reader) {
	defer close(d.events)
	defer alog.Recover("codex.readLoop")
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			alog.Raw("RX", string(line))
			d.handleLine(line)
		}
		if err != nil {
			return
		}
	}
}

// readLine reads a single '\n'-terminated line of arbitrary length.
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadBytes('\n')
		buf = append(buf, chunk...)
		if err == nil {
			return trimNewline(buf), nil
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return trimNewline(buf), err
	}
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// readStderr drains the process's stderr into the debug log so it never blocks
// the child and is inspectable after the fact.
func (d *Driver) readStderr(r io.Reader) {
	defer alog.Recover("codex.readStderr")
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		alog.Raw("ERR", sc.Text())
	}
}

// handleLine classifies one decoded line and routes it: a response is
// delivered to whichever pending request it answers, a notification is
// translated (translate.go), and a server-initiated request is surfaced as an
// approval (approval.go).
func (d *Driver) handleLine(line []byte) {
	env, kind, err := classifyLine(line)
	if err != nil {
		alog.Printf("codex: decode error: %v", err)
		return
	}
	switch kind {
	case lineResponse:
		d.deliverResponse(env)
	case lineNotification:
		d.handleNotification(env.Method, env.Params)
	case lineServerRequest:
		d.handleServerRequest(env)
	default:
		alog.Printf("codex: unrecognized line shape: %s", line)
	}
}
