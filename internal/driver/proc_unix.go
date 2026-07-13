//go:build unix

package driver

import (
	"os"
	"syscall"
)

// detachedSysProcAttr starts claude in a new session, detaching it from acy's
// controlling terminal.
//
// Wiring the child's stdio to pipes is not enough: a controlling terminal is
// inherited through the session, not through fd 0/1/2. Without Setsid, claude —
// and every tool subprocess it spawns — can still open /dev/tty and reach the
// user's terminal behind the TUI's back: writes land as stray control
// characters, reads steal keystrokes from Bubble Tea.
//
// Being a session leader also makes the child its own process-group leader,
// which is what lets signalGroup reach its descendants.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// terminateGroup asks the child's whole process group to exit.
func terminateGroup(p *os.Process) error { return signalGroup(p, syscall.SIGTERM) }

// killGroup kills the child's whole process group. claude spawns tool
// subprocesses and MCP servers; killing only claude orphans them, and an orphan
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
