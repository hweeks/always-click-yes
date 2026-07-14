package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hweeks/always-click-yes/internal/driver"
)

// blockKinds flattens an event into the block types it carries, which is the
// assertion that matters: it proves the transcript is being read through
// driver.Message.Blocks() rather than a second, drifting parser.
func blockKinds(ev driver.Event) []string {
	var out []string
	for _, b := range ev.Message.Blocks() {
		out = append(out, b.Type)
	}
	return out
}

func TestReplayFile(t *testing.T) {
	evs, err := ReplayFile(filepath.Join("testdata", "transcript.jsonl"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	// Six conversation records survive: prompt, text+thinking, tool_use,
	// tool_result, array tool_result, final text. Everything else in the fixture is
	// noise, a sidechain, meta, or malformed.
	if len(evs) != 6 {
		for i, ev := range evs {
			t.Logf("%d: %s %v", i, ev.Type, blockKinds(ev))
		}
		t.Fatalf("got %d events, want 6", len(evs))
	}

	// Order is file order.
	wantTypes := []string{
		driver.TypeUser, driver.TypeAssistant, driver.TypeAssistant,
		driver.TypeUser, driver.TypeUser, driver.TypeAssistant,
	}
	for i, want := range wantTypes {
		if evs[i].Type != want {
			t.Errorf("event %d type = %q, want %q", i, evs[i].Type, want)
		}
	}

	// The user prompt is a bare string — the shape Blocks() has to normalize, and
	// the shape the live stream never sends (claude only echoes tool_results), so
	// this is the one the replay path exists to recover.
	if got := blockKinds(evs[0]); len(got) != 1 || got[0] != driver.BlockText {
		t.Errorf("prompt blocks = %v, want one text block", got)
	}
	if txt := evs[0].Message.Blocks()[0].Text; !strings.Contains(txt, "doc comment") {
		t.Errorf("prompt text = %q", txt)
	}

	if got := blockKinds(evs[1]); len(got) != 2 || got[0] != driver.BlockThinking || got[1] != driver.BlockText {
		t.Errorf("assistant blocks = %v, want thinking+text", got)
	}
	if got := blockKinds(evs[2]); len(got) != 1 || got[0] != driver.BlockToolUse {
		t.Errorf("tool_use blocks = %v", got)
	}
	if got := blockKinds(evs[3]); len(got) != 1 || got[0] != driver.BlockToolResult {
		t.Errorf("tool_result blocks = %v", got)
	}

	// An array-shaped tool_result, flagged as an error, still parses.
	last := evs[4].Message.Blocks()
	if len(last) != 1 || !last[0].IsError {
		t.Errorf("array tool_result = %+v, want one error block", last)
	}

	// Nothing from a sub-agent may reach the transcript view.
	for _, ev := range evs {
		for _, b := range ev.Message.Blocks() {
			if strings.Contains(b.Text, "SUBAGENT") {
				t.Fatalf("sidechain record leaked into the replay: %q", b.Text)
			}
		}
	}
}

// A session claude knows nothing about is not an error — the caller just gets an
// empty transcript and carries on.
func TestReplayMissingTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	evs, err := Replay(t.TempDir(), "no-such-session")
	if err != nil {
		t.Fatalf("err = %v, want nil for a missing transcript", err)
	}
	if len(evs) != 0 {
		t.Fatalf("got %d events, want none", len(evs))
	}
}

// Replay resolves the transcript through claude's own project layout, so a session
// id is all a caller needs.
func TestReplayFindsTranscriptByID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	dir, err := ProjectDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "sess-9.jsonl"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	evs, err := Replay(cwd, "sess-9")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Message.Blocks()[0].Text != "hello" {
		t.Fatalf("Replay = %+v", evs)
	}
}

// Tool inputs and results routinely dwarf a scanner's default token size; a long
// line must come back whole rather than truncated or dropped.
func TestReplayHandlesVeryLongLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.jsonl")

	huge := strings.Repeat("x", 2<<20) // 2 MiB in one record
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + huge + `"}]}}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	evs, err := ReplayFile(path)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if got := len(evs[0].Message.Blocks()[0].Text); got != len(huge) {
		t.Fatalf("text length = %d, want %d", got, len(huge))
	}
}
