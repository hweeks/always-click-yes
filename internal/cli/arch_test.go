package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hweeks/always-click-yes/internal/supervisor"
)

// writeACYJSON marshals body as .acy.json in dir and chdirs the test there.
func writeACYJSON(t *testing.T, dir string, body map[string]any) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".acy.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
}

// TestArchRequiresFleetSection proves acy arch refuses to start rather than
// silently running an architect with no engineers to delegate to — the same
// clear-error shape TestFleetDoctorRequiresFleetSection proves for
// `acy fleet doctor`.
func TestArchRequiresFleetSection(t *testing.T) {
	t.Run("no .acy.json", func(t *testing.T) {
		t.Chdir(t.TempDir())
		f := supervisor.Flags{}
		_, err := resolveArchFlags(&f, func(string) bool { return false })
		if err == nil {
			t.Fatal("want an error when the project has no .acy.json")
		}
		if !strings.Contains(err.Error(), "fleet") {
			t.Errorf("error should mention the missing fleet section: %v", err)
		}
	})

	t.Run(".acy.json with no fleet key", func(t *testing.T) {
		writeACYJSON(t, t.TempDir(), map[string]any{"model": "opus"})
		f := supervisor.Flags{}
		_, err := resolveArchFlags(&f, func(string) bool { return false })
		if err == nil {
			t.Fatal("want an error when .acy.json has no fleet section")
		}
		if !strings.Contains(err.Error(), "fleet") {
			t.Errorf("error should mention the missing fleet section: %v", err)
		}
	})
}

// TestArchModelDefault proves --model only falls back to defaultArchModel
// when neither the flag nor .acy.json set one — an explicit choice, from
// either source, must always win.
func TestArchModelDefault(t *testing.T) {
	fleetSection := map[string]any{"hosts": []map[string]any{{"name": "local"}}}

	tests := []struct {
		name        string
		flagModel   string
		flagChanged bool
		fileModel   string
		want        string
	}{
		{name: "flag set", flagModel: "sonnet", flagChanged: true, want: "sonnet"},
		{name: "file set", fileModel: "haiku", want: "haiku"},
		{name: "neither set", want: defaultArchModel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := map[string]any{"fleet": fleetSection}
			if tt.fileModel != "" {
				body["model"] = tt.fileModel
			}
			writeACYJSON(t, t.TempDir(), body)

			f := supervisor.Flags{Model: tt.flagModel}
			changed := func(name string) bool { return name == "model" && tt.flagChanged }
			if _, err := resolveArchFlags(&f, changed); err != nil {
				t.Fatal(err)
			}
			if f.Model != tt.want {
				t.Errorf("model = %q, want %q", f.Model, tt.want)
			}
		})
	}
}

func TestArchCmdSharesRunFlags(t *testing.T) {
	archFlags := newArchCmd().Flags()
	runFlags := newRunCmd().Flags()
	for _, name := range []string{"model", "countdown", "child-model", "task-budget", "resume", "continue"} {
		if archFlags.Lookup(name) == nil {
			t.Errorf("acy arch is missing --%s", name)
		}
		if runFlags.Lookup(name) == nil {
			t.Errorf("acy run is missing --%s (test setup)", name)
		}
	}
}
