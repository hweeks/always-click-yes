// Package e2e drives the whole supervisor against a real claude, on your real
// subscription. It is the only place acy is tested as the thing it actually is: a
// TUI, a gate socket, a hook subprocess, two claude sessions and a state file, all
// moving at once.
//
// Nothing here runs by default, and none of it can run in CI. Every test is gated
// on ACY_LIVE=1 (the same switch the live driver and gate tests use) because
// each one spends real tokens and takes real minutes:
//
//	ACY_LIVE=1 go test ./internal/e2e/ -v -timeout 20m
//	ACY_LIVE=1 go test ./internal/e2e/ -run TestE2EResume -v -timeout 10m
//
// Every test gets a scratch project directory, a scratch snapshot directory, and
// its own claude sessions, so a run can never touch your real work — but the
// *account* is yours, so expect a few cents and expect the model to be the model.
// A test here asserts on behavior claude cannot reasonably get wrong (a file
// exists; a phase changed; a prompt was or wasn't sent), never on its prose.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/hweeks/always-click-yes/internal/cli"
	"github.com/hweeks/always-click-yes/internal/state"
	"github.com/hweeks/always-click-yes/internal/ui"
)

// requireLive skips unless the caller has opted in to spending money.
func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("ACY_LIVE") == "" {
		t.Skip("set ACY_LIVE=1 to run the live e2e suite (it spends real tokens on your subscription)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("no claude binary on PATH")
	}
}

// acyBinary builds a real acy once per test run. The PreToolUse hook is this binary
// re-invoked as `acy hook`, and under `go test` the running executable is the test
// binary — which has no hook subcommand. Without a real binary to point at, every
// gated tool would hang forever waiting for a hook that can't answer.
var acyBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "acy-e2e-bin-")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "acy")

	root, err := filepath.Abs("../..")
	if err != nil {
		return "", err
	}
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build acy: %w\n%s", err, out)
	}
	return bin, nil
})

// harness is one supervised run, driven without a terminal.
type harness struct {
	t   *testing.T
	sup *cli.Supervisor

	mu     sync.Mutex
	model  ui.Model
	closed bool

	msgs chan tea.Msg
	done chan struct{}
	ctx  context.Context
}

// options configures a harness. The zero value is a fresh run in a scratch project.
type options struct {
	Cwd       string        // scratch project; defaults to a new temp dir
	Resume    string        // resume this session id
	Continue  bool          // resume the newest run in Cwd
	Countdown time.Duration // gate countdown; short, so tests don't wait 30s per tool
	Model     string

	// ParentTools is the supervising session's --tools registry. Empty means
	// the product default, which is what a test of real behaviour wants.
	ParentTools []string
}

// newHarness wires a real supervisor — real gate socket, real hook settings, real
// claude launcher, real state files — and runs its Bubble Tea model
// headlessly, so a test can send keys and read the transcript without a terminal.
//
// It deliberately does not use tea.NewProgram: a Program wants a TTY and owns its
// own goroutine, and a test needs to *interleave* with the model — send a key, wait
// for a phase, read what was written to disk. Update() is a pure function of
// (model, msg), so a plain loop over the commands it returns is the whole runtime.
func newHarness(t *testing.T, opt options) *harness {
	t.Helper()
	requireLive(t)

	bin, err := acyBinary()
	if err != nil {
		t.Fatalf("build acy: %v", err)
	}

	if opt.Cwd == "" {
		opt.Cwd = t.TempDir()
	}
	if opt.Countdown == 0 {
		opt.Countdown = 2 * time.Second // long enough to veto in a test, short enough not to bore one
	}
	// An empty --tools list means the FULL registry, not the default one, so the
	// default has to be applied here: the harness builds cli.Flags directly and
	// never sees cobra's flag defaults. Getting this wrong would hand the
	// supervising session Write and Edit and quietly invalidate every test that
	// asserts it delegates.
	if opt.ParentTools == nil {
		opt.ParentTools = cli.DefaultParentTools
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sup, err := cli.NewSupervisor(ctx, cli.Flags{
		Bin:       "claude",
		Cwd:       opt.Cwd,
		HookBin:   bin,
		Model:     opt.Model,
		Countdown: opt.Countdown,
		MaxLines:  10,
		// The real parent registry, not a stub. It used to be a deliberately
		// useless single tool, which was fine when the plan phase only had to
		// avoid writing — but the supervising session's registry is now the
		// thing under test: it is what stops the parent doing the work itself
		// and makes it delegate instead.
		PlanTools: opt.ParentTools,
		LogPath:   filepath.Join(t.TempDir(), "acy-debug.log"),
		Resume:    opt.Resume,
		Continue:  opt.Continue,
	})
	if err != nil {
		t.Fatalf("wire supervisor: %v", err)
	}
	t.Cleanup(sup.Close)

	h := &harness{
		t:     t,
		sup:   sup,
		model: sup.Model,
		msgs:  make(chan tea.Msg, 256),
		done:  make(chan struct{}),
		ctx:   ctx,
	}

	// Init reads the model, so take its command *before* the loop goroutine can start
	// writing to it.
	init := h.model.Init()
	go h.loop()

	// Bubble Tea's first message is always the window size; the model stays unready
	// (and renders nothing) until it arrives.
	h.send(tea.WindowSizeMsg{Width: 100, Height: 40})
	h.exec(init)
	return h
}

// loop is the event loop: apply a message, run whatever commands come back.
func (h *harness) loop() {
	defer close(h.done)
	for {
		select {
		case <-h.ctx.Done():
			return
		case msg := <-h.msgs:
			if _, ok := msg.(tea.QuitMsg); ok {
				return
			}
			h.mu.Lock()
			next, cmd := h.model.Update(msg)
			h.model = next.(ui.Model)
			h.mu.Unlock()
			h.exec(cmd)
		}
	}
}

// exec runs a command off the loop and feeds its message back in, which is exactly
// what tea.Program does. Batched commands fan out; nil commands are no-ops.
func (h *harness) exec(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		switch m := msg.(type) {
		case nil:
			return
		case tea.BatchMsg:
			for _, c := range m {
				h.exec(c)
			}
		default:
			h.send(msg)
		}
	}()
}

