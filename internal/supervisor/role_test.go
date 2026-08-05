package supervisor

import (
	"testing"

	"github.com/hweeks/always-click-yes/internal/mcp"
	"github.com/hweeks/always-click-yes/internal/ui"
)

// roleAndPrompt is the only fork ArchMode makes in NewSupervisor: everything
// else about the parent session — tools, hooks, the gate — stays identical.
func TestRoleAndPromptArchMode(t *testing.T) {
	role, prompt := roleAndPrompt(true)
	if role != mcp.RoleArchitect {
		t.Errorf("role = %v, want RoleArchitect", role)
	}
	if prompt != ui.ArchSystemPrompt {
		t.Error("prompt should be ui.ArchSystemPrompt")
	}
}

func TestRoleAndPromptDefault(t *testing.T) {
	role, prompt := roleAndPrompt(false)
	if role != mcp.RoleParent {
		t.Errorf("role = %v, want RoleParent", role)
	}
	if prompt != ui.ParentSystemPrompt {
		t.Error("prompt should be ui.ParentSystemPrompt")
	}
}
