package engineerd

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/hweeks/always-click-yes/internal/engineerwire"
)

// StoredSpec is spec.json: the wire Spec an engineer was launched with, plus
// the two paths RunDetachedTarget needs that are not part of the wire
// protocol itself — the shared clone this engineer's worktree branches from,
// and where that worktree lives. Both travel with the spec because they are
// exactly as much a part of "the whole of what this engineer knows about its
// job" as Spec is; there is no earlier process to ask for them either.
type StoredSpec struct {
	Spec        engineerwire.Spec `json:"spec"`
	ClonePath   string            `json:"clone_path"`
	WorktreeDir string            `json:"worktree_dir"`
}

// WriteSpec saves s to dir/spec.json.
func WriteSpec(dir string, s StoredSpec) error {
	buf, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	return os.WriteFile(filepath.Join(dir, SpecFile), buf, 0o600) //nolint:gosec // 0o600, not executable
}

// ReadSpec loads dir/spec.json.
func ReadSpec(dir string) (StoredSpec, error) {
	var s StoredSpec
	buf, err := os.ReadFile(filepath.Join(dir, SpecFile)) //nolint:gosec // dir is a validated engineer id path
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(buf, &s); err != nil {
		return s, err
	}
	return s, nil
}
