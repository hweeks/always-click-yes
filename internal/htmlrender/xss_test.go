package htmlrender

import (
	"strings"
	"testing"
)

// Every body this package renders is untrusted. Claude writes the prose, and a
// tool result is whatever a command printed — a grep over an HTML file, a commit
// message someone wrote, a page a WebFetch pulled down. None of it is escaped by
// the time it arrives, and all of it has to come out inert.
//
// mustBeInert (inert_test.go) parses the fragment and walks it, so these assert
// on what a browser would build rather than on what the string spells.

// A tool result full of script. This is the realistic one: `cat index.html`.
func TestXSSScriptInAToolResult(t *testing.T) {
	body := "matches in index.html:\n<script>alert(1)</script>\n<img src=x onerror=alert(1)>"
	got := Entry("toolOK", "", body, "", "")
	mustBeInert(t, got)

	// And the result is still *readable*. A tool result whose text vanished is a
	// transcript that lies about what the command printed, so the payload has to
	// survive — as text, inside a <pre>, which is exactly where it is harmless.
	pres := elements(t, got, "pre")
	if len(pres) != 1 {
		t.Fatalf("want one <pre>, got %d:\n%s", len(pres), got)
	}
	text := textOf(t, got)
	for _, want := range []string{
		"matches in index.html:",
		"<script>alert(1)</script>",
		"<img src=x onerror=alert(1)>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the text %q is not in the rendered output:\n%s", want, got)
		}
	}
	// The tags are text nodes, not elements — which is the whole claim.
	if els := elements(t, got, "script"); len(els) != 0 {
		t.Errorf("a <script> element was built:\n%s", got)
	}
	if els := elements(t, got, "img"); len(els) != 0 {
		t.Errorf("an <img> element was built:\n%s", got)
	}
}

// The same payloads through the markdown path, where a renderer *could* be told
// to pass raw HTML through and deliberately is not.
func TestXSSRawHTMLInClaudeProse(t *testing.T) {
	body := "Here is the fix.\n\n<script>alert(1)</script>\n\n" +
		"<img src=x onerror=alert(1)>\n\n" +
		"Inline <b onmouseover=alert(1)>bold</b> and that is all."
	got := Entry("claude", "", body, "", "")
	mustBeInert(t, got)

	if els := elements(t, got, "script"); len(els) != 0 {
		t.Errorf("a <script> element was built:\n%s", got)
	}
	// The prose around it is intact — goldmark dropped the raw HTML, it did not
	// abandon the document.
	text := textOf(t, got)
	for _, want := range []string{"Here is the fix.", "and that is all."} {
		if !strings.Contains(text, want) {
			t.Errorf("prose %q was lost:\n%s", want, got)
		}
	}
}

// A markdown link is a link goldmark builds itself — the href is not raw HTML,
// so nothing about "we never enable unsafe" touches it. This is the case the
// sanitizer's URL policy exists for.
func TestXSSJavascriptURLInAMarkdownLink(t *testing.T) {
	body := "click [here](javascript:alert(1)) or [there](JaVaScRiPt:alert(2))" +
		" or ![pic](javascript:alert(3))"
	got := Entry("claude", "", body, "", "")
	mustBeInert(t, got)

	// Nothing navigable survives: every <a> lost its href and every <img> its
	// src, rather than keeping one pointing at a scheme the browser might yet
	// resolve. (bluemonday drops the whole <a> when its only attribute goes; the
	// assertion is written to hold either way, because which one it does is
	// bluemonday's business and not this package's contract.)
	for _, a := range elements(t, got, "a") {
		if v, ok := attr(a, "href"); ok {
			t.Errorf("an <a> kept href=%q:\n%s", v, got)
		}
	}
	for _, img := range elements(t, got, "img") {
		if v, ok := attr(img, "src"); ok {
			t.Errorf("an <img> kept src=%q:\n%s", v, got)
		}
	}
	// The link text stays: the sentence is still readable.
	text := textOf(t, got)
	for _, want := range []string{"click", "here", "there"} {
		if !strings.Contains(text, want) {
			t.Errorf("link text %q was lost:\n%s", want, got)
		}
	}

	// And an ordinary link is untouched, or the assertions above would pass for
	// the wrong reason — a policy that dropped every href would look identical.
	ok := Entry("claude", "", "read [the docs](https://example.com/x)", "", "")
	links := elements(t, ok, "a")
	if len(links) != 1 {
		t.Fatalf("want one link, got %d:\n%s", len(links), ok)
	}
	if href, has := attr(links[0], "href"); !has || href != "https://example.com/x" {
		t.Errorf("a plain https link did not survive: href=%q (present=%v)", href, has)
	}
}

