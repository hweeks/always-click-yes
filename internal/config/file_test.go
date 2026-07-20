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
