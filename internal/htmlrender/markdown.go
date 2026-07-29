package htmlrender

import (
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

// md is the one markdown converter, built once.
//
// Two things about how it is configured are load-bearing:
//
//   - There is no goldmark/renderer/html.WithUnsafe. Goldmark does not pass raw
//     HTML through unless it is told to, and it is deliberately not told to: a
//     tool result full of <script> arrives here as markdown like anything else,
//     and the renderer replaces the raw block with a comment the sanitizer then
//     drops. Adding WithUnsafe would defeat this package's entire reason to
//     exist in Go rather than in the client.
//
//   - Fenced code goes through chroma, via the node renderer below, rather than
//     through a new module dependency (goldmark-highlighting). chroma is already
//     a direct dependency and already configured in class mode for the CSP, so
//     the extension would only be a wrapper around a formatter this package
//     holds anyway — and it would be a second place where the theme and the
//     class prefix could drift from the terminal's.
//
// GFM is on because the model writes GFM: tables, strikethrough and bare URLs
// that should be links. Its task-list checkboxes render as <input>, which the
// policy strips — a checkbox is not markup a transcript needs.
var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(
		// Priority below goldmark's own renderer (1000). Node renderers are
		// registered in reverse priority order, so the lower number registers
		// last and its code-block funcs win.
		renderer.WithNodeRenderers(util.Prioritized(&chromaCodeBlocks{}, 100)),
	),
)

// markdown converts CommonMark+GFM to HTML, falling back to escaped text with
// its line breaks kept if the conversion fails. goldmark only errors on a write
// failure to the buffer, so the fallback is theory — but a transcript entry that
// renders as plain prose beats one that renders as nothing.
func markdown(src string) string {
	if strings.TrimSpace(src) == "" {
		return ""
	}
	var b strings.Builder
	if err := md.Convert([]byte(src), &b); err != nil {
		return textLines(src)
	}
	return b.String()
}

// chromaCodeBlocks renders fenced and indented code blocks with chroma instead
// of goldmark's <pre><code class="language-go">, so a webview gets the same
// per-token markup for code in prose that it gets for a tool call's code.
type chromaCodeBlocks struct{}

func (r *chromaCodeBlocks) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindFencedCodeBlock, r.render)
	reg.Register(ast.KindCodeBlock, r.render)
}

func (r *chromaCodeBlocks) render(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	var lang string
	if fenced, ok := node.(*ast.FencedCodeBlock); ok {
		lang = string(fenced.Language(source))
	}

	// A code block's text lives in its line segments, not in child nodes — which
	// is also why this returns WalkSkipChildren: there is nothing below to walk,
	// and letting goldmark descend would emit the text a second time, unescaped.
	var code strings.Builder
	lines := node.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		code.Write(seg.Value(source))
	}

	// codeBlock falls back to an escaped <pre> for an unknown or absent
	// language, which is exactly right for a fence with no info string.
	if _, err := w.WriteString(codeBlock(code.String(), lang)); err != nil {
		return ast.WalkStop, err
	}
	return ast.WalkSkipChildren, nil
}
