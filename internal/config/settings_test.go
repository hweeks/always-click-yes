package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/hweeks/always-click-yes/internal/mcp"
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

type mcpServerEntry struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

func readMCPServers(t *testing.T, path string) map[string]mcpServerEntry {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		MCPServers map[string]mcpServerEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("mcp config not valid JSON: %v", err)
	}
	return parsed.MCPServers
}

// With no extra servers, WriteMCPConfig's output must be byte-identical to
// before extra servers existed: exactly one entry, acy's own.
func TestWriteMCPConfigNoExtrasIsAcyOnly(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteMCPConfig(dir, "/opt/acy", "/tmp/acy-x/ask.sock", mcp.RoleParent)
	if err != nil {
		t.Fatal(err)
	}
	servers := readMCPServers(t, path)
	if len(servers) != 1 {
		t.Fatalf("mcpServers = %+v, want exactly one entry", servers)
	}
	if _, ok := servers[mcp.ServerName]; !ok {
		t.Errorf("mcpServers missing %q: %+v", mcp.ServerName, servers)
	}
}

// One extra server is merged in alongside acy's own, and a later call with
// no extras (e.g. for the child role) still produces only acy's entry.
func TestWriteMCPConfigWithExtraServer(t *testing.T) {
	dir := t.TempDir()
	extra := ExtraMCPServer{
		Name:    "jira",
		Command: "/usr/local/bin/jira-mcp",
		Args:    []string{"--site", "example.atlassian.net"},
		Env:     map[string]string{"JIRA_TOKEN": "secret"},
	}
	path, err := WriteMCPConfig(dir, "/opt/acy", "/tmp/acy-x/ask.sock", mcp.RoleParent, extra)
	if err != nil {
		t.Fatal(err)
	}
	servers := readMCPServers(t, path)
	if len(servers) != 2 {
		t.Fatalf("mcpServers = %+v, want two entries", servers)
	}
	if _, ok := servers[mcp.ServerName]; !ok {
		t.Errorf("mcpServers missing %q: %+v", mcp.ServerName, servers)
	}
	jira, ok := servers["jira"]
	if !ok {
		t.Fatalf("mcpServers missing jira entry: %+v", servers)
	}
	if jira.Command != extra.Command {
		t.Errorf("jira.Command = %q, want %q", jira.Command, extra.Command)
	}
	if len(jira.Args) != 2 || jira.Args[0] != "--site" || jira.Args[1] != "example.atlassian.net" {
		t.Errorf("jira.Args = %v, want %v", jira.Args, extra.Args)
	}
	if jira.Env["JIRA_TOKEN"] != "secret" {
		t.Errorf("jira.Env = %v, want JIRA_TOKEN=secret", jira.Env)
	}

	childPath, err := WriteMCPConfig(dir, "/opt/acy", "/tmp/acy-x/ask.sock", mcp.RoleChild)
	if err != nil {
		t.Fatal(err)
	}
	childServers := readMCPServers(t, childPath)
	if len(childServers) != 1 {
		t.Fatalf("child mcpServers = %+v, want exactly one entry", childServers)
	}
	if _, ok := childServers[mcp.ServerName]; !ok {
		t.Errorf("child mcpServers missing %q: %+v", mcp.ServerName, childServers)
	}
}
