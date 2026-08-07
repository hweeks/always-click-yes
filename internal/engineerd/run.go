package engineerd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/engineer"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// RunDetachedTarget is what the detached process spawned by
// `acy engineer start` executes: read dir/spec.json, open its journal, start
// the control socket, drive an engineer.Core built with the real supervisor
// builder to completion, and clean up the pid file on the way out.
//
// It opens alog itself, pointed at dir/debug.log, before anything else runs —
// this is a fresh process with no earlier --log flag to inherit one from, and
// once open, every alog.Printf/alog.Raw call anywhere in the process (the
// driver's raw event stream included) lands in that one file. engineer.Config
// is therefore built with an empty LogPath: supervisor.NewSupervisor would
// otherwise call alog.Open a second time and reopen (truncating) whatever
// path it was given, which is only ever a problem if the two disagree.
func RunDetachedTarget(dir string) error {
	if _, err := alog.Open(filepath.Join(dir, DebugLog)); err != nil {
		return fmt.Errorf("engineerd: opening debug log: %w", err)
	}
	defer alog.Close()

	id := filepath.Base(dir)
	alog.Printf("engineerd: starting engineer %s in %s", id, dir)

	stored, err := ReadSpec(dir)
	if err != nil {
		return fmt.Errorf("engineerd: reading spec: %w", err)
	}

	j, err := engineerwire.Open(dir)
	if err != nil {
		return fmt.Errorf("engineerd: opening journal: %w", err)
	}
	defer func() { _ = j.Close() }()

	if err := writePIDFile(dir); err != nil {
		return fmt.Errorf("engineerd: writing pid file: %w", err)
	}
	defer removePIDFile(dir)

	core := engineer.NewCore(buildEngineerConfig(id, stored), j)

	ctrl, err := ListenControl(dir, core)
	if err != nil {
		return fmt.Errorf("engineerd: starting control socket: %w", err)
	}
	defer func() { _ = ctrl.Close() }()

	result := core.Run(context.Background())
	alog.Printf("engineerd: engineer %s finished: outcome=%s summary=%s", id, result.Outcome, result.Summary)
	return nil
}

// buildEngineerConfig turns a StoredSpec into the engineer.Config
// RunDetachedTarget drives. VerifyTimeout comes out as 0 whenever
// stored.Spec.VerifyTimeoutSeconds is 0 — an old spec.json from before that
// field existed, or one built without going through config.FleetConfig's
// resolve — which is a valid "no timeout" time.Duration that verify.Run
// already treats as "bound only by ctx" per its own doc comment.
func buildEngineerConfig(id string, stored StoredSpec) engineer.Config {
	return engineer.Config{
		Spec:        stored.Spec,
		EngineerID:  id,
		ClonePath:   stored.ClonePath,
		WorktreeDir: stored.WorktreeDir,

		VerifyCommands: stored.Spec.VerifyCommands,
		VerifyTimeout:  time.Duration(stored.Spec.VerifyTimeoutSeconds) * time.Second,
	}
}

func pidFilePath(dir string) string { return filepath.Join(dir, PIDFile) }

func writePIDFile(dir string) error {
	return os.WriteFile(pidFilePath(dir), []byte(strconv.Itoa(os.Getpid())), 0o600) //nolint:gosec // 0o600, not executable
}

func removePIDFile(dir string) {
	_ = os.Remove(pidFilePath(dir))
}
