package engineerwire

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hweeks/always-click-yes/internal/alog"
)

const journalFileName = "journal.ndjson"

// pollInterval is how often Follow rereads the journal file for new lines.
// Polling, not a watch, is the point: Follow's whole reason to exist is the
// attach process reconnecting to a detached engineer, which means the
// follower usually runs in a different process from the writer — there is no
// in-memory channel to fan out on, and the repo takes no new dependency for
// this (fsnotify) any more than it did for a uuid (internal/orchestrator/dispatch.go).
const pollInterval = 150 * time.Millisecond

// Journal persists every outbound message an engineer sends, in the order it
// sent them, so a reconnecting architect can replay exactly what it missed.
// Seq is the whole contract: replay from seq N is byte-precise and lossless
// (docs/engineer-protocol.md). Inbound messages are never journaled.
//
// One writer, many readers. Append is safe for concurrent goroutines within
// the writer via mu. ReplayFrom and Follow read the file directly through
// their own handle and expect no exclusive access, since they are meant to
// run from a separate attach process entirely.
type Journal struct {
	mu      sync.Mutex
	f       *os.File
	path    string
	lastSeq int64
}

// Open creates (or reopens) the journal in dir. Reopening scans the existing
// file to recover the last seq written, tolerating — and dropping — a torn
// final line: the mark of a writer that crashed mid-Append. Dropping it
// (rather than merely skipping it in memory) matters because Append writes
// raw bytes with no leading newline of its own; leaving the fragment on disk
// would glue it to the very next line and corrupt that one too.
func Open(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		alog.Printf("engineerwire: journal mkdir %s: %v", dir, err)
		return nil, err
	}
	path := filepath.Join(dir, journalFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		alog.Printf("engineerwire: journal open %s: %v", path, err)
		return nil, err
	}

	msgs, validSize, err := scanJournal(path)
	if err != nil {
		_ = f.Close()
		alog.Printf("engineerwire: journal recover %s: %v", path, err)
		return nil, err
	}
	if info, statErr := f.Stat(); statErr == nil && info.Size() > validSize {
		if err := f.Truncate(validSize); err != nil {
			_ = f.Close()
			alog.Printf("engineerwire: journal truncate %s: %v", path, err)
			return nil, err
		}
	}

	var lastSeq int64
	if len(msgs) > 0 {
		lastSeq = seqOf(msgs[len(msgs)-1])
	}
	return &Journal{f: f, path: path, lastSeq: lastSeq}, nil
}

// Close closes the underlying file.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.f.Close()
}

// Append assigns msg the next seq and the current time, writes it as one
// line, and returns the finalized copy. msg must be one of the four outbound
// message types (Hello, Event, Question, Result); inbound messages are never
// journaled.
func (j *Journal) Append(msg any) (any, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	seq := j.lastSeq + 1
	at := time.Now().UTC().Format(time.RFC3339)

	var final any
	switch m := msg.(type) {
	case Hello:
		m.Seq, m.At = seq, at
		final = m
	case Event:
		m.Seq, m.At = seq, at
		final = m
	case Question:
		m.Seq, m.At = seq, at
		final = m
	case Result:
		m.Seq, m.At = seq, at
		final = m
	default:
		return nil, fmt.Errorf("engineerwire: journal: %T is not an outbound message", msg)
	}

	line, err := Marshal(final)
	if err != nil {
		return nil, err
	}
	// One Write call for the whole line: a torn-tail read has to be able to
	// trust that anything short of a full line means the process died mid
	// write, not mid one-of-several partial writes.
	if _, err := j.f.Write(line); err != nil {
		alog.Printf("engineerwire: journal append %s: %v", j.path, err)
		return nil, err
	}
	j.lastSeq = seq
	return final, nil
}

// ReplayFrom returns every persisted message with seq >= from, in order.
func (j *Journal) ReplayFrom(from int64) ([]any, error) {
	msgs, _, err := scanJournal(j.path)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		if seqOf(m) >= from {
			out = append(out, m)
		}
	}
	return out, nil
}

// Follow replays everything from fromSeq and then polls the journal file for
// newly appended lines until ctx is done, at which point the returned channel
// is closed.
func (j *Journal) Follow(ctx context.Context, fromSeq int64) <-chan any {
	out := make(chan any, 16)
	go func() {
		defer close(out)
		defer alog.Recover("engineerwire.Follow")

		backlog, err := j.ReplayFrom(fromSeq)
		if err != nil {
			alog.Printf("engineerwire: follow replay %s: %v", j.path, err)
			return
		}
		next := fromSeq
		for _, m := range backlog {
			select {
			case out <- m:
				next = seqOf(m) + 1
			case <-ctx.Done():
				return
			}
		}

		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				more, err := j.ReplayFrom(next)
				if err != nil {
					alog.Printf("engineerwire: follow poll %s: %v", j.path, err)
					continue
				}
				for _, m := range more {
					select {
					case out <- m:
						next = seqOf(m) + 1
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return out
}

// seqOf extracts Seq from any outbound message type.
func seqOf(msg any) int64 {
	switch m := msg.(type) {
	case Hello:
		return m.Seq
	case Event:
		return m.Seq
	case Question:
		return m.Seq
	case Result:
		return m.Seq
	default:
		return 0
	}
}

// scanJournal reads path line by line and decodes every complete,
// newline-terminated line it finds. validSize is the byte offset just past
// the last such line — i.e. the length the file would be with any torn final
// line (a write cut short by a crash) dropped. A missing file reads as
// empty, not an error: Open calls this before anything has ever been
// appended.
func scanJournal(path string) (msgs []any, validSize int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()

	br := bufio.NewReaderSize(f, 64*1024)
	var offset int64
	lineNum := 0
	for {
		line, rerr := br.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			if rerr != nil {
				break
			}
			offset += int64(len(line))
			continue
		}
		if rerr != nil {
			// EOF reached before this line found its terminator: a torn
			// tail. Stop here and leave it out of both msgs and validSize.
			break
		}
		lineNum++
		msg, derr := decodeLine(trimmed)
		if derr != nil {
			return nil, 0, fmt.Errorf("engineerwire: journal: corrupt line %d of %s: %w", lineNum, path, derr)
		}
		msgs = append(msgs, msg)
		offset += int64(len(line))
	}
	return msgs, offset, nil
}
