package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestList(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := t.TempDir()
	dir, err := ProjectDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	older := filepath.Join(dir, "aaaa1111.jsonl")
	newer := filepath.Join(dir, "bbbb2222.jsonl")
	writeFile(t, older, `{"type":"user","message":{"role":"user","content":"older task prompt"}}`+"\n")
	writeFile(t, newer, `{"type":"summary","summary":"newer summary line"}`+"\n"+
		`{"type":"user","message":{"role":"user","content":"ignored"}}`+"\n")

	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now().Add(-1 * time.Minute)
	if err := os.Chtimes(older, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, recent, recent); err != nil {
		t.Fatal(err)
	}

	list, err := List(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(list))
	}
	if list[0].ID != "bbbb2222" {
		t.Errorf("expected newest first, got %q", list[0].ID)
	}
	if list[0].Summary != "newer summary line" {
		t.Errorf("expected summary record used, got %q", list[0].Summary)
	}
	if !strings.Contains(list[1].Summary, "older task prompt") {
		t.Errorf("expected first user message as summary, got %q", list[1].Summary)
	}
}

func TestListMissingDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	list, err := List(t.TempDir())
	if err != nil {
		t.Fatalf("expected no error for missing dir, got %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list, got %d", len(list))
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
