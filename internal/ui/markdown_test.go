package ui

import (
	"strings"
	"testing"
)

func TestRenderMarkdownStyles(t *testing.T) {
	out := renderMarkdown("This is **bold** text.", 60)
	if strings.Contains(out, "**") {
		t.Errorf("expected markdown bold markers to be consumed, got:\n%q", out)
	}
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("expected ANSI styling in output, got:\n%q", out)
	}
	if !strings.Contains(out, "bold") {
		t.Errorf("expected the word 'bold' to survive rendering, got:\n%q", out)
	}
}

func TestRenderMarkdownList(t *testing.T) {
	out := renderMarkdown("- one\n- two\n", 60)
	// Bullets should be rendered, not left as literal leading dashes.
	if strings.Contains(out, "- one") {
		t.Errorf("expected list markers to be styled, got:\n%q", out)
	}
	if !strings.Contains(out, "one") || !strings.Contains(out, "two") {
		t.Errorf("expected list item text to survive, got:\n%q", out)
	}
}

func TestRenderMarkdownRendererCached(t *testing.T) {
	_ = renderMarkdown("x", 42)
	_ = renderMarkdown("y", 42)
	mdMu.Lock()
	_, ok := mdRenderers[42]
	mdMu.Unlock()
	if !ok {
		t.Error("expected a cached renderer for width 42")
	}
}
