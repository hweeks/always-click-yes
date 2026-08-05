package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStub writes an executable shell script named name in dir and returns
// its full path.
func writeStub(t *testing.T, dir, name, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture, needs +x
		t.Fatal(err)
	}
	return path
}

// shq single-quotes s for safe embedding in a POSIX shell script — the stub
// scripts below splice in tempdir paths this way rather than trusting they
// never contain a shell metacharacter.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// readFile is a small test convenience wrapping os.ReadFile with a Fatal.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
