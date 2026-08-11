package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/hweeks/always-click-yes/internal/mcp"
)

// JiraMCP is the MCP server acy should launch for Jira access: a command,
// its args, and any environment variables it needs.
type JiraMCP struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// JiraConfig is the optional "jira" object in .acy.json: the MCP server a
// project wants merged in alongside acy's own, plus a few identifying
// fields the architect can use when filing tickets. A project with no
// "jira" key parses exactly as it always has — File.Jira stays nil, and
// nothing below runs.
type JiraConfig struct {
	Server     string   `json:"server,omitempty"`
	MCP        *JiraMCP `json:"mcp"`
	ProjectKey string   `json:"projectKey,omitempty"`
	Site       string   `json:"site,omitempty"`
}

// defaultJiraServer is the name the Jira MCP server is registered under
// when jira.server is not set.
const defaultJiraServer = "jira"

// resolve fills in every default and validates the jira config; path is
// only for error messages. Env values of the form "$NAME" are expanded in
// place from the process environment — this is how a project's .acy.json
// can reference a Jira API token without committing it to the repo.
func (j *JiraConfig) resolve(path string) error {
	if j.Server == "" {
		j.Server = defaultJiraServer
	}
	if j.MCP == nil {
		return fmt.Errorf("%s: jira.mcp is required", path)
	}
	if j.MCP.Command == "" {
		return fmt.Errorf("%s: jira.mcp.command is required", path)
	}
	if j.Server == mcp.ServerName {
		return fmt.Errorf("%s: jira.server %q must not collide with acy's own MCP server name %q", path, j.Server, mcp.ServerName)
	}

	for key, val := range j.MCP.Env {
		if !strings.HasPrefix(val, "$") || len(val) <= 1 {
			continue
		}
		name := val[1:]
		expanded, ok := os.LookupEnv(name)
		if !ok {
			return fmt.Errorf("%s: jira.mcp.env %q references environment variable %q, which is not set", path, key, name)
		}
		j.MCP.Env[key] = expanded
	}

	return nil
}

// ExtraServer returns this config's MCP server as an ExtraMCPServer, ready
// to be merged into --mcp-config alongside acy's own.
func (j *JiraConfig) ExtraServer() ExtraMCPServer {
	return ExtraMCPServer{
		Name:    j.Server,
		Command: j.MCP.Command,
		Args:    j.MCP.Args,
		Env:     j.MCP.Env,
	}
}
