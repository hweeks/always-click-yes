package tickets

import (
	"fmt"
	"sort"
	"strings"
)

// Validate checks the whole store for problems no single Put can prevent
// because they span files: a duplicate id (two files, or a file hand-edited
// to claim an existing one), a depends_on or stack_on that names nothing, a
// stack_on claimed by more than one ticket, and a dependency or stack_on
// cycle. Put already blocks a dangling depends_on or stack_on and List
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
		if t.StackOn != "" {
			if _, ok := byID[t.StackOn]; !ok {
				return fmt.Errorf("tickets: %s: stack_on references unknown ticket %q", t.ID, t.StackOn)
			}
		}
	}

	// A stack is linear: at most one ticket may claim a given id as its
	// stack_on parent. Two tickets both stacking on the same parent would
	// mean the parent's branch forks into two divergent lines, which is not
	// a "stack" in the sense this field models.
	claimants := make(map[string][]string, len(tickets))
	for _, t := range tickets {
		if t.StackOn != "" {
			claimants[t.StackOn] = append(claimants[t.StackOn], t.ID)
		}
	}
	parents := make([]string, 0, len(claimants))
	for parent := range claimants {
		parents = append(parents, parent)
	}
	sort.Strings(parents)
	for _, parent := range parents {
		children := claimants[parent]
		if len(children) > 1 {
			sort.Strings(children)
			return fmt.Errorf("tickets: %s: claimed as stack_on by more than one ticket: %s",
				parent, strings.Join(children, ", "))
		}
	}

	if err := detectCycle(tickets, "dependency", func(id string) []string {
		return byID[id].DependsOn
	}); err != nil {
		return err
	}
	return detectCycle(tickets, "stack_on", func(id string) []string {
		if parent := byID[id].StackOn; parent != "" {
			return []string{parent}
		}
		return nil
	})
}

// detectCycle walks the graph formed by edges — a function from a ticket id
// to the ids it points at — and returns an error if any cycle exists. It is
// parameterized so the same DFS serves both depends_on (which can fan out to
// several edges per ticket) and stack_on (which has at most one) without
// duplicating the graph-walk logic. label distinguishes the two in the error
// message.
func detectCycle(tickets []Ticket, label string, edges func(id string) []string) error {
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
			return fmt.Errorf("tickets: %s cycle: %s -> %s", label, strings.Join(path, " -> "), id)
		}
		state[id] = visiting
		for _, next := range edges(id) {
			if err := visit(next, append(path, id)); err != nil {
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
