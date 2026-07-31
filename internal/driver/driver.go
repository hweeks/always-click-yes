package driver

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
)

// Options configures how the claude subprocess is launched.
type Options struct {
	Bin            string   // path/name of the claude binary (default "claude")
	Cwd            string   // working directory for the process
	Model          string   // --model (optional)
	PermissionMode string   // --permission-mode (optional): plan, default, acceptEdits, ...
	SettingsPath   string   // --settings <file> (optional; registers the PreToolUse hook)
	ResumeID       string   // --resume <session-id> (optional)
	SessionID      string   // --session-id <uuid> (optional)
	IncludeHooks   bool     // --include-hook-events
	AllowedTools   []string // --allowedTools (optional; pre-approves tools)
	ExtraArgs      []string // any additional raw args
	UseAPIKey      bool     // bill ANTHROPIC_API_KEY instead of the claude.ai login (see childEnv)
	// Env overlays variables for the claude process. It is used for an
	// Anthropic-compatible gateway: claude sees the gateway URL and its local
	// token, never an upstream provider credential.
	Env map[string]string
	// StripEnv removes inherited variables even when they are not Anthropic
	// credentials. A managed gateway uses this to keep OPENAI_API_KEY and peers
	// out of a child that may run Bash.
	StripEnv []string

	// Tools is --tools: the *built-in* tools claude may have at all. Empty means
	// the full registry. Unlike --allowedTools (which pre-approves tools that
	// exist), this decides what exists — a tool left out cannot be called, or even
	// attempted. It is how the plan phase is made read-only, and it leaves MCP
	// tools untouched (verified: --tools Read,Grep,Glob still yields a registry
	// containing mcp__acy__AskUserQuestion).
	Tools []string

	// AppendSystemPrompt is --append-system-prompt. acy uses it to tell the model
	// the things only acy knows: that it cannot arm its own run, and that a human
	// with a keyboard is the one who ends the plan phase.
	AppendSystemPrompt string

	// MCPConfigPath is --mcp-config: registers acy as an MCP server so the model
	// gets mcp__acy__AskUserQuestion / mcp__acy__PresentPlan.
	MCPConfigPath string

	// StrictMCP is --strict-mcp-config: use *only* our MCP config, ignoring the
	// user's own servers. Set during PLAN, where a stray third-party server would
	// add tools the read-only --tools registry was carefully chosen to exclude.
	StrictMCP bool
}

// Driver owns a running claude process and surfaces its decoded event stream.
type Driver struct {
	opts   Options
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan Event

	exited  chan struct{} // closed once the process is reaped
	waitErr error         // result of cmd.Wait; read only after exited is closed

	writeMu sync.Mutex
	once    sync.Once
	reqSeq  atomic.Int64
}

// Args returns the CLI arguments the driver will pass to claude. Exposed for
// testing and for logging the exact invocation.
func (o Options) Args() []string {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
	if o.IncludeHooks {
		args = append(args, "--include-hook-events")
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.PermissionMode != "" {
		args = append(args, "--permission-mode", o.PermissionMode)
	}
	if o.SettingsPath != "" {
		args = append(args, "--settings", o.SettingsPath)
	}
	// --resume names an existing session; --session-id names a new one. Passing both
	// asks claude to be in two sessions at once, and it refuses. Resuming wins: the
	// session already exists, so the id we would have assigned is moot.
	switch {
	case o.ResumeID != "":
		args = append(args, "--resume", o.ResumeID)
	case o.SessionID != "":
		args = append(args, "--session-id", o.SessionID)
	}
	if len(o.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(o.AllowedTools, ","))
	}
	if len(o.Tools) > 0 {
		args = append(args, "--tools", strings.Join(o.Tools, ","))
	}
	if o.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", o.AppendSystemPrompt)
	}
	if o.MCPConfigPath != "" {
		args = append(args, "--mcp-config", o.MCPConfigPath)
	}
	if o.StrictMCP {
		args = append(args, "--strict-mcp-config")
	}
	args = append(args, o.ExtraArgs...)
	return args
}

// New creates a Driver. Call Start to launch the process.
func New(opts Options) *Driver {
	if opts.Bin == "" {
		opts.Bin = "claude"
	}
	return &Driver{
		opts:   opts,
		events: make(chan Event, 64),
	}
}

