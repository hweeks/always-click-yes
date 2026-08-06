package fleet

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// TestLocalTransportStartRoundTrip proves the whole Start path against a
// stub acy binary: the spec really goes out on stdin, --clone carries the
// configured repo path, and the printed ack line comes back parsed.
func TestLocalTransportStartRoundTrip(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	stdinFile := filepath.Join(dir, "stdin")

	script := "#!/bin/sh\n" +
		"printf '%s' \"$*\" > " + shq(argvFile) + "\n" +
		"cat > " + shq(stdinFile) + "\n" +
		"echo '{\"engineer_id\":\"e1\",\"dir\":\"/tmp/engineers/e1\",\"pid\":4242}'\n"
	stub := writeStub(t, dir, "acy", script)

	tr, err := NewLocalTransport(stub, "/srv/repo")
	if err != nil {
		t.Fatal(err)
	}

	spec := engineerwire.Spec{Ticket: "T-1", Title: "do the thing", Brief: "brief", Success: "criteria", BaseBranch: "main"}
	ack, err := tr.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if ack != (StartAck{EngineerID: "e1", Dir: "/tmp/engineers/e1", PID: 4242}) {
		t.Errorf("ack = %+v", ack)
	}

	if got, want := strings.TrimSpace(readFile(t, argvFile)), "engineer start --clone /srv/repo"; got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}

	var gotSpec engineerwire.Spec
	if err := json.Unmarshal([]byte(readFile(t, stdinFile)), &gotSpec); err != nil {
		t.Fatalf("stdin was not a valid spec line: %v", err)
	}
	// Spec has a Type field Marshal stamps; compare the fields we set.
	if gotSpec.Ticket != spec.Ticket || gotSpec.Title != spec.Title || gotSpec.Brief != spec.Brief {
		t.Errorf("stdin spec = %+v, want %+v", gotSpec, spec)
	}
}

// TestLocalTransportStartCapturesStderr proves a failing start surfaces the
// child's stderr in the returned error, rather than just an exit code.
func TestLocalTransportStartCapturesStderr(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'boom: no such clone' >&2\nexit 1\n"
	stub := writeStub(t, dir, "acy", script)

	tr, err := NewLocalTransport(stub, "/srv/repo")
	if err != nil {
		t.Fatal(err)
	}
	_, err = tr.Start(context.Background(), engineerwire.Spec{Ticket: "T-1"})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "boom: no such clone") {
		t.Errorf("error does not carry stderr: %v", err)
	}
}

// TestLocalTransportAttachRoundTrip proves the whole Attach path against a
// stub acy binary that cats a canned journal: --from carries fromSeq, every
// message streams through onMsg in order, and a Result ends the call with a
// nil error.
func TestLocalTransportAttachRoundTrip(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")

	hello, err := engineerwire.Marshal(engineerwire.Hello{Seq: 1, At: "2024-01-01T00:00:00Z", EngineerID: "e1", Host: "h", PID: 99})
	if err != nil {
		t.Fatal(err)
	}
	event, err := engineerwire.Marshal(engineerwire.Event{Seq: 2, At: "2024-01-01T00:00:01Z", Kind: engineerwire.EventPhase, Text: "working"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engineerwire.Marshal(engineerwire.Result{Seq: 3, At: "2024-01-01T00:00:02Z", Outcome: "success", Summary: "done"})
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(dir, "journal.ndjson")
	writeFixture(t, fixture, hello, event, result)

	script := "#!/bin/sh\n" +
		"printf '%s' \"$*\" > " + shq(argvFile) + "\n" +
		"cat " + shq(fixture) + "\n"
	stub := writeStub(t, dir, "acy", script)

	tr, err := NewLocalTransport(stub, "/srv/repo")
	if err != nil {
		t.Fatal(err)
	}

	var got []any
	err = tr.Attach(context.Background(), "e1", 1, strings.NewReader(""), func(msg any) {
		got = append(got, msg)
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3: %+v", len(got), got)
	}
	if _, ok := got[0].(engineerwire.Hello); !ok {
		t.Errorf("got[0] = %T, want Hello", got[0])
	}
	if _, ok := got[1].(engineerwire.Event); !ok {
		t.Errorf("got[1] = %T, want Event", got[1])
	}
	if r, ok := got[2].(engineerwire.Result); !ok || r.Outcome != "success" {
		t.Errorf("got[2] = %+v, want a successful Result", got[2])
	}

	if got, want := strings.TrimSpace(readFile(t, argvFile)), "engineer attach e1 --from 1"; got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func writeFixture(t *testing.T, path string, lines ...[]byte) {
	t.Helper()
	var all []byte
	for _, l := range lines {
		all = append(all, l...)
	}
	if err := os.WriteFile(path, all, 0o600); err != nil {
		t.Fatal(err)
	}
}
