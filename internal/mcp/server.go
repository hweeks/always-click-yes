package mcp

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"sync"

	"github.com/hweeks/always-click-yes/internal/alog"
)

// Pending is an in-flight tools/call awaiting an answer from the TUI. The
// supervisor receives it on Bridge.Requests, opens the picker, then calls Resolve.
//
// Deliberately shaped like gate.Pending, but kept separate: a gate request answers
// allow/deny, an ask request answers with text, and folding both into one channel
// would make every consumer branch on the tool name to know what it was even
// looking at.
type Pending struct {
	Req Request

	reply chan Answer
	done  chan struct{} // closed if the mcp child dies first
	once  sync.Once
	dOnce sync.Once
}

// NewPending constructs a Pending and returns the channel its answer arrives on.
// Used by the bridge internally, and by tests that feed the UI a request directly.
func NewPending(req Request) (*Pending, <-chan Answer) {
	p := &Pending{
		Req:   req,
		reply: make(chan Answer, 1),
		done:  make(chan struct{}),
	}
	return p, p.reply
}

// Resolve delivers the answer to the waiting `acy mcp` child, unblocking claude's
// turn. Safe to call more than once; later calls are no-ops.
func (p *Pending) Resolve(a Answer) {
	p.once.Do(func() { p.reply <- a })
}

// Done reports a channel closed if the mcp child disconnects before an answer —
// e.g. because the driver it belonged to was stopped and its process group killed.
func (p *Pending) Done() <-chan struct{} { return p.done }

// Abandon marks the asker gone: the `acy mcp` child has disconnected, so no answer
// can reach claude any more. The bridge calls it when the connection drops; tests
// call it to stand in for a dead session.
func (p *Pending) Abandon() {
	p.dOnce.Do(func() { close(p.done) })
}

// Bridge accepts connections from `acy mcp` children and surfaces their blocked
// tools/calls as Pending requests.
type Bridge struct {
	ln       net.Listener
	path     string
	requests chan *Pending
}

// Listen creates the ask socket inside dir and starts accepting connections.
func Listen(dir string) (*Bridge, error) {
	path := filepath.Join(dir, "mcp.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	b := &Bridge{
		ln:       ln,
		path:     path,
		requests: make(chan *Pending, 16),
	}
	go b.accept()
	return b, nil
}

// SocketPath returns the socket path to hand the mcp child via --socket.
func (b *Bridge) SocketPath() string { return b.path }

// Requests is the stream of tools/calls waiting on the UI.
func (b *Bridge) Requests() <-chan *Pending { return b.requests }

// Close stops the listener.
func (b *Bridge) Close() error { return b.ln.Close() }

func (b *Bridge) accept() {
	defer close(b.requests)
	defer alog.Recover("mcp.accept")
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.handle(conn)
	}
}

func (b *Bridge) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	defer alog.Recover("mcp.handle")

	br := bufio.NewReaderSize(conn, maxLine)
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var req Request
	if err := json.Unmarshal(trimNL(line), &req); err != nil {
		_ = writeJSON(conn, Answer{Text: "(malformed request)"})
		return
	}

	p, reply := NewPending(req)

	// Notice the child dying while we hold the question on screen.
	go func() {
		_, _ = io.Copy(io.Discard, br)
		p.Abandon()
	}()

	b.requests <- p
	alog.Printf("mcp: request tool=%s use_id=%s", req.Tool, req.ToolUseID)

	var a Answer
	select {
	case a = <-reply:
	case <-p.done:
		return // child already gone; nothing to answer
	}
	alog.Printf("mcp: answered tool=%s", req.Tool)
	_ = writeJSON(conn, a)
}

func writeJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
