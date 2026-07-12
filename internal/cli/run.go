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
	"github.com/hweeks/always-click-yes/internal/ui"
)

type runFlags struct {
	model     string
	bin       string
	countdown time.Duration
	logPath   string
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
	cmd.Flags().StringVar(&f.bin, "claude-bin", "claude", "path to the claude binary")
	cmd.Flags().DurationVar(&f.countdown, "countdown", 30*time.Second, "auto-approve delay per gated tool")
	cmd.Flags().StringVar(&f.logPath, "log", "acy-debug.log", "debug log file (raw event stream, gate decisions, transitions); empty to disable")
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
	launcher := func(ctx context.Context, phase ui.Phase, resumeID string) (*driver.Driver, error) {
		opts := driver.Options{Bin: f.bin, Model: f.model}
		switch phase {
		case ui.PhaseAutoRun:
			opts.PermissionMode = "default"
			opts.SettingsPath = settingsPath
			opts.IncludeHooks = true
			opts.ResumeID = resumeID
		default: // PhasePlan
			opts.PermissionMode = "plan"
		}
		d := driver.New(opts)
		if err := d.Start(ctx); err != nil {
			return nil, err
		}
		return d, nil
	}

	model := ui.New(nil, ui.Config{
		Ctx:       ctx,
		Launcher:  launcher,
		GateReqs:  srv.Requests(),
		Countdown: f.countdown,
		LogPath:   logPath,
	})

	// Alt-screen by default; ACY_NO_ALTSCREEN=1 keeps output inline (useful for
	// capturing frames when smoke-testing under a pseudo-terminal).
	var progOpts []tea.ProgramOption
	if os.Getenv("ACY_NO_ALTSCREEN") == "" {
		progOpts = append(progOpts, tea.WithAltScreen())
	}

	_, err = tea.NewProgram(model, progOpts...).Run()
	return err
}
