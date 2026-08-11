package tickets

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"time"

	"github.com/hweeks/always-click-yes/internal/alog"
)

// entry pairs a parsed Ticket with the file it came from, so Put can
// overwrite an existing ticket's file in place rather than guessing a new
// name for it from a title that may have changed since it was created.
type entry struct {
	Ticket
	path string
}

// load reads and parses every ticket file in the store. A missing tickets
// directory is not an error — a repo that has never used arch mode has no
// .acy/tickets at all — but a directory that exists and holds a file load
// cannot parse IS one, named clearly, per file: a store this deterministic
// should never paper over a bad file by pretending it isn't there.
func (s *Store) load() ([]entry, error) {
	dir := s.dir()
	files, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("tickets: reading %s: %w", dir, err)
	}

	entries := make([]entry, 0, len(files))
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, f.Name())
		b, err := os.ReadFile(path) //nolint:gosec // path comes from our own tickets-dir listing
		if err != nil {
			return nil, fmt.Errorf("tickets: reading %s: %w", path, err)
		}
		t, err := parse(b)
		if err != nil {
			return nil, fmt.Errorf("tickets: %s: %w", path, err)
		}
		entries = append(entries, entry{Ticket: t, path: path})
	}
	return entries, nil
}

// List returns every ticket, sorted by id.
func (s *Store) List() ([]Ticket, error) {
	entries, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Ticket, len(entries))
	for i, e := range entries {
		out[i] = e.Ticket
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get returns the ticket with the given id, or ErrNotFound.
func (s *Store) Get(id string) (Ticket, error) {
	entries, err := s.load()
	if err != nil {
		return Ticket{}, err
	}
	for _, e := range entries {
		if e.ID == id {
			return e.Ticket, nil
		}
	}
	return Ticket{}, fmt.Errorf("tickets: %q: %w", id, ErrNotFound)
}

// danglingRef reports whether name is a reference Put should reject: a
// non-empty field value naming a ticket id that isn't in exists. It is
// shared by depends_on (checked once per entry) and stack_on (checked once,
// since it is a single scalar) so the two fields' "does this id exist"
// semantics can't drift apart.
func danglingRef(exists map[string]bool, ticketID, field, name string) error {
	if name == "" || exists[name] {
		return nil
	}
	return fmt.Errorf("tickets: %s: %s references unknown ticket %q", ticketID, field, name)
}

// Put validates t, stamps Updated, and writes it atomically. A brand new
// ticket gets a fresh <id>-<slug>.md; updating an existing one reuses
// whatever file it already lives at, so renaming a ticket's title never
// orphans a second file under the old slug.
//
// depends_on and stack_on entries must each name a ticket that already
// exists in the store, except t's own id — which may not exist yet (t could
// be new) and is allowed regardless. A self-referencing depends_on is still
// a dependency cycle; Validate is what catches it. A self-referencing
// stack_on is rejected earlier, by validateShape, since it is never legal.
func (s *Store) Put(t Ticket) error {
	if err := validateShape(t); err != nil {
		return fmt.Errorf("tickets: %w", err)
	}

	entries, err := s.load()
	if err != nil {
		return err
	}

	exists := map[string]bool{t.ID: true}
	path := ""
	for _, e := range entries {
		exists[e.ID] = true
		if e.ID == t.ID {
			path = e.path
		}
	}
	for _, dep := range t.DependsOn {
		if err := danglingRef(exists, t.ID, "depends_on", dep); err != nil {
			return err
		}
	}
	if err := danglingRef(exists, t.ID, "stack_on", t.StackOn); err != nil {
		return err
	}

	t.Updated = s.clock().UTC()
	if t.Body != "" && !strings.HasSuffix(t.Body, "\n") {
		t.Body += "\n"
	}

	dir := s.dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("tickets: creating %s: %w", dir, err)
	}
	if path == "" {
		path = filepath.Join(dir, t.ID+"-"+slugify(t.Title)+".md")
	}

	if err := writeAtomic(path, render(t)); err != nil {
		return err
	}
	alog.Printf("tickets: wrote %s (status=%s)", t.ID, t.Status)
	return nil
}

// UpdateStatus transitions a ticket to status and, if note is non-empty,
// appends it to the ticket's "## Log" section with an RFC3339 timestamp.
func (s *Store) UpdateStatus(id, status, note string) error {
	return s.UpdateFields(id, status, note, "", "", "")
}

// UpdateFields transitions a ticket to status and, if note is non-empty,
// appends it to the ticket's "## Log" section with an RFC3339 timestamp — the
// same as UpdateStatus — and additionally records branch, pr, and/or jira when
// given. An empty branch, pr, or jira leaves the ticket's existing value alone,
// so a caller that only knows the status (or only the branch, or only the PR,
// or only the Jira key) never clobbers what an earlier call already recorded.
func (s *Store) UpdateFields(id, status, note, branch, pr, jira string) error {
	t, err := s.Get(id)
	if err != nil {
		return err
	}
	t.Status = status
	if branch != "" {
		t.Branch = branch
	}
	if pr != "" {
		t.PR = pr
	}
	if jira != "" {
		t.Jira = jira
	}
	if note != "" {
		t.Body = appendLog(t.Body, note, s.clock().UTC().Format(time.RFC3339))
	}
	return s.Put(t)
}

// writeAtomic writes data to path via a temp file in the same directory
// followed by a rename, so a crash mid-write leaves the previous file (or
// none) intact rather than a truncated ticket.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*.md")
	if err != nil {
		return fmt.Errorf("tickets: creating temp file in %s: %w", dir, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // a no-op once the rename below has moved it

	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("tickets: chmod %s: %w", name, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("tickets: writing %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("tickets: closing %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("tickets: renaming %s to %s: %w", name, path, err)
	}
	return nil
}
