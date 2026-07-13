package driver

import (
	"slices"
	"strings"
	"testing"
)

func TestArgsAllowedTools(t *testing.T) {
	args := Options{
		PermissionMode: "plan",
		AllowedTools:   []string{"Monitor", "mcp__srv__Read"},
	}.Args()

	i := slices.Index(args, "--allowedTools")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("expected --allowedTools with a value, got: %v", args)
	}
	if got := args[i+1]; got != "Monitor,mcp__srv__Read" {
		t.Errorf("expected comma-joined tools, got %q", got)
	}
}

func TestArgsNoAllowedToolsWhenEmpty(t *testing.T) {
	args := Options{PermissionMode: "plan"}.Args()
	if slices.Contains(args, "--allowedTools") {
		t.Errorf("did not expect --allowedTools with no tools set, got: %v", strings.Join(args, " "))
	}
}
