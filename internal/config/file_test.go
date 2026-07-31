package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadFileMissingIsNotAnError(t *testing.T) {
	f, found, err := LoadFile(t.TempDir())
	if err != nil {
		t.Fatalf("missing file: unexpected error %v", err)
	}
	if found {
		t.Fatalf("missing file reported found, f=%+v", f)
	}
}

func TestLoadFileParsesEveryField(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, `{
		"model": "opus",
		"claudeBin": "/opt/claude",
		"countdown": "20s",
		"log": "",
		"maxLines": 25,
		"planTools": ["Read", "Grep"],
		"useApiKey": true
	}`)

	f, found, err := LoadFile(dir)
	if err != nil || !found {
		t.Fatalf("LoadFile: found=%v err=%v", found, err)
	}
	if f.Model != "opus" || f.ClaudeBin != "/opt/claude" {
		t.Errorf("strings: %+v", f)
	}
	if f.Countdown == nil || time.Duration(*f.Countdown) != 20*time.Second {
		t.Errorf("countdown: %v", f.Countdown)
	}
	if f.Log == nil || *f.Log != "" {
		t.Errorf("log should be set-and-empty (logging disabled), got %v", f.Log)
	}
	if f.MaxLines == nil || *f.MaxLines != 25 {
		t.Errorf("maxLines: %v", f.MaxLines)
	}
	if len(f.PlanTools) != 2 || f.PlanTools[0] != "Read" {
		t.Errorf("planTools: %v", f.PlanTools)
	}
	if f.UseAPIKey == nil || !*f.UseAPIKey {
		t.Errorf("useApiKey: %v", f.UseAPIKey)
	}
	if f.Path != path {
		t.Errorf("Path = %q, want %q", f.Path, path)
	}
}

func TestLoadFileUnsetFieldsStayNil(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{"model": "opus"}`)

	f, _, err := LoadFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if f.Countdown != nil || f.Log != nil || f.MaxLines != nil || f.UseAPIKey != nil || f.PlanTools != nil {
		t.Errorf("unset fields must stay nil so the overlay can tell absence from zero: %+v", f)
	}
}

// Every parse failure must name the file — the error is all the user sees.
func TestLoadFileRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"malformed json":  `{"model": `,
		"unknown key":     `{"modle": "opus"}`,
		"bare number dur": `{"countdown": 30}`,
		"bad duration":    `{"countdown": "fast"}`,
		"trailing junk":   `{"model": "opus"} {"more": true}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, content)
			_, _, err := LoadFile(dir)
			if err == nil {
				t.Fatalf("%s: want an error, got none", name)
			}
			if !strings.Contains(err.Error(), FileName) {
				t.Errorf("error should name the file: %v", err)
			}
		})
	}
}

func TestDurationRoundTrips(t *testing.T) {
	d := Duration(90 * time.Second)
	b, err := d.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"1m30s"` {
		t.Errorf("marshal: %s", b)
	}
	var back Duration
	if err := back.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	if back != d {
		t.Errorf("round trip: %v != %v", back, d)
	}
}

// The child knobs are what let a run be priced: children do the bulk of the
// tokens, so being able to point them at a cheaper model or cap them is the
// main lever a user has after the architecture itself.
func TestLoadFileReadsChildKnobs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{"childModel":"sonnet","childEffort":"low","taskBudget":2.5,"runBudget":7.5}`)

	got, found, err := LoadFile(dir)
	if err != nil || !found {
		t.Fatalf("LoadFile: found=%v err=%v", found, err)
	}
	if got.ChildModel != "sonnet" {
		t.Errorf("ChildModel = %q, want sonnet", got.ChildModel)
	}
	if got.ChildEffort != "low" {
		t.Errorf("ChildEffort = %q, want low", got.ChildEffort)
	}
	if got.TaskBudget == nil || *got.TaskBudget != 2.5 {
		t.Errorf("TaskBudget = %v, want 2.5", got.TaskBudget)
	}
	if got.RunBudget == nil || *got.RunBudget != 7.5 {
		t.Errorf("RunBudget = %v, want 7.5", got.RunBudget)
	}
}

// A zero budget is meaningful — it means "no ceiling" — so the field is a
// pointer and an explicit 0 must survive as one.
func TestTaskBudgetZeroIsExplicit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{"taskBudget":0}`)

	got, _, err := LoadFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskBudget == nil {
		t.Fatal("an explicit 0 should decode as a set value, not absent")
	}
	if *got.TaskBudget != 0 {
		t.Errorf("TaskBudget = %v, want 0", *got.TaskBudget)
	}
}
