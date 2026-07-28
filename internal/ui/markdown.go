package ui

import (
	"strings"
	"sync"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
)

// Building a glamour renderer is comparatively expensive, and rebuild() re-renders
// every entry on each resize/tick. Cache one renderer per wrap width.
var (
	mdMu        sync.Mutex
	mdRenderers = map[int]*glamour.TermRenderer{}
)

// mdStyle is glamour's dark theme with the document margin/indent zeroed so
// prose sits flush under the "claude" badge instead of glamour's default
// 2-space margin and leading blank line. Fenced code blocks are pinned to the
// same chroma theme syntax.go uses for tool-call code, so highlighted code looks
// identical whether Claude wrote it in prose or ran it as a tool. Glamour only
// honors Theme when its inline Chroma palette is nil (see glamour/ansi/codeblock.go).
func mdStyle() ansi.StyleConfig {
	s := styles.DarkStyleConfig
	zero := uint(0)
	s.Document.Margin = &zero
	s.Document.Indent = &zero
	s.Document.BlockPrefix = ""
	s.Document.BlockSuffix = ""
	s.CodeBlock.Chroma = nil
	s.CodeBlock.Theme = chromaTheme
	return s
}

func markdownRenderer(width int) (*glamour.TermRenderer, error) {
	mdMu.Lock()
	defer mdMu.Unlock()
	if r, ok := mdRenderers[width]; ok {
		return r, nil
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(mdStyle()),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return nil, err
	}
	mdRenderers[width] = r
	return r, nil
}

// renderMarkdown renders CommonMark to ANSI-styled, width-wrapped terminal text.
// It falls back to plain width-wrapping if glamour is unavailable or errors.
func renderMarkdown(body string, width int) string {
	if width < 1 {
		width = 1
	}
	plain := func() string { return lipgloss.NewStyle().Width(width).Render(body) }

	r, err := markdownRenderer(width)
	if err != nil {
		return plain()
	}
	out, err := r.Render(body)
	if err != nil {
		return plain()
	}
	// glamour word-wraps and emits ANSI already; do not re-wrap. Trim the
	// surrounding blank lines glamour adds.
	return strings.Trim(out, "\n")
}
