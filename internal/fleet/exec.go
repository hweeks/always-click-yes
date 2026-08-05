package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// runStart runs cmd (already built with the right argv) with spec piped to
// its stdin, and decodes the single StartAck line it prints on stdout.
// Shared by localTransport and sshTransport, which differ only in how cmd
// itself is constructed.
func runStart(cmd *exec.Cmd, spec engineerwire.Spec, errPrefix string) (StartAck, error) {
	line, err := engineerwire.Marshal(spec)
	if err != nil {
		return StartAck{}, fmt.Errorf("%s: encoding spec: %w", errPrefix, err)
	}
	cmd.Stdin = bytes.NewReader(line)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		alog.Printf("%s: %v: %s", errPrefix, err, stderr.String())
		return StartAck{}, fmt.Errorf("%s: %w: %s", errPrefix, err, bytes.TrimSpace(stderr.Bytes()))
	}

	var ack StartAck
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &ack); err != nil {
		return StartAck{}, fmt.Errorf("%s: decoding ack: %w (stdout: %q)", errPrefix, err, stdout.String())
	}
	return ack, nil
}

// runAttach starts cmd (already built with the right argv and Stdin set to
// the caller's inbound reader) and decodes its stdout as an engineerwire
// stream, calling onMsg for each message until the process exits.
//
// A Result message ends the attach cleanly (nil error). Anything else —
// ctx ending, the process dying, the connection dropping — is reported back
// so Follow knows to reattach; only ctx ending gets ctx.Err() specifically,
// since that is the one case the caller must not retry.
func runAttach(ctx context.Context, cmd *exec.Cmd, onMsg func(any), errPrefix string) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%s: stdout pipe: %w", errPrefix, err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: starting: %w", errPrefix, err)
	}

	sawResult := false
	dec := engineerwire.NewDecoder(stdout)
	for {
		msg, decErr := dec.Decode()
		if decErr != nil {
			break
		}
		onMsg(msg)
		if _, ok := msg.(engineerwire.Result); ok {
			sawResult = true
		}
	}

	waitErr := cmd.Wait()
	if sawResult {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if stderr.Len() > 0 {
		alog.Printf("%s: exited: %v: %s", errPrefix, waitErr, stderr.String())
	}
	if waitErr != nil {
		return fmt.Errorf("%s: %w: %s", errPrefix, waitErr, bytes.TrimSpace(stderr.Bytes()))
	}
	return fmt.Errorf("%s: connection ended before a result arrived", errPrefix)
}
