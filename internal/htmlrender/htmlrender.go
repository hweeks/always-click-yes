// Package htmlrender turns one transcript entry into an HTML fragment.
//
// It exists because the second front end — a VS Code webview fed by an HTTP
// server — has to render the same entries the terminal does without shipping a
// JavaScript markdown or syntax-highlighting library. The webview's CSP forbids
// unsafe-inline, so styling travels as classes and a stylesheet (StyleSheet)
// rather than as inline style attributes, and the prose is turned into markup
// here, in Go, by the same chroma the TUI already highlights with.
//
// It takes primitives — kind, title, body, raw, lang — rather than a ui.Entry,
// and imports nothing from internal/ui. That is not tidiness: ui imports this
// package to stamp the HTML onto an entry at ingest, so a dependency back the
// other way would be an import cycle.
//
// # Everything here is untrusted
//
// A body is model output or a raw tool result. A `git log` that prints a commit
// message containing <script>, a grep hit on an HTML file, a model quoting an
// onerror attribute — all of them arrive here verbatim and all of them must come
// out inert. Two independent things make that true, and neither is allowed to be
// the only one:
//
//   - goldmark is never given WithUnsafe, so raw HTML in markdown is dropped
//     rather than passed through, and every non-markdown kind is escaped here
//     rather than parsed at all.
//   - every fragment is then run through bluemonday (see policy), which knows
//     nothing about how it was produced and would strip a <script> that some
//     future renderer bug let through.
package htmlrender

import (
	"html"
	"strings"
)

// Entry kinds, as they travel on the wire. These are the values in the
// entryKinds table in internal/ui/frame.go, duplicated here as literals rather
// than shared: they are protocol, and the two packages agreeing by accident of a
// shared constant would hide a rename that broke a client. The kinds this file
// does not name fall to the default branch, which is the safe one.
const (
	kindClaude   = "claude"
	kindPlan     = "plan"
	kindComplete = "complete"
	kindTool     = "tool"
	kindToolOK   = "toolOK"
	kindToolErr  = "toolErr"
	kindThinking = "thinking"
	kindFlow     = "flow"
)

// Entry renders one transcript entry as a self-contained HTML fragment.
//
// The arguments are the frame's Entry fields: kind selects the treatment, title
// is the tool name where there is one, body is the ANSI-stripped plain text, raw
// is the unhighlighted source behind a tool body, and lang names raw's language.
//
// The wrapper is this package's own markup and is therefore trusted; everything
// inside it came from the run and is sanitized. That split is why the wrapper
// keeps its classes while the content is only allowed the handful chroma needs:
// the sanitizer never sees the chrome, so the chrome never has to be permitted
// by a policy loose enough to also permit a class an attacker chose.
//
// There is no error return, matching highlight() in internal/ui: a body that
// cannot be rendered as markdown or highlighted as code falls back to escaped
// text, because a transcript entry that renders plainly is a far better outcome
// than one that does not render at all.
func Entry(kind, title, body, raw, lang string) string {
	var b strings.Builder
	b.WriteString(`<div class="acy-entry acy-entry--`)
	b.WriteString(html.EscapeString(kind))
	b.WriteString(`">`)
	if title != "" {
		b.WriteString(`<div class="acy-entry__title">`)
		b.WriteString(html.EscapeString(title))
		b.WriteString(`</div>`)
	}
	b.WriteString(sanitize(entryBody(kind, body, raw, lang)))
	b.WriteString(`</div>`)
	return b.String()
}

// entryBody renders the content of an entry, before sanitizing.
//
// Note what is deliberately missing: a line cap. renderEntry in internal/ui
// clamps tool output, results and thinking to maxLines because a terminal has a
// fixed viewport and no way to scroll inside one entry. A webview has neither
// constraint — it collapses a long block itself, with the whole thing already in
// the DOM — so truncating here would throw away text the client is expected to
// be able to expand to.
func entryBody(kind, body, raw, lang string) string {
	switch kind {
	case kindClaude, kindPlan, kindComplete:
		// The model writes markdown, and these three are the kinds that carry its
		// prose: ordinary replies, the proposed plan, and the completion summary.
		return markdown(body)

	case kindTool:
		// raw, not body: body is what the terminal shows, which for a highlighted
		// tool is chroma's terminal256 output with the escapes taken back out
		// again. raw is the source that was highlighted, which is what a second
		// highlighter needs. An empty lang means toolBodyParts found no language
		// (a Read, a Grep) — that body is an argument preview, not code, so it
		// stays preformatted rather than being lexed as something it isn't.
		if lang != "" {
			return codeBlock(raw, lang)
		}
		return preText(raw)

	case kindToolOK, kindToolErr, kindThinking:
		return preText(body)

	case kindFlow:
		// body carries both halves — the ascii lanes followed by a fenced
		// mermaid block, exactly as render.go's eFlow case shows them in the
		// terminal — while raw is the mermaid source alone. Rendering raw
		// here would show the webview only half of what the terminal shows,
		// the very divergence Frame exists to prevent. chroma has no mermaid
		// lexer, so codeBlock(raw, lang) would only ever fall back to the
		// same escaped preformatted text preText gives anyway, which is why
		// there is no highlighting to lose by rendering body as plain text
		// instead.
		return preText(body)
	}

	// meta, you, turn, good, warn, queued — one-liners and short messages, where
	// markdown would be an invitation to interpret a user's asterisks and a <pre>
	// would refuse to wrap.
	return textLines(body)
}

// preText is escaped text in a <pre>: monospaced, whitespace preserved, and
// incapable of being anything else because every markup character is already an
// entity by the time it gets there.
func preText(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return `<pre class="acy-pre">` + html.EscapeString(s) + `</pre>`
}

// textLines is escaped text with its line breaks preserved as <br>, for the
// entries that are prose-shaped but not markdown.
func textLines(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = html.EscapeString(ln)
	}
	return strings.Join(lines, "<br>\n")
}
