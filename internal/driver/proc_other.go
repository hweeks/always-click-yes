//go:build !unix

package driver

import (
	"os"
	"syscall"
)

// Sessions and process groups are a unix concept; elsewhere we can only reach
// the child itself. See proc_unix.go for what this buys us there.

func detachedSysProcAttr() *syscall.SysProcAttr { return nil }

func terminateGroup(p *os.Process) error { return killGroup(p) }

func killGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Kill()
}
