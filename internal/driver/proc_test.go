//go:build unix

package driver

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
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

// The child must not be able to reach the user's terminal. Wiring its stdio to
// pipes is not enough: a controlling terminal is inherited through the session,
// not through fd 0/1/2, so without detachedSysProcAttr the child — and every tool
// subprocess claude spawns — can open /dev/tty and write to the screen or steal
// keystrokes from the TUI.
//
// go test has no controlling terminal, so re-exec under script(1) to get a pty.
// The control case (no SysProcAttr) must succeed, or the test proves nothing.
func TestDetachedChildCannotOpenTTY(t *testing.T) {
	const probe = `printf BREACH > /dev/tty`

	if os.Getenv("ACY_TTY_CHILD") != "" {
		if f, err := os.Open("/dev/tty"); err != nil {
			t.Skipf("no controlling terminal even under script(1): %v", err)
		} else {
			_ = f.Close()
		}

		// Control: an ordinary child inherits our terminal and can write to it.
		// If this ever fails, the detached case below is not proving anything.
		if err := exec.Command("sh", "-c", probe).Run(); err != nil {
			t.Fatalf("control case: an attached child could not reach /dev/tty (%v) — "+
				"the detached assertion below would be vacuous", err)
		}

		// The real assertion: detached, there is no /dev/tty to open.
		attached := exec.Command("sh", "-c", probe)
		attached.SysProcAttr = detachedSysProcAttr()
		if err := attached.Run(); err == nil {
			t.Error("a detached child still opened /dev/tty; it should have no controlling terminal")
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
	cmd := ptyCommand(exe, "-test.run", "TestDetachedChildCannotOpenTTY", "-test.v")
	cmd.Env = append(os.Environ(), "ACY_TTY_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("under a pty: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") && !strings.Contains(string(out), "SKIP") {
		t.Fatalf("inner test failed under a pty:\n%s", out)
	}
	// The control child's write lands on the pty; the detached one's must not. So
	// exactly one BREACH is expected — a second would mean detaching did nothing.
	if n := strings.Count(string(out), "BREACH"); n > 1 {
		t.Fatalf("a detached child reached the terminal (%d writes):\n%s", n, out)
	}
}
