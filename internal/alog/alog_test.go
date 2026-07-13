package alog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openTemp points the process-wide logger at a fresh file and guarantees it is
// closed again, so one test's Open can't leak into the next. alog is global
// mutable state: these tests must never run in parallel.
func openTemp(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "acy.log")
	if _, err := Open(p); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(Close)
	return p
}

func readLog(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(b)
}

// The logger is off until Open: every acy run started without --log, and every
// `go test`, leaves it writing to io.Discard. Logging from that state must be a
// silent no-op, not a nil-writer panic — acy logs from init paths that run before
// any Open, and from goroutines that outlive Close.
//
// There is nothing to read back (the writes go nowhere by definition), so what
// this pins is that they are harmless. TestOpenRoundTrip pins that they *do* land
// once a file is open, which is the other half of the same contract.
func TestLoggingIsOffUntilOpen(t *testing.T) {
	Close() // discard state, whatever an earlier test left behind
	Printf("dropped %d", 1)
	Raw("TX", "dropped")
}

// Open -> Printf -> Close is the whole contract: the line lands in the file and
// Path names it.
func TestOpenRoundTrip(t *testing.T) {
	p := openTemp(t)

	if got := Path(); got != p {
		t.Errorf("Path() = %q, want %q", got, p)
	}
	Printf("hello %d", 42)
	Raw("TX", `{"type":"user"}`)
	Close()

	body := readLog(t, p)
	if !strings.Contains(body, "hello 42") {
		t.Errorf("log missing the Printf line:\n%s", body)
	}
	if !strings.Contains(body, `TX {"type":"user"}`) {
		t.Errorf("log missing the tagged raw payload verbatim:\n%s", body)
	}
}

// Open truncates: a resumed run must not append to the previous run's log.
func TestOpenTruncates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "acy.log")
	if err := os.WriteFile(p, []byte("stale from a previous run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(p); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(Close)
	Printf("fresh")
	Close()

	if body := readLog(t, p); strings.Contains(body, "stale") {
		t.Errorf("Open did not truncate; the old run's log survived:\n%s", body)
	}
}

// After Close the logger goes back to discarding. Writing to a closed *os.File
// would error, and Printf ignores that error — so this pins that a late log line
// (a goroutine still winding down at exit) neither panics nor corrupts the file.
func TestPrintfAfterCloseIsSilent(t *testing.T) {
	p := openTemp(t)
	Printf("before close")
	Close()

	before := readLog(t, p)
	Printf("after close")
	Raw("RX", "after close")

	if after := readLog(t, p); after != before {
		t.Errorf("a write after Close changed the file:\nbefore=%q\nafter=%q", before, after)
	}
}

// Recover is deferred at the top of every goroutine acy starts. Bubble Tea only
// recovers panics raised inside its own loop, so without this a panic in any
// other goroutine kills the process without unwinding the terminal restore —
// leaving raw mode and the alt-screen on, which is how the shell ends up spewing
// control characters afterwards. It must both stop the panic and record it.
func TestRecoverStopsPanicAndLogsStack(t *testing.T) {
	p := openTemp(t)

	// If Recover fails to swallow the panic it keeps unwinding, and this test
	// function dies with it — the failure is loud either way.
	func() {
		defer Recover("event pump")
		panic("boom")
	}()
	Close()

	body := readLog(t, p)
	if !strings.Contains(body, "panic in event pump: boom") {
		t.Errorf("log missing the panic site and value:\n%s", body)
	}
	if !strings.Contains(body, "goroutine") {
		t.Errorf("log missing the stack trace; without it a background panic is undiagnosable:\n%s", body)
	}
}
