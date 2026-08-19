package supervisor

import (
	"os"
	"strings"
	"testing"

	"github.com/hweeks/always-click-yes/internal/codex"
	"github.com/hweeks/always-click-yes/internal/mcp"
	"github.com/hweeks/always-click-yes/internal/orchestrator"
	"github.com/hweeks/always-click-yes/internal/ui"
)

// mcpServersOf decodes the "mcp_servers" table out of a codex.Options.Config
// overlay, the shape codexMCPServerConfig builds.
func mcpServersOf(t *testing.T, cfg map[string]any) map[string]any {
	t.Helper()
	raw, ok := cfg["mcp_servers"]
	if !ok {
		t.Fatalf("config = %+v, want an mcp_servers key", cfg)
	}
	servers, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("mcp_servers = %#v, want a map", raw)
	}
	return servers
}

// mcpArgsFor pulls the args array out of the acy MCP server entry, the
// place --role travels.
func mcpArgsFor(t *testing.T, cfg map[string]any) []string {
	t.Helper()
	servers := mcpServersOf(t, cfg)
	acy, ok := servers[mcp.ServerName].(map[string]any)
	if !ok {
		t.Fatalf("mcp_servers[%q] = %#v, want a map", mcp.ServerName, servers[mcp.ServerName])
	}
	args, ok := acy["args"].([]string)
	if !ok {
		t.Fatalf("args = %#v, want []string", acy["args"])
	}
	return args
}

// TestCodexParentOptionsCarrySafetyInvariants proves the parent's
// codex.Options carry every guarantee this backend depends on: a read-only
// sandbox, an approval policy that actually raises requests, the parent's
// own MCP role (never the child's), and the parent system prompt as
// DeveloperInstructions.
func TestCodexParentOptionsCarrySafetyInvariants(t *testing.T) {
	f := Flags{CodexBin: "codex-bin", Model: "gpt-x"}
	opts := codexParentOptions(f, "/bin/acy", "/tmp/mcp.sock", mcp.RoleParent, "you are the parent", nil, nil, ui.LaunchSpec{})

	if opts.Sandbox != "read-only" {
		t.Errorf("parent Sandbox = %q, want %q", opts.Sandbox, "read-only")
	}
	if !codexApprovalRaising(opts.ApprovalPolicy) {
		t.Errorf("parent ApprovalPolicy = %q, want a policy that raises approvals", opts.ApprovalPolicy)
	}
	if opts.DeveloperInstructions != "you are the parent" {
		t.Errorf("parent DeveloperInstructions = %q, want the parent prompt", opts.DeveloperInstructions)
	}
	if opts.Bin != "codex-bin" {
		t.Errorf("parent Bin = %q, want f.CodexBin", opts.Bin)
	}
	if opts.Model != "gpt-x" {
		t.Errorf("parent Model = %q, want f.Model", opts.Model)
	}

	args := mcpArgsFor(t, opts.Config)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--role "+string(mcp.RoleParent)) {
		t.Errorf("args = %v, want the parent role", args)
	}
}

// TestCodexParentOptionsUsesLaunchSpecModelOverFlagsModel mirrors the claude
// launcher's own precedence: a per-launch model override wins over the run's
// default.
func TestCodexParentOptionsUsesLaunchSpecModelOverFlagsModel(t *testing.T) {
	f := Flags{Model: "default-model"}
	opts := codexParentOptions(f, "/bin/acy", "/tmp/mcp.sock", mcp.RoleParent, "prompt", nil, nil, ui.LaunchSpec{Model: "override-model", ResumeID: "sess-123"})
	if opts.Model != "override-model" {
		t.Errorf("Model = %q, want the LaunchSpec override", opts.Model)
	}
	if opts.ResumeID != "sess-123" {
		t.Errorf("ResumeID = %q, want the LaunchSpec's", opts.ResumeID)
	}
}

