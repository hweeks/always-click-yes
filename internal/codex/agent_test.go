package codex_test

import (
	"github.com/hweeks/always-click-yes/internal/codex"
	"github.com/hweeks/always-click-yes/internal/ui"
)

// codex.Driver must satisfy ui.Agent so it can drive acy's Bubble Tea model in
// place of *driver.Driver (the claude backend). If this stops compiling, the
// package no longer does the one thing it exists to do.
var _ ui.Agent = (*codex.Driver)(nil)
