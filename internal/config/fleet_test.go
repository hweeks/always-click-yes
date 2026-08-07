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
	if h.Shell != "" {
		t.Errorf("Shell = %q, want empty — a host with no shell configured leaves derivation to rcWrap", h.Shell)
	}
	wantCommands := []string{"go build ./...", "go test -race ./...", "gofmt -l .", "golangci-lint run ./..."}
	if !reflect.DeepEqual(fl.VerifyCommands, wantCommands) {
		t.Errorf("VerifyCommands = %v, want %v", fl.VerifyCommands, wantCommands)
	}
	if fl.VerifyTimeoutSeconds == nil || *fl.VerifyTimeoutSeconds != 900 {
		t.Errorf("VerifyTimeoutSeconds = %v, want 900", fl.VerifyTimeoutSeconds)
	}
}

// An explicit "verifyCommands": [] is preserved as an empty, non-nil slice —
// distinct from the absent case, which defaults to the four commands above.
// This is how verification gets disabled for a project.
func TestLoadFileFleetVerifyCommandsExplicitEmptyDisablesVerification(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{"fleet": {"verifyCommands": []}}`)

	f, _, err := LoadFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Fleet.VerifyCommands) != 0 {
		t.Errorf("VerifyCommands = %v, want empty", f.Fleet.VerifyCommands)
	}
}

// A whitespace-only entry in verifyCommands is rejected, naming its index.
func TestLoadFileFleetVerifyCommandsRejectsWhitespaceEntry(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{"fleet": {"verifyCommands": ["go build ./...", "   "]}}`)

	_, _, err := LoadFile(dir)
	if err == nil {
		t.Fatal("want an error, got none")
	}
	if !strings.Contains(err.Error(), "verifyCommands[1]") {
		t.Errorf("error should name the offending index: %v", err)
	}
}

