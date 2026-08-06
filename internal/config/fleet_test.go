package config

import (
	"reflect"
	"strings"
	"testing"
)

// A project with no "fleet" key must parse exactly as it always did — Fleet
// stays nil and every other field is unaffected.
func TestLoadFileWithoutFleetIsUnaffected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{
		"model": "opus",
		"claudeBin": "/opt/claude",
		"countdown": "20s",
		"maxLines": 25,
		"planTools": ["Read", "Grep"],
		"useApiKey": true
	}`)

	f, found, err := LoadFile(dir)
	if err != nil || !found {
		t.Fatalf("LoadFile: found=%v err=%v", found, err)
	}
	if f.Fleet != nil {
		t.Fatalf("Fleet = %+v, want nil", f.Fleet)
	}
	if f.Model != "opus" || f.ClaudeBin != "/opt/claude" || f.MaxLines == nil || *f.MaxLines != 25 {
		t.Errorf("unrelated fields disturbed by an absent fleet key: %+v", f)
	}
}

// An empty fleet object still gets every default applied, and a local host
// with no repoPath defaults to the project directory.
func TestLoadFileFleetDefaults(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{"fleet": {"hosts": [{"name": "local"}]}}`)

	f, _, err := LoadFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	fl := f.Fleet
	if fl == nil {
		t.Fatal("Fleet is nil")
	}
	if fl.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want main", fl.BaseBranch)
	}
	if fl.PRCap == nil || *fl.PRCap != 4 {
		t.Errorf("PRCap = %v, want 4", fl.PRCap)
	}
	if fl.DeadmanHours == nil || *fl.DeadmanHours != 24 {
		t.Errorf("DeadmanHours = %v, want 24", fl.DeadmanHours)
	}
	if fl.TicketCommit != "direct" {
		t.Errorf("TicketCommit = %q, want direct", fl.TicketCommit)
	}
	if len(fl.Hosts) != 1 {
		t.Fatalf("Hosts = %+v, want 1 entry", fl.Hosts)
	}
	h := fl.Hosts[0]
	if h.RepoPath != dir {
		t.Errorf("local host RepoPath = %q, want project dir %q", h.RepoPath, dir)
	}
	if h.MaxEngineers == nil || *h.MaxEngineers != 1 {
		t.Errorf("MaxEngineers = %v, want 1", h.MaxEngineers)
	}
	if h.ACYBin != "acy" {
		t.Errorf("ACYBin = %q, want acy", h.ACYBin)
	}
}

