package engineerd

import (
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// buildEngineerConfig is what RunDetachedTarget calls to turn a StoredSpec
// into the engineer.Config it drives — this exercises that mapping directly,
// without spawning the real supervisor RunDetachedTarget would otherwise
// need.
func TestBuildEngineerConfigCarriesVerifyConfig(t *testing.T) {
	stored := StoredSpec{
		Spec: engineerwire.Spec{
			Ticket:               "T1",
			VerifyCommands:       []string{"go build ./...", "go test ./..."},
			VerifyTimeoutSeconds: 300,
		},
		ClonePath:   "/srv/repo",
		WorktreeDir: "/srv/worktree",
	}

	cfg := buildEngineerConfig("e1", stored)

	if cfg.EngineerID != "e1" {
		t.Errorf("EngineerID = %q, want e1", cfg.EngineerID)
	}
	if cfg.ClonePath != stored.ClonePath || cfg.WorktreeDir != stored.WorktreeDir {
		t.Errorf("ClonePath/WorktreeDir = %q/%q, want %q/%q", cfg.ClonePath, cfg.WorktreeDir, stored.ClonePath, stored.WorktreeDir)
	}
	if len(cfg.VerifyCommands) != len(stored.Spec.VerifyCommands) {
		t.Fatalf("VerifyCommands = %v, want %v", cfg.VerifyCommands, stored.Spec.VerifyCommands)
	}
	for i, cmd := range stored.Spec.VerifyCommands {
		if cfg.VerifyCommands[i] != cmd {
			t.Errorf("VerifyCommands[%d] = %q, want %q", i, cfg.VerifyCommands[i], cmd)
		}
	}
	if want := 300 * time.Second; cfg.VerifyTimeout != want {
		t.Errorf("VerifyTimeout = %v, want %v", cfg.VerifyTimeout, want)
	}
}

// A stored spec with VerifyTimeoutSeconds left at 0 — an old spec.json from
// before the field existed, or one built without going through
// config.FleetConfig's resolve — must produce a zero time.Duration, not
// panic or fall back to some other default: verify.Run already treats 0 as
// "no timeout, bound only by ctx".
func TestBuildEngineerConfigZeroVerifyTimeout(t *testing.T) {
	stored := StoredSpec{Spec: engineerwire.Spec{Ticket: "T1"}}

	cfg := buildEngineerConfig("e1", stored)

	if cfg.VerifyTimeout != 0 {
		t.Errorf("VerifyTimeout = %v, want 0", cfg.VerifyTimeout)
	}
	if len(cfg.VerifyCommands) != 0 {
		t.Errorf("VerifyCommands = %v, want empty", cfg.VerifyCommands)
	}
}
