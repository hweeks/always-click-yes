package verify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func requireBinary(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not on PATH: %v", name, err)
	}
}

func TestDefaultRunnerEcho(t *testing.T) {
	requireBinary(t, "echo")
	out, exitCode, err := DefaultRunner(context.Background(), "/tmp", os.Environ(), []string{"echo", "hello"})
	if err != nil {
		t.Fatalf("DefaultRunner: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("output = %q, want it to contain %q", out, "hello")
	}
}

func TestDefaultRunnerFalse(t *testing.T) {
	requireBinary(t, "false")
	_, exitCode, err := DefaultRunner(context.Background(), "/tmp", os.Environ(), []string{"false"})
	if err != nil {
		t.Fatalf("DefaultRunner: %v", err)
	}
	if exitCode != 1 {
		t.Errorf("exitCode = %d, want 1", exitCode)
	}
}

func TestDefaultRunnerTimeout(t *testing.T) {
	requireBinary(t, "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := DefaultRunner(ctx, "/tmp", os.Environ(), []string{"sleep", "5"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestDefaultRunnerAbsentBinary(t *testing.T) {
	const missing = "acy-verify-definitely-not-a-real-binary"
	if _, err := exec.LookPath(missing); err == nil {
		t.Skipf("%s unexpectedly found on PATH", missing)
	}
	_, exitCode, err := DefaultRunner(context.Background(), "/tmp", os.Environ(), []string{missing})
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("err = %v, want ErrNotInstalled", err)
	}
	if exitCode != -1 {
		t.Errorf("exitCode = %d, want -1", exitCode)
	}
}
