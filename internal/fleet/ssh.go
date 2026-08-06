package fleet

import (
	"context"
	"io"
	"os/exec"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// sshTransport runs an engineer on a remote host: the same `acy engineer
// start`/`attach` argv as localTransport, wrapped in an ssh invocation that
// never prompts.
type sshTransport struct {
	target    string   // ssh target, e.g. "user@host"
	acyBin    string   // the acy binary on the remote host
	clonePath string   // the remote clone path
	path      []string // extra PATH directories on the remote host (FleetHost.Path)
}

// NewSSHTransport returns a Transport that runs engineers on target over
// ssh. An empty acyBin defaults to "acy" on the remote $PATH, matching
// FleetHost's own default. path is FleetHost.Path — extra directories
// prepended to the remote PATH before the engineer argv runs; nil means the
// host declared none.
func NewSSHTransport(target, acyBin, clonePath string, path []string) Transport {
	if acyBin == "" {
		acyBin = "acy"
	}
	return &sshTransport{target: target, acyBin: acyBin, clonePath: clonePath, path: path}
}

// sshBatchArgs is the non-interactive ssh preamble shared by every command
// this package sends to target, engineer argv or otherwise.
//
// BatchMode=yes is not negotiable: an unattended fleet run has nobody at a
// terminal to answer a password or host-key prompt, so a broken key or an
// unrecognized host must fail the connection immediately rather than hang
// forever waiting for input that will never come. The ServerAlive options
// exist for the same reason on the other end of a run — Attach can be open
// for hours, and a dead connection has to be *detected*, not just eventually
// time out on its own schedule, so Follow's reattach loop gets a chance to
// run.
func sshBatchArgs(target string) []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		target,
		"--",
	}
}

// sshArgs wraps engineerArgs (an `acy engineer ...` argv) as a
// non-interactive ssh invocation. When dirs (FleetHost.Path) is empty, the
// argv is passed through exactly as it always has been — acyBin and
// engineerArgs as separate, unquoted elements, which ssh joins with spaces
// on its own before handing them to the remote shell. When dirs is
// non-empty, the whole remote command is instead composed as a single
// quoted `export PATH=...; exec ...` string, so the extended PATH is set
// before acyBin — and everything it goes on to exec — ever runs.
func sshArgs(target, acyBin string, engineerArgs []string, dirs []string) []string {
	if len(dirs) == 0 {
		args := append(sshBatchArgs(target), acyBin)
		return append(args, engineerArgs...)
	}
	argv := append([]string{acyBin}, engineerArgs...)
	return append(sshBatchArgs(target), pathPreamble(dirs)+quoteArgv(argv))
}

func (t *sshTransport) command(ctx context.Context, engineerArgs []string) *exec.Cmd {
	return exec.CommandContext(ctx, "ssh", sshArgs(t.target, t.acyBin, engineerArgs, t.path)...) //nolint:gosec // target/acyBin are operator-configured, not user input
}

func (t *sshTransport) Start(ctx context.Context, spec engineerwire.Spec) (StartAck, error) {
	cmd := t.command(ctx, startArgs(t.clonePath))
	return runStart(cmd, spec, "fleet: ssh start on "+t.target)
}

func (t *sshTransport) Attach(ctx context.Context, engineerID string, fromSeq int64, in io.Reader, onMsg func(any)) error {
	cmd := t.command(ctx, attachArgs(engineerID, fromSeq))
	cmd.Stdin = in
	return runAttach(ctx, cmd, onMsg, "fleet: ssh attach on "+t.target)
}
