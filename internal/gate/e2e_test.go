package gate_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/gate"
)

// buildBinary compiles the real acy binary (so the hook subcommand exists).
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "acy")
	build := exec.Command("go", "build", "-o", bin, "github.com/hweeks/always-click-yes")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// drainToEnd reads a driver's events until a turn ends, returning the session id
// seen on init (if any).
func drainToEnd(d *driver.Driver) string {
	var session string
	for ev := range d.Events() {
		if ev.IsInit() {
			session = ev.SessionID
		}
		if ev.IsTurnEnd() {
			return session
		}
	}
	return session
}

// TestGateE2E drives the full chain: claude -> PreToolUse hook -> unix socket ->
// (this test acting as supervisor) -> allow -> tool executes. Opt-in via ACY_LIVE=1.
//
//	ACY_LIVE=1 go test ./internal/gate/ -run TestGateE2E -v
func TestGateE2E(t *testing.T) {
	if os.Getenv("ACY_LIVE") == "" {
		t.Skip("set ACY_LIVE=1 to run the live gate E2E")
	}

	bin := buildBinary(t)
	sockDir := t.TempDir()
	workDir := t.TempDir()

	srv, err := gate.Listen(sockDir)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	settings, err := config.WriteHookSettings(sockDir, bin, srv.SocketPath())
	if err != nil {
		t.Fatal(err)
	}

	// Supervisor stand-in: approve every gate, remember the tool names.
	var gateCount int32
	var sawBash atomic.Bool
	go func() {
		for p := range srv.Requests() {
			t.Logf("GATE: %s %s", p.Input.ToolName, string(p.Input.ToolInput))
			atomic.AddInt32(&gateCount, 1)
			if p.Input.ToolName == "Bash" {
				sawBash.Store(true)
			}
			p.Resolve(gate.Decision{Behavior: gate.Allow, Reason: "test auto-approve"})
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	drv := driver.New(driver.Options{
		Model:          "sonnet",
		PermissionMode: "default",
		SettingsPath:   settings,
		IncludeHooks:   true,
		Cwd:            workDir,
	})
	if err := drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer drv.Stop()

	if err := drv.Send("Run exactly this shell command using the Bash tool: echo hello > out.txt"); err != nil {
		t.Fatal(err)
	}

	for ev := range drv.Events() {
		if ev.IsTurnEnd() {
			t.Logf("result: stop=%s cost=$%.4f", ev.StopReason, ev.TotalCostUSD)
			drv.Stop()
		}
	}

	if atomic.LoadInt32(&gateCount) == 0 {
		t.Fatal("no permission gate was requested — hook chain not wired")
	}
	if !sawBash.Load() {
		t.Errorf("expected a Bash gate (claude may have used another tool)")
	}
	out, err := os.ReadFile(filepath.Join(workDir, "out.txt"))
	if err != nil {
		t.Fatalf("approved tool did not run (out.txt missing): %v", err)
	}
	if !strings.Contains(string(out), "hello") {
		t.Errorf("out.txt = %q, want hello", out)
	}
	t.Logf("gates approved: %d", atomic.LoadInt32(&gateCount))
}

// TestPhaseHandoffE2E proves the PLAN->AUTO-RUN handoff: a plan is built in one
// process (plan mode, no hooks), then a fresh process resumes the SAME session
// with hooks + gating and implements it. Verifies --resume carries plan context.
// Opt-in via ACY_LIVE=1.
func TestPhaseHandoffE2E(t *testing.T) {
	if os.Getenv("ACY_LIVE") == "" {
		t.Skip("set ACY_LIVE=1 to run the live handoff E2E")
	}

	bin := buildBinary(t)
	sockDir := t.TempDir()
	workDir := t.TempDir()

	srv, err := gate.Listen(sockDir)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	settings, err := config.WriteHookSettings(sockDir, bin, srv.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for p := range srv.Requests() {
			t.Logf("GATE: %s %s", p.Input.ToolName, string(p.Input.ToolInput))
			p.Resolve(gate.Decision{Behavior: gate.Allow})
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// --- PLAN phase: describe the work, do NOT do it. Capture the session. ---
	plan := driver.New(driver.Options{Model: "sonnet", PermissionMode: "plan", Cwd: workDir})
	if err := plan.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := plan.Send("Make a one-step plan: create a file named hello.txt whose only contents are the single word banana. Do NOT create it yet — just describe the plan."); err != nil {
		t.Fatal(err)
	}
	session := drainToEnd(plan)
	plan.Stop()
	if session == "" {
		t.Fatal("no session id captured from plan phase")
	}
	t.Logf("planned session: %s", session)

	// --- AUTO-RUN phase: resume the SAME session, now with hooks + gating. ---
	auto := driver.New(driver.Options{
		Model: "sonnet", PermissionMode: "default", Cwd: workDir,
		SettingsPath: settings, IncludeHooks: true, ResumeID: session,
	})
	if err := auto.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer auto.Stop()
	// Kickoff refers only to "the plan" — the file/word live solely in the
	// resumed session's context.
	if err := auto.Send("The plan is approved. Implement it now."); err != nil {
		t.Fatal(err)
	}
	drainToEnd(auto)

	got, err := os.ReadFile(filepath.Join(workDir, "hello.txt"))
	if err != nil {
		t.Fatalf("resumed session did not implement the plan (hello.txt missing): %v", err)
	}
	if !strings.Contains(strings.ToLower(string(got)), "banana") {
		t.Fatalf("hello.txt = %q, want it to contain 'banana' (plan context lost across resume)", got)
	}
	t.Logf("handoff OK: hello.txt = %q", strings.TrimSpace(string(got)))
}