// TestCodexChildOptionsCarryChildInvariants proves the dispatched child's
// codex.Options carry the deliberate asymmetry with the parent: a writable
// sandbox, the child's own MCP role, and the report schema as OutputSchema.
func TestCodexChildOptionsCarryChildInvariants(t *testing.T) {
	f := Flags{CodexBin: "codex-bin", ChildModel: "gpt-child", ChildEffort: "high"}
	opts := codexChildOptions(f, "/bin/acy", "/tmp/mcp.sock", nil, nil)

	if opts.Sandbox != "workspace-write" {
		t.Errorf("child Sandbox = %q, want %q", opts.Sandbox, "workspace-write")
	}
	if opts.ApprovalPolicy != codexApprovalPolicy {
		t.Errorf("child ApprovalPolicy = %q, want exactly the parent's %q — a child must never be more trusted", opts.ApprovalPolicy, codexApprovalPolicy)
	}
	if opts.DeveloperInstructions != ui.ChildSystemPrompt {
		t.Errorf("child DeveloperInstructions = %q, want ui.ChildSystemPrompt", opts.DeveloperInstructions)
	}
	if string(opts.OutputSchema) != orchestrator.ReportSchema {
		t.Errorf("child OutputSchema does not match orchestrator.ReportSchema")
	}
	if opts.Effort != "high" {
		t.Errorf("child Effort = %q, want f.ChildEffort", opts.Effort)
	}
	if opts.Model != "gpt-child" {
		t.Errorf("child Model = %q, want f.ChildModel", opts.Model)
	}

	args := mcpArgsFor(t, opts.Config)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--role "+string(mcp.RoleChild)) {
		t.Errorf("args = %v, want the child role", args)
	}
	if strings.Contains(joined, "--role "+string(mcp.RoleParent)) {
		t.Errorf("args = %v, must never carry the parent role", args)
	}
}

// TestCodexChildOptionsDoesNotLeakClaudeDefaultModel proves the default
// --child-model=sonnet does not get sent to Codex, where it is not a supported
// ChatGPT model name. The first dogfood child reached app-server successfully
// but failed its turn with that exact 400 before doing any work.
func TestCodexChildOptionsDoesNotLeakClaudeDefaultModel(t *testing.T) {
	opts := codexChildOptions(Flags{Model: "gpt-parent", ChildModel: "sonnet"}, "/bin/acy", "/tmp/mcp.sock", nil, nil)
	if opts.Model != "gpt-parent" {
		t.Errorf("child Model = %q, want parent Codex model %q rather than Claude default sonnet", opts.Model, "gpt-parent")
	}

	opts = codexChildOptions(Flags{ChildModel: "sonnet"}, "/bin/acy", "/tmp/mcp.sock", nil, nil)
	if opts.Model != "" {
		t.Errorf("child Model = %q, want empty so Codex selects its own default", opts.Model)
	}
}

// TestAssertCodexParentSafeAcceptsTheRealConstruction proves the actual
// codexParentOptions output passes the safety check — the check must not be
// stricter than what the constructor above actually produces.
func TestAssertCodexParentSafeAcceptsTheRealConstruction(t *testing.T) {
	opts := codexParentOptions(Flags{}, "/bin/acy", "/tmp/mcp.sock", mcp.RoleParent, "prompt", nil, nil, ui.LaunchSpec{})
	if err := assertCodexParentSafe(opts); err != nil {
		t.Fatalf("the real construction failed its own safety check: %v", err)
	}
}

// TestAssertCodexParentSafeRejectsWritableSandbox proves the refusal: a
// parent whose sandbox has been loosened to writable must never launch.
func TestAssertCodexParentSafeRejectsWritableSandbox(t *testing.T) {
	opts := codexParentOptions(Flags{}, "/bin/acy", "/tmp/mcp.sock", mcp.RoleParent, "prompt", nil, nil, ui.LaunchSpec{})
	opts.Sandbox = "workspace-write"
	if err := assertCodexParentSafe(opts); err == nil {
		t.Fatal("a writable parent sandbox must be rejected")
	}
}

// TestAssertCodexParentSafeRejectsApprovalsDisabled proves the other half of
// the refusal: a parent whose approval policy would never raise a request
// must never launch either.
func TestAssertCodexParentSafeRejectsApprovalsDisabled(t *testing.T) {
	opts := codexParentOptions(Flags{}, "/bin/acy", "/tmp/mcp.sock", mcp.RoleParent, "prompt", nil, nil, ui.LaunchSpec{})
	opts.ApprovalPolicy = "never"
	if err := assertCodexParentSafe(opts); err == nil {
		t.Fatal("an approval policy that never raises a request must be rejected")
	}
}

