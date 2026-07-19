package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/gate"
	"github.com/hweeks/always-click-yes/internal/mcp"
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
	Resume    string
	Continue  bool

	Cwd     string // working directory for claude; "" = acy's own
	HookBin string // binary the PreToolUse hook re-invokes; "" = this executable
}

// defaultPlanTools is the built-in registry the plan phase runs with (--tools).
// Everything here is read-only except Bash, which earns its place because plans
// worth trusting come from running the tests and reading the git log — and which is
// consequently the only mutation vector left, so it is the one thing the gate still
// puts a countdown on during PLAN (see ui.enqueue).
//
// Write, Edit, NotebookEdit and Task are absent by design. Their absence, not a
// permission mode, is what makes planning safe now.
var defaultPlanTools = []string{
	"Read", "Grep", "Glob", "Bash", "WebFetch", "WebSearch", "Skill", "ToolSearch",
}

func newRunCmd() *cobra.Command {
	var f Flags
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Launch the supervisor TUI",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSupervisor(cmd.Context(), f)
		},
	}
	cmd.Flags().StringVar(&f.Model, "model", "", "model to use (e.g. sonnet, opus); default = claude's default")
	cmd.Flags().StringVar(&f.Bin, "claude-bin", "claude", "path to the claude binary")
	cmd.Flags().DurationVar(&f.Countdown, "countdown", 30*time.Second, "auto-approve delay per gated tool")
	cmd.Flags().StringVar(&f.LogPath, "log", "acy-debug.log", "debug log file (raw event stream, gate decisions, transitions); empty to disable")
	cmd.Flags().IntVar(&f.MaxLines, "max-lines", 10, "max lines shown per tool call/result/thinking block in the transcript")
	cmd.Flags().StringSliceVar(&f.PlanTools, "plan-tools", defaultPlanTools, "the built-in tools claude may use while planning (--tools). This is the registry, not an allowlist: anything left out cannot be called at all, which is what keeps the plan phase read-only. acy's own mcp__acy__* tools are always available.")
	cmd.Flags().BoolVar(&f.UseAPIKey, "use-api-key", false, "bill ANTHROPIC_API_KEY instead of the claude.ai login; by default the key is stripped from claude's environment, since headless runs would otherwise use it silently")
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

	mcpConfigPath, err := config.WriteMCPConfig(tmp, exe, bridge.SocketPath())
	if err != nil {
		return fail(fmt.Errorf("write mcp config: %w", err))
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
		switch spec.Phase {
		case ui.PhaseAutoRun:
			opts.AppendSystemPrompt = ui.AutoRunSystemPrompt
		default: // PhasePlan
			opts.Tools = f.PlanTools
			opts.AppendSystemPrompt = ui.PlanSystemPrompt
			// Only acy's MCP server while planning: a server from the user's own
			// config would put tools straight back into the registry that --tools was
			// chosen to keep out.
			opts.StrictMCP = true
		}
		d := driver.New(opts)
		if err := d.Start(ctx); err != nil {
			return nil, err
		}
		return d, nil
	}

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
		Ctx:       ctx,
		Launcher:  launcher,
		GateReqs:  srv.Requests(),
		AskReqs:   bridge.Requests(),
		Countdown: f.Countdown,
		LogPath:   logPath,
		MaxLines:  f.MaxLines,
		Cwd:       cwd,
		Resume:    resumeID,
		LoadState: state.Load,
		SaveState: state.Save,
		Replay:    func(id string) ([]driver.Event, error) { return session.Replay(cwd, id) },
		Sessions:  func() ([]session.Info, error) { return session.List(cwd) },
	})

	return &Supervisor{Model: model, Close: closeAll}, nil
}

func runSupervisor(ctx context.Context, f Flags) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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
