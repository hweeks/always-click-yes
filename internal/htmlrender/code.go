package htmlrender

import (
	"fmt"
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// Theme selects which chroma palette StyleSheet emits. It is a property of the
// *stylesheet*, never of a rendered entry: the formatter below writes class
// names and no colors at all, so a client switches theme by swapping one CSS
// document and never by re-rendering a transcript it already has.
type Theme int

const (
	// ThemeDark must stay dracula. It is the same palette chromaTheme pins in
	// internal/ui/syntax.go, and the point of naming it twice is that the
	// terminal and the webview highlight the same file the same colors — a run
	// watched in both at once should not look like two programs.
	ThemeDark Theme = iota

	// ThemeLight is dracula's counterpart for a light editor. github is chosen
	// because it is the light style people have already been reading code in for
	// a decade; a light theme's job is to be unremarkable.
	ThemeLight
)

func (t Theme) String() string {
	switch t {
	case ThemeDark:
		return "dracula"
	case ThemeLight:
		return "github"
	}
	return ""
}

// formatter is chroma's HTML formatter in class mode.
//
// WithClasses(true) is not a preference. The webview's Content-Security-Policy
// has no unsafe-inline, so a style="color:#f8f8f2" attribute — which is what
// this formatter emits by default — would be dropped by the browser and the code
// would render unhighlighted. Classes plus StyleSheet is the only shape that
// survives the CSP, and it is also what lets the theme change without touching a
// single stored entry.
var formatter = chromahtml.New(chromahtml.WithClasses(true))

// codeBlock highlights source as a <pre class="chroma"> block.
//
// It mirrors highlightWith in internal/ui/syntax.go, including its contract:
// anything it cannot do — an unknown language, a lexer error — falls back to the
// text rendered plainly rather than to an error, so a caller can use it
// unconditionally.
func codeBlock(code, lang string) string {
	if strings.TrimSpace(code) == "" {
		return ""
	}
	lexer := lexers.Get(lang)
	if lexer == nil {
		return preText(code)
	}
	it, err := chroma.Coalesce(lexer).Tokenise(nil, code)
	if err != nil {
		return preText(code)
	}
	var b strings.Builder
	if err := formatter.Format(&b, chromaStyle(ThemeDark), it); err != nil {
		return preText(code)
	}
	return b.String()
}

// StyleSheet is the chroma CSS for a theme: the class definitions the fragments
// Entry produces are written against.
//
// It is a separate document from the entries on purpose. Every entry is rendered
// once, at ingest, and carries no color; the theme lives entirely here, so a
// client that follows the editor from dark to light re-fetches one stylesheet
// and re-renders nothing.
func StyleSheet(t Theme) (string, error) {
	name := t.String()
	style, ok := styles.Registry[name]
	if !ok || style == nil {
		// Deliberately an error rather than chroma's silent fallback style: the
		// dark theme has to be the same dracula the terminal uses, and a chroma
		// upgrade that dropped or renamed it would otherwise show up as a webview
		// that quietly stopped matching `acy run`.
		return "", fmt.Errorf("htmlrender: chroma has no style %q", name)
	}
	var b strings.Builder
	if err := formatter.WriteCSS(&b, style); err != nil {
		return "", fmt.Errorf("htmlrender: writing the %s stylesheet: %w", name, err)
	}
	return b.String(), nil
}

// chromaStyle is the style to hand the formatter while formatting. In class mode
// it decides nothing about the output — the classes come from token types — but
// the formatter still dereferences it, so it must never be nil.
func chromaStyle(t Theme) *chroma.Style {
	if s, ok := styles.Registry[t.String()]; ok && s != nil {
		return s
	}
	return styles.Fallback
}
