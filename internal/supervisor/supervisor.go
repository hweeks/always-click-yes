// Package supervisor wires the gate server, hook settings, MCP bridge,
// launcher, spawner and orchestrator into a running acy supervisor. It is the
// shared foundation for every front end that starts one — `acy run`'s
// terminal, `acy serve`'s HTTP hub, and a future headless `acy engineer` —
// none of which may import each other.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/codex"
	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/fleet"
	"github.com/hweeks/always-click-yes/internal/gate"
	"github.com/hweeks/always-click-yes/internal/gateway"
	"github.com/hweeks/always-click-yes/internal/gitops"
	"github.com/hweeks/always-click-yes/internal/mcp"
	"github.com/hweeks/always-click-yes/internal/orchestrator"
	"github.com/hweeks/always-click-yes/internal/session"
	"github.com/hweeks/always-click-yes/internal/state"
	"github.com/hweeks/always-click-yes/internal/ui"
)

// MaxSnapshots is how many resumable runs acy keeps state for. Old enough to
// cover "the thing I was doing last week", small enough that the directory
// never becomes a problem of its own.
const MaxSnapshots = 200

// Defaults are deliberately finite. A child that needs more can be resumed with
// an explicit budget, but a vague dispatch must not consume an entire login
// window before anyone gets a chance to inspect its first report.
const (
	DefaultTaskBudgetUSD = 10.0
	DefaultRunBudgetUSD  = 50.0
)

// Flags configures a supervisor run. Cobra binds the fields it exposes as flags;
// Cwd and HookBin have no flag and exist for the live e2e suite, which needs to run
// the supervisor against a scratch project and a real acy binary.
type Flags struct {
	Model     string
	Bin       string
	Countdown time.Duration
	LogPath   string
	MaxLines  int
	PlanTools []string
	UseAPIKey bool
	// Provider selects an optional Anthropic-compatible gateway. Hosted
	// OpenAI-compatible APIs run through a private LiteLLM sidecar; vLLM points
	// at an already-running local Messages API.
	Provider   string
	GatewayBin string
	GatewayURL string

	// Agent selects which coding-agent CLI acy supervises: "claude" or
	// "codex". This is deliberately a different axis from Provider above.
	// Provider selects which *model backend* claude talks to — an
	// Anthropic-compatible gateway (LiteLLM or vLLM) sitting behind
	// ANTHROPIC_BASE_URL/ANTHROPIC_AUTH_TOKEN. Agent selects which *CLI
	// process* acy drives in the first place. Collapsing the two would make
	// `--provider openai --agent codex` unreadable — one flag would be
	// answering two unrelated questions at once. CodexBin is codex's
	// equivalent of Bin (claude's binary path).
	Agent    string
	CodexBin string

	// Child knobs. A dispatched child is a different kind of session from the
	// supervising one — it does the work, unwatched, and then throws its whole
	// context away — so it is worth being able to price and pace it separately.
	ChildModel  string
	ChildEffort string
	TaskBudget  float64
	RunBudget   float64
	Resume      string
	Continue    bool

	Cwd     string // working directory for claude; "" = acy's own
	HookBin string // binary the PreToolUse hook re-invokes; "" = this executable

	// Fleet wires arch mode's remote engineers into the parent session (nil =
	// today's behavior: the fleet tools are refused with mcp.FleetUnavailable).
	// acy arch is the only caller that sets this — it builds a fleet.Manager
	// from the project's fleet config before calling NewSupervisor.
	Fleet ui.FleetManager

	// Tickets wires arch mode's ticket board into the parent session (nil =
	// ReadTickets/UpdateTicket are refused with mcp.TicketsUnavailable). acy
	// arch is the only caller that sets this, alongside Fleet.
	Tickets ui.TicketStore

	// Jira wires the project's optional Jira MCP server into the run (nil in
	// every existing caller). Only `acy arch` sets it, and only the
	// ARCHITECT's own --mcp-config ever merges it in — never the dispatched
	// child's, which stays exactly as it always has.
	Jira *config.JiraConfig

	// ArchMode runs the parent session as the architect (mcp.RoleArchitect,
	// ui.ArchSystemPromptFor) instead of the default parent (mcp.RoleParent,
	// ui.ParentSystemPrompt). False in every existing caller, so run/serve are
	// unchanged.
	ArchMode bool

	// ConfigPath is the .acy.json the settings were overlaid from, "" if none.
	// Not a flag: runSupervisor stamps it so the UI can say where its settings
	// came from.
	ConfigPath string

	// StartupNote is a human-readable notice the UI shows once at startup, ""
	// if none. Not a flag: `acy arch` is the only caller that sets it today,
	// e.g. when fleet.stackMode "ask" got silently downgraded to "off"
	// because gh-stack wasn't available — the run must not be blocked over
	// that, but a human should still be told why stacking isn't on offer.
	StartupNote string

	// StackMode is the run's already-resolved effective fleet.stackMode ("off",
	// "ask", or "chain") — never the raw configured value, which
	// cli/arch.go's resolveStackMode may have downgraded. Not a flag: `acy
	// arch` is the only caller that sets it, alongside StartupNote, so the
	// architect's system prompt can say the right thing about whether
	// stacking is on offer.
	StackMode string

	// GitRunner is AssembleStack's git/gh runner. Not a flag: `acy arch` is
	// the only caller that sets it, alongside Fleet — it always passes
	// gitops.DefaultRunner.
	GitRunner gitops.Runner

	// Trunk is the fleet's resolved base branch, the trunk AssembleStack
	// rebases and links against. Not a flag: `acy arch` is the only caller
	// that sets it, alongside Fleet/StackMode.
	Trunk string

	// RenderHTML asks each transcript entry to carry a server-rendered HTML
	// fragment (ui.Frame.Entries[].HTML). Not a flag either: `acy serve` stamps
	// it because its client is a webview that cannot render markdown itself,
	// and `acy run` leaves it off because a terminal would pay goldmark and
	// chroma on every ingested entry to produce markup nothing ever reads.
	RenderHTML bool

	// AltScreen asks for the alternate screen buffer. Also not a flag:
	// runSupervisor stamps it from ACY_NO_ALTSCREEN. Bubble Tea v2 takes the
	// alt-screen off the tea.View the model returns rather than off a program
	// option, so the decision has to travel all the way into the model — and a
	// headless caller (the e2e harness) leaves it alone by leaving it false.
	AltScreen bool

	// InterceptAsk, when set, is offered every AskUserQuestion request before
	// the UI sees it. Return true to take ownership (the interceptor must
	// eventually Resolve or Abandon the Pending); false forwards it to the
	// model's picker as usual. Non-Ask tools are never offered.
	InterceptAsk func(p *mcp.Pending) bool
}

