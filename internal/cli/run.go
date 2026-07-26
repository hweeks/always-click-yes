package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/gate"
	"github.com/hweeks/always-click-yes/internal/mcp"
	"github.com/hweeks/always-click-yes/internal/orchestrator"
	"github.com/hweeks/always-click-yes/internal/session"
	"github.com/hweeks/always-click-yes/internal/state"
	"github.com/hweeks/always-click-yes/internal/term"
	"github.com/hweeks/always-click-yes/internal/ui"
)

// maxSnapshots is how many resumable runs acy keeps state for. Old enough to cover
// "the thing I was doing last week", small enough that the directory never becomes
// a problem of its own.
const maxSnapshots = 200

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

	// Child knobs. A dispatched child is a different kind of session from the
	// supervising one — it does the work, unwatched, and then throws its whole
	// context away — so it is worth being able to price and pace it separately.
	ChildModel  string
	ChildEffort string
	TaskBudget  float64
	Resume      string
	Continue    bool

	Cwd     string // working directory for claude; "" = acy's own
	HookBin string // binary the PreToolUse hook re-invokes; "" = this executable

	// ConfigPath is the .acy.json the settings were overlaid from, "" if none.
	// Not a flag: runSupervisor stamps it so the UI can say where its settings
	// came from.
	ConfigPath string
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
	if c.ChildModel != "" && !changed("child-model") {
		f.ChildModel = c.ChildModel
	}
	if c.ChildEffort != "" && !changed("child-effort") {
		f.ChildEffort = c.ChildEffort
	}
	if c.TaskBudget != nil && !changed("task-budget") {
		f.TaskBudget = *c.TaskBudget
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

func newRunCmd() *cobra.Command {
	var f Flags
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Launch the supervisor TUI",
		RunE: func(cmd *cobra.Command, args []string) error {
			// root re-exposes this command's flags via AddFlagSet — the same
			// pflag instances — so Changed is accurate no matter which command
			// actually parsed the line.
			return runSupervisor(cmd.Context(), f, cmd.Flags().Changed)
		},
	}
	cmd.Flags().StringVar(&f.Model, "model", "", "model to use (e.g. sonnet, opus); default = claude's default")
	cmd.Flags().StringVar(&f.Bin, "claude-bin", "claude", "path to the claude binary")
	cmd.Flags().DurationVar(&f.Countdown, "countdown", 30*time.Second, "auto-approve delay per gated tool")
	cmd.Flags().StringVar(&f.LogPath, "log", "acy-debug.log", "debug log file (raw event stream, gate decisions, transitions); empty to disable")
	cmd.Flags().IntVar(&f.MaxLines, "max-lines", 10, "max lines shown per tool call/result/thinking block in the transcript")
	cmd.Flags().StringSliceVar(&f.PlanTools, "plan-tools", DefaultParentTools, "the built-in tool registry for the supervising session, in both phases (--tools). This is the registry, not an allowlist: anything left out cannot be called at all, which is what keeps the session you talk to unable to change your code. Dispatched children always get the full set; acy's own mcp__acy__* tools are always available.")
	cmd.Flags().BoolVar(&f.UseAPIKey, "use-api-key", false, "bill ANTHROPIC_API_KEY instead of the claude.ai login; by default the key is stripped from claude's environment, since headless runs would otherwise use it silently")
	cmd.Flags().StringVar(&f.ChildModel, "child-model", "", "model for dispatched tasks; empty = same as --model. A cheaper model here is often the single biggest saving, since children do the bulk of the work")
	cmd.Flags().StringVar(&f.ChildEffort, "child-effort", "", "reasoning effort for dispatched tasks (low, medium, high, xhigh, max); empty = claude's default")
	cmd.Flags().Float64Var(&f.TaskBudget, "task-budget", 0, "spend ceiling in USD for one dispatched task (0 = none). A runaway child stops instead of running until you notice")
	cmd.Flags().StringVar(&f.Resume, "resume", "", "resume a prior acy session by id, restoring its transcript, phase and cost")
	cmd.Flags().BoolVarP(&f.Continue, "continue", "c", false, "resume the most recent acy session in this directory")
	cmd.MarkFlagsMutuallyExclusive("resume", "continue")
	return cmd
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

// Supervisor is a fully wired supervisor: the TUI model, and the resources it owns
// for as long as it runs.
type Supervisor struct {
	Model ui.Model
	Close func() // releases the gate socket, the generated settings, and the log
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

	// Start the gate server, then generate settings that point claude's
	// PreToolUse hook at it.
	srv, err := gate.Listen(tmp)
	if err != nil {
		return fail(fmt.Errorf("gate listen: %w", err))
	}
	closers = append(closers, func() { _ = srv.Close() })

	settingsPath, err := config.WriteHookSettings(tmp, exe, srv.SocketPath())
	if err != nil {
		return fail(fmt.Errorf("write settings: %w", err))
	}

	// The ask bridge: acy's own MCP server gives claude the two tools `claude -p`
	// otherwise has no equivalent of — a question picker and a way to hand over a
	// plan. Its socket is separate from the gate's: a gate answers allow/deny, a
	// question answers with text, and one channel carrying both would make every
	// consumer branch on the tool name to know what it was even looking at.
	bridge, err := mcp.Listen(tmp)
	if err != nil {
		return fail(fmt.Errorf("mcp listen: %w", err))
	}
	closers = append(closers, func() { _ = bridge.Close() })

	// Two configs, differing only in --role. A child is launched with the child
	// one, so it never sees Dispatch: without that split it would inherit the
	// parent's config, gain the ability to delegate, and spawn an unbounded tree
	// of unsupervised processes.
	mcpConfigPath, err := config.WriteMCPConfig(tmp, exe, bridge.SocketPath(), mcp.RoleParent)
	if err != nil {
		return fail(fmt.Errorf("write mcp config: %w", err))
	}
	mcpChildConfigPath, err := config.WriteMCPConfig(tmp, exe, bridge.SocketPath(), mcp.RoleChild)
	if err != nil {
		return fail(fmt.Errorf("write child mcp config: %w", err))
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
	launcher := func(ctx context.Context, spec ui.LaunchSpec) (*driver.Driver, error) {
		model := spec.Model
		if model == "" {
			model = f.Model
		}
		opts := driver.Options{
			Bin:            f.Bin,
			Cwd:            f.Cwd,
			Model:          model,
			UseAPIKey:      f.UseAPIKey,
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
		opts.AppendSystemPrompt = ui.ParentSystemPrompt
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
			Model:              f.Model,
			UseAPIKey:          f.UseAPIKey,
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

	// One child at a time. See orchestrator.New: acy's MCP server handles
	// tools/call serially, so a second Dispatch is not even read off stdin until
	// the first returns — and two children editing one working tree would
	// corrupt each other regardless.
	orch := orchestrator.New(spawn, 1)
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
	}

	model := ui.New(nil, ui.Config{
		Ctx:        ctx,
		Launcher:   launcher,
		GateReqs:   srv.Requests(),
		AskReqs:    bridge.Requests(),
		Countdown:  f.Countdown,
		LogPath:    logPath,
		ConfigPath: f.ConfigPath,
		MaxLines:   f.MaxLines,
		Cwd:        cwd,
		Resume:     resumeID,
		Dispatcher: orch,
		LoadState:  state.Load,
		SaveState:  state.Save,
		Replay:     func(id string) ([]driver.Event, error) { return session.Replay(cwd, id) },
		Sessions:   func() ([]session.Info, error) { return session.List(cwd) },
	})

	return &Supervisor{Model: model, Close: closeAll}, nil
}

func runSupervisor(ctx context.Context, f Flags, changed func(string) bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Overlay the project's .acy.json before wiring anything: the settings it
	// carries (log path, countdown, model …) shape the supervisor itself. A file
	// that exists but doesn't parse aborts the run — an unattended tool must not
	// quietly fall back to defaults its project tried to override.
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
		applyFileConfig(&f, file, changed)
	}

	sup, err := NewSupervisor(ctx, f)
	if err != nil {
		return err
	}
	defer sup.Close()

	// Old snapshots are worth keeping (you may want to resume a run from last week)
	// but not forever. Tidying is best-effort and never blocks the run.
	defer func() { _ = state.Prune(maxSnapshots) }()

	// Alt-screen by default; ACY_NO_ALTSCREEN=1 keeps output inline (useful for
	// capturing frames when smoke-testing under a pseudo-terminal).
	progOpts := []tea.ProgramOption{tea.WithContext(ctx)}
	if os.Getenv("ACY_NO_ALTSCREEN") == "" {
		progOpts = append(progOpts, tea.WithAltScreen())
	}

	// Bubble Tea's init() has already queried the terminal by now; throw away any
	// reply still sitting in the input queue, before it gets read as keystrokes.
	term.DrainInput(os.Stdin)

	_, err = tea.NewProgram(sup.Model, progOpts...).Run()
	return err
}
