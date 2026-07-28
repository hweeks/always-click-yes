package htmlrender

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// The adversarial tests need to assert on what a *browser* would see, not on
// what the string looks like. Substring assertions cannot do that here: a tool
// result that prints the word "onerror" is perfectly safe as text and perfectly
// fatal as an attribute, and `strings.Contains(got, "onerror")` cannot tell the
// two apart — it fails on the harmless case and would pass a payload spelled
// with an entity. So the fragment is parsed with the same html5 parser
// bluemonday sanitizes through, and the assertions walk the tree.
//
// This is also why these helpers assert positively. "no <script> in the output"
// is satisfied by a renderer that dropped the entry entirely; "the parsed tree
// contains this text, in a <pre>, and every element and attribute in it is on
// the allowlist" is not.

// parseFragment parses an entry fragment in a <div> context and returns its
// roots.
func parseFragment(t *testing.T, frag string) []*html.Node {
	t.Helper()
	ctx := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(frag), ctx)
	if err != nil {
		t.Fatalf("the fragment is not parseable HTML: %v\n%s", err, frag)
	}
	return nodes
}

// safeElements is everything this package is allowed to put in a fragment:
// markdown's structural tags, chroma's spans, and its own chrome. Anything else
// in a parsed tree came from a body, which means markup from a tool result or
// from the model was rendered as markup.
var safeElements = map[string]bool{
	"div": true, "p": true, "pre": true, "code": true, "span": true, "br": true,
	"strong": true, "em": true, "b": true, "i": true, "u": true, "s": true, "del": true,
	"a": true, "img": true, "hr": true, "blockquote": true,
	"ul": true, "ol": true, "li": true, "dl": true, "dt": true, "dd": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"table": true, "thead": true, "tbody": true, "tr": true, "th": true, "td": true,
	"sub": true, "sup": true, "abbr": true, "cite": true, "mark": true, "samp": true, "var": true,
}

// urlAttrs are the attributes a browser will resolve and fetch or navigate to.
// Their values are the only place a scheme can hide.
var urlAttrs = map[string]bool{
	"href": true, "src": true, "srcset": true, "action": true, "formaction": true,
	"data": true, "poster": true, "background": true, "cite": true, "xlink:href": true,
}

// safeSchemes is what bluemonday's standard URL policy permits. A value with no
// scheme at all (a relative path, a fragment) is fine too.
var safeSchemes = map[string]bool{"http": true, "https": true, "mailto": true}

// mustBeInert walks a rendered fragment and fails on anything a browser could
// execute: an element outside safeElements, an event-handler attribute, or a URL
// pointing at a scheme the sanitizer was supposed to reject.
func mustBeInert(t *testing.T, frag string) {
	t.Helper()
	for _, root := range parseFragment(t, frag) {
		walk(root, func(n *html.Node) {
			if n.Type != html.ElementNode {
				return
			}
			name := strings.ToLower(n.Data)
			if !safeElements[name] {
				t.Errorf("fragment contains a <%s> element:\n%s", name, frag)
			}
			for _, a := range n.Attr {
				attr := strings.ToLower(a.Key)
				// Every event handler starts with "on", so this is the whole
				// class rather than a list of the ones anyone remembered.
				if strings.HasPrefix(attr, "on") {
					t.Errorf("fragment carries the event handler %s=%q on <%s>:\n%s", attr, a.Val, name, frag)
				}
				if attr == "srcdoc" || attr == "style" {
					t.Errorf("fragment carries %s on <%s>:\n%s", attr, name, frag)
				}
				if urlAttrs[attr] {
					if s := scheme(a.Val); s != "" && !safeSchemes[s] {
						t.Errorf("fragment carries %s=%q — scheme %q:\n%s", attr, a.Val, s, frag)
					}
				}
			}
		})
	}
}

// scheme extracts a URL's scheme, lowercased, the way a browser tolerantly
// would: leading whitespace and control characters (including the tab and
// newline a `java&#9;script:` payload hides behind) are ignored, and anything
// before the first colon that is not a path or query separator is the scheme.
func scheme(v string) string {
	v = strings.Map(func(r rune) rune {
		if r <= ' ' {
			return -1
		}
		return r
	}, v)
	i := strings.IndexAny(v, ":/?#")
	if i < 0 || v[i] != ':' {
		return ""
	}
	return strings.ToLower(v[:i])
}

// textOf is the fragment's rendered text, which is what a reader actually sees.
// Chroma splits a single line of code across a dozen class spans, so "is the
// 400th line still there" is a question about text, never about the markup.
func textOf(t *testing.T, frag string) string {
	t.Helper()
	var b strings.Builder
	for _, root := range parseFragment(t, frag) {
		walk(root, func(n *html.Node) {
			if n.Type == html.TextNode {
				b.WriteString(n.Data)
			}
		})
	}
	return b.String()
}

// elements collects every element with the given name from a fragment.
func elements(t *testing.T, frag, name string) []*html.Node {
	t.Helper()
	var out []*html.Node
	for _, root := range parseFragment(t, frag) {
		walk(root, func(n *html.Node) {
			if n.Type == html.ElementNode && strings.EqualFold(n.Data, name) {
				out = append(out, n)
			}
		})
	}
	return out
}

// attr reads an attribute's value, and reports whether it is there at all —
// "the img lost its src" and "the img has an empty src" are different outcomes.
func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val, true
		}
	}
	return "", false
}

func walk(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, fn)
	}
}
