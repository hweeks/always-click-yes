package cli

import (
	"reflect"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/config"
)

// fullFile is a .acy.json that sets every field, for exercising both sides of
// the precedence rule.
func fullFile() config.File {
	return config.File{
		Model:     "opus",
		ClaudeBin: "/opt/claude",
		Countdown: new(config.Duration(20 * time.Second)),
		Log:       new(""),
		MaxLines:  new(25),
		PlanTools: []string{"Read"},
		UseAPIKey: new(true),
		Path:      "/proj/.acy.json",
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
		TaskBudget: defaultTaskBudgetUSD,
		RunBudget:  defaultRunBudgetUSD,
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
