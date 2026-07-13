//go:build !darwin && !linux

package term

// Flushing the tty input queue is an ioctl whose constants we only carry for
// darwin and linux; elsewhere, live with the stale replies.
func flushInput(int) error { return nil }