// A fenced code block whose content is HTML. chroma lexes it and emits the
// markup character by character wrapped in class spans, so this is where an
// escaping bug in the formatter — or a policy that let an attribute through —
// would show up.
func TestXSSHTMLInsideAFencedCodeBlock(t *testing.T) {
	body := "how to embed it:\n\n```html\n<script>alert(1)</script>\n" +
		"<a href=\"javascript:alert(2)\">x</a>\n```"
	got := Entry("claude", "", body, "", "")
	mustBeInert(t, got)

	// The code is shown, highlighted, as text: the block is chroma's, the tags
	// inside it are text nodes, and there is no <script> or <a> element at all.
	if !strings.Contains(got, `<pre class="chroma">`) {
		t.Errorf("the fence did not render as a chroma block:\n%s", got)
	}
	for _, name := range []string{"script", "a"} {
		if els := elements(t, got, name); len(els) != 0 {
			t.Errorf("the fenced code built a real <%s>:\n%s", name, got)
		}
	}
	text := textOf(t, got)
	for _, want := range []string{"<script>alert(1)</script>", `<a href="javascript:alert(2)">x</a>`} {
		if !strings.Contains(text, want) {
			t.Errorf("the code %q is not shown as text:\n%s", want, got)
		}
	}
}

// The same, arriving as a tool call's source rather than as prose — Write on an
// .html file is the everyday version of this.
func TestXSSHTMLInAToolCodeBlock(t *testing.T) {
	src := "<div onclick=\"alert(1)\">hi</div>\n<script src=\"//evil\"></script>"
	got := Entry("tool", "Write", src, src, "html")
	mustBeInert(t, got)

	if !strings.Contains(got, `<pre class="chroma">`) {
		t.Errorf("the tool source did not render as a chroma block:\n%s", got)
	}
	if els := elements(t, got, "script"); len(els) != 0 {
		t.Errorf("the tool source built a real <script>:\n%s", got)
	}
	// Every <div> is this package's own chrome, so none of them carries the
	// onclick the source spells out — and the source is legible as text.
	if !strings.Contains(textOf(t, got), `<div onclick="alert(1)">hi</div>`) {
		t.Errorf("the source is not shown as text:\n%s", got)
	}
}

// The mermaid source is a ticket title/id a model or Jira integration chose,
// so it is untrusted the same way a tool's source is. chroma has no mermaid
// lexer, so this exercises codeBlock's plain fallback rather than its
// tokenised path — the other side of the tool coverage above, where a lexer
// exists.
func TestXSSInAFlowDiagram(t *testing.T) {
	raw := "flowchart TD\n    t1[\"<script>alert(1)</script>\"]:::todo\n" +
		"    t2[\"<img src=x onerror=alert(1)>\"]:::todo\n"
	got := Entry("flow", "", "", raw, "mermaid")
	mustBeInert(t, got)

	if els := elements(t, got, "script"); len(els) != 0 {
		t.Errorf("a <script> element was built:\n%s", got)
	}
	if els := elements(t, got, "img"); len(els) != 0 {
		t.Errorf("an <img> element was built:\n%s", got)
	}
	text := textOf(t, got)
	for _, want := range []string{"<script>alert(1)</script>", "<img src=x onerror=alert(1)>"} {
		if !strings.Contains(text, want) {
			t.Errorf("the mermaid source %q is not shown as text:\n%s", want, got)
		}
	}
}

