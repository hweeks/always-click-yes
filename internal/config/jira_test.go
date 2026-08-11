package config

import (
	"os"
	"strings"
	"testing"

	"github.com/hweeks/always-click-yes/internal/mcp"
)

// A project with no "jira" key must parse exactly as it always did — Jira
// stays nil and every other field is unaffected.
func TestLoadFileWithoutJiraIsUnaffected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{
		"model": "opus",
		"claudeBin": "/opt/claude",
		"countdown": "20s",
		"maxLines": 25,
		"planTools": ["Read", "Grep"],
		"useApiKey": true
	}`)

	f, found, err := LoadFile(dir)
	if err != nil || !found {
		t.Fatalf("LoadFile: found=%v err=%v", found, err)
	}
	if f.Jira != nil {
		t.Fatalf("Jira = %+v, want nil", f.Jira)
	}
	if f.Model != "opus" || f.ClaudeBin != "/opt/claude" || f.MaxLines == nil || *f.MaxLines != 25 {
		t.Errorf("unrelated fields disturbed by an absent jira key: %+v", f)
	}
}

// Every jira field, explicitly set, parses correctly, including an env
// value expanded from the process environment.
func TestLoadFileJiraExplicitValues(t *testing.T) {
	t.Setenv("SOME_TEST_VAR", "secret")

	dir := t.TempDir()
	writeFile(t, dir, `{
		"jira": {
			"server": "my-jira",
			"mcp": {
				"command": "/usr/local/bin/jira-mcp",
				"args": ["--transport", "stdio"],
				"env": {
					"JIRA_TOKEN": "$SOME_TEST_VAR",
					"JIRA_LITERAL": "not-a-var"
				}
			},
			"projectKey": "ENG",
			"site": "example.atlassian.net"
		}
	}`)

	f, found, err := LoadFile(dir)
	if err != nil || !found {
		t.Fatalf("LoadFile: found=%v err=%v", found, err)
	}
	j := f.Jira
	if j == nil {
		t.Fatal("Jira is nil")
	}
	if j.Server != "my-jira" {
		t.Errorf("Server = %q, want my-jira", j.Server)
	}
	if j.ProjectKey != "ENG" {
		t.Errorf("ProjectKey = %q, want ENG", j.ProjectKey)
	}
	if j.Site != "example.atlassian.net" {
		t.Errorf("Site = %q, want example.atlassian.net", j.Site)
	}
	if j.MCP == nil {
		t.Fatal("MCP is nil")
	}
	if j.MCP.Command != "/usr/local/bin/jira-mcp" {
		t.Errorf("Command = %q", j.MCP.Command)
	}
	if len(j.MCP.Args) != 2 || j.MCP.Args[0] != "--transport" || j.MCP.Args[1] != "stdio" {
		t.Errorf("Args = %v", j.MCP.Args)
	}
	if j.MCP.Env["JIRA_TOKEN"] != "secret" {
		t.Errorf("JIRA_TOKEN = %q, want secret (expanded from $SOME_TEST_VAR)", j.MCP.Env["JIRA_TOKEN"])
	}
	if j.MCP.Env["JIRA_LITERAL"] != "not-a-var" {
		t.Errorf("JIRA_LITERAL = %q, want literal value unchanged", j.MCP.Env["JIRA_LITERAL"])
	}
}

// server defaults to "jira" when not set.
func TestLoadFileJiraServerDefaults(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{"jira": {"mcp": {"command": "/bin/jira-mcp"}}}`)

	f, _, err := LoadFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if f.Jira.Server != defaultJiraServer {
		t.Errorf("Server = %q, want %q", f.Jira.Server, defaultJiraServer)
	}
}

// jira.mcp missing entirely is rejected.
func TestLoadFileJiraRequiresMCP(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{"jira": {}}`)

	_, _, err := LoadFile(dir)
	if err == nil {
		t.Fatal("want an error, got none")
	}
	if !strings.Contains(err.Error(), FileName) || !strings.Contains(err.Error(), "jira.mcp is required") {
		t.Errorf("error should name the file and reason: %v", err)
	}
}

// jira.mcp present but missing "command" is rejected, naming the file and
// the missing field.
func TestLoadFileJiraRequiresMCPCommand(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{"jira": {"mcp": {}}}`)

	_, _, err := LoadFile(dir)
	if err == nil {
		t.Fatal("want an error, got none")
	}
	if !strings.Contains(err.Error(), FileName) {
		t.Errorf("error should name the file: %v", err)
	}
	if !strings.Contains(err.Error(), "jira.mcp.command") {
		t.Errorf("error should name jira.mcp.command: %v", err)
	}
}

// jira.server colliding with acy's own MCP server name is rejected.
func TestLoadFileJiraRejectsServerNameCollision(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{"jira": {"server": "`+mcp.ServerName+`", "mcp": {"command": "/bin/jira-mcp"}}}`)

	_, _, err := LoadFile(dir)
	if err == nil {
		t.Fatal("want an error, got none")
	}
	if !strings.Contains(err.Error(), "must not collide") {
		t.Errorf("error should mention the collision: %v", err)
	}
}

// An env value of the form "$NAME" that references an unset environment
// variable is rejected, naming the variable.
func TestLoadFileJiraRejectsUnsetEnvVar(t *testing.T) {
	const varName = "ACY_JIRA_TEST_UNSET_VAR"
	if err := os.Unsetenv(varName); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	writeFile(t, dir, `{"jira": {"mcp": {"command": "/bin/jira-mcp", "env": {"JIRA_TOKEN": "$`+varName+`"}}}}`)

	_, _, err := LoadFile(dir)
	if err == nil {
		t.Fatal("want an error, got none")
	}
	if !strings.Contains(err.Error(), varName) {
		t.Errorf("error should name the missing variable %q: %v", varName, err)
	}
}

// ExtraServer maps a resolved JiraConfig onto the shape WriteMCPConfig
// merges into --mcp-config.
func TestJiraConfigExtraServer(t *testing.T) {
	t.Setenv("SOME_TEST_VAR", "secret")

	dir := t.TempDir()
	writeFile(t, dir, `{
		"jira": {
			"server": "my-jira",
			"mcp": {
				"command": "/usr/local/bin/jira-mcp",
				"args": ["--transport", "stdio"],
				"env": {"JIRA_TOKEN": "$SOME_TEST_VAR"}
			}
		}
	}`)

	f, _, err := LoadFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	extra := f.Jira.ExtraServer()
	if extra.Name != "my-jira" {
		t.Errorf("Name = %q, want my-jira", extra.Name)
	}
	if extra.Command != "/usr/local/bin/jira-mcp" {
		t.Errorf("Command = %q", extra.Command)
	}
	if len(extra.Args) != 2 {
		t.Errorf("Args = %v", extra.Args)
	}
	if extra.Env["JIRA_TOKEN"] != "secret" {
		t.Errorf("Env[JIRA_TOKEN] = %q, want secret", extra.Env["JIRA_TOKEN"])
	}
}
