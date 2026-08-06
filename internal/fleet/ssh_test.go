package fleet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// withStubSSH prepends a directory containing a stub "ssh" executable to
// PATH for the duration of the test, so sshTransport's hardcoded exec.Command
// "ssh" resolves to it instead of a real ssh binary.
func withStubSSH(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestSSHTransportStartRoundTrip proves Start wraps the engineer argv in a
// non-interactive ssh invocation and the remote argv survives intact after
// `--`, using a stub "ssh" that just records what it was called with.
func TestSSHTransportStartRoundTrip(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	stdinFile := filepath.Join(dir, "stdin")

	script := "#!/bin/sh\n" +
		"printf '%s' \"$*\" > " + shq(argvFile) + "\n" +
		"cat > " + shq(stdinFile) + "\n" +
		"echo '{\"engineer_id\":\"e2\",\"dir\":\"/home/box/e2\",\"pid\":555}'\n"
	writeStub(t, dir, "ssh", script)
	withStubSSH(t, dir)

	tr := NewSSHTransport("user@box1", "/opt/acy", "/srv/repo", nil, "", "")
	spec := engineerwire.Spec{Ticket: "T-2", Title: "remote task"}
	ack, err := tr.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if ack != (StartAck{EngineerID: "e2", Dir: "/home/box/e2", PID: 555}) {
		t.Errorf("ack = %+v", ack)
	}

	wantArgv := "-o BatchMode=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=4 user@box1 -- /opt/acy engineer start --clone /srv/repo"
	if got := strings.TrimSpace(readFile(t, argvFile)); got != wantArgv {
		t.Errorf("argv = %q, want %q", got, wantArgv)
	}
	if !strings.Contains(readFile(t, stdinFile), `"ticket":"T-2"`) {
		t.Errorf("spec did not reach stdin: %q", readFile(t, stdinFile))
	}
}

// TestSSHTransportStartExtendsPATH proves a host with Path set runs the
// remote engineer argv behind an `export PATH=...; exec` preamble, not just
// the bare acyBin+args ssh would otherwise send.
func TestSSHTransportStartExtendsPATH(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")

	script := "#!/bin/sh\n" +
		"printf '%s' \"$*\" > " + shq(argvFile) + "\n" +
		"cat > /dev/null\n" +
		"echo '{\"engineer_id\":\"e3\",\"dir\":\"/home/box/e3\",\"pid\":777}'\n"
	writeStub(t, dir, "ssh", script)
	withStubSSH(t, dir)

	tr := NewSSHTransport("user@box1", "/opt/acy", "/srv/repo", []string{"/opt/homebrew/bin", "/home/box1/.local/bin"}, "", "")
	spec := engineerwire.Spec{Ticket: "T-3", Title: "remote task"}
	if _, err := tr.Start(context.Background(), spec); err != nil {
		t.Fatalf("Start: %v", err)
	}

	wantArgv := "-o BatchMode=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=4 user@box1 -- " +
		"export PATH='/opt/homebrew/bin':'/home/box1/.local/bin':$PATH; exec '/opt/acy' 'engineer' 'start' '--clone' '/srv/repo'"
	if got := strings.TrimSpace(readFile(t, argvFile)); got != wantArgv {
		t.Errorf("argv = %q, want %q", got, wantArgv)
	}
}

// TestSSHTransportStartWithRc proves a host with Rc set runs the remote
// engineer argv sourced from that rc file, inside a `zsh -c` wrapper, and
// that this composes with a Path preamble rather than replacing it.
func TestSSHTransportStartWithRc(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")

	script := "#!/bin/sh\n" +
		"printf '%s' \"$*\" > " + shq(argvFile) + "\n" +
		"cat > /dev/null\n" +
		"echo '{\"engineer_id\":\"e4\",\"dir\":\"/home/box/e4\",\"pid\":888}'\n"
	writeStub(t, dir, "ssh", script)
	withStubSSH(t, dir)

	tr := NewSSHTransport("user@box1", "/opt/acy", "/srv/repo", []string{"/opt/homebrew/bin"}, "~/.zshrc", "")
	spec := engineerwire.Spec{Ticket: "T-4", Title: "remote task"}
	if _, err := tr.Start(context.Background(), spec); err != nil {
		t.Fatalf("Start: %v", err)
	}

	wantArgv := "-o BatchMode=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=4 user@box1 -- " +
		rcWrap("~/.zshrc", "", pathPreamble([]string{"/opt/homebrew/bin"})+quoteArgv([]string{"/opt/acy", "engineer", "start", "--clone", "/srv/repo"}))
	if got := strings.TrimSpace(readFile(t, argvFile)); got != wantArgv {
		t.Errorf("argv = %q, want %q", got, wantArgv)
	}
}

// TestSSHTransportStartWithRcAndShellOverride proves an explicit
// FleetHost.Shell wins over rcWrap's own derivation from the rc file's
// basename — an operator override always beats the guess.
func TestSSHTransportStartWithRcAndShellOverride(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")

	script := "#!/bin/sh\n" +
		"printf '%s' \"$*\" > " + shq(argvFile) + "\n" +
		"cat > /dev/null\n" +
		"echo '{\"engineer_id\":\"e5\",\"dir\":\"/home/box/e5\",\"pid\":999}'\n"
	writeStub(t, dir, "ssh", script)
	withStubSSH(t, dir)

	tr := NewSSHTransport("user@box1", "/opt/acy", "/srv/repo", nil, "~/.bashrc", "fish")
	spec := engineerwire.Spec{Ticket: "T-5", Title: "remote task"}
	if _, err := tr.Start(context.Background(), spec); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := strings.TrimSpace(readFile(t, argvFile))
	wantArgv := "-o BatchMode=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=4 user@box1 -- " +
		rcWrap("~/.bashrc", "fish", quoteArgv([]string{"/opt/acy", "engineer", "start", "--clone", "/srv/repo"}))
	if got != wantArgv {
		t.Errorf("argv = %q, want %q", got, wantArgv)
	}
	if !strings.Contains(got, "fish -c") {
		t.Errorf("argv should invoke fish, the explicit override, not bash derived from ~/.bashrc: %q", got)
	}
}

// TestSSHTransportAttachRoundTrip mirrors the local round trip: a stub "ssh"
// cats a canned journal, proving the messages stream through onMsg and the
// remote argv (including --from) is intact after the BatchMode/ServerAlive
// prefix and the `--`.
func TestSSHTransportAttachRoundTrip(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")

	result, err := engineerwire.Marshal(engineerwire.Result{Seq: 1, At: "2024-01-01T00:00:00Z", Outcome: "success", Summary: "done"})
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(dir, "journal.ndjson")
	writeFixture(t, fixture, result)

	script := "#!/bin/sh\n" +
		"printf '%s' \"$*\" > " + shq(argvFile) + "\n" +
		"cat " + shq(fixture) + "\n"
	writeStub(t, dir, "ssh", script)
	withStubSSH(t, dir)

	tr := NewSSHTransport("box2", "", "/srv/repo", nil, "", "")
	var got []any
	err = tr.Attach(context.Background(), "e9", 5, strings.NewReader(""), func(msg any) {
		got = append(got, msg)
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}
	if _, ok := got[0].(engineerwire.Result); !ok {
		t.Errorf("got[0] = %T, want Result", got[0])
	}

	wantArgv := "-o BatchMode=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=4 box2 -- acy engineer attach e9 --from 5"
	if got := strings.TrimSpace(readFile(t, argvFile)); got != wantArgv {
		t.Errorf("argv = %q, want %q", got, wantArgv)
	}
}
