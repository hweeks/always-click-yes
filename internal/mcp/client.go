package mcp

import (
	"bufio"
	"encoding/json"
	"net"
	"time"
)

// dialTimeout bounds only the connect. The read that follows is deliberately
// unbounded: it is a human deciding, and the countdown that stops them thinking
// forever lives in the TUI (see ui.expireAsk), not here.
const dialTimeout = 5 * time.Second

// Ask forwards a tools/call to the supervisor and blocks until the TUI answers it.
// Used by the `mcp` subcommand.
func Ask(socket string, req Request) (Answer, error) {
	conn, err := net.DialTimeout("unix", socket, dialTimeout)
	if err != nil {
		return Answer{}, err
	}
	defer func() { _ = conn.Close() }()

	b, err := json.Marshal(req)
	if err != nil {
		return Answer{}, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return Answer{}, err
	}

	var a Answer
	if err := json.NewDecoder(bufio.NewReaderSize(conn, maxLine)).Decode(&a); err != nil {
		return Answer{}, err
	}
	return a, nil
}
