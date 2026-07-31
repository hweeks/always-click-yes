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

func TestChildEnvOverlaysGatewayAndStripsUpstreamKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-upstream")
	t.Setenv("ANTHROPIC_BASE_URL", "https://wrong.example")

	env := (Options{
		Env:      map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:4000", "ANTHROPIC_AUTH_TOKEN": "local-token"},
		StripEnv: []string{"OPENAI_API_KEY"},
	}).childEnv()

	if slices.Contains(env, "OPENAI_API_KEY=sk-upstream") {
		t.Fatal("upstream key leaked into claude environment")
	}
	if !slices.Contains(env, "ANTHROPIC_BASE_URL=http://127.0.0.1:4000") || !slices.Contains(env, "ANTHROPIC_AUTH_TOKEN=local-token") {
		t.Fatalf("gateway environment missing: %v", env)
	}
	if slices.Contains(env, "ANTHROPIC_BASE_URL=https://wrong.example") {
		t.Fatal("inherited endpoint was not replaced")
	}
}
