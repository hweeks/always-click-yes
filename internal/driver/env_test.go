package driver

import (
	"slices"
	"strings"
	"testing"
)

// An ANTHROPIC_API_KEY in the ambient shell silently overrides the claude.ai login
// for headless runs, so the default must drop it.
func TestChildEnvStripsAPIKeyByDefault(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("PATH", "/usr/bin")

	env := Options{}.childEnv()

	if slices.ContainsFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, "ANTHROPIC_API_KEY=")
	}) {
		t.Error("ANTHROPIC_API_KEY survived childEnv; runs would bill the API account")
	}
	if !slices.Contains(env, "PATH=/usr/bin") {
		t.Error("childEnv dropped the rest of the environment")
	}
}

func TestChildEnvKeepsAPIKeyWhenOptedIn(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")

	if env := (Options{UseAPIKey: true}).childEnv(); env != nil {
		t.Errorf("UseAPIKey should inherit the parent env (nil), got %d entries", len(env))
	}
}
