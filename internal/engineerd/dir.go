// Package engineerd is the daemon plumbing for one detached engineer
// process: where its state lives on disk, the control socket an architect
// sends Answer/Cancel over, the Attach loop that streams its journal back
// out, and the glue that starts an engineer.Core inside the detached
// process. internal/cli wires this to `acy engineer start/attach/tail`; see
// docs/engineer-protocol.md for the wire contract this all sits on top of.
package engineerd

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hweeks/always-click-yes/internal/state"
)

// File names inside one engineer's state directory.
const (
	SpecFile    = "spec.json"
	PIDFile     = "pid"
	ControlSock = "control.sock"
	DebugLog    = "debug.log"
)

// RootDir is the directory every engineer's state lives under:
// $ACY_STATE_DIR/engineers, falling back like internal/state does to
// <user config dir>/acy/engineers. Not internal/state's own directory —
// an engineer is not a supervised acy session, it runs one.
func RootDir() (string, error) {
	if d := os.Getenv(state.EnvDir); d != "" {
		return filepath.Join(d, "engineers"), nil
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "acy", "engineers"), nil
}

// Dir is one engineer's state directory: RootDir()/<id>.
func Dir(id string) (string, error) {
	if err := validID(id); err != nil {
		return "", err
	}
	root, err := RootDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, id), nil
}

// EnsureDir creates id's state directory (and RootDir, if it does not exist
// yet) and returns its path.
func EnsureDir(id string) (string, error) {
	dir, err := Dir(id)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// validID rejects an id that would escape RootDir when joined onto it —
// mirroring internal/state's validID, since an engineer id reaches a CLI
// flag the same unchecked way a claude session id does.
func validID(id string) error {
	if id == "" || id != filepath.Base(id) || strings.ContainsAny(id, `/\`) || strings.HasPrefix(id, ".") {
		return fmt.Errorf("engineerd: unsafe engineer id %q", id)
	}
	return nil
}

// NewID mints a short, unique engineer id: e<unix-seconds>-<8 hex digits>.
// Hand-rolled rather than pulling in a uuid dependency, the same call
// internal/orchestrator/dispatch.go's newUUID already made.
func NewID() string {
	var b [4]byte
	now := time.Now()
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice, and an id that is merely
		// unique-enough beats refusing to start an engineer over it.
		return fmt.Sprintf("e%d-%08x", now.Unix(), now.UnixNano()&0xffffffff)
	}
	return fmt.Sprintf("e%d-%x", now.Unix(), b[:])
}
