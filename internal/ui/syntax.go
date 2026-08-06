package ui

import (
	"path/filepath"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// chromaTheme is the one syntax palette the whole UI uses: tool-call code is
// highlighted with it here, and mdStyle pins glamour's fenced-code blocks to the
// same theme so Claude's prose and the tool transcript read as one system.
const chromaTheme = "dracula"

// highlight colors code with ANSI escapes for the named language. It returns the
// input unchanged when the language is unknown or highlighting fails, so callers
// can use it unconditionally.
func highlight(code, lang string) string {
	return highlightWith(lexers.Get(lang), code)
}

// highlightFile picks the language from the file name and highlights accordingly.
func highlightFile(code, path string) string {
	if path == "" {
		return code
	}
	return highlightWith(lexers.Match(filepath.Base(path)), code)
}

// langForFile names the language chroma would pick for a file, lowercased, so a
// renderer that is not a terminal (the webview) can ask its own highlighter for
// the same language rather than re-deriving the extension table. Empty when
// nothing matches — the caller must treat that as "plain text", not as an error.
func langForFile(path string) string {
	if path == "" {
		return ""
	}
	lexer := lexers.Match(filepath.Base(path))
	if lexer == nil {
		return ""
	}
	return strings.ToLower(lexer.Config().Name)
}

// highlightWith runs chroma's 256-color terminal formatter over code. Highlighting
// happens once, at ingest time — rebuild() used to re-render every entry on each
// tick while a countdown is up; it now memoizes across ticks (see
// rendercache.go), but still re-lexes every entry on a resize, and re-lexing the
// transcript at that rate would burn CPU for no visual gain.
func highlightWith(lexer chroma.Lexer, code string) string {
	if lexer == nil || strings.TrimSpace(code) == "" {
		return code
	}
	it, err := chroma.Coalesce(lexer).Tokenise(nil, code)
	if err != nil {
		return code
	}
	var b strings.Builder
	if err := formatters.Get("terminal256").Format(&b, styles.Get(chromaTheme), it); err != nil {
		return code
	}
	return strings.TrimRight(b.String(), "\n")
}
