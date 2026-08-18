package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileName is the project-level config file acy reads from the directory it
// supervises. It carries the run settings a project wants every launch to use,
// so the VS Code extension (and anyone tired of retyping flags) can run a bare
// `acy run`.
const FileName = ".acy.json"

// File is the parsed .acy.json. Every field mirrors a run flag; a nil pointer
// means "not set", so the overlay can tell an explicit zero from an absence.
// Resume/continue are deliberately not here — they describe one invocation,
// not the project.
type File struct {
	Model      string    `json:"model,omitempty"`
	ClaudeBin  string    `json:"claudeBin,omitempty"`
	Countdown  *Duration `json:"countdown,omitempty"`
	Log        *string   `json:"log,omitempty"` // pointer: "" is a meaningful value (disable logging)
	MaxLines   *int      `json:"maxLines,omitempty"`
	PlanTools  []string  `json:"planTools,omitempty"`
	UseAPIKey  *bool     `json:"useApiKey,omitempty"`
	Provider   string    `json:"provider,omitempty"`
	GatewayBin string    `json:"gatewayBin,omitempty"`
	GatewayURL string    `json:"gatewayUrl,omitempty"`

	// Agent selects which coding-agent CLI acy supervises ("claude" or
	// "codex") — a different axis from Provider, which selects which model
	// backend claude itself talks to. CodexBin is codex's equivalent of
	// ClaudeBin.
	Agent    string `json:"agent,omitempty"`
	CodexBin string `json:"codexBin,omitempty"`

	// Child knobs: a dispatched task is a separate process doing the actual
	// work, so it can be priced and paced separately from the session you talk
	// to. childModel is usually the most effective one — children do the bulk
	// of the tokens.
	ChildModel  string   `json:"childModel,omitempty"`
	ChildEffort string   `json:"childEffort,omitempty"`
	TaskBudget  *float64 `json:"taskBudget,omitempty"`
	RunBudget   *float64 `json:"runBudget,omitempty"`

	// Fleet is arch mode's optional config: engineer defaults and the hosts
	// they can run on. Nil means the project has no "fleet" key at all.
	Fleet *FleetConfig `json:"fleet,omitempty"`

	// Jira is the optional config for a project's Jira MCP server. Nil means
	// the project has no "jira" key at all.
	Jira *JiraConfig `json:"jira,omitempty"`

	// Path is where the file was read from, for the "loaded config" line.
	Path string `json:"-"`
}

// Duration parses from a JSON string in Go duration syntax ("30s", "2m").
type Duration time.Duration

// UnmarshalJSON implements strict duration parsing: a string like "20s". A bare
// number is rejected on purpose — nobody remembers whether it would have meant
// seconds or nanoseconds, and the error says what to write instead.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("want a duration string like \"30s\", got %s", strings.TrimSpace(string(b)))
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %q: %w", s, err)
	}
	*d = Duration(dur)
	return nil
}

// MarshalJSON keeps a File round-trippable (the extension writes one).
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// LoadFile reads <dir>/.acy.json. A missing file is not an error — it returns
// found=false and a zero File. A file that exists but cannot be parsed IS an
// error, loudly: silently ignoring a typo'd config and running with defaults is
// exactly the kind of surprise an unattended tool cannot afford.
func LoadFile(dir string) (File, bool, error) {
	path := filepath.Join(dir, FileName)
	b, err := os.ReadFile(path) //nolint:gosec // the project's own config, by design
	if errors.Is(err, os.ErrNotExist) {
		return File{}, false, nil
	}
	if err != nil {
		return File{}, false, fmt.Errorf("%s: %w", path, err)
	}

	var f File
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields() // a typo'd key must fail, not silently fall back
	if err := dec.Decode(&f); err != nil {
		return File{}, false, fmt.Errorf("%s: %w", path, err)
	}
	// Trailing garbage after the object is as much a mistake as an unknown key.
	if dec.More() {
		return File{}, false, fmt.Errorf("%s: unexpected content after the JSON object", path)
	}
	if f.TaskBudget != nil && *f.TaskBudget < 0 {
		return File{}, false, fmt.Errorf("%s: taskBudget must be zero or greater", path)
	}
	if f.RunBudget != nil && *f.RunBudget < 0 {
		return File{}, false, fmt.Errorf("%s: runBudget must be zero or greater", path)
	}
	if f.Fleet != nil {
		if err := f.Fleet.resolve(dir, path); err != nil {
			return File{}, false, err
		}
	}
	if f.Jira != nil {
		if err := f.Jira.resolve(path); err != nil {
			return File{}, false, err
		}
	}
	f.Path = path
	return f, true, nil
}
