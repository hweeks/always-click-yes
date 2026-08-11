package tickets

import (
	"fmt"
	"strings"
)

// StackChains walks the stack_on relation across the whole board and returns
// one ordered id slice per chain of length >= 2, root first. At most one
// ticket may claim a given stack_on parent — tickets.Store enforces that on
// Put — so this relation can only ever branch into disjoint chains, never a
// tree, and a simple parent-to-single-child map is enough to walk it.
func StackChains(ts []Ticket) [][]string {
	childOf := make(map[string]string, len(ts))
	for _, t := range ts {
		if t.StackOn != "" {
			childOf[t.StackOn] = t.ID
		}
	}

	var chains [][]string
	for _, t := range ts {
		if t.StackOn != "" {
			continue // not a root
		}
		chain := []string{t.ID}
		for cur := t.ID; ; {
			next, ok := childOf[cur]
			if !ok {
				break
			}
			chain = append(chain, next)
			cur = next
		}
		if len(chain) >= 2 {
			chains = append(chains, chain)
		}
	}
	return chains
}

// statusOrder is the fixed lane order both Mermaid's classDefs and ASCII's
// lanes present statuses in, so either rendering is byte-stable across runs
// regardless of which statuses happen to be present on the board.
var statusOrder = []string{StatusTodo, StatusInProgress, StatusInReview, StatusMerged, StatusBlocked}

// statusColor is the classDef fill color for each status, chosen for visual
// distinctness rather than any meaning beyond that.
var statusColor = map[string]string{
	StatusTodo:       "#e0e0e0",
	StatusInProgress: "#fff3b0",
	StatusInReview:   "#bde0fe",
	StatusMerged:     "#b7e4c7",
	StatusBlocked:    "#f8b4b4",
}

// escapeMermaidLabel makes s safe inside a quoted mermaid node label: a
// literal double quote would close the label early, and a literal newline
// would break mermaid's line parser, so both are replaced with mermaid's own
// escape/line-break syntax instead of being passed through.
func escapeMermaidLabel(s string) string {
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "\r\n", "<br/>")
	s = strings.ReplaceAll(s, "\n", "<br/>")
	return s
}

// Mermaid renders the board as a mermaid flowchart: one node per ticket
// (labelled with id, title, status, and Jira key when set), a solid edge per
// depends_on entry, a dotted "stacking" edge per stack_on relation, and a
// classDef per status coloring its nodes. It never iterates a map in the
// emission path — only the ordered statusOrder slice and ts itself — so its
// output is byte-stable for a given board.
func Mermaid(ts []Ticket) string {
	var b strings.Builder
	b.WriteString("flowchart TD\n")

	if len(ts) == 0 {
		b.WriteString("    empty[\"no tickets yet\"]\n")
		return b.String()
	}

	for _, t := range ts {
		label := fmt.Sprintf("%s: %s [%s]", t.ID, t.Title, t.Status)
		if t.Jira != "" {
			label += " (" + t.Jira + ")"
		}
		fmt.Fprintf(&b, "    %s[\"%s\"]:::%s\n", t.ID, escapeMermaidLabel(label), t.Status)
	}

	for _, t := range ts {
		for _, dep := range t.DependsOn {
			fmt.Fprintf(&b, "    %s --> %s\n", dep, t.ID)
		}
	}
	for _, t := range ts {
		if t.StackOn != "" {
			fmt.Fprintf(&b, "    %s -.->|stacking| %s\n", t.StackOn, t.ID)
		}
	}

	for _, status := range statusOrder {
		fmt.Fprintf(&b, "    classDef %s fill:%s\n", status, statusColor[status])
	}

	return b.String()
}

// ASCII renders the board as a plain-text status-lane summary: the five
// statuses in a fixed order, each listing that status's ticket ids, followed
// by any stack chains drawn as "a -> b -> c". It imports nothing from
// internal/ui — this package must not learn about styling.
func ASCII(ts []Ticket) string {
	if len(ts) == 0 {
		return "no tickets yet\n"
	}

	byStatus := make(map[string][]string, len(statusOrder))
	for _, t := range ts {
		byStatus[t.Status] = append(byStatus[t.Status], t.ID)
	}

	var b strings.Builder
	for _, status := range statusOrder {
		ids := byStatus[status]
		fmt.Fprintf(&b, "[%s] (%d)\n", status, len(ids))
		if len(ids) == 0 {
			b.WriteString("  (none)\n")
			continue
		}
		for _, id := range ids {
			fmt.Fprintf(&b, "  - %s\n", id)
		}
	}

	if chains := StackChains(ts); len(chains) > 0 {
		b.WriteString("\nstacks:\n")
		for _, chain := range chains {
			fmt.Fprintf(&b, "  %s\n", strings.Join(chain, " -> "))
		}
	}

	return b.String()
}
