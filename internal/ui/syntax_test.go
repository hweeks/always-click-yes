package ui

import (
	"strings"
	"testing"
)

// Unknown languages and unmatchable file names must fall back to the input
// unchanged — highlight is called unconditionally on every tool body.
func TestHighlightFallsBackUnchanged(t *testing.T) {
	const code = "some opaque text ((("
	if got := highlight(code, "no-such-language"); got != code {
		t.Errorf("unknown lang: got %q, want input unchanged", got)
	}
	if got := highlightFile(code, ""); got != code {
		t.Errorf("empty path: got %q, want input unchanged", got)
	}
	if got := highlight("", "go"); got != "" {
		t.Errorf("empty code: got %q, want empty", got)
	}
}

func TestHighlightAddsAnsiAndPreservesText(t *testing.T) {
	const code = "package main\nfunc main() {}"
	got := highlightFile(code, "/tmp/x.go")
	if !strings.Contains(got, "\x1b[") {
		t.Fatalf("expected ANSI in highlighted Go, got %q", got)
	}
	if s := stripAnsi(got); s != code {
		t.Errorf("stripped = %q, want the original code back", s)
	}
}

func TestHighlightDiff(t *testing.T) {
	got := highlight("- old line\n+ new line", "diff")
	if !strings.Contains(got, "\x1b[") {
		t.Errorf("expected ANSI in a highlighted diff, got %q", got)
	}
}
