package tickets

import (
	"fmt"
	"strings"
)

// Validate checks the whole store for problems no single Put can prevent
// because they span files: a duplicate id (two files, or a file hand-edited
// to claim an existing one), a depends_on that names nothing, and a
// dependency cycle. Put already blocks a dangling depends_on and List
// already rejects a malformed file, so Validate is the safety net for a
// store that got into a bad shape some other way — an out-of-band edit, or a
// bug in an older acy.
func (s *Store) Validate() error {
	tickets, err := s.List()
	if err != nil {
		return err
	}

	byID := make(map[string]Ticket, len(tickets))
	for _, t := range tickets {
		if _, dup := byID[t.ID]; dup {
			return fmt.Errorf("tickets: duplicate ticket id %q", t.ID)
		}
		byID[t.ID] = t
	}

	for _, t := range tickets {
		for _, dep := range t.DependsOn {
			if _, ok := byID[dep]; !ok {
				return fmt.Errorf("tickets: %s: depends_on references unknown ticket %q", t.ID, dep)
			}
		}
	}

	const (
		visiting = 1
		done     = 2
	)
	state := make(map[string]int, len(tickets))

	var visit func(id string, path []string) error
	visit = func(id string, path []string) error {
		switch state[id] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("tickets: dependency cycle: %s -> %s", strings.Join(path, " -> "), id)
		}
		state[id] = visiting
		for _, dep := range byID[id].DependsOn {
			if err := visit(dep, append(path, id)); err != nil {
				return err
			}
		}
		state[id] = done
		return nil
	}

	for _, t := range tickets {
		if state[t.ID] == done {
			continue
		}
		if err := visit(t.ID, nil); err != nil {
			return err
		}
	}
	return nil
}