// verifyTimeoutSeconds of zero or negative is rejected; absent defaults to
// 900 (covered by TestLoadFileFleetDefaults above).
func TestLoadFileFleetVerifyTimeoutSecondsRejectsNonPositive(t *testing.T) {
	cases := map[string]string{
		"zero":     `{"fleet": {"verifyTimeoutSeconds": 0}}`,
		"negative": `{"fleet": {"verifyTimeoutSeconds": -1}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, content)
			_, _, err := LoadFile(dir)
			if err == nil {
				t.Fatalf("%s: want an error, got none", name)
			}
			if !strings.Contains(err.Error(), "verifyTimeoutSeconds") {
				t.Errorf("%s: error should name the field: %v", name, err)
			}
		})
	}
}

// A full, realistic .acy.json fixture including verifyCommands and
// verifyTimeoutSeconds alongside the rest of the fleet config parses
// cleanly under LoadFile's strict "unknown key" parsing.
func TestLoadFileFleetVerifyFieldsCoexistWithRestOfFleetConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{
		"fleet": {
			"baseBranch": "main",
			"prCap": 4,
			"engineerModel": "sonnet",
			"engineerChildModel": "sonnet",
			"engineerEffort": "medium",
			"engineerBudgetUSD": 15,
			"runBudgetUSD": 200,
			"deadmanHours": 24,
			"ticketCommit": "direct",
			"verifyCommands": ["go build ./...", "go test -race ./...", "gofmt -l .", "golangci-lint run ./..."],
			"verifyTimeoutSeconds": 900,
			"hosts": [
				{ "name": "local" },
				{
					"name": "box2",
					"ssh": "you@box2.example.com",
					"repoPath": "/home/you/proj",
					"maxEngineers": 2,
					"acyBin": "acy",
					"path": ["/opt/homebrew/bin", "/home/you/.local/bin"],
					"rc": "~/.zshrc"
				}
			]
		}
	}`)

	f, found, err := LoadFile(dir)
	if err != nil || !found {
		t.Fatalf("LoadFile: found=%v err=%v", found, err)
	}
	fl := f.Fleet
	wantCommands := []string{"go build ./...", "go test -race ./...", "gofmt -l .", "golangci-lint run ./..."}
	if !reflect.DeepEqual(fl.VerifyCommands, wantCommands) {
		t.Errorf("VerifyCommands = %v, want %v", fl.VerifyCommands, wantCommands)
	}
	if fl.VerifyTimeoutSeconds == nil || *fl.VerifyTimeoutSeconds != 900 {
		t.Errorf("VerifyTimeoutSeconds = %v, want 900", fl.VerifyTimeoutSeconds)
	}
	if fl.BaseBranch != "main" || fl.TicketCommit != "direct" || len(fl.Hosts) != 2 {
		t.Errorf("rest of fleet config disturbed by new keys: %+v", fl)
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
				{"name": "box1", "ssh": "user@box1", "repoPath": "/srv/repo", "maxEngineers": 2, "acyBin": "/opt/acy", "path": ["/opt/homebrew/bin", "/home/box1/.local/bin"], "rc": "~/.bashrc", "shell": "bash"}
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
	if box1.Rc != "~/.bashrc" {
		t.Errorf("box1.Rc = %q, want ~/.bashrc", box1.Rc)
	}
	if box1.Shell != "bash" {
		t.Errorf("box1.Shell = %q, want bash", box1.Shell)
	}
	if local.Rc != "" {
		t.Errorf("local.Rc = %q, want empty (not set)", local.Rc)
	}
	if local.Shell != "" {
		t.Errorf("local.Shell = %q, want empty (not set)", local.Shell)
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
		"duplicate host names":         `{"fleet": {"hosts": [{"name": "a"}, {"name": "a"}]}}`,
		"maxEngineers zero":            `{"fleet": {"hosts": [{"name": "a", "maxEngineers": 0}]}}`,
		"maxEngineers negative":        `{"fleet": {"hosts": [{"name": "a", "maxEngineers": -1}]}}`,
		"ssh without repoPath":         `{"fleet": {"hosts": [{"name": "a", "ssh": "user@host"}]}}`,
		"host missing a name":          `{"fleet": {"hosts": [{"ssh": "user@host", "repoPath": "/x"}]}}`,
		"bad ticketCommit":             `{"fleet": {"ticketCommit": "sideways"}}`,
		"negative prCap":               `{"fleet": {"prCap": -1}}`,
		"negative deadman":             `{"fleet": {"deadmanHours": -1}}`,
		"negative engineer budget":     `{"fleet": {"engineerBudgetUSD": -1}}`,
		"negative run budget":          `{"fleet": {"runBudgetUSD": -1}}`,
		"unknown key in fleet":         `{"fleet": {"bogusKey": "main"}}`,
		"unknown key in host":          `{"fleet": {"hosts": [{"name": "a", "bogusKey": 2}]}}`,
		"relative path entry":          `{"fleet": {"hosts": [{"name": "a", "path": ["bin"]}]}}`,
		"tilde path entry":             `{"fleet": {"hosts": [{"name": "a", "path": ["~/bin"]}]}}`,
		"rc missing a leading ~/ or /": `{"fleet": {"hosts": [{"name": "a", "rc": ".zshrc"}]}}`,
		"rc as a bare word":            `{"fleet": {"hosts": [{"name": "a", "rc": "zshrc"}]}}`,
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

// Unlike a fleet `path` entry, a "~"-prefixed rc is accepted: it is only
// ever handed to the remote shell as the argument of a `source` call, which
// is the one place a leading "~" is exactly what's wanted.
func TestLoadFileFleetRcAcceptsTildeAndAbsolutePaths(t *testing.T) {
	cases := map[string]string{
		"tilde":    "~/.zshrc",
		"absolute": "/etc/profile",
	}
	for name, rc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, `{"fleet": {"hosts": [{"name": "box1", "rc": "`+rc+`"}]}}`)
			f, _, err := LoadFile(dir)
			if err != nil {
				t.Fatal(err)
			}
			if f.Fleet.Hosts[0].Rc != rc {
				t.Errorf("Rc = %q, want %q", f.Fleet.Hosts[0].Rc, rc)
			}
		})
	}
}
