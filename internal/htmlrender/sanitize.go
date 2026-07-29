package htmlrender

import (
	"sync"

	"github.com/microcosm-cc/bluemonday"
)

// policy is the second line of defence.
//
// The first is that nothing in this package ever asks a renderer to emit raw
// HTML: goldmark is built without WithUnsafe, and every other kind is escaped
// before it becomes markup. The policy exists for the case where that is wrong —
// a goldmark option someone adds later, a chroma formatter that starts emitting
// an attribute it didn't, a kind that falls through a branch nobody updated. A
// sanitizer that only ever sees safe input is doing its job.
//
// UGCPolicy is the starting point because that is exactly what this is: the
// output of a markdown conversion. It already drops every event-handler
// attribute (nothing is allowed unless named) and restricts href/src to
// http/https/mailto, which is what makes a javascript: link inert.
//
// It is extended in precisely one direction. UGCPolicy refuses "class" globally
// — the comment in bluemonday says "we are not allowing users to style their own
// content", which is the right instinct for a forum and the wrong one here,
// because chroma's class-based formatter has nothing *but* classes and the CSP
// forbids the inline styles that would be the alternative. So class is permitted
// on the three elements chroma writes, and nowhere else, matched against
// bluemonday's own space-separated-token pattern so a class can never carry a
// quote out of its attribute.
var policy = sync.OnceValue(func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("class").Matching(bluemonday.SpaceSeparatedTokens).OnElements("span", "code", "pre")
	return p
})

// sanitize strips everything the policy does not explicitly permit.
func sanitize(fragment string) string {
	if fragment == "" {
		return ""
	}
	return policy().Sanitize(fragment)
}
