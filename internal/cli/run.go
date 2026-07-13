package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/driver"
	"github.com/hweeks/always-click-yes/internal/gate"
	"github.com/hweeks/always-click-yes/internal/judge"
	"github.com/hweeks/always-click-yes/internal/session"
	"github.com/hweeks/always-click-yes/internal/term"
	"github.com/hweeks/always-click-yes/internal/ui"
)

type runFlags struct {
	model      string
	judgeModel string
	bin        string
	countdown  time.Duration
	logPath    string
	maxLines   int
	planTools  []string
	useAPIKey  bool
}

func newRunCmd() *cobra.Command {
	var f runFlags
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Launch the supervisor TUI",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSupervisor(cmd.Context(), f)
		},
	}
	cmd.Flags().StringVar(&f.model, "model", "", "model to use (e.g. sonnet, opus); default = claude's default")
	cmd.Flags().StringVar(&f.judgeModel, "judge-model", "", "model for the independent completion judge; default = --model")
	cmd.Flags().StringVar(&f.bin, "claude-bin", "claude", "path to the claude binary")
	cmd.Flags().DurationVar(&f.countdown, "countdown", 30*time.Second, "auto-approve delay per gated tool")
	cmd.Flags().StringVar(&f.logPath, "log", "acy-debug.log", "debug log file (raw event stream, gate decisions, transitions); empty to disable")
	cmd.Flags().IntVar(&f.maxLines, "max-lines", 10, "max lines shown per tool call/result/thinking block in the transcript")
	cmd.Flags().StringSliceVar(&f.planTools, "plan-tools", []string{"Monitor", "AskUserQuestion"}, "tools pre-approved during plan mode via --allowedTools (exact tool names, e.g. Monitor or mcp__<server>__Monitor)")
	cmd.Flags().BoolVar(&f.useAPIKey, "use-api-key", false, "bill ANTHROPIC_API_KEY instead of the claude.ai login; by default the key is stripped from claude's environment, since headless runs would otherwise use it silently")
	return cmd
}

func runSupervisor(ctx context.Context, f runFlags) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}

	logPath := ""
	if f.logPath != "" {
		if p, err := alog.Open(f.logPath); err == nil {
			logPath = p
			defer alog.Close()
			alog.Printf("=== always-click-yes run: model=%q countdown=%s ===", f.model, f.countdown)
		}
	}

	// Temp workspace for the socket + generated settings.
	tmp, err := os.MkdirTemp("", "acy-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	// Start the gate server, then generate settings that point claude's
	// PreToolUse hook at it.
	srv, err := gate.Listen(tmp)
	if err != nil {
		return fmt.Errorf("gate listen: %w", err)
	}
	defer func() { _ = srv.Close() }()

	settingsPath, err := config.WriteHookSettings(tmp, exe, srv.SocketPath())
	if err != nil {
		return fmt.Errorf("write settings: %w", err)
	}

	// launcher starts a claude driver appropriate to each phase: PLAN is
	// interactive and ungated; AUTO-RUN resumes the same session with the
	// PreToolUse hook wired in and the default (gated) permission mode.
	launcher := func(ctx context.Context, spec ui.LaunchSpec) (*driver.Driver, error) {
		model := spec.Model
		if model == "" {
			model = f.model
		}
		opts := driver.Options{Bin: f.bin, Model: model, UseAPIKey: f.useAPIKey}
		switch spec.Phase {
		case ui.PhaseAutoRun:
			opts.PermissionMode = "default"
			opts.SettingsPath = settingsPath
			opts.IncludeHooks = true
			opts.ResumeID = spec.ResumeID
		default: // PhasePlan
			opts.PermissionMode = "plan"
			opts.ResumeID = spec.ResumeID // set by /resume to continue a prior session
			// Plan mode refuses non-read-only tools and has no gate wired in;
			// pre-approve the configured tools so they still run.
			//
			// AskUserQuestion stays in the default set for the day it works, but it
			// is currently inert: `claude -p` does not put AskUserQuestion in its
			// tool registry at all (see AGENTS.md), and --allowedTools cannot
			// allowlist a tool that isn't there. Harmless either way.
			opts.AllowedTools = f.planTools
		}
		d := driver.New(opts)
		if err := d.Start(ctx); err != nil {
			return nil, err
		}
		return d, nil
	}

	// judgeFn runs an independent, one-shot claude session (fresh session, no
	// hooks, tools disabled) to decide whether the plan is complete.
	judgeModel := f.judgeModel
	if judgeModel == "" {
		judgeModel = f.model
	}
	judgeFn := func(ctx context.Context, plan, lastMsg string) (judge.Result, error) {
		return judge.Assess(ctx, judge.Options{Bin: f.bin, Model: judgeModel, UseAPIKey: f.useAPIKey}, plan, lastMsg)
	}

	model := ui.New(nil, ui.Config{
		Ctx:       ctx,
		Launcher:  launcher,
		Judge:     judgeFn,
		GateReqs:  srv.Requests(),
		Countdown: f.countdown,
		LogPath:   logPath,
		MaxLines:  f.maxLines,
		Sessions: func() ([]session.Info, error) {
			cwd, err := os.Getwd()
			if err != nil {
				return nil, err
			}
			return session.List(cwd)
		},
	})

	// Alt-screen by default; ACY_NO_ALTSCREEN=1 keeps output inline (useful for
	// capturing frames when smoke-testing under a pseudo-terminal).
	progOpts := []tea.ProgramOption{tea.WithContext(ctx)}
	if os.Getenv("ACY_NO_ALTSCREEN") == "" {
		progOpts = append(progOpts, tea.WithAltScreen())
	}

	// Bubble Tea's init() has already queried the terminal by now; throw away any
	// reply still sitting in the input queue, before it gets read as keystrokes.
	term.DrainInput(os.Stdin)

	_, err = tea.NewProgram(model, progOpts...).Run()
	return err
}
