package term

import (
	"os"
	"testing"
)

// DrainInput runs unconditionally at startup, so it has to tolerate a stdin that
// is not a terminal at all — a pipe under `go test`, a redirect in CI, `acy < f`.
// A nil file is the degenerate case of that and must not panic.
func TestDrainInputOnNilIsSafe(t *testing.T) {
	DrainInput(nil)
}

// On a non-tty the ioctl fails with ENOTTY and DrainInput swallows it. The
// important part is that it swallows the *error* without swallowing the *data*:
// if the ENOTTY path ever turned into a real read, a piped stdin would silently
// lose whatever was buffered in it.
func TestDrainInputOnPipeKeepsData(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	const want = "hello"
	if _, err := w.WriteString(want); err != nil {
		t.Fatal(err)
	}
	w.Close()

	DrainInput(r)

	buf := make([]byte, len(want))
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("read after drain: %v", err)
	}
	if got := string(buf[:n]); got != want {
		t.Errorf("piped stdin held %q after the drain, want %q — the drain ate data it should have ignored", got, want)
	}
}