// applyFileConfig overlays a .acy.json onto the flags. Precedence is
// defaults < file < explicit flags: a field only moves when the file sets it
// AND the corresponding flag was not given on the command line. `changed`
// answers by flag name — cobra's FlagSet.Changed — so the overlay can tell a
// defaulted flag from an explicit one.
func applyFileConfig(f *Flags, c config.File, changed func(string) bool) {
	if c.Model != "" && !changed("model") {
		f.Model = c.Model
	}
	if c.ClaudeBin != "" && !changed("claude-bin") {
		f.Bin = c.ClaudeBin
	}
	if c.Countdown != nil && !changed("countdown") {
		f.Countdown = time.Duration(*c.Countdown)
	}
	if c.Log != nil && !changed("log") {
		f.LogPath = *c.Log
	}
	if c.MaxLines != nil && !changed("max-lines") {
		f.MaxLines = *c.MaxLines
	}
	if c.PlanTools != nil && !changed("plan-tools") {
		f.PlanTools = c.PlanTools
	}
	if c.UseAPIKey != nil && !changed("use-api-key") {
		f.UseAPIKey = *c.UseAPIKey
	}
	if c.Provider != "" && !changed("provider") {
		f.Provider = c.Provider
	}
	if c.GatewayBin != "" && !changed("gateway-bin") {
		f.GatewayBin = c.GatewayBin
	}
	if c.GatewayURL != "" && !changed("gateway-url") {
		f.GatewayURL = c.GatewayURL
	}
	if c.Agent != "" && !changed("agent") {
		f.Agent = c.Agent
	}
	if c.CodexBin != "" && !changed("codex-bin") {
		f.CodexBin = c.CodexBin
	}
	if c.ChildModel != "" && !changed("child-model") {
		f.ChildModel = c.ChildModel
	}
	if c.ChildEffort != "" && !changed("child-effort") {
		f.ChildEffort = c.ChildEffort
	}
	if c.TaskBudget != nil && !changed("task-budget") {
		f.TaskBudget = *c.TaskBudget
	}
	if c.RunBudget != nil && !changed("run-budget") {
		f.RunBudget = *c.RunBudget
	}
	f.ConfigPath = c.Path
}

