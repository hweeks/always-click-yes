package ui

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/hweeks/always-click-yes/internal/driver"
)

// fakeInfo satisfies os.FileInfo without implementing any of it: resolvePaths
// only ever looks at the error, so a nil embedded interface documents that the
// value is never inspected — and panics loudly if that ever stops being true.
type fakeInfo struct{ fs.FileInfo }

// fakeStat is the injected filesystem: exactly these paths exist, nothing else.
func fakeStat(exists ...string) func(string) (os.FileInfo, error) {
	set := make(map[string]bool, len(exists))
	for _, p := range exists {
		set[p] = true
	}
	return func(p string) (os.FileInfo, error) {
		if set[p] {
			return fakeInfo{}, nil
		}
		return nil, fs.ErrNotExist
	}
}

// The rule the whole feature rests on: what counts as "this paste is paths".
// Every case here is a string some terminal actually produces on a drag, or a
// piece of prose that must never be mistaken for one.
func TestResolvePaths(t *testing.T) {
	const home = "/home/tester"
	t.Setenv("HOME", home)

	cases := []struct {
		name   string
		text   string
		cwd    string
		exists []string
		want   []string
		wantOK bool
	}{{
		name:   "plain absolute path",
		text:   "/tmp/plan.md",
		cwd:    "/work",
		exists: []string{"/tmp/plan.md"},
		want:   []string{"/tmp/plan.md"},
		wantOK: true,
	}, {
		name:   "surrounding whitespace and a trailing newline",
		text:   "  /tmp/plan.md\n",
		cwd:    "/work",
		exists: []string{"/tmp/plan.md"},
		want:   []string{"/tmp/plan.md"},
		wantOK: true,
	}, {
		name:   "backslash-escaped spaces",
		text:   `/tmp/my\ file\ name.md`,
		cwd:    "/work",
		exists: []string{"/tmp/my file name.md"},
		want:   []string{"/tmp/my file name.md"},
		wantOK: true,
	}, {
		name:   "single-quoted path",
		text:   `'/tmp/my file.md'`,
		cwd:    "/work",
		exists: []string{"/tmp/my file.md"},
		want:   []string{"/tmp/my file.md"},
		wantOK: true,
	}, {
		name:   "double-quoted path",
		text:   `"/tmp/my file.md"`,
		cwd:    "/work",
		exists: []string{"/tmp/my file.md"},
		want:   []string{"/tmp/my file.md"},
		wantOK: true,
	}, {
		name:   "tilde expands to the home directory",
		text:   "~/notes.md",
		cwd:    "/work",
		exists: []string{home + "/notes.md"},
		want:   []string{home + "/notes.md"},
		wantOK: true,
	}, {
		name:   "two paths on two lines",
		text:   "/tmp/a.md\n/tmp/b.md",
		cwd:    "/work",
		exists: []string{"/tmp/a.md", "/tmp/b.md"},
		want:   []string{"/tmp/a.md", "/tmp/b.md"},
		wantOK: true,
	}, {
		name:   "two space-separated paths",
		text:   "/tmp/a.md /tmp/b.md",
		cwd:    "/work",
		exists: []string{"/tmp/a.md", "/tmp/b.md"},
		want:   []string{"/tmp/a.md", "/tmp/b.md"},
		wantOK: true,
	}, {
		name:   "a directory counts",
		text:   "/tmp/some-dir",
		cwd:    "/work",
		exists: []string{"/tmp/some-dir"},
		want:   []string{"/tmp/some-dir"},
		wantOK: true,
	}, {
		name:   "relative path resolves against cwd",
		text:   "internal/ui/view.go",
		cwd:    "/work",
		exists: []string{"/work/internal/ui/view.go"},
		want:   []string{"/work/internal/ui/view.go"},
		wantOK: true,
	}, {
		name:   "a sentence naming a real file stays text",
		text:   "check the Makefile for the build target",
		cwd:    "/work",
		exists: []string{"/work/Makefile"},
		wantOK: false,
	}, {
		name:   "a path that does not exist stays text",
		text:   "/tmp/nope.md",
		cwd:    "/work",
		exists: []string{"/tmp/plan.md"},
		wantOK: false,
	}, {
		name:   "a bare existing filename with no separator stays text",
		text:   "Makefile",
		cwd:    "/work",
		exists: []string{"/work/Makefile"},
		wantOK: false,
	}, {
		name:   "one bad path poisons the whole paste",
		text:   "/tmp/a.md /tmp/gone.md",
		cwd:    "/work",
		exists: []string{"/tmp/a.md"},
		wantOK: false,
	}, {
		name:   "a path inside a sentence stays text",
		text:   "please read /tmp/a.md first",
		cwd:    "/work",
		exists: []string{"/tmp/a.md"},
		wantOK: false,
	}, {
		name:   "more paths than the cap stays text",
		text:   strings.TrimSpace(strings.Repeat("/tmp/a.md ", maxPastedPaths+1)),
		cwd:    "/work",
		exists: []string{"/tmp/a.md"},
		wantOK: false,
	}, {
		name:   "an unterminated quote stays text",
		text:   `"/tmp/a.md`,
		cwd:    "/work",
		exists: []string{"/tmp/a.md"},
		wantOK: false,
	}, {
		name:   "empty text",
		text:   "",
		cwd:    "/work",
		exists: []string{"/work"},
		wantOK: false,
	}, {
		name:   "whitespace only",
		text:   "   \n\t ",
		cwd:    "/work",
		exists: []string{"/work"},
		wantOK: false,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolvePaths(tc.text, tc.cwd, fakeStat(tc.exists...))
			if ok != tc.wantOK {
				t.Fatalf("resolvePaths(%q) ok = %v, want %v (got %v)", tc.text, ok, tc.wantOK, got)
			}
			if !tc.wantOK {
				if got != nil {
					t.Errorf("a rejected paste returned paths: %v", got)
				}
				return
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("resolvePaths(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// A path with a space has to come back out quotable, or two attached files read
// as three.
func TestFormatPathsQuotesSpaces(t *testing.T) {
	got := formatPaths([]string{"/tmp/a.md", "/tmp/my file.md"})
	want := `/tmp/a.md "/tmp/my file.md"`
	if got != want {
		t.Errorf("formatPaths = %q, want %q", got, want)
	}
}

// Past two paths the note counts rather than lists, so it cannot wrap and grow
// the footer layout() measures.
func TestAttachNoteCountsPastTwoPaths(t *testing.T) {
	if got := attachNote(nil, 80, "Claude"); got != "" {
		t.Errorf("attachNote(nil) = %q, want empty", got)
	}
	if got := attachNote([]string{"/tmp/a.md"}, 80, "Claude"); !strings.Contains(got, "/tmp/a.md") {
		t.Errorf("attachNote does not name the single path: %q", got)
	}
	got := attachNote([]string{"/tmp/a.md", "/tmp/b.md", "/tmp/c.md"}, 80, "Claude")
	if !strings.Contains(got, "3 paths") || strings.Contains(got, "/tmp/a.md") {
		t.Errorf("attachNote = %q, want a count rather than a list", got)
	}
}

// A long path must lose its *front*, never the explanation on its tail: a bare
// truncated path with no "attached" on the end says nothing about why it is
// there, and the line has to stay one row either way.
func TestAttachNoteElidesALongPathAndStaysOneRow(t *testing.T) {
	const width = 60
	long := "/Users/someone/very/deeply/nested/project/dir/internal/ui/view.go"

	got := attachNote([]string{long}, width, "Claude")
	if lipgloss.Width(got) > width {
		t.Errorf("note is %d cells wide, want <= %d: %q", lipgloss.Width(got), width, got)
	}
	if !strings.HasSuffix(got, "attached — Claude will read it") {
		t.Errorf("note lost its explanation: %q", got)
	}
	if !strings.Contains(got, "view.go") {
		t.Errorf("note lost the file name: %q", got)
	}
}

// tempFile writes a file into a fresh directory and returns both.
func tempFile(t *testing.T, name string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("# plan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

// The wiring: a dropped file becomes an absolute path in the composer, and the
// footer says so.
func TestPasteOfAFilePathAttachesIt(t *testing.T) {
	dir, path := tempFile(t, "plan.md")

	m := sizedModel(t)
	m.cwd = dir
	tm, _ := m.Update(tea.PasteMsg{Content: path})
	m = tm.(Model)

	if !strings.Contains(m.input.Value(), path) {
		t.Errorf("composer = %q, want it to hold %q", m.input.Value(), path)
	}
	footer := stripAnsi(m.footerView())
	if !strings.Contains(footer, "attached") || !strings.Contains(footer, "plan.md") {
		t.Errorf("footer does not report the attachment:\n%s", footer)
	}
}

// The escaped form Terminal.app produces for a dragged file, resolved against
// the real filesystem rather than a fake one.
func TestPasteOfAnEscapedFilePathAttachesIt(t *testing.T) {
	dir, path := tempFile(t, "my plan.md")

	m := sizedModel(t)
	m.cwd = dir
	tm, _ := m.Update(tea.PasteMsg{Content: strings.ReplaceAll(path, " ", `\ `)})
	m = tm.(Model)

	// Quoted on the way in, because the space would otherwise read as a separator.
	if want := `"` + path + `"`; !strings.Contains(m.input.Value(), want) {
		t.Errorf("composer = %q, want it to hold %q", m.input.Value(), want)
	}
	if !strings.Contains(stripAnsi(m.footerView()), "attached") {
		t.Errorf("footer does not report the attachment:\n%s", stripAnsi(m.footerView()))
	}
}

// Ordinary prose keeps the old behaviour exactly: inserted verbatim, no note.
func TestPasteOfProseIsVerbatimAndAttachesNothing(t *testing.T) {
	const prose = "rewrite the gate countdown so it pauses on Ctrl+R"

	m := sizedModel(t)
	m.cwd = t.TempDir()
	tm, _ := m.Update(tea.PasteMsg{Content: prose})
	m = tm.(Model)

	if m.input.Value() != prose {
		t.Errorf("composer = %q, want the paste verbatim", m.input.Value())
	}
	if m.attached != nil {
		t.Errorf("prose attached %v", m.attached)
	}
	if strings.Contains(stripAnsi(m.footerView()), "attached") {
		t.Errorf("footer claims an attachment for prose:\n%s", stripAnsi(m.footerView()))
	}
}

// An attached path lands at the front of the composer, where a leading slash
// used to make it an unknown /command: the message was swallowed with a warning
// and never sent. Commands are single bare words, so a separator disqualifies.
func TestALeadingAbsolutePathIsNotASlashCommand(t *testing.T) {
	if _, _, ok := parseCommand("/Users/me/plan.md read this"); ok {
		t.Error("an absolute path parsed as a slash command")
	}
	for _, s := range []string{"/help", "/queue clear", "/model claude-opus-5"} {
		if _, _, ok := parseCommand(s); !ok {
			t.Errorf("%q no longer parses as a command", s)
		}
	}
}

// A stale 📎 line under an empty box would promise the next message a file it
// never carries, so sending has to take the attachment state with it.
func TestSendingClearsTheAttachment(t *testing.T) {
	dir, path := tempFile(t, "plan.md")

	sent := &strings.Builder{}
	m := sizedModel(t)
	m.cwd = dir
	m.drv = driver.NewWithWriter(driver.Options{}, nopCloser{sent})

	tm, _ := m.Update(tea.PasteMsg{Content: path})
	m = tm.(Model)
	m = typeInto(m, "read this")
	tm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = tm.(Model)

	if !strings.Contains(sent.String(), path) {
		t.Errorf("the path never reached the driver:\n%s", sent.String())
	}
	if m.input.Value() != "" {
		t.Errorf("composer = %q, want it cleared by the send", m.input.Value())
	}
	if m.attached != nil {
		t.Errorf("attachment survived the send: %v", m.attached)
	}
	if strings.Contains(stripAnsi(m.footerView()), "attached") {
		t.Errorf("the attachment line outlived the message:\n%s", stripAnsi(m.footerView()))
	}
}