// The title is a tool name claude chose, so it is untrusted too — and it is the
// one string this package writes into its own trusted chrome.
func TestXSSInTheTitle(t *testing.T) {
	got := Entry("tool", `Bash"><script>alert(1)</script>`, "echo hi", "echo hi", "bash")
	mustBeInert(t, got)
	if !strings.Contains(got, `<div class="acy-entry__title">`) {
		t.Errorf("the title block is malformed:\n%s", got)
	}
	if !strings.Contains(textOf(t, got), `Bash"><script>alert(1)</script>`) {
		t.Errorf("the title is not shown as text:\n%s", got)
	}
}

// So is the kind, which becomes a class name. It should never be anything but a
// value from ui's entryKinds table, but "should never" is not a guarantee, and
// this one lands inside an attribute.
func TestXSSInTheKind(t *testing.T) {
	got := Entry(`meta" onmouseover="alert(1)`, "", "hello", "", "")
	mustBeInert(t, got)
	if !strings.Contains(textOf(t, got), "hello") {
		t.Errorf("the body was lost:\n%s", got)
	}
}

// The kinds that are neither markdown nor code still escape. meta, you, turn,
// good, warn and queued are short strings the TUI prints verbatim, and a client
// inserting them as innerHTML must be as safe as the rest.
func TestXSSInPlainKinds(t *testing.T) {
	for _, kind := range []string{"meta", "you", "turn", "good", "warn", "queued"} {
		got := Entry(kind, "", "<script>alert(1)</script>\n<img src=x onerror=alert(1)>", "", "")
		mustBeInert(t, got)
		if els := elements(t, got, "script"); len(els) != 0 {
			t.Errorf("%s: a <script> element was built:\n%s", kind, got)
		}
		if !strings.Contains(textOf(t, got), "<script>alert(1)</script>") {
			t.Errorf("%s: the text was dropped rather than escaped:\n%s", kind, got)
		}
		// The line break survives as markup, because these entries are prose-
		// shaped and the TUI renders them on two lines.
		if len(elements(t, got, "br")) != 1 {
			t.Errorf("%s: the line break was lost:\n%s", kind, got)
		}
	}
}

// An unknown kind must fall to the safe branch, not to the markdown one. A kind
// nobody anticipated should render as text — the same instinct
// readOnlyParentTools follows in internal/ui: the default is the cautious one.
func TestXSSUnknownKindFallsToEscapedText(t *testing.T) {
	got := Entry("someFutureKind", "", "**not bold** <script>alert(1)</script>", "", "")
	mustBeInert(t, got)
	if !strings.Contains(got, "**not bold**") {
		t.Errorf("an unknown kind was rendered as markdown:\n%s", got)
	}
}

// A scheme a browser would resolve can hide behind whitespace and control
// characters that a naive check reads straight past. bluemonday normalises the
// value before deciding, and this pins that it does.
func TestXSSObfuscatedSchemes(t *testing.T) {
	for _, href := range []string{
		"java\tscript:alert(1)",
		"java\nscript:alert(1)",
		" javascript:alert(1)",
		"JAVASCRIPT:alert(1)",
		"data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==",
		"vbscript:msgbox(1)",
	} {
		got := Entry("claude", "", "[x]("+strings.ReplaceAll(href, " ", "%20")+")", "", "")
		mustBeInert(t, got)
		for _, a := range elements(t, got, "a") {
			if v, ok := attr(a, "href"); ok {
				t.Errorf("href %q survived as %q:\n%s", href, v, got)
			}
		}
	}
}
