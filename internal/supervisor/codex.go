package supervisor

import (
	"encoding/json"
	"fmt"

	"github.com/hweeks/always-click-yes/internal/codex"
	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/mcp"
	"github.com/hweeks/always-click-yes/internal/orchestrator"
	"github.com/hweeks/always-click-yes/internal/session"
	"github.com/hweeks/always-click-yes/internal/ui"
)

// codexSessionSources returns the /resume sources a codex run gets: an
// honestly empty session list, and a Replay that errors rather than
// silently falling back to claude's ~/.claude/projects transcript reader —
// codex's own on-disk transcript format is entirely different
// (docs/codex-cli-findings.md §7), so reading it as claude's would either
// find nothing or, worse, find an unrelated claude session that happens to
// share an id. Real codex transcript replay is deferred to a follow-up
// task; until then this is the honest answer, not a silent fallback.
func codexSessionSources() (func() ([]session.Info, error), func(id string) ([]driver.Event, error)) {
	sessions := func() ([]session.Info, error) { return nil, nil }
	replay := func(id string) ([]driver.Event, error) {
		return nil, fmt.Errorf("resuming a codex session is not supported yet")
	}
	return sessions, replay
}

// codexApprovalPolicy is the approval policy every codex-backed session runs
// under, parent and child alike: "untrusted", not "on-request".
//
// docs/codex-cli-findings.md §3 live-verified "on-request" raising a real
// item/commandExecution/requestApproval — but only for a write the sandbox
// itself was already about to block. That leaves it to the *model's own
// judgment* whether an ordinary-looking command needs a human decision at
// all, which is not the guarantee acy makes anywhere else: on claude, every
// Bash/Edit/Write call raises the PreToolUse hook unconditionally, no matter
// how safe the model believes it is. "untrusted" is codex's mode that asks
// for a decision on anything not on its own small built-in trusted list,
// which is the closer analog to that invariant. Without it, "the parent's
// shell calls would run unwatched" is not hypothetical — it is exactly what
// "on-request" plus a command the model considers safe would do.
const codexApprovalPolicy = "untrusted"

// codexApprovalRaising reports whether policy is one of codex's modes that
// can actually produce a ServerRequest approval at all — "never" (and the
// empty default) cannot, by definition (docs/codex-cli-findings.md §3).
// assertCodexParentSafe checks against this rather than strict equality
// with codexApprovalPolicy, so a future change to the policy chosen above
// does not also have to be echoed into the safety check.
func codexApprovalRaising(policy string) bool {
	switch policy {
	case "on-request", "untrusted":
		return true
	default:
		return false
	}
}

// assertCodexParentSafe is the last line of defence before a codex-backed
// supervising session is launched: it refuses, with a clear error, unless
// the options actually carry a read-only sandbox and a policy that raises
// approvals at all.
//
// thread/start's explicit sandbox/approvalPolicy params take precedence over
// the user's own ~/.codex/config.toml (docs/codex-cli-findings.md §2's
// per-invocation config), which is what makes this enforceable — but the
// point of asserting it here, rather than trusting the construction above to
// stay correct forever, is that a future edit loosening either field must
// fail loudly at launch instead of quietly running the supervising session
// unsandboxed and ungated.
func assertCodexParentSafe(opts codex.Options) error {
	if opts.Sandbox != "read-only" {
		return fmt.Errorf("codex parent safety check failed: sandbox is %q, want %q — refusing to launch an unsandboxed supervising session", opts.Sandbox, "read-only")
	}
	if !codexApprovalRaising(opts.ApprovalPolicy) {
		return fmt.Errorf("codex parent safety check failed: approval policy is %q, which never raises an approval — refusing to launch a supervising session whose tool calls acy's gate would never see", opts.ApprovalPolicy)
	}
	return nil
}

// codexMCPServerConfig builds the inline config.mcp_servers overlay
// thread/start accepts (docs/codex-cli-findings.md §5, live-verified:
// params.config takes an mcp_servers table directly, no file and no write
// to ~/.codex/config.toml). command/args are exactly what config.WriteMCPConfig
// generates for claude's --mcp-config file — config.MCPServerArgv is the one
// shared construction, so the two paths can never drift into invoking acy's
// own MCP server two different ways.
func codexMCPServerConfig(exe, socketPath string, role mcp.Role) map[string]any {
	command, args := config.MCPServerArgv(exe, socketPath, role)
	return map[string]any{
		"mcp_servers": map[string]any{
			mcp.ServerName: map[string]any{
				"command": command,
				"args":    args,
			},
		},
	}
}

