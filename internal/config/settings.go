// Package config generates the temporary --settings file that registers our
// PreToolUse hook, pointing claude at the supervisor's gate socket.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// shellQuote single-quotes a string for safe use in a POSIX shell command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
