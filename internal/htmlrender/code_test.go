package htmlrender

import (
	"strings"
	"testing"
)

// Both themes have to produce CSS, and it has to be class-based CSS: the
// webview's CSP has no unsafe-inline, so a stylesheet is the only place color
// can live at all.
func TestStyleSheets(t *testing.T) {
	for _, tc := range []struct {
		theme Theme
		name  string
	}{
		{ThemeDark, "dracula"},
		{ThemeLight, "github"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			css, err := StyleSheet(tc.theme)
			if err != nil {
				t.Fatalf("StyleSheet: %v", err)
			}
			if tc.theme.String() != tc.name {
				t.Errorf("theme names %q, want %q", tc.theme, tc.name)
			}
			if len(css) < 500 {
				t.Errorf("stylesheet is %d bytes — that is not a syntax theme:\n%s", len(css), css)
			}
			// The selectors the fragments are written against. .chroma is the
			// <pre> wrapper; .k and .s are keyword and string, the two token
			// classes any style has to define for highlighting to mean anything.
			for _, sel := range []string{".chroma", ".chroma .k", ".chroma .s"} {
				if !strings.Contains(css, sel+" ") && !strings.Contains(css, sel+" {") {
					t.Errorf("no rule for %s in the %s stylesheet", sel, tc.name)
				}
			}
			// Class-based means exactly this: no style attributes anywhere in a
			// rendered entry, and all the color here.
			if !strings.Contains(css, "color:") && !strings.Contains(css, "color: ") {
				t.Errorf("the %s stylesheet defines no colors", tc.name)
			}
		})
	}
}

// The dark theme must stay dracula, matching chromaTheme in internal/ui/syntax.go.
// A run watched in the terminal and the webview at once should not look like two
// different programs, and this is the only thing holding the two names together.
func TestDarkThemeMatchesTheTerminal(t *testing.T) {
	if got := ThemeDark.String(); got != "dracula" {
		t.Errorf("ThemeDark = %q, want dracula (chromaTheme in internal/ui/syntax.go)", got)
	}
}

// A theme chroma does not have is an error rather than chroma's silent fallback
// style: a chroma upgrade that renamed dracula would otherwise show up as a
// webview that quietly stopped matching the terminal.
func TestStyleSheetUnknownTheme(t *testing.T) {
	if _, err := StyleSheet(Theme(99)); err == nil {
		t.Error("an unknown theme produced a stylesheet")
	}
}

// A rendered entry carries no color of its own. This is what makes the theme a
// property of the stylesheet — a client switches by swapping one CSS document
// and re-renders nothing — and it is what the CSP requires.
func TestEntriesCarryNoInlineStyles(t *testing.T) {
	for _, tc := range goldenCases() {
		got := Entry(tc.kind, tc.title, tc.body, tc.raw, tc.lang)
		if strings.Contains(got, "style=") {
			t.Errorf("%s: fragment carries an inline style, which the CSP will drop:\n%s", tc.name, got)
		}
		if strings.Contains(got, "#") && strings.Contains(got, "color") {
			t.Errorf("%s: fragment looks like it carries a color:\n%s", tc.name, got)
		}
	}
}

// An unknown language is plain text, not an error and not a guess. lang comes
// from chroma's own lexer table via langForFile, but "" is a normal value and a
// name chroma dropped in an upgrade must not take an entry down with it.
func TestCodeBlockUnknownLanguage(t *testing.T) {
	for _, lang := range []string{"", "no-such-language", "brainfuck-9000"} {
		got := codeBlock("some text", lang)
		if !strings.Contains(got, "some text") {
			t.Errorf("lang %q lost the code: %s", lang, got)
		}
		if strings.Contains(got, "chroma") {
			t.Errorf("lang %q was highlighted anyway: %s", lang, got)
		}
	}
}
