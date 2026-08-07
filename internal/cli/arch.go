package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/fleet"
	"github.com/hweeks/always-click-yes/internal/gitops"
	"github.com/hweeks/always-click-yes/internal/state"
	"github.com/hweeks/always-click-yes/internal/supervisor"
	"github.com/hweeks/always-click-yes/internal/term"
	"github.com/hweeks/always-click-yes/internal/tickets"
)

// defaultArchModel is what --model becomes when arch mode is started with
// neither an explicit flag nor a .acy.json model: an architect spends most of
// its budget launching and reacting to engineers rather than writing prose,
// so the stronger default is worth it in a way it might not be for `run`.
const defaultArchModel = "opus"

// stackProbeTimeout bounds resolveStackMode's gh-stack availability probe. A
// `gh stack --version` check has no business taking longer than this.
const stackProbeTimeout = 5 * time.Second

func newArchCmd() *cobra.Command {
	var f supervisor.Flags
	cmd := &cobra.Command{
		Use:   "arch",
		Short: "Launch the architect TUI: plan, then delegate tickets to a fleet of engineers",
		Long: "arch runs the same supervisor `acy run` does, with two differences: the\n" +
			"parent session is the architect (it gains the fleet tools LaunchEngineer,\n" +
			"Await, AnswerEngineer, FleetStatus) and every armed ticket becomes a full\n" +
			"engineer instance on a configured fleet host rather than a local child.\n" +
			"\n" +
			"It requires a \"fleet\" section in .acy.json — see `acy fleet doctor` to\n" +
			"check the hosts it names before trusting a run to them.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArch(cmd.Context(), f, cmd.Flags().Changed)
		},
	}
	addRunFlags(cmd, &f)
	return cmd
}

// resolveArchFlags applies the same overlay and budget check runSupervisor
// does, then arch mode's own two rules: a fleet section in .acy.json is
// required, not optional, and --model gets a stronger default than the empty
// string once neither the flag nor the file set one. Split out from runArch
// so a test can exercise both rules without spinning up a fleet.Manager or a
// tea.Program.
func resolveArchFlags(f *supervisor.Flags, changed func(string) bool) (*config.FleetConfig, error) {
	if err := supervisor.OverlayFileConfig(f, changed); err != nil {
		return nil, err
	}
	if f.TaskBudget < 0 || f.RunBudget < 0 {
		return nil, errors.New("--task-budget and --run-budget must be zero or greater")
	}

	cwd := f.Cwd
	if cwd == "" {
		var err error
		if cwd, err = os.Getwd(); err != nil {
			return nil, err
		}
	}
	file, found, err := config.LoadFile(cwd)
	if err != nil {
		return nil, err
	}
	if !found || file.Fleet == nil {
		return nil, fmt.Errorf("acy arch: %s has no \"fleet\" section — run `acy fleet doctor` once one exists", config.FileName)
	}

	if f.Model == "" {
		f.Model = defaultArchModel
	}
	return file.Fleet, nil
}

// resolveStackMode decides the effective fleet.stackMode for this run,
// probing gh-stack's availability whenever the config asks for anything but
// "off". The probe is bounded by timeout so a wedged `gh` at startup cannot
// wedge `acy arch` itself — a timeout is just another probe failure, not a
// distinct case.
//
// "ask" and "chain" deliberately diverge once the probe fails. "ask" means
// the architect may *offer* stacking to a human during planning; an
// architect must never ask a human to choose an option it cannot then
// perform, so an unavailable probe silently downgrades the effective mode to
// "off" and returns an explanatory note instead of an error — the run itself
// must not be blocked over this. "chain" means an operator explicitly
// decided stacking is mandatory for this run; silently substituting "off"
// there would hide that decision from them, so this returns a hard,
// actionable error instead of a note.
func resolveStackMode(ctx context.Context, cfg *config.FleetConfig, run gitops.Runner, dir string, timeout time.Duration) (mode, note string, err error) {
	if cfg.StackMode == "off" {
		return "off", "", nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	probeErr := gitops.StackAvailable(probeCtx, run, dir)
	if probeErr == nil {
		return cfg.StackMode, "", nil
	}

	reason := "the gh-stack extension isn't usable — install it with `gh extension install github/gh-stack`"
	if errors.Is(probeErr, gitops.ErrStackNotEnabled) {
		reason = "stacked pull requests are a public preview feature and must be enabled for this repository"
	}

	if cfg.StackMode == "chain" {
		return "", "", fmt.Errorf("acy arch: fleet.stackMode is \"chain\" but %s: %w", reason, probeErr)
	}

	return "off", fmt.Sprintf("fleet.stackMode is %q, but %s, so this run starts with stacking off.", cfg.StackMode, reason), nil
}

// runArch is runSupervisor's arch-mode sibling: the same overlay, the same
// budget check, the same tea.Program path, plus the fleet manager arch mode
// needs and requires.
func runArch(ctx context.Context, f supervisor.Flags, changed func(string) bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	fleetCfg, err := resolveArchFlags(&f, changed)
	if err != nil {
		return err
	}

	cwd := f.Cwd
	if cwd == "" {
		if cwd, err = os.Getwd(); err != nil {
			return err
		}
	}

	mode, note, err := resolveStackMode(ctx, fleetCfg, gitops.DefaultRunner, cwd, stackProbeTimeout)
	if err != nil {
		return err
	}
	fleetCfg.StackMode = mode
	f.StartupNote = note

	prCap := 0
	if fleetCfg.PRCap != nil {
		prCap = *fleetCfg.PRCap
	}
	watcher := fleet.NewPRWatcher(cwd, gitops.DefaultRunner, 0, nil)
	go watcher.Run(ctx)

	opts := []fleet.Option{fleet.WithPRWatcher(watcher, prCap)}
	if fleetCfg.StackMode != "off" {
		keeper := fleet.NewStackKeeper(cwd, gitops.DefaultRunner, fleetCfg.BaseBranch)
		opts = append(opts, fleet.WithStackKeeper(keeper))
	}
	manager := fleet.NewManager(*fleetCfg, fleet.ForHost, opts...)
	f.ArchMode = true
	f.Fleet = manager
	f.Tickets = tickets.New(cwd, fleetCfg.TicketCommit, gitops.DefaultRunner)

	// Alt-screen by default; ACY_NO_ALTSCREEN=1 keeps output inline, exactly as
	// `acy run` decides it.
	f.AltScreen = os.Getenv("ACY_NO_ALTSCREEN") == ""

	sup, err := supervisor.NewSupervisor(ctx, f)
	if err != nil {
		manager.Close()
		return err
	}
	defer sup.Close()
	// The fleet outlives NewSupervisor's own closers — Flags.Fleet only hands
	// the manager to the model, it does not hand over ownership — so arch is
	// the one place that must close it, cancelling every engineer still running.
	defer manager.Close()

	defer func() { _ = state.Prune(supervisor.MaxSnapshots) }()

	term.DrainInput(os.Stdin)

	_, err = tea.NewProgram(sup.Model, tea.WithContext(ctx)).Run()
	return err
}
