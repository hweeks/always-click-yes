package ui

import "github.com/hweeks/always-click-yes/internal/driver"

// Agent is the seam that lets a second CLI backend drive the same model.
// *driver.Driver — the Anthropic `claude` CLI wrapper — is the only
// implementation that exists today, but nothing here names it: the model
// talks to whatever is behind this interface and stays ignorant of which
// process, or which vendor's CLI, is actually running.
//
// It is a superset of orchestrator.Child (the same four methods minus
// Interrupt), because a dispatched child is never interjected into — only
// the parent session the human is watching can be redirected mid-turn.
type Agent interface {
	Events() <-chan driver.Event
	Send(string) error
	Interrupt() error
	Stop()
}
