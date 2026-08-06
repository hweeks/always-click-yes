//go:build !unix

package cli

import (
	"errors"
	"os/exec"
)

// Sessions are a unix concept; there is no equivalent detach story
// elsewhere, so this is a clear refusal rather than a silently-attached
// child. See engineer_detach_unix.go for what unix gets instead.
func detachChild(cmd *exec.Cmd) error {
	return errors.New("acy engineer start: detached engineers are only supported on unix")
}