// DefaultParentTools is the built-in registry the *supervising* session runs with
// (--tools), in both phases — it is one process now, and arming does not change
// what it can reach for.
//
// Three read-only tools, and nothing else. Not a permission setting: Write,
// Edit, Bash and Task are not denied, they do not exist in this registry at all,
// which is a guarantee no prompt can talk its way past. It is also what lets the
// system prompt stop saying "do not implement" — there is nothing to implement
// with. Real work happens in dispatched children, which do get the full set.
//
// Bash is the notable removal: it used to be here so a plan could be informed by
// running the tests. It is also the one tool that can change anything from a
// read-only registry, and its output is exactly the unbounded tool result that
// grows a context. Dispatch a task to run things instead.
var DefaultParentTools = []string{"Read", "Grep", "Glob"}

// childModel keeps the parent as the fallback while making an explicit
// --child-model (or .acy.json childModel) real. The old launcher accidentally
// always used f.Model, turning the most important cost knob into a no-op.
func childModel(f Flags) string {
	if f.ChildModel != "" {
		return f.ChildModel
	}
	return f.Model
}

// roleAndPrompt picks the parent session's MCP role and appended system
// prompt. archMode is the only fork: everything else about the parent —
// tools, hooks, the gate — stays identical between the two, which is what
// lets a child never see the difference (it is always RoleChild). stackMode
// and jira are only consulted in the arch case, where stackMode is the run's
// already-resolved effective fleet.stackMode and jira is the project's
// configured Jira section, or nil when none is configured.
func roleAndPrompt(archMode bool, stackMode string, jira *config.JiraConfig) (mcp.Role, string) {
	if archMode {
		return mcp.RoleArchitect, ui.ArchSystemPromptFor(stackMode, jira)
	}
	return mcp.RoleParent, ui.ParentSystemPrompt
}

// jiraExtraServers returns the extra MCP servers the ARCHITECT's own
// --mcp-config should merge in. Nil outside arch mode or when no jira
// section is configured, so a plain run and a dispatched child never see
// a difference at all.
func jiraExtraServers(f Flags) []config.ExtraMCPServer {
	if !f.ArchMode || f.Jira == nil {
		return nil
	}
	return []config.ExtraMCPServer{f.Jira.ExtraServer()}
}

// resumeTarget resolves the session the run should restore, or "" for a cold start.
// --continue keys on acy's own snapshots rather than claude's transcript list: only
// sessions acy actually supervised have one, so it can never land on a bare claude
// session.
func resumeTarget(f Flags, cwd string) (string, error) {
	// root.go re-exposes run's flags without its exclusion group, so enforce it here
	// too rather than silently letting one win.
	if f.Resume != "" && f.Continue {
		return "", errors.New("--resume and --continue are mutually exclusive")
	}
	id := f.Resume
	if f.Continue {
		s, ok, err := state.Latest(cwd)
		if err != nil {
			return "", fmt.Errorf("--continue: %w", err)
		}
		if !ok {
			return "", fmt.Errorf("--continue: no previous acy session in %s", cwd)
		}
		id = s.SessionID
	}
	if id == "" {
		return "", nil
	}
	// An id may have been superseded if claude ever forks on resume; follow it to
	// the run that is actually live.
	resolved, err := state.Resolve(id)
	if err != nil {
		return id, nil //nolint:nilerr // an unreadable snapshot is no reason not to try the id
	}
	return resolved, nil
}

// filterAskReqs wraps a bridge's request stream so an interceptor gets first
// look at every AskUserQuestion. A request the interceptor claims (returns
// true) never reaches the returned channel — the interceptor now owns
// resolving or abandoning it. Everything else, including a declined Ask,
// forwards unchanged. The returned channel closes once in does.
func filterAskReqs(in <-chan *mcp.Pending, intercept func(p *mcp.Pending) bool) <-chan *mcp.Pending {
	out := make(chan *mcp.Pending)
	go func() {
		defer close(out)
		for p := range in {
			if p.Req.Tool == mcp.ToolAsk && intercept(p) {
				continue
			}
			out <- p
		}
	}()
	return out
}

