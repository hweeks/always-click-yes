package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/hweeks/always-click-yes/internal/engineerd"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// newEngineerCmd is the hidden "arch mode" command group: it starts, attaches
// to, and tails detached engineer processes. Hidden because arch mode is not
// yet something a human runs directly — an architect acy process is meant to
// spawn and drive these — but the subcommands themselves are ordinary, not
// individually hidden, since that architect drives them exactly as shown
// here (`start` on stdin, `attach`/`tail` on an id).
func newEngineerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "engineer",
		Short:  "Internal: start and observe detached engineer processes (arch mode)",
		Hidden: true,
	}
	cmd.AddCommand(newEngineerStartCmd())
	cmd.AddCommand(newEngineerRunCmd())
	cmd.AddCommand(newEngineerAttachCmd())
	cmd.AddCommand(newEngineerTailCmd())
	return cmd
}

// startResult is the one line `start` prints to stdout on success.
type startResult struct {
	EngineerID string `json:"engineer_id"`
	Dir        string `json:"dir"`
	PID        int    `json:"pid"`
}

func newEngineerStartCmd() *cobra.Command {
	var clonePath string
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Read a Spec from stdin and launch a detached engineer for it",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEngineerStart(os.Stdin, cmd.OutOrStdout(), clonePath)
		},
	}
	cmd.Flags().StringVar(&clonePath, "clone", "", "the shared git clone this engineer's worktree branches from")
	_ = cmd.MarkFlagRequired("clone")
	return cmd
}

// runEngineerStart reads one Spec line from in, allocates the engineer its
// state directory, and re-execs this binary detached to run it — printing
// {"engineer_id","dir","pid"} to out on success. It never waits on the
// child beyond confirming the process started.
func runEngineerStart(in io.Reader, out io.Writer, clonePath string) error {
	id, dir, err := prepareEngineerStart(in, clonePath)
	if err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("acy engineer start: resolving this binary: %w", err)
	}

	devnull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("acy engineer start: opening %s: %w", os.DevNull, err)
	}
	defer func() { _ = devnull.Close() }()

	logPath := filepath.Join(dir, engineerd.DebugLog)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // dir is this engineer's own state directory
	if err != nil {
		return fmt.Errorf("acy engineer start: opening %s: %w", logPath, err)
	}
	defer func() { _ = logFile.Close() }()

	child := exec.Command(exe, "engineer", "__run", "--dir", dir) //nolint:gosec // exe is our own resolved path, dir is ours
	child.Stdin = devnull
	child.Stdout = logFile
	child.Stderr = logFile
	if err := detachChild(child); err != nil {
		return fmt.Errorf("acy engineer start: %w", err)
	}

	line, err := json.Marshal(startResult{EngineerID: id, Dir: dir, PID: child.Process.Pid})
	if err != nil {
		return fmt.Errorf("acy engineer start: encoding the result line: %w", err)
	}
	_, err = fmt.Fprintln(out, string(line))
	return err
}

// prepareEngineerStart is the part of `start` that needs no child process:
// decode the one Spec line on in, mint an id, and write spec.json. Split out
// so it can be exercised without ever detaching a real process — the real
// detached start is proven by a guarded live e2e, not here.
func prepareEngineerStart(in io.Reader, clonePath string) (id, dir string, err error) {
	dec := engineerwire.NewDecoder(in)
	msg, err := dec.Decode()
	if err != nil {
		return "", "", fmt.Errorf("acy engineer start: reading the spec: %w", err)
	}
	spec, ok := msg.(engineerwire.Spec)
	if !ok {
		return "", "", fmt.Errorf("acy engineer start: expected a spec line, got %T", msg)
	}

	id = engineerd.NewID()
	dir, err = engineerd.EnsureDir(id)
	if err != nil {
		return "", "", fmt.Errorf("acy engineer start: %w", err)
	}

	stored := engineerd.StoredSpec{
		Spec:        spec,
		ClonePath:   clonePath,
		WorktreeDir: filepath.Join(dir, "worktree"),
	}
	if err := engineerd.WriteSpec(dir, stored); err != nil {
		return "", "", fmt.Errorf("acy engineer start: writing spec: %w", err)
	}
	return id, dir, nil
}

func newEngineerRunCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:    "__run",
		Short:  "Internal: run one detached engineer (spawned by `acy engineer start`)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return engineerd.RunDetachedTarget(dir)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "the engineer's state directory")
	_ = cmd.MarkFlagRequired("dir")
	return cmd
}

func newEngineerAttachCmd() *cobra.Command {
	var from int64
	cmd := &cobra.Command{
		Use:   "attach <id>",
		Short: "Attach to a running engineer: stream its journal, forward answers and cancels",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := engineerd.Dir(args[0])
			if err != nil {
				return err
			}
			return engineerd.Attach(cmd.Context(), dir, from, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	cmd.Flags().Int64Var(&from, "from", 1, "replay the journal starting at this seq")
	return cmd
}

func newEngineerTailCmd() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "tail <id>",
		Short: "Print an engineer's journal in a human-readable form",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := engineerd.Dir(args[0])
			if err != nil {
				return err
			}
			return tailEngineer(cmd.Context(), dir, follow, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&follow, "follow", true,
		"keep following after the replay and exit once a result arrives (false: print the replay once and exit)")
	return cmd
}

// tailEngineer prints dir's journal, one line per message, timestamp first.
// With follow it keeps reading past the replay and stops the moment a Result
// arrives; without it, it prints the replay once and returns.
func tailEngineer(ctx context.Context, dir string, follow bool, out io.Writer) error {
	j, err := engineerwire.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = j.Close() }()

	if !follow {
		msgs, err := j.ReplayFrom(1)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			printTail(out, m)
		}
		return nil
	}

	for msg := range j.Follow(ctx, 1) {
		printTail(out, msg)
		if _, ok := msg.(engineerwire.Result); ok {
			return nil
		}
	}
	return ctx.Err()
}

// printTail formats one journal message as a timestamp, its type, and its
// text — exactly the "timestamps + type + text" `acy engineer tail` promises.
func printTail(w io.Writer, msg any) {
	switch m := msg.(type) {
	case engineerwire.Hello:
		_, _ = fmt.Fprintf(w, "%s hello engineer=%s host=%s pid=%d\n", tailTime(m.At), m.EngineerID, m.Host, m.PID)
	case engineerwire.Event:
		_, _ = fmt.Fprintf(w, "%s event %s %s\n", tailTime(m.At), m.Kind, m.Text)
	case engineerwire.Question:
		_, _ = fmt.Fprintf(w, "%s question %s\n", tailTime(m.At), m.QuestionID)
	case engineerwire.Result:
		_, _ = fmt.Fprintf(w, "%s result %s %s\n", tailTime(m.At), m.Outcome, m.Summary)
	}
}

// tailTime renders an RFC3339 wire timestamp as a shorter, still-sortable
// local form; a message whose "at" fails to parse (should never happen — it
// is always written by Journal.Append) prints verbatim rather than dropping it.
func tailTime(at string) string {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return at
	}
	return t.Local().Format("15:04:05")
}
