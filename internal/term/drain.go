// Package term holds the terminal fixups Bubble Tea doesn't do for us.
package term

import "os"

// DrainInput discards whatever is queued on the terminal's input buffer.
//
// Bubble Tea v1 calls lipgloss.HasDarkBackground() from its package init(), so
// before main() even runs, termenv writes an OSC 11 background-color query *and*
// a CSI 6n cursor-position request to the tty (bubbletea's tea_init.go calls this
// a workaround and says it goes away in v2). Two of termenv's three exit paths
// return without consuming the terminal's replies, and it restores ECHO on the
// way out — so bytes like ESC]11;rgb:… and ESC[42;1R can still be sitting in the
// input queue when we take the terminal. Bubble Tea then parses them as phantom
// key events, or the shell echoes them as control characters once we exit.
//
// We never use the answer (the palette is static and glamour gets an explicit
// style), so the whole exchange is pure cost. Draining the queue right before the
// program starts reading throws the stale replies away. It also drops type-ahead,
// which is what we want: the TUI should start from a clean slate.
//
// A non-tty stdin (tests, pipes) is a no-op.
func DrainInput(f *os.File) {
	if f == nil {
		return
	}
	_ = flushInput(int(f.Fd()))
}
