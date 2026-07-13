//go:build darwin

package term

// fionread is FIONREAD from <sys/filio.h>: _IOR('f', 127, int). Hand-defined for
// the same reason drain_darwin.go hand-defines fread — x/sys/unix does not export
// it on darwin.
const fionread = 0x4004667f
