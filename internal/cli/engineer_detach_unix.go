//go:build unix

package cli

import (
	"os/exec"
	"syscall"
)

// detachChild starts cmd fully detached from this process: a new session, so
// it survives `acy engineer start` exiting and inherits no controlling
// terminal (internal/driver/proc_unix.go's detachedSysProcAttr is the same
// idea, for the same reason — the child must not be able to reach a
// terminal behind the caller's back).
func detachChild(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