// Supervisor is a fully wired supervisor: the TUI model, and the resources it owns
// for as long as it runs.
type Supervisor struct {
	Model ui.Model
	Close func() // releases the gate socket, the generated settings, and the log

	// Sessions and LoadState are the exact two functions the model was given for
	// its /resume picker. They are handed back so a second front end can build
	// the same picker from the same sources — `acy serve` passes them straight to
	// internal/server — rather than re-deriving "which sessions belong to this
	// project" and getting a different answer than the terminal.
	Sessions  func() ([]session.Info, error)
	LoadState func(id string) (state.Snapshot, bool, error)
}

// NewSupervisor builds the supervisor exactly as `acy run` does — the same gate
// server, the same generated hook settings, the same launcher, the same
// persistence. Exported so the live e2e suite can drive the real thing rather
// than a re-creation of it that would drift the moment this file changed.
func NewSupervisor(ctx context.Context, f Flags) (*Supervisor, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate self: %w", err)
	}
	// The hook is this binary re-invoked as `acy hook`. Under `go test` the running
	// executable is the test binary, which has no hook subcommand — so the e2e suite
	// builds a real acy and points HookBin at it.
	if f.HookBin != "" {
		exe = f.HookBin
	}

	var closers []func()
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	// Anything already opened must be released if a later step fails.
	fail := func(err error) (*Supervisor, error) {
		closeAll()
		return nil, err
	}

	logPath := ""
	if f.LogPath != "" {
		if p, err := alog.Open(f.LogPath); err == nil {
			logPath = p
			closers = append(closers, alog.Close)
			alog.Printf("=== always-click-yes run: model=%q countdown=%s ===", f.Model, f.Countdown)
			if f.ConfigPath != "" {
				alog.Printf("config: loaded %s", f.ConfigPath)
			}
		}
	}

	// Temp workspace for the socket + generated settings.
	tmp, err := os.MkdirTemp("", "acy-")
	if err != nil {
		return fail(err)
	}
	closers = append(closers, func() { _ = os.RemoveAll(tmp) })

	// A provider gateway is established once for the whole supervisor, not once
	// per child. LiteLLM alone inherits the real upstream key; every Claude
	// process receives only a fresh localhost token, so child Bash commands
	// cannot exfiltrate OPENAI_API_KEY/CEREBRAS_API_KEY/etc.
	agent := f.Agent
	if agent == "" {
		agent = "claude"
	}
	switch agent {
	case "claude", "codex":
	default:
		return fail(fmt.Errorf("unsupported --agent %q (want claude or codex)", agent))
	}

	claudeEnv := map[string]string(nil)
	var stripEnv []string
	provider := f.Provider
	if provider == "" {
		provider = "anthropic"
	}
	if agent == "codex" && provider != "anthropic" {
		return fail(fmt.Errorf("--agent codex is incompatible with --provider %q: the provider gateway works by pointing ANTHROPIC_BASE_URL/ANTHROPIC_AUTH_TOKEN at a sidecar that translates Anthropic Messages API requests, and that is meaningless to a codex process — codex speaks its own protocol to its own backend, not claude's. Use --provider anthropic (the default) with --agent codex, or drop --agent codex", provider))
	}
	switch provider {
	case "anthropic":
		// Existing claude.ai / ANTHROPIC_API_KEY behaviour is unchanged.
	case "openai", "cerebras", "fireworks", "openrouter":
		if f.Model == "" {
			if model, ok := gateway.DefaultModel(provider); ok {
				f.Model = model
			}
		}
		if f.ChildModel == "sonnet" {
			f.ChildModel = f.Model
		}
		proxy, startErr := gateway.Start(ctx, tmp, f.GatewayBin, provider, f.Model, childModel(f))
		if startErr != nil {
			return fail(startErr)
		}
		closers = append(closers, proxy.Close)
		claudeEnv = map[string]string{"ANTHROPIC_BASE_URL": proxy.URL, "ANTHROPIC_AUTH_TOKEN": proxy.Token}
		stripEnv = []string{proxy.UpstreamKey}
	case "vllm":
		url := f.GatewayURL
		if url == "" {
			url = "http://127.0.0.1:8000"
		}
		claudeEnv = map[string]string{"ANTHROPIC_BASE_URL": url, "ANTHROPIC_AUTH_TOKEN": "acy-local"}
	default:
		return fail(fmt.Errorf("unsupported --provider %q (want anthropic, openai, cerebras, fireworks, openrouter, or vllm)", provider))
	}

	// The ask bridge: acy's own MCP server gives the supervising session the
	// tools neither `claude -p` nor codex has a built-in equivalent of — a
	// question picker and a way to hand over a plan. Shared by both agents:
	// codex reaches it the same way claude does, `<self> mcp --socket <path>
	// --role <role>`, just registered inline on thread/start instead of via a
	// --mcp-config file (see buildCodexPieces). Its socket is separate from
	// the gate's: a gate answers allow/deny, a question answers with text,
	// and one channel carrying both would make every consumer branch on the
	// tool name to know what it was even looking at.
	bridge, err := mcp.Listen(tmp)
	if err != nil {
		return fail(fmt.Errorf("mcp listen: %w", err))
	}
	closers = append(closers, func() { _ = bridge.Close() })

	// The parent's own role varies with ArchMode — RoleArchitect gains the
	// fleet tools, RoleChild never does — but a child is always RoleChild
	// regardless, on both agents, so it never sees them either.
	parentRole, parentPrompt := roleAndPrompt(f.ArchMode, f.StackMode, f.Jira)

	addCloser := func(fn func()) { closers = append(closers, fn) }
	var pieces agentPieces
	if agent == "codex" {
		// Constructed here, not inside buildCodexPieces, so a test can build
		// its own codex.Bridge and hand it to buildCodexPieces directly —
		// proving GateReqs really is that bridge's own stream, and that a
		// driver newCodexDriver builds actually attaches to it — without
		// starting a real codex process.
		codexBridge := codex.NewBridge()
		addCloser(codexBridge.Close)
		pieces = buildCodexPieces(f, exe, bridge, codexBridge, parentRole, parentPrompt, claudeEnv, stripEnv)
	} else {
		pieces, err = buildClaudePieces(f, tmp, exe, bridge, parentRole, parentPrompt, claudeEnv, stripEnv, addCloser)
		if err != nil {
			return fail(err)
		}
	}

	// One child at a time. The MCP server keeps reading calls while an Ask or
	// Dispatch is blocked, so the orchestrator's own limit is what serializes
	// actual child work. Two children editing one working tree would corrupt each
	// other regardless.
	orch := orchestrator.NewWithLimits(pieces.spawn, 1, orchestrator.Limits{
		DefaultTaskBudgetUSD: f.TaskBudget,
		RunBudgetUSD:         f.RunBudget,
	})
	closers = append(closers, func() { orch.Close() })

	cwd := f.Cwd
	if cwd == "" {
		if cwd, err = os.Getwd(); err != nil {
			return fail(fmt.Errorf("cwd: %w", err))
		}
	}
	resumeID, err := resumeTarget(f, cwd)
	if err != nil {
		return fail(err)
	}
	if resumeID != "" {
		alog.Printf("resume: restoring session %s", resumeID)
		if snap, ok, loadErr := state.Load(resumeID); loadErr != nil {
			alog.Printf("resume: could not seed child budget: %v", loadErr)
		} else if ok {
			// Engineer spend counts against the same run-budget ceiling as
			// local child dispatches — both are money this run has already
			// spent, and a resumed run must not forget either half of it.
			var engineerSpent float64
			for _, e := range snap.Engineers {
				engineerSpent += e.CostUSD
			}
			orch.SeedSpent(snap.ChildCost + engineerSpent)

			// The fleet's own run-budget ceiling only tracks engineer spend,
			// so it is seeded with that half alone. fleet.Manager.Resume —
			// called later, once the ui.Model's own resume flow reaches the
			// snapshot's engineers — repopulates the ledger with these same
			// costs; spentLocked takes the higher of the seed and the
			// ledger sum rather than adding them, so seeding here first
			// never double-counts once that happens.
			if fm, ok := f.Fleet.(*fleet.Manager); ok {
				fm.SeedSpent(engineerSpent)
			}
		}
	}

	// Bound once and shared: the model's picker and any second front end read the
	// same list of resumable sessions, from the same cwd.
	//
	// codex gets sources of its own that honestly report nothing available,
	// rather than claude's ~/.claude/projects transcripts: those record
	// claude sessions in an entirely different on-disk shape
	// (docs/codex-cli-findings.md §7), and listing or replaying them under a
	// codex run would show the wrong history, not merely an empty one. Real
	// codex transcript replay is deferred to a follow-up task — see
	// codexSessionSources.
	sessions := func() ([]session.Info, error) { return session.List(cwd) }
	replay := func(id string) ([]driver.Event, error) { return session.Replay(cwd, id) }
	if agent == "codex" {
		sessions, replay = codexSessionSources()
	}

	// Wiring stays exactly as before when no interceptor is set — no extra
	// goroutine, no channel copy — so a future headless engineer runtime is the
	// only caller that pays for this.
	askReqs := bridge.Requests()
	if f.InterceptAsk != nil {
		askReqs = filterAskReqs(askReqs, f.InterceptAsk)
	}

	model := ui.New(nil, ui.Config{
		Ctx:          ctx,
		Launcher:     pieces.launcher,
		GateReqs:     pieces.gateReqs,
		AskReqs:      askReqs,
		Countdown:    f.Countdown,
		LogPath:      logPath,
		ConfigPath:   f.ConfigPath,
		Agent:        f.Agent,
		StartupNote:  f.StartupNote,
		MaxLines:     f.MaxLines,
		Cwd:          cwd,
		RenderHTML:   f.RenderHTML,
		AltScreen:    f.AltScreen,
		Resume:       resumeID,
		Dispatcher:   orch,
		Fleet:        f.Fleet,
		Tickets:      f.Tickets,
		GitRunner:    f.GitRunner,
		Trunk:        f.Trunk,
		StackMode:    f.StackMode,
		LoadState:    state.Load,
		SaveState:    state.Save,
		Replay:       replay,
		Sessions:     sessions,
		Branch:       func() (string, error) { return gitops.CurrentBranch(ctx, gitops.DefaultRunner, cwd) },
		ParentNoExec: pieces.parentNoExec,
	})

	return &Supervisor{
		Model:     model,
		Close:     closeAll,
		Sessions:  sessions,
		LoadState: state.Load,
	}, nil
}

