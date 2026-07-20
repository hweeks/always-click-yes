package ui

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolBody(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		want   []string // substrings that must appear
		absent []string // substrings that must not appear
	}{
		{"Bash", `{"command":"go test ./...","description":"run tests"}`, []string{"go test ./..."}, nil},
		{"Write", `{"file_path":"/tmp/x.go","content":"package main\nfunc main(){}"}`,
			[]string{"/tmp/x.go", "package main", "func main"}, nil},
		{"Edit", `{"file_path":"a.txt","old_string":"foo","new_string":"bar"}`,
			[]string{"a.txt", "- foo", "+ bar"}, nil},
		{"Read", `{"file_path":"big.log","offset":100,"limit":50}`,
			[]string{"big.log", "offset 100", "limit 50"}, nil},
		{"Grep", `{"pattern":"TODO","path":"src"}`, []string{"TODO"}, nil},
	}
	for _, c := range cases {
		// Bash/Write/Edit bodies come back syntax-highlighted; match the text
		// without the ANSI in between the tokens.
		got := stripAnsi(toolBody(c.name, json.RawMessage(c.input)))
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("toolBody(%s): want substring %q in %q", c.name, w, got)
			}
		}
		for _, a := range c.absent {
			if strings.Contains(got, a) {
				t.Errorf("toolBody(%s): unexpected substring %q in %q", c.name, a, got)
			}
		}
	}
}

// TestToolBodyHighlights guards the chroma wiring: code-bearing tools must come
// back with ANSI color in them, and stripping it must recover the exact input.
func TestToolBodyHighlights(t *testing.T) {
	got := toolBody("Bash", json.RawMessage(`{"command":"echo hello"}`))
	if !strings.Contains(got, "\x1b[") {
		t.Errorf("expected ANSI highlighting in a Bash body, got %q", got)
	}
	if s := stripAnsi(got); s != "echo hello" {
		t.Errorf("stripped body = %q, want the original command back", s)
	}
}

func TestToolBodyEmpty(t *testing.T) {
	if got := toolBody("Read", nil); got != "" {
		t.Errorf("expected empty for nil input, got %q", got)
	}
}
