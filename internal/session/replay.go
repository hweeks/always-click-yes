package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/hweeks/always-click-yes/internal/driver"
)

// record is one line of a claude transcript. The message is a *driver.Message —
// the on-disk shape is a superset of the stream-json event, so the blocks come
// back out through driver's own parser and acy keeps exactly one of those.
type record struct {
	Type        string          `json:"type"` // "user" | "assistant" | claude's own bookkeeping
	IsSidechain bool            `json:"isSidechain"`
	IsMeta      bool            `json:"isMeta"`
	SessionID   string          `json:"sessionId"`
	Message     *driver.Message `json:"message"`
}

// Replay reads the transcript claude keeps for a session and returns it as the
// events the UI would have ingested live, oldest first. A missing transcript is
// no events and no error — a session can be resumable to claude and unknown to us.
//
// What is dropped, and why:
//
//   - sub-agent records (isSidechain): a sub-agent's whole conversation is in the
//     same file, and replaying it would bury the conversation the user actually had;
//   - claude's bookkeeping records (summary, ai-title, attachment, queue-operation,
//     last-prompt, mode): not conversation;
//   - unparseable lines: skipped, never fatal. A transcript we can only partly read
//     is still worth most of its value, and claude may add record types at any time.
//
// Note the two things that are not in the file at all: there are no result records,
// so cost and turn boundaries cannot be recovered here (that is what the state
// snapshot is for), and no init records, so a replay can never clobber the live
// session id or model.
func Replay(cwd, id string) ([]driver.Event, error) {
	path, err := transcriptPath(cwd, id)
	if err != nil {
		return nil, err
	}
	return ReplayFile(path)
}

// transcriptPath locates a session's transcript, preferring the project directory
// the slug rules predict and falling back to a search across every project.
//
// The fallback is not paranoia. The slug rules are claude's private business — they
// already surprised us once (symlinks are resolved, and dots and underscores become
// dashes, neither of which was obvious) — and a session id is globally unique, so
// searching for it is exact. Getting the directory wrong should cost a few
// milliseconds, not the user's whole transcript.
func transcriptPath(cwd, id string) (string, error) {
	dir, err := ProjectDir(cwd)
	if err != nil {
		return "", err
	}
	direct := filepath.Join(dir, id+".jsonl")
	if _, err := os.Stat(direct); err == nil {
		return direct, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return direct, nil //nolint:nilerr // no home, no fallback: let the direct path fail honestly
	}
	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", id+".jsonl"))
	if err != nil || len(matches) == 0 {
		return direct, nil //nolint:nilerr // nothing found: ReplayFile reports the miss as an empty transcript
	}
	return matches[0], nil
}

// ReplayFile is Replay against an explicit transcript path.
func ReplayFile(path string) ([]driver.Event, error) {
	f, err := os.Open(path) //nolint:gosec // the path is derived from the caller's own cwd and session id
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	// A Reader, not a Scanner: transcript lines carry whole tool inputs and results
	// and routinely run to tens of kilobytes, which is the same reason the live
	// driver reads its stream this way (see driver.readEvents).
	br := bufio.NewReaderSize(f, 1<<20)
	var out []driver.Event
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			if ev, ok := eventFor(line); ok {
				out = append(out, ev)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, err
		}
	}
}

// eventFor converts a transcript line into an event, reporting whether it is part
// of the conversation at all.
func eventFor(line []byte) (driver.Event, bool) {
	var rec record
	if json.Unmarshal(line, &rec) != nil {
		return driver.Event{}, false
	}
	if rec.Message == nil || rec.IsSidechain || rec.IsMeta {
		return driver.Event{}, false
	}
	if rec.Type != driver.TypeUser && rec.Type != driver.TypeAssistant {
		return driver.Event{}, false
	}
	return driver.Event{
		Type:      rec.Type,
		SessionID: rec.SessionID,
		Message:   rec.Message,
		Raw:       json.RawMessage(line),
	}, true
}

// readLine reads one newline-terminated line of any length, returning it without
// the newline. A final line with no trailing newline is returned with io.EOF.
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // a line longer than the buffer; keep going
		}
		return trimNewline(buf), err
	}
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
