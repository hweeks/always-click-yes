package engineerd

import (
	"os"
	"testing"
)

// shortTempDir is t.TempDir() with a short, test-name-independent leaf: a
// control.sock lives under this dir, and unix's sockaddr_un caps the whole
// path at about 104 bytes on macOS — comfortably shorter than
// t.TempDir()'s own path, which embeds this package's (deliberately
// descriptive, and therefore long) test names.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "engd")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
