package e2e

import "testing"

// The one test in this package that costs nothing and runs in CI.
//
// Everything else here is ACY_LIVE-only, which means a chord that silently
// stopped matching would never fail a build — it would just get typed into the
// composer and the gate would auto-approve, exactly as the veto test did after
// the bindings moved off bare letters. handleGateKey (internal/ui/update.go)
// switches on msg.String(), and Ctrl+G arming does too, so these strings are
// the whole contract between the harness and the model.
func TestKeyChordsStringifyAsTheGateExpects(t *testing.T) {
	for want, got := range map[string]string{
		"ctrl+g": keyCtrlG.String(),
		"ctrl+x": keyCtrlX.String(),
	} {
		if got != want {
			t.Errorf("key stringifies as %q, want %q — the model would never match it", got, want)
		}
	}
}
