package htmlrender

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The goldens pin the markup every entry kind produces, so a change to a
// renderer, a goldmark option or the sanitizer policy shows up as a diff a human
// reads rather than as a webview that quietly renders something else.
//
// Regenerate with:
//
//	ACY_UPDATE_GOLDEN=1 go test ./internal/htmlrender/
//
// and read the diff. In this package particularly: a golden that grew a tag or
// an attribute is the question worth asking, because the input is untrusted.

// kindCase is one entry as the frame would carry it.
type kindCase struct {
	name  string
	kind  string
	title string
	body  string
	raw   string
	lang  string
}

// goldenCases covers every kind in ui's entryKinds table plus the two shapes a
// tool call takes. Missing one means a kind whose markup nothing pins.
func goldenCases() []kindCase {
	prose := "Here is the **plan**.\n\n" +
		"1. move the parser\n2. add a test\n\n" +
		"```go\nfunc main() { fmt.Println(\"hi\") }\n```\n\n" +
		"See `frame.go` and <https://example.com/docs>."

	return []kindCase{
		{name: "meta", kind: "meta", body: "settings from /p/.acy.json"},
		{name: "you", kind: "you", body: "port the parser\nand add a test"},
		{name: "claude", kind: "claude", body: prose},
		{name: "thinking", kind: "thinking", body: "The lexer is in parse.go.\nThat is where the bug is."},
		{
			name: "tool", kind: "tool", title: "Bash",
			body: "go test ./...", raw: "go test ./...", lang: "bash",
		},
		{
			// A tool whose arguments are not code: no lang, so no lexer, so a
			// preformatted preview rather than a guess.
			name: "tool-nolang", kind: "tool", title: "Read",
			body: "big.log  (offset 100)", raw: "big.log  (offset 100)",
		},
		{
			name: "tool-diff", kind: "tool", title: "Edit",
			body: "a.txt\n- foo\n+ bar", raw: "a.txt\n- foo\n+ bar", lang: "diff",
		},
		{name: "toolOK", kind: "toolOK", body: "ok\t0.412s\nPASS"},
		{name: "toolErr", kind: "toolErr", body: "exit status 1\nparse.go:12: undefined: x"},
		{name: "plan", kind: "plan", body: "1. move the parser\n2. add a test"},
		{name: "turn", kind: "turn", body: "── turn 3 · 12s · $0.42 ──"},
		{name: "complete", kind: "complete", body: "**Done.** 3 tasks, $1.25"},
		{name: "good", kind: "good", body: "✔ auto-approved · ⚙ Bash"},
		{name: "warn", kind: "warn", body: "⚠ vetoed · ⚙ Bash"},
		{name: "queued", kind: "queued", body: "also update the docs"},
		{
			// chroma has no mermaid lexer, so this falls back to preformatted
			// escaped text — the point is showing the source, not the diagram.
			name: "flow", kind: "flow",
			raw:  "flowchart TD\n    t1[\"t1: add x [todo]\"]:::todo\n",
			lang: "mermaid",
		},
	}
}

func TestEntryGoldens(t *testing.T) {
	for _, tc := range goldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := Entry(tc.kind, tc.title, tc.body, tc.raw, tc.lang)
			path := filepath.Join("testdata", tc.name+".html")

			if os.Getenv("ACY_UPDATE_GOLDEN") != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v — regenerate with ACY_UPDATE_GOLDEN=1", err)
			}
			if got != strings.TrimSuffix(string(want), "\n") {
				t.Errorf("markup changed.\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

// Every kind wears its name as a class, because that is how a client styles an
// entry: it is handed a fragment, not a switch statement.
func TestEntryCarriesItsKind(t *testing.T) {
	for _, tc := range goldenCases() {
		got := Entry(tc.kind, tc.title, tc.body, tc.raw, tc.lang)
		if want := `class="acy-entry acy-entry--` + tc.kind + `"`; !strings.Contains(got, want) {
			t.Errorf("%s: fragment does not carry %s:\n%s", tc.name, want, got)
		}
	}
}

// An empty body must not produce an empty-looking-but-present block. A tool
// with no arguments is a title and nothing else.
func TestEntryEmptyBody(t *testing.T) {
	got := Entry("toolOK", "", "   \n  ", "", "")
	if strings.Contains(got, "<pre") {
		t.Errorf("blank body produced a block: %s", got)
	}
	got = Entry("tool", "Bash", "", "", "bash")
	if want := `<div class="acy-entry acy-entry--tool"><div class="acy-entry__title">Bash</div></div>`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// The terminal caps an expandable block at maxLines because it has a fixed
// viewport and no way to scroll inside one entry. A webview has neither
// constraint and collapses long output itself, so truncating here would throw
// away text the client is expected to be able to expand to.
func TestEntryDoesNotCapLines(t *testing.T) {
	var lines []string
	for i := range 400 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	body := strings.Join(lines, "\n")

	for _, kind := range []string{"toolOK", "toolErr", "thinking"} {
		got := Entry(kind, "", body, "", "")
		if text := textOf(t, got); !strings.Contains(text, "line 399") {
			t.Errorf("%s: the 400th line is missing — something capped the block", kind)
		}
		if strings.Contains(got, "more lines") {
			t.Errorf("%s: fragment carries the terminal's truncation footer", kind)
		}
	}

	// And the same for code, which the terminal clamps too. Chroma splits one
	// line across several class spans, so this is a question about text.
	got := Entry("tool", "Write", body, body, "go")
	if text := textOf(t, got); !strings.Contains(text, "line 399") {
		t.Error("the code block was capped")
	}
}
