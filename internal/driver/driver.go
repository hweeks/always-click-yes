package driver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

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
	AllowedTools   []string // --allowedTools (optional; pre-approves tools, e.g. so a read tool runs under --permission-mode plan)
	ExtraArgs      []string // any additional raw args
}

// Driver owns a running claude process and surfaces its decoded event stream.
type Driver struct {
	opts   Options
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan Event

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
	if o.ResumeID != "" {
		args = append(args, "--resume", o.ResumeID)
	}
	if o.SessionID != "" {
		args = append(args, "--session-id", o.SessionID)
	}
	if len(o.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(o.AllowedTools, ","))
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
// process stdin. It never launches claude, so Send/SendToolResult/Interrupt can be
// exercised and their exact wire format inspected in tests. Events() stays empty.
// Test-only; production code uses New + Start.
func NewWithWriter(opts Options, w io.WriteCloser) *Driver {
	d := New(opts)
	d.stdin = w
	return d
}

// Start launches the claude subprocess and begins streaming events. The provided
// context cancels the process when done.
func (d *Driver) Start(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, d.opts.Bin, d.opts.Args()...)
	if d.opts.Cwd != "" {
		cmd.Dir = d.opts.Cwd
	}

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
	alog.Printf("driver: start %s %v (cwd=%q)", d.opts.Bin, d.opts.Args(), cmd.Dir)

	go d.readEvents(stdout)
	go d.readStderr(stderr)
	return nil
}

// readEvents decodes NDJSON lines. It uses a bufio.Reader (not Scanner) because
// individual lines can exceed Scanner's default token size (thinking signatures,
// large tool inputs).
func (d *Driver) readEvents(r io.Reader) {
	defer close(d.events)
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

// SendToolResult answers a pending tool call (e.g. AskUserQuestion) by injecting
// a user message whose content is a single tool_result block referencing the
// tool_use id. This unblocks a turn that is waiting on client-side tool output.
func (d *Driver) SendToolResult(toolUseID string, content any) error {
	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": toolUseID,
					"content":     content,
				},
			},
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

// Wait blocks until the process exits.
func (d *Driver) Wait() error {
	if d.cmd == nil {
		return nil
	}
	return d.cmd.Wait()
}

// Stop closes stdin and kills the process if still running.
func (d *Driver) Stop() {
	d.once.Do(func() {
		_ = d.CloseInput()
		if d.cmd != nil && d.cmd.Process != nil {
			_ = d.cmd.Process.Kill()
		}
	})
}
