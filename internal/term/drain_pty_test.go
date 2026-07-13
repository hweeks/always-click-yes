//go:build darwin || linux

package term

import (
	"bytes"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// ptyCommand builds a command that runs argv under a pseudo-terminal via
// script(1). The two script(1) lineages take the command differently, and
// LookPath can't tell them apart because both are named "script":
//
//	BSD/macOS:   script [options] [file] [command ...]
//	util-linux:  script [options] [file]   — command only via -c, extra
//	             operands are a usage error
//
// So dispatch on GOOS rather than probing. -e makes util-linux exit with the
// child's status, matching the BSD behaviour the caller relies on.
//
// Duplicated from internal/driver/proc_test.go: both are unexported test-only
// helpers in different packages, and one shared testutil package for two callers
// is not worth the indirection.
func ptyCommand(argv ...string) *exec.Cmd {
	if runtime.GOOS == "linux" {
		return exec.Command("script", "-qe", "-c", shellJoin(argv), "/dev/null")
	}
	return exec.Command("script", append([]string{"-q", "/dev/null"}, argv...)...)
}

// shellJoin renders argv as a single single-quoted shell word list, for the
// util-linux -c form. The test binary's path can contain anything.
func shellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// pending reports how many bytes are sitting unread in the terminal's input
// queue. FIONREAD is the whole reason this test can be deterministic: it observes
// the queue without consuming it, so the test can prove the type-ahead arrived,
// drain it, and prove it is gone — with no sleeps and no destructive reads.
func pending(fd int) (int, error) {
	return unix.IoctlGetInt(fd, fionread)
}

// awaitPending polls until the input queue is non-empty, so the assertion below
// is never racing the parent's write.
func awaitPending(fd int) (int, error) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		n, err := pending(fd)
		if err != nil {
			return 0, err
		}
		if n > 0 {
			return n, nil
		}
		if time.Now().After(deadline) {
			return 0, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// flushInput must actually discard queued terminal input. This is the assertion
// that pins the per-GOOS ioctl: BSD TIOCFLUSH takes an int*, Linux TCFLSH takes
// the arg by value, and getting either wrong fails silently — the ioctl returns
// no error and the stale OSC 11 / CSI 6n replies stay queued to be parsed as
// phantom keys, which is the bug this package exists to prevent.
//
// go test has no controlling terminal, so re-exec under script(1) to get a pty.
// script forwards the parent's stdin into the pty master, which is how the child
// gets type-ahead sitting in its own input queue.
//
// The control case (data really is queued before the flush) is what stops the
// assertion from being vacuous: a flushInput that did nothing at all would still
// "pass" a test that never had anything to flush.
func TestDrainFlushesPendingInput(t *testing.T) {
	if os.Getenv("ACY_DRAIN_CHILD") != "" {
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			t.Skipf("no controlling terminal even under script(1): %v", err)
		}
		defer tty.Close()
		fd := int(tty.Fd())

		// Control: the parent's type-ahead reaches our input queue. Without this,
		// the flush below would have nothing to do and would prove nothing.
		n, err := awaitPending(fd)
		if err != nil {
			t.Skipf("FIONREAD unsupported on this tty: %v", err)
		}
		if n == 0 {
			t.Skip("no type-ahead arrived through script(1); nothing to flush, assertion would be vacuous")
		}
		t.Logf("control: %d bytes queued before the drain", n)

		if err := flushInput(fd); err != nil {
			t.Fatalf("flushInput on a real tty: %v", err)
		}

		left, err := pending(fd)
		if err != nil {
			t.Fatalf("FIONREAD after flush: %v", err)
		}
		if left != 0 {
			t.Errorf("%d bytes still queued after flushInput; the input queue was not flushed", left)
		}
		return
	}

	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("no script(1) to give the test a controlling terminal")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// Hold the write end open for the child's lifetime: on EOF script(1) may close
	// the master and hang up the pty before the child has looked at its queue.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Close()

	var out bytes.Buffer
	cmd := ptyCommand(exe, "-test.run", "TestDrainFlushesPendingInput", "-test.v")
	cmd.Env = append(os.Environ(), "ACY_DRAIN_CHILD=1")
	cmd.Stdin = pr
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.WaitDelay = 30 * time.Second

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pr.Close()

	// The type-ahead. The trailing newline matters: in canonical mode FIONREAD
	// reports nothing until a line is complete.
	if _, err := pw.WriteString("TYPEAHEAD\n"); err != nil {
		t.Fatal(err)
	}

	err = cmd.Wait()
	if err != nil {
		t.Fatalf("under a pty: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "PASS") && !strings.Contains(out.String(), "SKIP") {
		t.Fatalf("inner test failed under a pty:\n%s", out.String())
	}
	t.Logf("inner run:\n%s", out.String())
}
