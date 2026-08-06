package cli

import (
	"context"
	"errors"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/hweeks/always-click-yes/internal/state"
	"github.com/hweeks/always-click-yes/internal/supervisor"
	"github.com/hweeks/always-click-yes/internal/term"
)

func newRunCmd() *cobra.Command {
	var f supervisor.Flags
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
	addRunFlags(cmd, &f)
	return cmd
}

// addRunFlags registers the settings that describe a *run* — everything that
// shapes the supervisor itself — on cmd, bound to f.
//
// It exists because there are two commands that start a supervisor now: `run`
// drives it with a terminal, `serve` drives it over HTTP, and they must be the
// same run underneath. Two flag lists would drift the moment one gained a knob,
// and the drift would be silent: `acy serve --child-effort high` would parse
// nowhere and simply do nothing.
//
// It binds to a *Flags the caller owns, and registers on cmd's own FlagSet, so
// cobra's Changed keeps answering per command. applyFileConfig keys on exactly
// that — the .acy.json overlay only moves a field the command line did not set —
// so sharing the registration must not mean sharing the pflag instances.
func addRunFlags(cmd *cobra.Command, f *supervisor.Flags) {
	cmd.Flags().StringVar(&f.Model, "model", "", "model to use (e.g. sonnet, opus); default = claude's default")
	cmd.Flags().StringVar(&f.Bin, "claude-bin", "claude", "path to the claude binary")
	cmd.Flags().DurationVar(&f.Countdown, "countdown", 30*time.Second, "auto-approve delay per gated tool")
	cmd.Flags().StringVar(&f.LogPath, "log", "acy-debug.log", "debug log file (raw event stream, gate decisions, transitions); empty to disable")
	cmd.Flags().IntVar(&f.MaxLines, "max-lines", 10, "max lines shown per tool call/result/thinking block in the transcript")
	cmd.Flags().StringSliceVar(&f.PlanTools, "plan-tools", supervisor.DefaultParentTools, "the built-in tool registry for the supervising session, in both phases (--tools). This is the registry, not an allowlist: anything left out cannot be called at all, which is what keeps the session you talk to unable to change your code. Dispatched children always get the full set; acy's own mcp__acy__* tools are always available.")
	cmd.Flags().BoolVar(&f.UseAPIKey, "use-api-key", false, "bill ANTHROPIC_API_KEY instead of the claude.ai login; by default the key is stripped from claude's environment, since headless runs would otherwise use it silently")
	cmd.Flags().StringVar(&f.Provider, "provider", "anthropic", "model provider: anthropic, openai, cerebras, fireworks, openrouter, or vllm")
	cmd.Flags().StringVar(&f.GatewayBin, "gateway-bin", "litellm", "LiteLLM binary for hosted non-Anthropic providers")
	cmd.Flags().StringVar(&f.GatewayURL, "gateway-url", "", "Anthropic Messages endpoint for --provider vllm (default http://127.0.0.1:8000)")
	cmd.Flags().StringVar(&f.ChildModel, "child-model", "sonnet", "model for dispatched tasks (default sonnet); a cheaper child is often the single biggest saving, since children do the bulk of the work")
	cmd.Flags().StringVar(&f.ChildEffort, "child-effort", "", "reasoning effort for dispatched tasks (low, medium, high, xhigh, max); empty = claude's default")
	cmd.Flags().Float64Var(&f.TaskBudget, "task-budget", supervisor.DefaultTaskBudgetUSD, "spend ceiling in USD for one dispatched task (0 = unlimited; default $10)")
	cmd.Flags().Float64Var(&f.RunBudget, "run-budget", supervisor.DefaultRunBudgetUSD, "spend ceiling in USD across dispatched tasks (0 = unlimited; default $50)")
	cmd.Flags().StringVar(&f.Resume, "resume", "", "resume a prior acy session by id, restoring its transcript, phase and cost")
	cmd.Flags().BoolVarP(&f.Continue, "continue", "c", false, "resume the most recent acy session in this directory")
	cmd.MarkFlagsMutuallyExclusive("resume", "continue")
}

func runSupervisor(ctx context.Context, f supervisor.Flags, changed func(string) bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := supervisor.OverlayFileConfig(&f, changed); err != nil {
		return err
	}
	if f.TaskBudget < 0 || f.RunBudget < 0 {
		return errors.New("--task-budget and --run-budget must be zero or greater")
	}

	// Alt-screen by default; ACY_NO_ALTSCREEN=1 keeps output inline (useful for
	// capturing frames when smoke-testing under a pseudo-terminal).
	f.AltScreen = os.Getenv("ACY_NO_ALTSCREEN") == ""

	sup, err := supervisor.NewSupervisor(ctx, f)
	if err != nil {
		return err
	}
	defer sup.Close()

	// Old snapshots are worth keeping (you may want to resume a run from last week)
	// but not forever. Tidying is best-effort and never blocks the run.
	defer func() { _ = state.Prune(supervisor.MaxSnapshots) }()

	// v2 needs no keyboard-enhancement option: the renderer always asks the
	// terminal for Kitty key disambiguation (keyboardEnhancementsFlags starts at
	// flag 1 unconditionally), which is the part that makes shift+enter and
	// alt+enter arrive as themselves rather than as a bare CR. tea.View's
	// KeyboardEnhancements field only requests the *extra* features — key repeat
	// and release events, alternate keycodes — and acy wants none of them.
	//
	// Bubble Tea's init() has already queried the terminal by now; throw away any
	// reply still sitting in the input queue, before it gets read as keystrokes.
	// v2 dropped that init-time query, but a terminal that answered a *previous*
	// process is still a terminal with an unread reply in the queue.
	term.DrainInput(os.Stdin)

	_, err = tea.NewProgram(sup.Model, tea.WithContext(ctx)).Run()
	return err
}
