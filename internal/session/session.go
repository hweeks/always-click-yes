// Package session discovers resumable claude sessions for a working directory by
// reading the per-project transcript files claude stores under
// ~/.claude/projects/<slug>/<session-id>.jsonl.
package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Info describes one resumable session, newest first when returned by List.
type Info struct {
	ID      string    // session id (the .jsonl filename without extension)
	ModTime time.Time // last-modified time of the transcript file
	Summary string    // a one-line human summary (first user prompt or stored summary)
}

// List returns the resumable sessions for cwd, sorted newest first. A missing or
// unreadable project directory yields an empty list and no error, so callers can
// treat "no sessions" and "no directory" the same way.
func List(cwd string) ([]Info, error) {
	dir, err := ProjectDir(cwd)
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, path := range matches {
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() {
			continue
		}
		id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		out = append(out, Info{
			ID:      id,
			ModTime: fi.ModTime(),
			Summary: firstSummary(path),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// ProjectDir returns the directory claude uses to store transcripts for cwd.
//
// The slug rules are claude's, and they are not simply "swap the slashes" —
// verified against real transcripts on macOS:
//
//   - symlinks are resolved first: /var/folders/… is stored under -private-var-folders-…,
//     because /var is a symlink to /private/var;
//   - every character outside [A-Za-z0-9-] becomes a dash, not just the separator:
//     /tmp/my.dotted_dir is stored as -tmp-my-dotted-dir.
//
// Getting either wrong means silently finding no sessions for a project, which is
// why Replay does not rely on this alone — see transcriptPath.
func ProjectDir(cwd string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects", Slug(cwd)), nil
}

// Slug is claude's name for a project directory.
func Slug(cwd string) string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	// claude stores the resolved path, so a path reached through a symlink must be
	// resolved the same way or it lands in a directory of its own. A path that does
	// not exist yet cannot be resolved — keep it as-is rather than failing.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	var b strings.Builder
	b.Grow(len(abs))
	for _, r := range abs {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// firstSummary scans the head of a transcript for a human-readable label: a
// stored "summary" record if present, otherwise the first user message text.
func firstSummary(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for i := 0; i < 500 && sc.Scan(); i++ {
		var rec struct {
			Type    string `json:"type"`
			Summary string `json:"summary"`
			Message *struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		if rec.Type == "summary" && strings.TrimSpace(rec.Summary) != "" {
			return oneLine(rec.Summary)
		}
		if rec.Message != nil && rec.Message.Role == "user" {
			if t := contentText(rec.Message.Content); t != "" {
				return oneLine(t)
			}
		}
	}
	return ""
}

// contentText extracts text from a message content field that may be a plain
// string or an array of {type:"text",text:...} blocks.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				return b.Text
			}
		}
	}
	return ""
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return s
}
