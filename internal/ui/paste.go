package ui

import (
	"os"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
)

// attachPaste inserts a pasted file reference into the composer as ordinary
// editable text, and reports whether it claimed the paste. The paths go in
// plain — no chip, no hidden state — so the user can backspace one, type around
// it, and read exactly what the message will say.
//
// Nothing is read here, deliberately. The supervising session's registry is
// Read/Grep/Glob (see ParentSystemPrompt), so a path is the whole payload:
// inlining the file would spend the tokens acy exists to save, and a screenshot
// works for free because claude's own Read handles images.
func (m *Model) attachPaste(text string) bool {
	cwd := m.cwd
	if cwd == "" {
		// A run always has one; a test model may not. Falling back keeps a
		// relative paste resolvable rather than silently never matching.
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	paths, ok := resolvePaths(text, cwd, os.Stat)
	if !ok {
		return false
	}
	ins := formatPaths(paths)
	// Keep the path a separate word from whatever was already typed, and leave the
	// cursor past a trailing space so the sentence can just continue.
	if v := m.input.Value(); v != "" && !strings.HasSuffix(v, " ") && !strings.HasSuffix(v, "\n") {
		ins = " " + ins
	}
	m.input.InsertString(ins + " ")
	m.attached = append(m.attached, paths...)
	return true
}

// clearComposer empties the message box and drops the attachment note with it.
// The note is a description of the box's contents, so the two can only ever be
// cleared together — every send, queue and command path goes through here.
func (m *Model) clearComposer() {
	m.input.Reset()
	m.attached = nil
}

// maxPastedPaths caps how many tokens a paste may hold and still be considered a
// list of files. Without it every pasted paragraph would be tokenised and
// stat'ed word by word; with it, anything longer than a plausible drag of a few
// files is prose by definition and never touches the filesystem.
const maxPastedPaths = 8

// resolvePaths decides whether a pasted string is *entirely* file references and,
// if so, returns them as cleaned absolute paths.
//
// It is pure apart from the injected stat, so the rule can be tested without a
// filesystem — which matters, because the rule is the whole feature: guess wrong
// in one direction and a dragged file lands as shell gibberish, guess wrong in
// the other and a sentence mentioning Makefile turns into a path.
//
// The two guards that keep it honest are the token cap above and the separator
// test in isPathish: a bare word is never a path here even when a file by that
// name exists in cwd.
func resolvePaths(text, cwd string, stat func(string) (os.FileInfo, error)) ([]string, bool) {
	toks, ok := splitPasteTokens(text)
	if !ok || len(toks) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(toks))
	for _, tok := range toks {
		if !isPathish(tok) {
			return nil, false
		}
		p, ok := absPath(tok, cwd)
		if !ok {
			return nil, false
		}
		if _, err := stat(p); err != nil {
			return nil, false // one miss and the whole paste is text again
		}
		out = append(out, p)
	}
	return out, true
}

// isPathish is the false-positive guard: a token only counts as a file reference
// if it *looks* addressed — it carries a separator or starts at the home
// directory. Statting bare words instead would mean pasting "Makefile" into a
// sentence silently became a path, and which words qualify would then depend on
// the working directory rather than on what was typed.
func isPathish(tok string) bool {
	if tok == "" {
		return false
	}
	if strings.HasPrefix(tok, "~") {
		return true
	}
	return strings.ContainsRune(tok, '/') || strings.ContainsRune(tok, filepath.Separator)
}

// absPath expands a leading ~ and resolves a relative path against cwd. cwd is
// the model's project directory; when it is empty the process's own working
// directory stands in, which is the same thing for every real run.
func absPath(tok, cwd string) (string, bool) {
	if tok == "~" || strings.HasPrefix(tok, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		tok = filepath.Join(home, strings.TrimPrefix(tok, "~"))
	}
	if filepath.IsAbs(tok) {
		return filepath.Clean(tok), true
	}
	if cwd == "" {
		abs, err := filepath.Abs(tok)
		if err != nil {
			return "", false
		}
		return abs, true
	}
	return filepath.Join(cwd, tok), true
}

// splitPasteTokens splits a paste the way a shell would split a drag-and-drop:
// whitespace separates, a backslash escapes the next character, and single or
// double quotes hold a run together. Terminals pick different ones of those
// three for the same dropped file (Terminal.app escapes, others quote), so all
// of them have to work or dragging a file with a space in its name is a coin
// flip.
//
// ok is false for an unterminated quote or for more than maxPastedPaths tokens —
// both mean "stop scanning, this is text".
func splitPasteTokens(text string) ([]string, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, false
	}
	var (
		toks  []string
		cur   strings.Builder
		quote rune // 0, '\'' or '"'
		open  bool // cur holds a token, even if it is the empty string ("")
	)
	flush := func() {
		if open {
			toks = append(toks, cur.String())
			cur.Reset()
			open = false
		}
	}
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\\' && quote != '\'' && i+1 < len(runes):
			// A backslash is literal inside single quotes; everywhere else it hands
			// the next rune through untouched, which is how an escaped space in a
			// dropped path survives.
			i++
			cur.WriteRune(runes[i])
			open = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			open = true
		case r == '\'' || r == '"':
			quote = r
			open = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
			open = true
		}
		if len(toks) > maxPastedPaths {
			return nil, false
		}
	}
	if quote != 0 {
		return nil, false // unterminated quote: not a path list we can trust
	}
	flush()
	if len(toks) > maxPastedPaths {
		return nil, false
	}
	return toks, true
}

// formatPaths renders resolved paths as the text the composer receives:
// space-separated, and quoted when a path contains a space so the message stays
// unambiguous about where one path ends and the next begins.
func formatPaths(paths []string) string {
	quoted := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.ContainsAny(p, " \t") {
			p = `"` + p + `"`
		}
		quoted = append(quoted, p)
	}
	return strings.Join(quoted, " ")
}

// attachNote is the dim line under the composer naming what a paste attached.
// Past two paths it counts instead of listing, and a path too long for the row
// is elided from the *left*: the note must stay one row (a wrapped footer would
// re-size the transcript, since layout() measures what footerView renders), and
// truncating from the tail like everything else here would cut off the word
// "attached" and leave a bare path with no explanation of why it is there.
//
// width <= 0 means "don't measure" — the note comes back whole. agent is the
// capitalized name (Model.agentProse) naming who will read the attachment.
func attachNote(paths []string, width int, agent string) string {
	if len(paths) == 0 {
		return ""
	}
	const prefix = "📎 "
	if len(paths) > 2 {
		return prefix + plural(len(paths), "path") + " attached — " + agent + " will read them"
	}
	suffix := " attached — " + agent + " will read it"
	list := strings.Join(paths, ", ")
	if avail := width - lipgloss.Width(prefix) - lipgloss.Width(suffix); width > 0 && lipgloss.Width(list) > avail {
		// Split what is left evenly and keep each path's tail: the file name is the
		// part that identifies it, the directory prefix is the part that is long.
		each := (avail - 2*(len(paths)-1)) / len(paths)
		short := make([]string, 0, len(paths))
		for _, p := range paths {
			short = append(short, elideLeft(p, each))
		}
		list = strings.Join(short, ", ")
	}
	return prefix + list + suffix
}

// elideLeft keeps the last n cells of s, marking the cut with a leading ellipsis.
func elideLeft(s string, n int) string {
	r := []rune(s)
	if n < 2 || len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-(n-1):])
}
