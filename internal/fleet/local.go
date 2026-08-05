package fleet

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// localTransport runs an engineer as a direct child of this process: `acy
// engineer start`/`attach` exec'd with no ssh in between.
type localTransport struct {
	acyBin    string
	clonePath string
}

// NewLocalTransport returns a Transport that spawns engineers on this
// machine, cloning from clonePath. An empty acyBin resolves to this
// process's own binary (os.Executable()) — the same acy that is running
// right now.
func NewLocalTransport(acyBin, clonePath string) (Transport, error) {
	if acyBin == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("fleet: local transport: resolving this binary: %w", err)
		}
		acyBin = exe
	}
	return &localTransport{acyBin: acyBin, clonePath: clonePath}, nil
}

func (t *localTransport) Start(ctx context.Context, spec engineerwire.Spec) (StartAck, error) {
	cmd := exec.CommandContext(ctx, t.acyBin, startArgs(t.clonePath)...) //nolint:gosec // acyBin/clonePath are operator-configured, not user input
	return runStart(cmd, spec, "fleet: local start")
}

func (t *localTransport) Attach(ctx context.Context, engineerID string, fromSeq int64, in io.Reader, onMsg func(any)) error {
	cmd := exec.CommandContext(ctx, t.acyBin, attachArgs(engineerID, fromSeq)...) //nolint:gosec // acyBin is operator-configured, not user input
	cmd.Stdin = in
	return runAttach(ctx, cmd, onMsg, "fleet: local attach")
}
