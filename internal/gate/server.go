package gate

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"sync"
)

// Pending is a single in-flight permission request awaiting a decision. The TUI
// receives it on Server.Requests, shows a countdown, then calls Resolve.
type Pending struct {
	Input PreToolUseInput

	reply chan Decision
	done  chan struct{} // closed if the hook connection dies first
	once  sync.Once
}

// NewPending constructs a Pending and returns the channel its decision will be
// delivered on. Used by the server internally and by tests/drivers that want to
// feed the UI a request programmatically.
func NewPending(in PreToolUseInput) (*Pending, <-chan Decision) {
	p := &Pending{
		Input: in,
		reply: make(chan Decision, 1),
		done:  make(chan struct{}),
	}
	return p, p.reply
}

// Resolve delivers the decision to the waiting hook. Safe to call once; later
// calls are no-ops.
func (p *Pending) Resolve(d Decision) {
	p.once.Do(func() { p.reply <- d })
}

// Done reports a channel closed if the hook disconnects before a decision.
func (p *Pending) Done() <-chan struct{} { return p.done }

// Server accepts hook connections and surfaces them as Pending requests.
type Server struct {
	ln       net.Listener
	path     string
	requests chan *Pending
}

// Listen creates a unix socket inside dir and starts accepting hook connections.
func Listen(dir string) (*Server, error) {
	path := filepath.Join(dir, "gate.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:       ln,
		path:     path,
		requests: make(chan *Pending, 16),
	}
	go s.accept()
	return s, nil
}

// SocketPath returns the socket path to hand to the hook via --socket.
func (s *Server) SocketPath() string { return s.path }

// Requests is the stream of pending permission requests.
func (s *Server) Requests() <-chan *Pending { return s.requests }

// Close stops the listener.
func (s *Server) Close() error { return s.ln.Close() }

func (s *Server) accept() {
	defer close(s.requests)
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	br := bufio.NewReaderSize(conn, 1<<20)
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var in PreToolUseInput
	if err := json.Unmarshal(trimNL(line), &in); err != nil {
		_ = writeJSON(conn, Decision{Behavior: Deny, Reason: "malformed hook request"})
		return
	}

	p, reply := NewPending(in)

	// Detect the hook disconnecting (e.g. supervisor shutdown) while we wait.
	go func() {
		_, _ = io.Copy(io.Discard, br)
		close(p.done)
	}()

	s.requests <- p

	var d Decision
	select {
	case d = <-reply:
	case <-p.done:
		return // hook already gone; nothing to answer
	}
	_ = writeJSON(conn, d)
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
