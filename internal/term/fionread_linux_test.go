//go:build linux

package term

// fionread is FIONREAD from <asm-generic/ioctls.h>, the same value as TIOCINQ.
const fionread = 0x541b
