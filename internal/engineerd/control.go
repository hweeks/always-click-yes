package engineerd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// Controller is what a control connection's decoded Answer/Cancel messages
// are routed to. *engineer.Core satisfies this directly — its
// Answer(questionID, text) and Cancel(reason) methods are exactly this
// shape — so RunDetachedTarget can hand a live Core straight to
// ListenControl with no adapter in between.
type Controller interface {
	Answer(questionID, text string) bool
	Cancel(reason string)
}

// ControlServer accepts connections on dir/control.sock. Any number of
// connections may be open at once; each is read independently on its own
// goroutine, so a malformed line or a slow reader on one never blocks
// another.
type ControlServer struct {
	ln   net.Listener
	path string
	wg   sync.WaitGroup
}

// ListenControl starts accepting control connections in dir, routing every
// decoded Answer/Cancel message to ctrl. A stale socket left behind by a
// crashed prior run at the same path is removed first — net.Listen("unix",…)
// otherwise refuses to bind over it.
func ListenControl(dir string, ctrl Controller) (*ControlServer, error) {
	path := filepath.Join(dir, ControlSock)
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	s := &ControlServer{ln: ln, path: path}
	s.wg.Add(1)
	go s.accept(ctrl)
	return s, nil
}

// SocketPath is the control socket's path, for a caller that wants to hand
// it to a client without recomputing it.
func (s *ControlServer) SocketPath() string { return s.path }

// Close stops the listener. Connections already accepted keep running until
// their peer disconnects; Close does not cut them off.
func (s *ControlServer) Close() error { return s.ln.Close() }

func (s *ControlServer) accept(ctrl Controller) {
	defer s.wg.Done()
	defer alog.Recover("engineerd.control.accept")
	var conns sync.WaitGroup
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			conns.Wait()
			return
		}
		conns.Go(func() {
			handleControl(conn, ctrl)
		})
	}
}

// handleControl reads NDJSON lines off conn until it closes, decoding each as
// an Answer or Cancel and routing it to ctrl. A malformed line, or a line
// naming a message type this socket does not accept, is logged and
// skipped — it does not end the connection, since one bad line from a
// misbehaving client should not cost every line after it.
func handleControl(conn net.Conn, ctrl Controller) {
	defer func() { _ = conn.Close() }()
	defer alog.Recover("engineerd.control.handle")

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var env struct {
			Type engineerwire.Type `json:"type"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			alog.Printf("engineerd: control: malformed line: %v", err)
			continue
		}
		switch env.Type {
		case engineerwire.TypeAnswer:
			var m engineerwire.Answer
			if err := json.Unmarshal(line, &m); err != nil {
				alog.Printf("engineerd: control: malformed answer: %v", err)
				continue
			}
			ok := ctrl.Answer(m.QuestionID, m.Text)
			alog.Printf("engineerd: control: answer question_id=%s ok=%v", m.QuestionID, ok)
		case engineerwire.TypeCancel:
			var m engineerwire.Cancel
			if err := json.Unmarshal(line, &m); err != nil {
				alog.Printf("engineerd: control: malformed cancel: %v", err)
				continue
			}
			ctrl.Cancel(m.Reason)
			alog.Printf("engineerd: control: cancel reason=%q", m.Reason)
		default:
			alog.Printf("engineerd: control: ignoring message type %q", env.Type)
		}
	}
}

// DialControl connects to dir's control socket.
func DialControl(dir string) (net.Conn, error) {
	return net.Dial("unix", filepath.Join(dir, ControlSock))
}

// SendControl marshals msg — an engineerwire.Answer or engineerwire.Cancel —
// and writes it as one NDJSON line to dir's control socket. It dials fresh
// each call and closes the connection afterward: the engineer this is aimed
// at may be long gone, so nothing here keeps state across calls for it to
// go stale.
func SendControl(dir string, msg any) error {
	line, err := engineerwire.Marshal(msg)
	if err != nil {
		return err
	}
	conn, err := DialControl(dir)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_, err = conn.Write(line)
	return err
}
