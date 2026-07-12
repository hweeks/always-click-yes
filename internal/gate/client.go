package gate

import (
	"bufio"
	"encoding/json"
	"net"
	"time"
)

// Ask forwards a raw PreToolUse JSON payload (as received on the hook's stdin) to
// the supervisor and returns its decision. Used by the `hook` subcommand.
func Ask(socket string, raw []byte) (Decision, error) {
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return Decision{}, err
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write(append(trimNL(raw), '\n')); err != nil {
		return Decision{}, err
	}

	var d Decision
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&d); err != nil {
		return Decision{}, err
	}
	return d, nil
}
