package engineerd

import (
	"testing"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// WriteSpec/ReadSpec is the only path spec.json ever travels through — a
// field that doesn't survive this round trip is invisible to
// RunDetachedTarget no matter what buildEngineerConfig does with it.
func TestWriteReadSpecRoundTripsVerifyConfig(t *testing.T) {
	dir := t.TempDir()
	want := StoredSpec{
		Spec: engineerwire.Spec{
			Ticket:               "T1",
			VerifyCommands:       []string{"go build ./...", "go test ./..."},
			VerifyTimeoutSeconds: 300,
		},
		ClonePath:   "/srv/repo",
		WorktreeDir: "/srv/worktree",
	}

	if err := WriteSpec(dir, want); err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	got, err := ReadSpec(dir)
	if err != nil {
		t.Fatalf("ReadSpec: %v", err)
	}

	if len(got.Spec.VerifyCommands) != len(want.Spec.VerifyCommands) {
		t.Fatalf("VerifyCommands = %v, want %v", got.Spec.VerifyCommands, want.Spec.VerifyCommands)
	}
	for i, cmd := range want.Spec.VerifyCommands {
		if got.Spec.VerifyCommands[i] != cmd {
			t.Errorf("VerifyCommands[%d] = %q, want %q", i, got.Spec.VerifyCommands[i], cmd)
		}
	}
	if got.Spec.VerifyTimeoutSeconds != want.Spec.VerifyTimeoutSeconds {
		t.Errorf("VerifyTimeoutSeconds = %d, want %d", got.Spec.VerifyTimeoutSeconds, want.Spec.VerifyTimeoutSeconds)
	}
}

// A spec.json with no verify config at all — an old file from before these
// fields existed — round-trips to a nil slice and a zero timeout, not to
// some other placeholder that would look like an explicit (empty) config.
func TestWriteReadSpecRoundTripsZeroVerifyConfig(t *testing.T) {
	dir := t.TempDir()
	want := StoredSpec{Spec: engineerwire.Spec{Ticket: "T1"}}

	if err := WriteSpec(dir, want); err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	got, err := ReadSpec(dir)
	if err != nil {
		t.Fatalf("ReadSpec: %v", err)
	}

	if len(got.Spec.VerifyCommands) != 0 {
		t.Errorf("VerifyCommands = %v, want empty", got.Spec.VerifyCommands)
	}
	if got.Spec.VerifyTimeoutSeconds != 0 {
		t.Errorf("VerifyTimeoutSeconds = %d, want 0", got.Spec.VerifyTimeoutSeconds)
	}
}
