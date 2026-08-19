package supervisor

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/config"
)

// fullFile is a .acy.json that sets every field, for exercising both sides of
// the precedence rule.
func fullFile() config.File {
	return config.File{
		Model:      "opus",
		ClaudeBin:  "/opt/claude",
		Countdown:  new(config.Duration(20 * time.Second)),
		Log:        new(""),
		MaxLines:   new(25),
		PlanTools:  []string{"Read"},
		UseAPIKey:  new(true),
		Provider:   "openai",
		GatewayBin: "/opt/litellm",
		GatewayURL: "http://127.0.0.1:8000",
		Path:       "/proj/.acy.json",
	}
}

// defaultFlags mirrors what cobra hands runSupervisor when nothing was typed.
func defaultFlags() Flags {
	return Flags{
		Bin:        "claude",
		Countdown:  30 * time.Second,
		LogPath:    "acy-debug.log",
		MaxLines:   10,
		PlanTools:  DefaultParentTools,
		ChildModel: "sonnet",
		TaskBudget: DefaultTaskBudgetUSD,
		RunBudget:  DefaultRunBudgetUSD,
	}
}

func TestApplyFileConfigOverridesDefaults(t *testing.T) {
	f := defaultFlags()
	applyFileConfig(&f, fullFile(), func(string) bool { return false })

	if f.Model != "opus" || f.Bin != "/opt/claude" || f.Countdown != 20*time.Second {
		t.Errorf("file values not applied: %+v", f)
	}
	if f.LogPath != "" {
		t.Errorf("log: an explicit empty string in the file must disable logging, got %q", f.LogPath)
	}
	if f.MaxLines != 25 || !f.UseAPIKey {
		t.Errorf("file values not applied: %+v", f)
	}
	if f.Provider != "openai" || f.GatewayBin != "/opt/litellm" || f.GatewayURL != "http://127.0.0.1:8000" {
		t.Errorf("gateway config not applied: %+v", f)
	}
	if !reflect.DeepEqual(f.PlanTools, []string{"Read"}) {
		t.Errorf("planTools: %v", f.PlanTools)
	}
	if f.ConfigPath != "/proj/.acy.json" {
		t.Errorf("ConfigPath: %q", f.ConfigPath)
	}
}

func TestApplyFileConfigNeverBeatsAnExplicitFlag(t *testing.T) {
	f := defaultFlags()
	f.Model = "haiku"
	f.Countdown = 5 * time.Second

	changed := map[string]bool{"model": true, "countdown": true}
	applyFileConfig(&f, fullFile(), func(name string) bool { return changed[name] })

	if f.Model != "haiku" || f.Countdown != 5*time.Second {
		t.Errorf("explicit flags lost to the file: %+v", f)
	}
	// Fields whose flags were NOT given still move.
	if f.MaxLines != 25 || f.Bin != "/opt/claude" {
		t.Errorf("unflagged fields should still take file values: %+v", f)
	}
}

func TestApplyFileConfigLeavesUnsetFieldsAlone(t *testing.T) {
	f := defaultFlags()
	applyFileConfig(&f, config.File{Path: "/proj/.acy.json"}, func(string) bool { return false })

	want := defaultFlags()
	want.ConfigPath = "/proj/.acy.json"
	if !reflect.DeepEqual(f, want) {
		t.Errorf("an empty file must change nothing but ConfigPath:\n got %+v\nwant %+v", f, want)
	}
}

func TestChildModelAndBudgetsOverlayAndRespectExplicitFlags(t *testing.T) {
	f := defaultFlags()
	applyFileConfig(&f, config.File{ChildModel: "sonnet", TaskBudget: new(1.5), RunBudget: new(6.0)}, func(string) bool { return false })
	if f.ChildModel != "sonnet" || f.TaskBudget != 1.5 || f.RunBudget != 6 {
		t.Errorf("overlay = %+v", f)
	}
	if got := childModel(f); got != "sonnet" {
		t.Errorf("childModel = %q, want sonnet", got)
	}
}

// agent/codexBin follow the same precedence rule as every other overlaid
// field: the file applies when the flag was not given, an explicit flag wins
// when it was.
func TestAgentAndCodexBinOverlayRespectsPrecedence(t *testing.T) {
	f := defaultFlags()
	applyFileConfig(&f, config.File{Agent: "codex", CodexBin: "/opt/codex"}, func(string) bool { return false })
	if f.Agent != "codex" || f.CodexBin != "/opt/codex" {
		t.Errorf("file values not applied: %+v", f)
	}

	f2 := defaultFlags()
	f2.Agent = "claude"
	changed := map[string]bool{"agent": true}
	applyFileConfig(&f2, config.File{Agent: "codex", CodexBin: "/opt/codex"}, func(name string) bool { return changed[name] })
	if f2.Agent != "claude" {
		t.Errorf("explicit --agent lost to the file: %+v", f2)
	}
	// codexBin was not itself flagged, so it should still take the file's value.
	if f2.CodexBin != "/opt/codex" {
		t.Errorf("unflagged codexBin should still take the file value: %+v", f2)
	}
}

// codexInertNote only fires for --agent codex, and only names flags the user
// actually typed — a defaulted flag must never produce noise.
func TestCodexInertNote(t *testing.T) {
	if note := codexInertNote(Flags{Agent: "claude"}, func(string) bool { return true }); note != "" {
		t.Errorf("claude agent should never produce a codex-inert note, got %q", note)
	}
	if note := codexInertNote(Flags{Agent: "codex"}, func(string) bool { return false }); note != "" {
		t.Errorf("no explicitly-set inert flags should mean no note, got %q", note)
	}

	cases := []struct {
		name    string
		changed string
		want    string
	}{
		{"plan-tools", "plan-tools", "--plan-tools"},
		{"use-api-key", "use-api-key", "--use-api-key"},
		{"task-budget", "task-budget", "--task-budget"},
		{"run-budget", "run-budget", "--run-budget"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			note := codexInertNote(Flags{Agent: "codex"}, func(n string) bool { return n == tt.changed })
			if !strings.Contains(note, tt.want) {
				t.Errorf("note = %q, want it to mention %q", note, tt.want)
			}
		})
	}
}

// OverlayFileConfig is where the note actually gets attached to Flags, so
// this exercises the full path: it must append to (never replace) whatever
// StartupNote a caller — like `acy arch`'s stacking downgrade — already set.
func TestOverlayFileConfigPreservesExistingStartupNote(t *testing.T) {
	dir := t.TempDir()
	changed := func(name string) bool { return name == "plan-tools" }

	f := defaultFlags()
	f.Agent = "codex"
	f.Cwd = dir
	f.StartupNote = "an existing note from another caller"
	if err := OverlayFileConfig(&f, changed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.StartupNote, "an existing note from another caller") {
		t.Errorf("existing StartupNote was clobbered: %q", f.StartupNote)
	}
	if !strings.Contains(f.StartupNote, "--plan-tools") {
		t.Errorf("StartupNote should still mention --plan-tools: %q", f.StartupNote)
	}
}

func TestOverlayFileConfigNoNoteWhenNothingChanged(t *testing.T) {
	dir := t.TempDir()

	f := defaultFlags()
	f.Agent = "codex"
	f.Cwd = dir
	if err := OverlayFileConfig(&f, func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if f.StartupNote != "" {
		t.Errorf("no explicitly-set inert flags should mean no note, got %q", f.StartupNote)
	}
}
