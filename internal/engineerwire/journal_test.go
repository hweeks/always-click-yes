package engineerwire

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJournalRecoversLastSeqAfterReopen(t *testing.T) {
	dir := t.TempDir()

	j, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := j.Append(Hello{EngineerID: "e1"}); err != nil {
		t.Fatalf("Append hello: %v", err)
	}
	if _, err := j.Append(Event{Kind: EventPhase, Text: "working"}); err != nil {
		t.Fatalf("Append event: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	j2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = j2.Close() }()

	final, err := j2.Append(Event{Kind: EventLog, Text: "resumed"})
	if err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}
	if got := seqOf(final); got != 3 {
		t.Errorf("seq after reopen = %d, want 3", got)
	}
}

func TestJournalReplayFromMidStream(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = j.Close() }()

	for i := range 5 {
		if _, err := j.Append(Event{Kind: EventLog, Text: fmt.Sprintf("line %d", i)}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	msgs, err := j.ReplayFrom(3)
	if err != nil {
		t.Fatalf("ReplayFrom: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if seqOf(msgs[0]) != 3 {
		t.Errorf("first replayed seq = %d, want 3", seqOf(msgs[0]))
	}
	if seqOf(msgs[len(msgs)-1]) != 5 {
		t.Errorf("last replayed seq = %d, want 5", seqOf(msgs[len(msgs)-1]))
	}
}

func TestJournalFollowSeesAppendedLine(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = j.Close() }()

	if _, err := j.Append(Hello{EngineerID: "e1"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	ch := j.Follow(t.Context(), 1)

	first, ok := <-ch
	if !ok {
		t.Fatal("channel closed before delivering the backlog")
	}
	if seqOf(first) != 1 {
		t.Fatalf("first seq = %d, want 1", seqOf(first))
	}

	if _, err := j.Append(Event{Kind: EventLog, Text: "new"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	select {
	case m, ok := <-ch:
		if !ok {
			t.Fatal("channel closed instead of delivering the new line")
		}
		if seqOf(m) != 2 {
			t.Errorf("seq = %d, want 2", seqOf(m))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Follow did not see the appended line in time")
	}
}

// TestJournalTornFinalLineIgnored simulates a writer that crashed mid-Append:
// a line written directly to the file with no trailing newline. Both replay
// and reopen-then-append must tolerate it.
func TestJournalTornFinalLineIgnored(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := j.Append(Hello{EngineerID: "e1"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(dir, "journal.ndjson")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("open for torn write: %v", err)
	}
	if _, err := f.WriteString(`{"type":"event","seq":2,"at":"2026-08-05T00:00:00Z","kind":"log","text":"cut of`); err != nil {
		t.Fatalf("write torn line: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close torn write: %v", err)
	}

	// Reopening must recover cleanly and drop the torn tail from disk.
	j2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen over a torn tail: %v", err)
	}
	defer func() { _ = j2.Close() }()

	msgs, err := j2.ReplayFrom(1)
	if err != nil {
		t.Fatalf("ReplayFrom: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1 (the torn line must be dropped)", len(msgs))
	}

	// The next Append must continue the seq correctly, not glue itself onto
	// the torn fragment.
	final, err := j2.Append(Event{Kind: EventLog, Text: "resumed"})
	if err != nil {
		t.Fatalf("Append after torn tail: %v", err)
	}
	if got := seqOf(final); got != 2 {
		t.Errorf("seq after torn tail = %d, want 2", got)
	}

	msgs, err = j2.ReplayFrom(1)
	if err != nil {
		t.Fatalf("ReplayFrom after append: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages after append, want 2", len(msgs))
	}
	ev, ok := msgs[1].(Event)
	if !ok || ev.Text != "resumed" {
		t.Errorf("second message = %#v, want the resumed event", msgs[1])
	}
}

func TestJournalAppendRejectsInboundMessage(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = j.Close() }()

	if _, err := j.Append(Spec{Ticket: "T1"}); err == nil {
		t.Fatal("expected Append to reject an inbound message")
	}
}