// agentPieces is what each agent's path builds: the gate request stream
// ui.Config.GateReqs reads from, whether the parent must be denied exec
// (ui.Config.ParentNoExec — claude never sets this; see its own doc comment
// for why codex must), and the launcher/spawn closures wired into the model
// and the orchestrator.
type agentPieces struct {
	gateReqs     <-chan *gate.Pending
	parentNoExec bool
	launcher     ui.Launcher
	spawn        orchestrator.Spawn
}

// buildClaudePieces wires the claude path exactly as NewSupervisor always
// has: the gate server, the generated hook settings, the two --mcp-config
// files (parent and child roles), and the launcher/spawn closures that close
// over them. Kept as its own function, its body unchanged from what used to
// sit inline in NewSupervisor, so the codex path beside it (buildCodexPieces)
// can never accidentally tangle with this one's comments or behavior.
func buildClaudePieces(f Flags, tmp, exe string, bridge *mcp.Bridge, parentRole mcp.Role, parentPrompt string, claudeEnv map[string]string, stripEnv []string, addCloser func(func())) (agentPieces, error) {
	// Start the gate server, then generate settings that point claude's
	// PreToolUse hook at it.
	srv, err := gate.Listen(tmp)
	if err != nil {
		return agentPieces{}, fmt.Errorf("gate listen: %w", err)
	}
	addCloser(func() { _ = srv.Close() })

	settingsPath, err := config.WriteHookSettings(tmp, exe, srv.SocketPath())
	if err != nil {
		return agentPieces{}, fmt.Errorf("write settings: %w", err)
	}

	// Two configs, differing only in --role. A child is launched with the child
	// one, so it never sees Dispatch: without that split it would inherit the
	// parent's config, gain the ability to delegate, and spawn an unbounded tree
	// of unsupervised processes.
	mcpConfigPath, err := config.WriteMCPConfig(tmp, exe, bridge.SocketPath(), parentRole, jiraExtraServers(f)...)
	if err != nil {
		return agentPieces{}, fmt.Errorf("write mcp config: %w", err)
	}
	mcpChildConfigPath, err := config.WriteMCPConfig(tmp, exe, bridge.SocketPath(), mcp.RoleChild)
	if err != nil {
		return agentPieces{}, fmt.Errorf("write child mcp config: %w", err)
	}

	// launcher starts a claude driver appropriate to each phase. Both get the
	// PreToolUse hook and acy's MCP server; they differ in what claude may reach for.
	//
	// PLAN deliberately does NOT use --permission-mode plan. That mode refuses to
	// execute any MCP tool call — "Cannot call mcp__acy__AskUserQuestion while in
	// plan mode", even with --allowedTools — which would leave acy's question picker
	// dead in the one phase where a human is sitting there to answer it. So PLAN runs
	// in `default` mode over a read-only --tools registry instead: Write and Edit are
	// not merely denied there, they do not exist, which is the stronger guarantee.
	// The planning contract plan mode used to inject is carried by the system prompt.
	launcher := func(ctx context.Context, spec ui.LaunchSpec) (ui.Agent, error) {
		model := spec.Model
		if model == "" {
			model = f.Model
		}
		opts := driver.Options{
			Bin:            f.Bin,
			Cwd:            f.Cwd,
			Model:          model,
			UseAPIKey:      f.UseAPIKey,
			Env:            claudeEnv,
			StripEnv:       stripEnv,
			PermissionMode: "default",
			SettingsPath:   settingsPath,
			IncludeHooks:   true,
			MCPConfigPath:  mcpConfigPath,
			ResumeID:       spec.ResumeID, // /resume, or arming, continues a prior session
		}
		// The same registry and the same prompt in both phases, because both
		// phases are now the same process. Arming changes what acy permits, not
		// what claude is: the supervising session can only ever read, and the
		// work is done by children it dispatches.
		opts.Tools = f.PlanTools
		opts.AppendSystemPrompt = parentPrompt
		// Only acy's MCP server: one from the user's own config would put tools
		// straight back into the registry that --tools was chosen to keep out.
		opts.StrictMCP = true
		d := driver.New(opts)
		if err := d.Start(ctx); err != nil {
			return nil, err
		}
		return d, nil
	}

	// spawn launches a child for one delegated task. It sits beside launcher on
	// purpose: both build a driver from the same settingsPath, and that shared
	// line is what gives a child exactly the same PreToolUse gate the parent
	// has. A child is not a trusted process — it is an unwatched one, which is
	// the opposite.
	//
	// What makes it disposable rather than merely separate: its own session id,
	// the full tool registry, a schema it must answer in, and a spend ceiling.
	// When it exits, the entire context it built up goes with it.
	spawn := func(ctx context.Context, t orchestrator.Task) (orchestrator.Child, error) {
		opts := driver.Options{
			Bin:                f.Bin,
			Cwd:                f.Cwd,
			Model:              childModel(f),
			UseAPIKey:          f.UseAPIKey,
			Env:                claudeEnv,
			StripEnv:           stripEnv,
			PermissionMode:     "default",
			SettingsPath:       settingsPath, // the same gate as the parent
			IncludeHooks:       true,
			MCPConfigPath:      mcpChildConfigPath, // Ask, but never Dispatch
			SessionID:          t.SessionID,        // pre-assigned, so gates attribute on arrival
			AppendSystemPrompt: ui.ChildSystemPrompt,
			ExtraArgs: []string{
				// The report is the child's whole return value, so claude
				// validates it rather than acy hoping for a sentinel.
				"--json-schema", orchestrator.ReportSchema,
				// Many short children share one system-prompt prefix; excluding
				// the per-machine sections lets them share its cache entry too.
				"--exclude-dynamic-system-prompt-sections",
			},
		}
		// The task's own ceiling wins over the run's, so the parent can spend
		// more on one hard task without raising the floor for every other.
		budget := t.BudgetUSD
		if budget <= 0 {
			budget = f.TaskBudget
		}
		if budget > 0 {
			opts.ExtraArgs = append(opts.ExtraArgs,
				"--max-budget-usd", strconv.FormatFloat(budget, 'f', 2, 64))
		}
		if f.ChildEffort != "" {
			opts.ExtraArgs = append(opts.ExtraArgs, "--effort", f.ChildEffort)
		}
		d := driver.New(opts)
		if err := d.Start(ctx); err != nil {
			return nil, err
		}
		return d, nil
	}

	return agentPieces{gateReqs: srv.Requests(), parentNoExec: false, launcher: launcher, spawn: spawn}, nil
}