func (h *harness) send(msg tea.Msg) {
	select {
	case h.msgs <- msg:
	case <-h.ctx.Done():
	}
}

// typeAndSend puts text in the composer and presses Enter, the way a user would.
//
// It waits for the session first. Launching claude is asynchronous, and a message
// sent before the driver lands is silently dropped — a real user cannot type that
// fast, but a test can, and the failure looks exactly like claude never answering.
func (h *harness) typeAndSend(text string) {
	h.t.Helper()
	h.waitFor("the claude session to launch", 60*time.Second, func(m ui.Model) bool {
		return m.HasDriver()
	})
	for _, r := range text {
		h.rune(r)
	}
	h.key(tea.KeyPressMsg{Code: tea.KeyEnter})
}

// keyCtrlG arms the run; keyCtrlX vetoes the gate in front. v2 has no KeyCtrlG
// constant to name them with: a modified key is its base code plus a modifier
// bit, which is the same change that lets shift+enter be told apart from enter.
//
// The gate matches on msg.String(), so a Code/Mod pair that stringifies to
// anything else would fall through to the composer and simply type — which is
// exactly how the veto test rotted when the bindings moved off bare letters.
// TestKeyChordsStringifyAsTheGateExpects pins that down, and it runs in CI.
var (
	keyCtrlG = tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl}
	keyCtrlX = tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}
)

func (h *harness) key(k tea.KeyPressMsg) { h.send(k) }

// rune presses a printable key. Text is what the model reads as typed input, so
// a key with a Code but no Text would move the cursor and insert nothing.
func (h *harness) rune(r rune) { h.send(tea.KeyPressMsg{Code: r, Text: string(r)}) }

// read borrows the model under the lock. Every assertion goes through here.
func (h *harness) read(fn func(ui.Model)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(h.model)
}

// waitFor polls until cond holds, and fails the test with the transcript if it
// never does. Polling is the honest primitive here: the thing we are waiting for is
// a real model finishing a real turn, and it takes as long as it takes.
func (h *harness) waitFor(what string, timeout time.Duration, cond func(ui.Model) bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var ok bool
		h.read(func(m ui.Model) { ok = cond(m) })
		if ok {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	var dump string
	h.read(func(m ui.Model) { dump = m.Transcript() })
	h.t.Fatalf("timed out after %s waiting for %s\n--- transcript ---\n%s", timeout, what, dump)
}

// stop tears the run down the way a crash would: the driver dies, and nothing gets
// a chance to tidy up. That is the state a resume has to cope with.
func (h *harness) crash() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	h.model.StopDriver()
}

// snapshotFor reads what acy persisted for a session — the file a resume reads back.
func snapshotFor(t *testing.T, id string) (state.Snapshot, bool) {
	t.Helper()
	s, ok, err := state.Load(id)
	if err != nil {
		t.Fatalf("load snapshot %s: %v", id, err)
	}
	return s, ok
}

// deleteSnapshot forgets everything acy knew about a session, leaving only claude's
// transcript — which is the exact state of a session acy never supervised.
func deleteSnapshot(t *testing.T, id string) {
	t.Helper()
	path, err := state.Path(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}
}

// scratchProject returns a temp dir to run claude in, isolated from the real repo,
// with an isolated snapshot directory to match. A live test must never be able to
// write to the project it is being run from.
func scratchProject(t *testing.T) string {
	t.Helper()
	t.Setenv(state.EnvDir, t.TempDir())
	return t.TempDir()
}

// readFileIn is a convenience for asserting that claude actually did the work.
func readFileIn(t *testing.T, dir, name string) (string, error) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // both halves are the test's own temp dir
	if errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return strings.TrimSpace(string(b)), nil
}
