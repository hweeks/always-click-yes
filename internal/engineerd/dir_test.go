package engineerd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootDirUsesACYStateDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ACY_STATE_DIR", dir)

	got, err := RootDir()
	if err != nil {
		t.Fatalf("RootDir: %v", err)
	}
	want := filepath.Join(dir, "engineers")
	if got != want {
		t.Errorf("RootDir() = %q, want %q", got, want)
	}
}

func TestRootDirFallsBackToUserConfigDir(t *testing.T) {
	t.Setenv("ACY_STATE_DIR", "")

	got, err := RootDir()
	if err != nil {
		t.Fatalf("RootDir: %v", err)
	}
	if !strings.Contains(got, filepath.Join("acy", "engineers")) {
		t.Errorf("RootDir() = %q, want it to end in acy/engineers", got)
	}
}

func TestDirRejectsUnsafeIDs(t *testing.T) {
	t.Setenv("ACY_STATE_DIR", t.TempDir())

	for _, id := range []string{"", "..", "../escape", "a/b", `a\b`, ".hidden"} {
		if _, err := Dir(id); err == nil {
			t.Errorf("Dir(%q) = nil error, want a rejection", id)
		}
	}
}

func TestEnsureDirCreatesTheDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ACY_STATE_DIR", root)

	id := NewID()
	dir, err := EnsureDir(id)
	if err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	want := filepath.Join(root, "engineers", id)
	if dir != want {
		t.Errorf("EnsureDir(%q) = %q, want %q", id, dir, want)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("EnsureDir did not create %q: %v", dir, err)
	}
}

func TestNewIDIsUniqueAndShort(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := NewID()
		if !strings.HasPrefix(id, "e") {
			t.Fatalf("NewID() = %q, want an e<seconds>-<hex> id", id)
		}
		if seen[id] {
			t.Fatalf("NewID() produced a duplicate: %q", id)
		}
		seen[id] = true
	}
}