// buildCodexPieces wires the codex path beside buildClaudePieces: no gate
// server and no hook settings file — codex's approval requests arrive
// in-band on the driver's own connection, so codexBridge is the entire gate
// mechanism this path needs (see the codex package's doc comment).
// ui.Config.GateReqs is wired to the Bridge's fan-in stream rather than a
// unix-socket server's, and ParentNoExec is true unconditionally: see
// ui.Config.ParentNoExec's own doc comment for why codex needs it and
// claude never does. codexBridge is built and its Close registered by the
// caller (not here) so a test can hand in its own bridge and observe
// attachment directly — see NewSupervisor's own comment on this call.
func buildCodexPieces(f Flags, exe string, bridge *mcp.Bridge, codexBridge *codex.Bridge, parentRole mcp.Role, parentPrompt string, codexEnv map[string]string, stripEnv []string) agentPieces {
	launcher := func(ctx context.Context, spec ui.LaunchSpec) (ui.Agent, error) {
		opts := codexParentOptions(f, exe, bridge.SocketPath(), parentRole, parentPrompt, codexEnv, stripEnv, spec)
		// Safety assertion — see assertCodexParentSafe's own comment: a
		// future edit that loosens the sandbox or the approval policy above
		// must fail loudly here rather than quietly launching an
		// unsandboxed, ungated supervising session.
		if err := assertCodexParentSafe(opts); err != nil {
			return nil, err
		}
		d := newCodexDriver(opts, codexBridge)
		if err := d.Start(ctx); err != nil {
			return nil, err
		}
		return d, nil
	}

	spawn := func(ctx context.Context, t orchestrator.Task) (orchestrator.Child, error) {
		opts := codexChildOptions(f, exe, bridge.SocketPath(), codexEnv, stripEnv)
		d := newCodexDriver(opts, codexBridge)
		if err := d.Start(ctx); err != nil {
			return nil, err
		}
		return d, nil
	}

	return agentPieces{gateReqs: codexBridge.Requests(), parentNoExec: true, launcher: launcher, spawn: spawn}
}

