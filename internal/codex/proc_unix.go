//go:build unix

package codex

// Duplicated from internal/driver/proc_unix.go rather than shared: these
// helpers are unexported, and internal/driver must not be modified to export
// them (this task adds a package, it does not touch existing ones). They are
// small and stable enough that duplicating them here is cheaper than the
// churn of carving out a new shared internal package for three functions.

import (
	"os"
	"syscall"
)

// detachedSysProcAttr starts codex in a new session, detaching it from acy's
// controlling terminal.
//
// Wiring the child's stdio to pipes is not enough: a controlling terminal is
// inherited through the session, not through fd 0/1/2. Without Setsid, codex —
// and every shell subprocess or MCP server it spawns — can still open
// /dev/tty and reach the user's terminal behind the TUI's back: writes land as
// stray control characters, reads steal keystrokes from Bubble Tea.
//
// Being a session leader also makes the child its own process-group leader,
// which is what lets signalGroup reach its descendants.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// terminateGroup asks the child's whole process group to exit.
func terminateGroup(p *os.Process) error { return signalGroup(p, syscall.SIGTERM) }

// killGroup kills the child's whole process group. codex spawns shell
// subprocesses and MCP servers; killing only codex orphans them, and an orphan
// still holding our tty could keep writing to it long after acy exits.
func killGroup(p *os.Process) error { return signalGroup(p, syscall.SIGKILL) }

// signalGroup signals the process group led by p. A negative pid addresses the
// group; detachedSysProcAttr guarantees the child's pid is also its group id.
func signalGroup(p *os.Process, sig syscall.Signal) error {
	if p == nil {
		return nil
	}
	return syscall.Kill(-p.Pid, sig)
}
