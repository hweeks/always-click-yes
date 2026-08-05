// Package tickets is a deterministic markdown ticket store for arch mode.
// Each ticket is one file at .acy/tickets/<id>-<slug>.md *in the repo being
// worked on* — not in acy's own state directory — so the ledger of what an
// arch run is doing travels with a clone or a PR diff instead of living in a
// database only acy can read.
//
// A ticket file is YAML-ish frontmatter between two "---" lines, then a
// markdown body (the brief). The frontmatter fields are all flat scalars or
// a single list, so parsing it does not need a YAML dependency — see
// format.go. Strict parsing mirrors internal/config's .acy.json philosophy:
// an unknown frontmatter key is an error, not a value quietly dropped.
package tickets

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hweeks/always-click-yes/internal/gitops"
)

// Status values a ticket can hold. There is no distinct Status type — the
// field travels as a plain string, so a frontmatter line decodes into it
// with no marshaling to get wrong.
const (
	StatusTodo       = "todo"
	StatusInProgress = "in-progress"
	StatusInReview   = "in-review"
	StatusMerged     = "merged"
	StatusBlocked    = "blocked"
)

var validStatuses = map[string]bool{
	StatusTodo:       true,
	StatusInProgress: true,
	StatusInReview:   true,
	StatusMerged:     true,
	StatusBlocked:    true,
}

// idPattern is the only shape a ticket id may take: lowercase, digits,
// dashes. It is what both a filename and a depends_on reference are built
// from, so anything looser would make one or the other ambiguous.
var idPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// Ticket is one unit of work as arch mode's ledger tracks it. Body is the
// brief — the markdown that follows the frontmatter — and, once
// UpdateStatus has appended to it at least once, ends with a "## Log"
// section of timestamped notes.
type Ticket struct {
	ID        string
	Title     string
	Status    string
	Branch    string
	PR        string
	DependsOn []string
	Updated   time.Time
	Body      string
}

// ErrNotFound is returned by Get when no ticket has the given id.
var ErrNotFound = errors.New("tickets: not found")

// Store is a markdown ticket store rooted at Root — the repo being worked
// on. Run is injectable so Commit can be tested against a real, hermetic
// git binary instead of the host's.
type Store struct {
	Root string
	Mode string // "direct" commits and best-effort pushes; "none" makes Commit a no-op.
	Run  gitops.Runner

	// now is overridable by tests in this package; New defaults it to
	// time.Now so production callers never see it.
	now func() time.Time
}

// New builds a Store rooted at root. mode mirrors the fleet config's
// ticketCommit ("direct" or "none").
func New(root, mode string, run gitops.Runner) *Store {
	return &Store{Root: root, Mode: mode, Run: run, now: time.Now}
}

func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// dir is .acy/tickets under Root, created on demand by Put.
func (s *Store) dir() string {
	return filepath.Join(s.Root, ".acy", "tickets")
}

// validateShape checks the fields every ticket must have regardless of where
// it came from: parsed off disk, or about to be written by Put. It does not
// check depends_on — that needs the rest of the store, and is Put's and
// Validate's job.
func validateShape(t Ticket) error {
	if !idPattern.MatchString(t.ID) {
		return fmt.Errorf("invalid id %q: must match [a-z0-9-]+", t.ID)
	}
	if strings.TrimSpace(t.Title) == "" {
		return errors.New("title is required")
	}
	if !validStatuses[t.Status] {
		return fmt.Errorf("invalid status %q", t.Status)
	}
	return nil
}