// TestNewCodexDriverAttachesToBridge proves the one thing this task's own
// description warns is the easiest to forget: a driver built for either the
// parent or a child is attached to the Bridge before it is ever started, so
// no approval it might raise during its own handshake can arrive
// unattached. Exercised via codex.NewBridge and codex.New directly — never
// Start — so nothing here launches a process.
func TestNewCodexDriverAttachesToBridge(t *testing.T) {
	bridge := codex.NewBridge()
	defer bridge.Close()

	opts := codexChildOptions(Flags{}, "/bin/acy", "/tmp/mcp.sock", nil, nil)
	d := newCodexDriver(opts, bridge)
	if d == nil {
		t.Fatal("newCodexDriver returned nil")
	}
	if got := bridge.Attached(); got != 1 {
		t.Fatalf("bridge.Attached() = %d, want 1", got)
	}

	// A second driver (standing in for a second dispatched child) must add
	// to the same bridge, not replace the first.
	d2 := newCodexDriver(codexChildOptions(Flags{}, "/bin/acy", "/tmp/mcp.sock", nil, nil), bridge)
	if d2 == nil {
		t.Fatal("newCodexDriver returned nil")
	}
	if got := bridge.Attached(); got != 2 {
		t.Fatalf("bridge.Attached() = %d, want 2 after a second driver", got)
	}
}

// TestBuildCodexPiecesWiresGateReqsFromTheBridge proves ui.Config.GateReqs
// for a codex run really is the codex.Bridge's own stream, not gate.Listen's
// — and that ParentNoExec is true unconditionally, never false the way
// claude leaves it.
func TestBuildCodexPiecesWiresGateReqsFromTheBridge(t *testing.T) {
	// A short, test-name-independent dir, anchored the same way NewSupervisor
	// itself anchors its socket dir: macOS's ~104-byte sockaddr_un limit has
	// already bitten this exact pattern twice (AGENTS.md) when a socket path
	// was built from t.TempDir(), which embeds the long test name.
	tmp, err := os.MkdirTemp("", "acysvtest")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	askBridge, err := mcp.Listen(tmp)
	if err != nil {
		t.Fatalf("mcp.Listen: %v", err)
	}
	defer func() { _ = askBridge.Close() }()

	codexBridge := codex.NewBridge()
	defer codexBridge.Close()

	pieces := buildCodexPieces(Flags{}, "/bin/acy", askBridge, codexBridge, mcp.RoleParent, "prompt", nil, nil)

	if !pieces.parentNoExec {
		t.Error("codex pieces must set ParentNoExec true")
	}
	if pieces.gateReqs == nil {
		t.Fatal("codex pieces must set a non-nil GateReqs")
	}
	if pieces.gateReqs != codexBridge.Requests() {
		t.Error("GateReqs must be exactly the codex.Bridge's own Requests() stream, not a different channel")
	}
	if pieces.launcher == nil || pieces.spawn == nil {
		t.Fatal("codex pieces must set both a launcher and a spawn")
	}
}

// TestCodexSessionSourcesReportNothingAvailable proves the deferred /resume
// decision: a codex run's Sessions must report an empty list (never claude's
// transcripts, which belong to a different backend), and Replay must error
// honestly rather than silently reading a claude transcript that happens to
// share a session id.
func TestCodexSessionSourcesReportNothingAvailable(t *testing.T) {
	sessions, replay := codexSessionSources()

	list, err := sessions()
	if err != nil {
		t.Fatalf("sessions() error = %v, want no error", err)
	}
	if len(list) != 0 {
		t.Errorf("sessions() = %v, want an empty list", list)
	}

	if _, err := replay("some-session-id"); err == nil {
		t.Fatal("replay() must error — resuming a codex session is not supported yet")
	}
}

// TestNewSupervisorAgentCodexBuildsAWorkingSupervisor is the end-to-end
// offline check: --agent codex must build a supervisor without error and
// without ever starting a process (NewSupervisor only wires closures; a
// driver only actually launches once the model's Init runs, which this test
// never triggers), and its Sessions must already reflect the deferred
// /resume decision.
func TestNewSupervisorAgentCodexBuildsAWorkingSupervisor(t *testing.T) {
	sup, err := NewSupervisor(t.Context(), Flags{Agent: "codex"})
	if err != nil {
		t.Fatalf("--agent codex should build a working supervisor: %v", err)
	}
	defer sup.Close()

	if sup.Sessions == nil {
		t.Fatal("Sessions must not be nil")
	}
	list, err := sup.Sessions()
	if err != nil {
		t.Fatalf("codex Sessions() should report an empty list, not an error: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("codex Sessions() = %v, want an empty list (deferred; claude's transcripts are the wrong store)", list)
	}
}