// codexParentOptions builds the codex.Options for the supervising session —
// the parent, or the architect in arch mode; never a child. Split out as a
// plain function, rather than folded into the launcher closure, so a test
// can assert on the constructed options directly without starting a process.
func codexParentOptions(f Flags, exe, mcpSocket string, parentRole mcp.Role, parentPrompt string, env map[string]string, stripEnv []string, spec ui.LaunchSpec) codex.Options {
	model := spec.Model
	if model == "" {
		model = f.Model
	}
	return codex.Options{
		Bin:      f.CodexBin,
		Cwd:      f.Cwd,
		Model:    model,
		ResumeID: spec.ResumeID,
		Env:      env,
		StripEnv: stripEnv,

		// Always read-only, with no knob to change it. Codex has no way to
		// remove the shell tool from the model's registry at all
		// (docs/codex-cli-findings.md §4) — sandbox and approval policy only
		// ever wrap an ever-present tool — so this plus ui.Config.ParentNoExec
		// (set true by the caller that wires this up) is what reconstructs,
		// for a codex-backed run, the guarantee claude gets structurally: the
		// session acy talks to cannot change the code.
		Sandbox: "read-only",

		// See codexApprovalPolicy's own comment. This is the single most
		// important option on this call: without it, acy's countdown gate
		// never sees a single one of the parent's tool calls.
		ApprovalPolicy: codexApprovalPolicy,

		// codex's direct equivalent of claude's --append-system-prompt
		// (docs/codex-cli-findings.md §6) — a plain thread/start param, no
		// flag or file needed, unlike claude's.
		DeveloperInstructions: parentPrompt,

		// The inline overlay giving the parent acy's own MCP server —
		// mcp__acy__Dispatch/Finish/AskUserQuestion/PresentPlan — live-
		// verified to work with no file and no write to the user's own
		// ~/.codex/config.toml (docs/codex-cli-findings.md §5).
		Config: codexMCPServerConfig(exe, mcpSocket, parentRole),
	}
}

// codexChildOptions builds the codex.Options for one dispatched child. It
// mirrors codexParentOptions in shape and diverges from it in exactly the
// ways the claude spawn closure already diverges from launcher — see each
// field's own comment for which is which.
func codexChildOptions(f Flags, exe, mcpSocket string, env map[string]string, stripEnv []string) codex.Options {
	opts := codex.Options{
		Bin:      f.CodexBin,
		Cwd:      f.Cwd,
		Model:    childModel(f),
		Env:      env,
		StripEnv: stripEnv,

		// Writable — the deliberate asymmetry with the parent's read-only
		// sandbox above. A child's whole job is to edit; wrapping it in a
		// sandbox it would just need to escape on every call would buy
		// nothing and cost every edit an extra approval round trip.
		Sandbox: "workspace-write",

		// The same policy as the parent, not a looser one: every child tool
		// call still counts down. A child must never be more trusted than it
		// is today.
		ApprovalPolicy: codexApprovalPolicy,

		DeveloperInstructions: ui.ChildSystemPrompt,

		// codex's --json-schema analog (TurnStartParams.outputSchema,
		// docs/codex-cli-findings.md §10) — attached to every turn/start this
		// driver issues. That recon could not live-verify which field on
		// turn completion actually carries the validated result (§10's own
		// caveat): if a child's report never arrives, that mismatch is the
		// first thing to check, not this schema.
		OutputSchema: json.RawMessage(orchestrator.ReportSchema),

		// The child's own MCP role, never the parent's — a child that
		// inherited Dispatch could spawn grandchildren without limit
		// (AGENTS.md's "THE RECURRING BUG" note is about a different tool,
		// but the parent/child MCP config split it depends on is this exact
		// mechanism on the claude side too).
		Config: codexMCPServerConfig(exe, mcpSocket, mcp.RoleChild),
	}
	if f.ChildEffort != "" {
		opts.Effort = f.ChildEffort
	}
	return opts
}

// newCodexDriver constructs a codex.Driver and attaches it to the bridge
// before Start is ever called, so no approval it might raise during its own
// handshake can arrive unattached. Shared by the parent launcher and the
// child spawner — both need exactly this, and a second copy of it is exactly
// the kind of drift that would leave one of them unattached and hanging on
// its first tool call.
func newCodexDriver(opts codex.Options, bridge *codex.Bridge) *codex.Driver {
	d := codex.New(opts)
	bridge.Attach(d)
	return d
}