// Every field, explicitly set, round-trips with no defaulting applied.
func TestLoadFileFleetExplicitValues(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{
		"fleet": {
			"baseBranch": "develop",
			"prCap": 9,
			"engineerModel": "opus",
			"engineerChildModel": "sonnet",
			"engineerEffort": "high",
			"engineerBudgetUSD": 12.5,
			"runBudgetUSD": 100,
			"deadmanHours": 6,
			"ticketCommit": "none",
			"hosts": [
				{"name": "local", "maxEngineers": 3},
				{"name": "box1", "ssh": "user@box1", "repoPath": "/srv/repo", "maxEngineers": 2, "acyBin": "/opt/acy", "path": ["/opt/homebrew/bin", "/home/box1/.local/bin"]}
			]
		}
	}`)

	f, _, err := LoadFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	fl := f.Fleet
	if fl.BaseBranch != "develop" {
		t.Errorf("BaseBranch = %q", fl.BaseBranch)
	}
	if *fl.PRCap != 9 {
		t.Errorf("PRCap = %v", *fl.PRCap)
	}
	if fl.EngineerModel != "opus" || fl.EngineerChildModel != "sonnet" || fl.EngineerEffort != "high" {
		t.Errorf("engineer knobs: %+v", fl)
	}
	if fl.EngineerBudgetUSD == nil || *fl.EngineerBudgetUSD != 12.5 {
		t.Errorf("EngineerBudgetUSD = %v", fl.EngineerBudgetUSD)
	}
	if fl.RunBudgetUSD == nil || *fl.RunBudgetUSD != 100 {
		t.Errorf("RunBudgetUSD = %v", fl.RunBudgetUSD)
	}
	if *fl.DeadmanHours != 6 {
		t.Errorf("DeadmanHours = %v", *fl.DeadmanHours)
	}
	if fl.TicketCommit != "none" {
		t.Errorf("TicketCommit = %q", fl.TicketCommit)
	}
	if len(fl.Hosts) != 2 {
		t.Fatalf("Hosts = %+v", fl.Hosts)
	}
	local, box1 := fl.Hosts[0], fl.Hosts[1]
	if local.RepoPath != dir {
		t.Errorf("local host RepoPath defaulted wrong: %q", local.RepoPath)
	}
	if *local.MaxEngineers != 3 {
		t.Errorf("local MaxEngineers = %v", *local.MaxEngineers)
	}
	if box1.SSH != "user@box1" || box1.RepoPath != "/srv/repo" || *box1.MaxEngineers != 2 || box1.ACYBin != "/opt/acy" {
		t.Errorf("box1 host: %+v", box1)
	}
	wantPath := []string{"/opt/homebrew/bin", "/home/box1/.local/bin"}
	if !reflect.DeepEqual(box1.Path, wantPath) {
		t.Errorf("box1.Path = %v, want %v", box1.Path, wantPath)
	}
	if len(local.Path) != 0 {
		t.Errorf("local.Path = %v, want empty (not set)", local.Path)
	}
}

func TestLoadFileFleetRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"duplicate host names":     `{"fleet": {"hosts": [{"name": "a"}, {"name": "a"}]}}`,
		"maxEngineers zero":        `{"fleet": {"hosts": [{"name": "a", "maxEngineers": 0}]}}`,
		"maxEngineers negative":    `{"fleet": {"hosts": [{"name": "a", "maxEngineers": -1}]}}`,
		"ssh without repoPath":     `{"fleet": {"hosts": [{"name": "a", "ssh": "user@host"}]}}`,
		"host missing a name":      `{"fleet": {"hosts": [{"ssh": "user@host", "repoPath": "/x"}]}}`,
		"bad ticketCommit":         `{"fleet": {"ticketCommit": "sideways"}}`,
		"negative prCap":           `{"fleet": {"prCap": -1}}`,
		"negative deadman":         `{"fleet": {"deadmanHours": -1}}`,
		"negative engineer budget": `{"fleet": {"engineerBudgetUSD": -1}}`,
		"negative run budget":      `{"fleet": {"runBudgetUSD": -1}}`,
		"unknown key in fleet":     `{"fleet": {"bogusKey": "main"}}`,
		"unknown key in host":      `{"fleet": {"hosts": [{"name": "a", "bogusKey": 2}]}}`,
		"relative path entry":      `{"fleet": {"hosts": [{"name": "a", "path": ["bin"]}]}}`,
		"tilde path entry":         `{"fleet": {"hosts": [{"name": "a", "path": ["~/bin"]}]}}`,
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

// prCap: 0 is a valid, if unusual, explicit choice (no PR is ever opened
// concurrently) — only negative values are rejected.
func TestLoadFileFleetPRCapZeroIsAllowed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{"fleet": {"prCap": 0}}`)

	f, _, err := LoadFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if f.Fleet.PRCap == nil || *f.Fleet.PRCap != 0 {
		t.Errorf("PRCap = %v, want explicit 0", f.Fleet.PRCap)
	}
}

// A relative or "~"-prefixed path entry is rejected with an error naming
// both the offending host and why: it is spliced into a remote shell
// command, where "~" never expands and a relative entry resolves against
// whatever directory ssh happens to land in.
func TestLoadFileFleetPathRejectsNonAbsoluteEntries(t *testing.T) {
	cases := map[string]string{
		"relative": "bin",
		"tilde":    "~/bin",
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, `{"fleet": {"hosts": [{"name": "box2", "path": ["`+entry+`"]}]}}`)
			_, _, err := LoadFile(dir)
			if err == nil {
				t.Fatalf("want an error for path entry %q, got none", entry)
			}
			if !strings.Contains(err.Error(), "box2") {
				t.Errorf("error should name the host: %v", err)
			}
			if !strings.Contains(err.Error(), "absolute") {
				t.Errorf("error should say why: %v", err)
			}
		})
	}
}

// Absolute path entries are accepted and preserved verbatim, in order.
func TestLoadFileFleetPathAcceptsAbsoluteEntries(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{"fleet": {"hosts": [{"name": "local", "path": ["/opt/homebrew/bin", "/home/you/.local/bin"]}]}}`)

	f, _, err := LoadFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/opt/homebrew/bin", "/home/you/.local/bin"}
	if !reflect.DeepEqual(f.Fleet.Hosts[0].Path, want) {
		t.Errorf("Path = %v, want %v", f.Fleet.Hosts[0].Path, want)
	}
}
