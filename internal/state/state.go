// Package state persists acy's own view of a run — the part claude's transcript
// does not record: which phase the run was in, the plan it was working to, how
// many auto-continue rounds it has spent, and what it has cost. Snapshots are
// keyed by claude's session id, so resuming a session restores acy alongside it.
//
// The conversation itself is deliberately not stored here. claude already keeps
// it under ~/.claude/projects, and internal/session replays it from there; a
// snapshot is a few hundred bytes of state that has nowhere else to live.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the snapshot format this build writes. Load refuses anything
// newer rather than half-applying a format it does not understand.
const SchemaVersion = 1

// EnvDir overrides the snapshot directory. Tests set it; a user could too.
const EnvDir = "ACY_STATE_DIR"

// Snapshot is acy's state for one claude session.
type Snapshot struct {
	Version   int    `json:"version"`
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`   // the project this run belongs to; --continue keys on it
	Phase     string `json:"phase"` // "PLAN" | "AUTO-RUN" | "COMPLETE"
	Model     string `json:"model,omitempty"`

	// PlanBody is the approved plan — the record of what the user armed, shown when
	// the run is resumed.
	PlanBody string `json:"plan_body,omitempty"`

	// Rounds is how many auto-nudge rounds the run has already spent. Restoring
	// it is what keeps maxAutoRounds bounding the whole run instead of handing it a
	// fresh budget on every resume.
	Rounds int `json:"rounds"`

	// CostSettled is what every process in this run has spent so far. A resumed
	// claude process restarts its own total_cost_usd at zero, so the tally survives
	// a restart only because it is banked here.
	CostSettled float64 `json:"cost_settled"`

	// Lineage and SupersededBy track a session id changing under us. claude 2.1.207
	// keeps the id across --resume (verified against real transcripts), so these
	// stay empty in practice — but if a future version forks on resume, Resolve
	// still finds the live run.
	Lineage      []string `json:"lineage,omitempty"`
	SupersededBy string   `json:"superseded_by,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
}

// Dir is where snapshots live: $ACY_STATE_DIR, else <user config dir>/acy/sessions.
// Not ~/.claude (that is claude's tree, not ours) and not the repo (a state file
// turning up in someone's git status is a bug report waiting to happen).
func Dir() (string, error) {
	if d := os.Getenv(EnvDir); d != "" {
		return d, nil
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "acy", "sessions"), nil
}

// Path is the snapshot file for a session id.
func Path(id string) (string, error) {
	if err := validID(id); err != nil {
		return "", err
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".json"), nil
}

// validID rejects ids that would escape the snapshot directory. Session ids reach
// us from claude's init event and from a --resume flag, so they are not ours to
// hand to a path join unchecked.
func validID(id string) error {
	if id == "" {
		return errors.New("state: empty session id")
	}
	if id != filepath.Base(id) || strings.ContainsAny(id, `/\`) || strings.HasPrefix(id, ".") {
		return fmt.Errorf("state: unsafe session id %q", id)
	}
	return nil
}

// Save writes a snapshot atomically: temp file in the same directory, synced, then
// renamed over the target. A crash mid-write leaves the previous snapshot intact
// rather than a truncated one — which matters, because surviving a crash is the
// entire job of this file.
func Save(s Snapshot) error {
	s.Version = SchemaVersion
	s.UpdatedAt = time.Now().UTC()

	path, err := Path(s.SessionID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')

	// Dot-prefixed: the *.json glob in All ignores it, so a crashed Save can never
	// be mistaken for a snapshot.
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+s.SessionID+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // a no-op once the rename has moved it

	if _, err := tmp.Write(buf); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Load reads the snapshot for a session id. A missing snapshot is not an error:
// most claude sessions were never supervised by acy, so callers get ok=false and
// fall back to a transcript-only resume.
func Load(id string) (Snapshot, bool, error) {
	path, err := Path(id)
	if err != nil {
		return Snapshot{}, false, err
	}
	buf, err := os.ReadFile(path) //nolint:gosec // path is derived from a validated id
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, err
	}
	var s Snapshot
	if err := json.Unmarshal(buf, &s); err != nil {
		return Snapshot{}, false, fmt.Errorf("state: parse %s: %w", path, err)
	}
	if s.Version > SchemaVersion {
		return Snapshot{}, false, nil // written by a newer acy; don't guess at its meaning
	}
	return s, true, nil
}

// maxHops bounds Resolve. A cycle in superseded_by should be impossible; hanging
// the TUI on one should be more impossible.
const maxHops = 10

// Resolve follows SupersededBy to the id of the run that is actually live, so
// resuming a stale id still lands on the current session.
func Resolve(id string) (string, error) {
	seen := map[string]bool{}
	for range maxHops {
		if seen[id] {
			return id, nil // a cycle: this id is as good an answer as any
		}
		seen[id] = true

		s, ok, err := Load(id)
		if err != nil || !ok || s.SupersededBy == "" {
			return id, err
		}
		id = s.SupersededBy
	}
	return id, nil
}

// All returns every readable snapshot, newest first. An unreadable file is skipped
// rather than failing the whole listing — one bad snapshot should not cost you the
// resume picker.
func All() ([]Snapshot, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(matches))
	for _, path := range matches {
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		s, ok, err := Load(id)
		if err != nil || !ok {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// Latest is the most recently updated live run for cwd — what `--continue` resumes.
// It keys on acy's own snapshots rather than claude's transcript list, which is what
// stops --continue from landing on a session acy never drove: a bare claude session
// has no snapshot at all.
func Latest(cwd string) (Snapshot, bool, error) {
	all, err := All()
	if err != nil {
		return Snapshot{}, false, err
	}
	want := abs(cwd)
	for _, s := range all {
		if s.SupersededBy != "" {
			continue
		}
		if abs(s.Cwd) == want {
			return s, true, nil
		}
	}
	return Snapshot{}, false, nil
}

func abs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// Prune keeps the newest keep snapshots and removes the rest. Best-effort: failing
// to tidy up is never worth failing a run over.
func Prune(keep int) error {
	if keep <= 0 {
		return nil
	}
	all, err := All()
	if err != nil {
		return err
	}
	if len(all) <= keep {
		return nil
	}
	for _, s := range all[keep:] {
		if path, err := Path(s.SessionID); err == nil {
			_ = os.Remove(path)
		}
	}
	return nil
}