// codexInertNote lists the explicitly-set, claude-specific flags that codex
// cannot honor, or "" if --agent isn't codex or none of them were actually
// set. changed is the same predicate applyFileConfig uses, so a flag left at
// its default never produces noise — only a flag the user actually typed is
// worth a notice.
func codexInertNote(f Flags, changed func(string) bool) string {
	if f.Agent != "codex" {
		return ""
	}
	var notes []string
	if changed("plan-tools") {
		notes = append(notes, "--plan-tools has no effect under --agent codex: codex has no way to remove a tool from the model's registry at all (docs/codex-cli-findings.md §4). acy compensates by denying the supervising session's non-read-only calls at the gate instead (ParentNoExec).")
	}
	if changed("use-api-key") {
		notes = append(notes, "--use-api-key has no effect under --agent codex: ANTHROPIC_API_KEY has no meaning for a codex process.")
	}
	if changed("task-budget") || changed("run-budget") {
		notes = append(notes, "--task-budget/--run-budget cannot be enforced under --agent codex: codex reports no dollar figure anywhere and has no --max-budget-usd analog (docs/codex-cli-findings.md §9). Spend limiting on codex is token-based, not dollar-based.")
	}
	if len(notes) == 0 {
		return ""
	}
	return strings.Join(notes, "\n")
}

// OverlayFileConfig applies the project's .acy.json to f.
//
// Both `run` and `serve` do this before wiring anything, because the settings it
// carries (log path, countdown, model …) shape the supervisor itself. A file
// that exists but doesn't parse aborts the command — an unattended tool must not
// quietly fall back to defaults its project tried to override.
func OverlayFileConfig(f *Flags, changed func(string) bool) error {
	cwd := f.Cwd
	if cwd == "" {
		var err error
		if cwd, err = os.Getwd(); err != nil {
			return fmt.Errorf("cwd: %w", err)
		}
	}
	if file, found, err := config.LoadFile(cwd); err != nil {
		return err
	} else if found {
		applyFileConfig(f, file, changed)
	}
	if note := codexInertNote(*f, changed); note != "" {
		if f.StartupNote == "" {
			f.StartupNote = note
		} else {
			f.StartupNote += "\n" + note
		}
	}
	return nil
}
