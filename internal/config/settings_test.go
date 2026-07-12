package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestWriteHookSettings(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteHookSettings(dir, "/opt/my bin/acy", "/tmp/acy-x/gate.sock")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var parsed struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("settings not valid JSON: %v", err)
	}
	if len(parsed.Hooks.PreToolUse) != 1 || len(parsed.Hooks.PreToolUse[0].Hooks) != 1 {
		t.Fatalf("unexpected structure: %s", b)
	}
	h := parsed.Hooks.PreToolUse[0]
	if h.Matcher != "*" {
		t.Errorf("matcher = %q, want *", h.Matcher)
	}
	cmd := h.Hooks[0].Command
	// paths with spaces must be quoted, and the hook subcommand + socket present
	if !strings.Contains(cmd, "hook --socket") ||
		!strings.Contains(cmd, "'/opt/my bin/acy'") ||
		!strings.Contains(cmd, "'/tmp/acy-x/gate.sock'") {
		t.Errorf("command malformed: %q", cmd)
	}
}
