//go:build darwin

package term

import "golang.org/x/sys/unix"

// fread is FREAD from <sys/file.h>: TIOCFLUSH's argument selecting the read
// (input) queue, so we discard pending input without touching pending output.
const fread = 0x1

func flushInput(fd int) error {
	return unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, fread)
}
