// Package config generates the temporary files that wire claude back to the
// supervisor: the --settings file registering our PreToolUse hook (pointing at the
// gate socket), and the --mcp-config registering acy as an MCP server (pointing at
// the ask socket).
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/hweeks/always-click-yes/internal/mcp"
)

// WriteHookSettings writes a settings JSON registering a PreToolUse hook that
// invokes `<exePath> hook --socket <socketPath>` for every tool. It returns the
// settings file path.
func WriteHookSettings(dir, exePath, socketPath string) (string, error) {
	command := shellQuote(exePath) + " hook --socket " + shellQuote(socketPath)

	settings := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "*", // all tools
					"hooks": []any{
						map[string]any{"type": "command", "command": command},
					},
				},
			},
		},
	}
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// ExtraMCPServer is one additional MCP server to merge into the --mcp-config
// alongside acy's own — e.g. a project's configured Jira server. Exec'd
// directly like acy's own entry, so Command must not be shell-quoted.
type ExtraMCPServer struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
}

// WriteMCPConfig writes the --mcp-config JSON registering acy's own binary as an
// MCP server, so claude gains the mcp__acy__* tools (AskUserQuestion, PresentPlan)
// that `claude -p` otherwise has no equivalent of, plus any extra servers a
// project has configured (e.g. Jira). It returns the config path.
//
// Unlike the hook, whose command claude runs through a shell, an MCP server is
// exec'd directly from a command + args array — so the path must NOT be
// shellQuoted here, or claude would try to exec a binary whose name begins with a
// literal quote.
// One config is written per role, and they differ only in that flag. A child
// inherits nothing: it is launched with the child config, so it never sees
// Dispatch and cannot spawn children of its own.
func WriteMCPConfig(dir, exePath, socketPath string, role mcp.Role, extra ...ExtraMCPServer) (string, error) {
	mcpServers := map[string]any{
		mcp.ServerName: map[string]any{
			"command": exePath,
			"args":    []string{"mcp", "--socket", socketPath, "--role", string(role)},
		},
	}
	for _, e := range extra {
		server := map[string]any{
			"command": e.Command,
		}
		if len(e.Args) > 0 {
			server["args"] = e.Args
		}
		if len(e.Env) > 0 {
			server["env"] = e.Env
		}
		mcpServers[e.Name] = server
	}
	cfg := map[string]any{
		"mcpServers": mcpServers,
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "mcp-"+string(role)+".json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// shellQuote single-quotes a string for safe use in a POSIX shell command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
