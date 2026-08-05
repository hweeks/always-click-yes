// Package fleet is the transport layer for arch mode: running an engineer
// process somewhere — the local machine or a remote host over ssh — and
// streaming its journal back. It knows nothing about deciding which host to
// use or how many engineers may run at once; that is the architect's job,
// not this package's. See docs/engineer-protocol.md for the wire protocol
// these transports carry, and internal/cli/engineer.go for the `acy engineer
// start`/`attach` commands they invoke.
package fleet

import (
	"context"
	"io"
	"strconv"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// StartAck is the one line `acy engineer start` prints on success.
type StartAck struct {
	EngineerID string `json:"engineer_id"`
	Dir        string `json:"dir"`
	PID        int    `json:"pid"`
}

// Transport runs an engineer somewhere and streams it back. Start spawns a
// detached engineer for spec and returns once it has acknowledged; Attach
// streams its journal from fromSeq, forwarding lines read from in as
// Answer/Cancel input, until a Result arrives, in is exhausted and the
// engineer is already done, or ctx ends — the same contract as
// engineerd.Attach, which both implementations ultimately run on the other
// end of the wire.
//
// onMsg is called with each decoded outbound message, boxed as one of
// engineerwire's Hello/Event/Question/Result structs (the same shape
// engineerwire.Decoder.Decode returns).
type Transport interface {
	Start(ctx context.Context, spec engineerwire.Spec) (StartAck, error)
	Attach(ctx context.Context, engineerID string, fromSeq int64, in io.Reader, onMsg func(any)) error
}

// startArgs is the argv `acy engineer start` is invoked with, shared by both
// transports — a local exec and an ssh-wrapped one differ only in what
// precedes this slice.
func startArgs(clonePath string) []string {
	return []string{"engineer", "start", "--clone", clonePath}
}

// attachArgs is the argv `acy engineer attach` is invoked with.
func attachArgs(engineerID string, fromSeq int64) []string {
	return []string{"engineer", "attach", engineerID, "--from", strconv.FormatInt(fromSeq, 10)}
}