// NewWithWriter builds a Driver whose injected messages go to w instead of a real
// process stdin. It never launches claude, so Send/Interrupt can be exercised and
// their exact wire format inspected in tests. Events() stays empty.
// Test-only; production code uses New + Start.
func NewWithWriter(opts Options, w io.WriteCloser) *Driver {
	d := New(opts)
	d.stdin = w
	return d
}

// childEnv returns the environment for the claude subprocess. ANTHROPIC_API_KEY
// silently takes precedence over the user's claude.ai login, and headless `claude -p`
// never shows the interactive "use this API key?" prompt, so a key merely present in
// the shell bills the API account for every run. Strip it unless asked otherwise.
func (o Options) childEnv() []string {
	if o.UseAPIKey && len(o.Env) == 0 && len(o.StripEnv) == 0 {
		return nil // nil => inherit the parent environment verbatim
	}
	parent := os.Environ()
	strip := make(map[string]bool, len(o.StripEnv)+1)
	if !o.UseAPIKey {
		strip["ANTHROPIC_API_KEY"] = true
	}
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

// Start launches the claude subprocess and begins streaming events. The provided
// context cancels the process when done.
func (d *Driver) Start(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, d.opts.Bin, d.opts.Args()...)
	if d.opts.Cwd != "" {
		cmd.Dir = d.opts.Cwd
	}
	cmd.Env = d.opts.childEnv()
	cmd.SysProcAttr = detachedSysProcAttr()
	// Cancelling ctx must take down claude's whole tree, not just claude: it
	// spawns tool subprocesses and MCP servers that would otherwise be orphaned.
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
		return fmt.Errorf("start claude: %w", err)
	}
	d.cmd = cmd
	d.stdin = stdin
	d.exited = make(chan struct{})
	alog.Printf("driver: start %s %v (cwd=%q)", d.opts.Bin, d.opts.Args(), cmd.Dir)

	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); d.readEvents(stdout) }()
	go func() { defer readers.Done(); d.readStderr(stderr) }()

	// Reap the process, but only once both pipes have hit EOF: cmd.Wait closes
	// them, and closing a pipe out from under a reader loses its output.
	go func() {
		readers.Wait()
		d.waitErr = cmd.Wait()
		close(d.exited)
	}()
	return nil
}

// readEvents decodes NDJSON lines. It uses a bufio.Reader (not Scanner) because
// individual lines can exceed Scanner's default token size (thinking signatures,
// large tool inputs).
func (d *Driver) readEvents(r io.Reader) {
	defer close(d.events)
	defer alog.Recover("driver.readEvents")
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			alog.Raw("RX", string(line))
			if ev, derr := Decode(line); derr == nil {
				d.events <- ev
			} else {
				alog.Printf("driver: decode error: %v", derr)
			}
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
			// strip trailing newline
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
	defer alog.Recover("driver.readStderr")
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		alog.Raw("ERR", sc.Text())
	}
}

// Events returns the decoded event stream. It is closed when the process exits.
func (d *Driver) Events() <-chan Event { return d.events }

// Send injects a user message into the running session.
func (d *Driver) Send(text string) error {
	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	alog.Raw("TX", string(b))
	b = append(b, '\n')

	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err = d.stdin.Write(b)
	return err
}

// There is deliberately no SendToolResult. Answering a tool call by injecting a
// tool_result block on stdin was how acy planned to answer AskUserQuestion, and it
// is wrong: the only tools acy answers are its own MCP tools, and a tools/call
// blocks claude on the *MCP server process's* JSON-RPC reply, not on its input
// stream. A tool_result written here would never be read and the turn would hang
// forever. Answers go back over the ask socket — see internal/mcp.

// Interrupt asks the running turn to abort (uses the interrupt_receipt_v1
// capability advertised at init). The current turn ends with terminal_reason
// "aborted_streaming"; a new user message can then redirect the session.
func (d *Driver) Interrupt() error {
	id := d.reqSeq.Add(1)
	payload := map[string]any{
		"type":       "control_request",
		"request_id": fmt.Sprintf("acy-int-%d", id),
		"request":    map[string]any{"subtype": "interrupt"},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	alog.Printf("driver: interrupt %s", payload["request_id"])

	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err = d.stdin.Write(b)
	return err
}

// CloseInput closes stdin, signaling the session that no more input is coming.
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
