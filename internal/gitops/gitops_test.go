package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hermeticRunner is a Runner backed by the real git/gh binaries, isolated
// from the host's global/system git config and given a fixed commit
// identity so these tests pass in CI with no ~/.gitconfig at all.
func hermeticRunner(t *testing.T) Runner {
	t.Helper()
	return func(ctx context.Context, dir, name string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=gitops-test",
			"GIT_AUTHOR_EMAIL=gitops-test@example.com",
			"GIT_COMMITTER_NAME=gitops-test",
			"GIT_COMMITTER_EMAIL=gitops-test@example.com",
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return stdout.String(), nil
	}
}

func mustRun(t *testing.T, run Runner, dir, name string, args ...string) string {
	t.Helper()
	out, err := run(context.Background(), dir, name, args...)
	if err != nil {
		t.Fatalf("%s %s (dir=%s): %v", name, strings.Join(args, " "), dir, err)
	}
	return out
}

// initRepo creates a non-bare repo at a fresh temp dir with one commit on
// baseBranch, and no origin remote.
func initRepo(t *testing.T, run Runner, baseBranch string) string {
	t.Helper()
	dir := t.TempDir()
	mustRun(t, run, dir, "git", "init", "-b", baseBranch)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("writing README.md: %v", err)
	}
	mustRun(t, run, dir, "git", "add", "README.md")
	mustRun(t, run, dir, "git", "commit", "-m", "initial commit")
	return dir
}

// cloneBareFrom creates a bare clone of source at a fresh temp path, giving
// tests a origin-shaped remote without touching the network.
func cloneBareFrom(t *testing.T, run Runner, source string) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "origin.git")
	mustRun(t, run, "", "git", "clone", "--bare", source, bare)
	return bare
}
