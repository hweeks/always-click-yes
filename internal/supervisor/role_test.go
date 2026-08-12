package supervisor

import (
	"testing"

	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/mcp"
	"github.com/hweeks/always-click-yes/internal/ui"
)

// roleAndPrompt is the only fork ArchMode makes in NewSupervisor: everything
// else about the parent session — tools, hooks, the gate — stays identical.
func TestRoleAndPromptArchMode(t *testing.T) {
	role, prompt := roleAndPrompt(true, "ask", nil)
	if role != mcp.RoleArchitect {
		t.Errorf("role = %v, want RoleArchitect", role)
	}
	if prompt != ui.ArchSystemPromptFor("ask", nil) {
		t.Error("prompt should be ui.ArchSystemPromptFor(\"ask\", nil)")
	}
}

func TestRoleAndPromptDefault(t *testing.T) {
	role, prompt := roleAndPrompt(false, "", nil)
	if role != mcp.RoleParent {
		t.Errorf("role = %v, want RoleParent", role)
	}
	if prompt != ui.ParentSystemPrompt {
		t.Error("prompt should be ui.ParentSystemPrompt")
	}
}

// jiraExtraServers is the only path Jira config takes into the ARCHITECT's
// own --mcp-config: nil unless both ArchMode is set and the project
// configured a jira section, so a plain run and a dispatched child never see
// a difference at all.
// roleAndPrompt must thread the configured Jira target through to the
// prompt unchanged — this is the only place a project's projectKey/site
// ever reach the architect.
func TestRoleAndPromptArchModeThreadsJiraConfig(t *testing.T) {
	jiraCfg := &config.JiraConfig{Server: "jira", ProjectKey: "ENG", Site: "acme.atlassian.net"}
	_, prompt := roleAndPrompt(true, "off", jiraCfg)
	if prompt != ui.ArchSystemPromptFor("off", jiraCfg) {
		t.Error("prompt should be ui.ArchSystemPromptFor(\"off\", jiraCfg)")
	}
}

func TestJiraExtraServers(t *testing.T) {
	jiraCfg := &config.JiraConfig{
		Server: "jira",
		MCP:    &config.JiraMCP{Command: "jira-mcp"},
	}

	if got := jiraExtraServers(Flags{ArchMode: false, Jira: jiraCfg}); got != nil {
		t.Errorf("ArchMode=false: jiraExtraServers = %v, want nil", got)
	}
	if got := jiraExtraServers(Flags{ArchMode: true, Jira: nil}); got != nil {
		t.Errorf("Jira=nil: jiraExtraServers = %v, want nil", got)
	}

	got := jiraExtraServers(Flags{ArchMode: true, Jira: jiraCfg})
	if len(got) != 1 {
		t.Fatalf("len(jiraExtraServers) = %d, want 1", len(got))
	}
	if got[0].Name != "jira" {
		t.Errorf("jiraExtraServers[0].Name = %q, want %q", got[0].Name, "jira")
	}
}
