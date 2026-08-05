package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hweeks/always-click-yes/internal/mcp"
	"github.com/hweeks/always-click-yes/internal/tickets"
)

// The architect's ticket board, from the UI's side.
//
// Unlike the fleet tools, there is no event stream here: ReadTickets and
// UpdateTicket are a plain request/response pair over the same ask socket,
// answered immediately from the store on disk. The board itself is the run's
// memory — see phase.go's ArchSystemPrompt — so keeping it current is the
// architect's job, not something acy infers on its behalf.

// TicketStore is the subset of *tickets.Store the model uses. Named here
// rather than imported wholesale so ui tests can supply a fake, mirroring how
// FleetManager already does for *fleet.Manager.
type TicketStore interface {
	List() ([]tickets.Ticket, error)
	UpdateStatus(id, status, note string) error
	Commit(ctx context.Context, msg string) error
}

// validTicketStatuses mirrors the tickets package's own (unexported) status
// set — the wire contract for UpdateTicket's "status" enum.
var validTicketStatuses = map[string]bool{
	tickets.StatusTodo:       true,
	tickets.StatusInProgress: true,
	tickets.StatusInReview:   true,
	tickets.StatusMerged:     true,
	tickets.StatusBlocked:    true,
}

// --- ReadTickets ---

func (m *Model) startReadTickets(p *mcp.Pending) {
	if m.tickets == nil {
		p.Resolve(mcp.Answer{Text: mcp.TicketsUnavailable})
		return
	}
	ts, err := m.tickets.List()
	if err != nil {
		p.Resolve(mcp.Answer{Text: "ReadTickets failed: " + err.Error()})
		return
	}
	p.Resolve(mcp.Answer{Text: renderTicketBoard(ts)})
}

// --- UpdateTicket ---

type updateTicketArgs struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Note   string `json:"note"`
}

// parseUpdateTicket decodes an UpdateTicket call, strictly: a missing required
// field or an invalid status names itself rather than silently no-op'ing.
func parseUpdateTicket(raw json.RawMessage) (updateTicketArgs, error) {
	var a updateTicketArgs
	if len(raw) == 0 {
		return a, errors.New("no arguments were given")
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("the arguments were not valid JSON: %w", err)
	}
	a.ID = strings.TrimSpace(a.ID)
	a.Status = strings.TrimSpace(a.Status)
	a.Note = strings.TrimSpace(a.Note)

	var missing []string
	if a.ID == "" {
		missing = append(missing, "id")
	}
	if a.Status == "" {
		missing = append(missing, "status")
	}
	if len(missing) > 0 {
		return a, fmt.Errorf("missing required field(s): %s", strings.Join(missing, ", "))
	}
	if !validTicketStatuses[a.Status] {
		return a, fmt.Errorf("invalid status %q: must be one of todo, in-progress, in-review, merged, blocked", a.Status)
	}
	return a, nil
}

// startUpdateTicket answers an UpdateTicket call: update the status, then
// commit. A push failure from Commit is not a failure of the call itself —
// the status change already landed locally — so it resolves as success with
// a note that the human should push; a protected main branch rejecting a
// direct push is normal, not an error.
func (m *Model) startUpdateTicket(p *mcp.Pending) {
	if m.tickets == nil {
		p.Resolve(mcp.Answer{Text: mcp.TicketsUnavailable})
		return
	}
	args, err := parseUpdateTicket(p.Req.Args)
	if err != nil {
		p.Resolve(mcp.Answer{Text: "UpdateTicket could not be read: " + err.Error() +
			". Nothing was changed. Fix the arguments and call it again."})
		return
	}
	if err := m.tickets.UpdateStatus(args.ID, args.Status, args.Note); err != nil {
		p.Resolve(mcp.Answer{Text: "UpdateTicket failed: " + err.Error()})
		return
	}

	commitErr := m.tickets.Commit(m.ctx, fmt.Sprintf("ticket %s: %s", args.ID, args.Status))
	switch {
	case commitErr == nil:
		p.Resolve(mcp.Answer{Text: fmt.Sprintf("ticket %s updated to %s", args.ID, args.Status)})
	case errors.Is(commitErr, tickets.ErrPushFailed):
		p.Resolve(mcp.Answer{Text: fmt.Sprintf(
			"ticket %s updated to %s and committed locally, but the push failed — the human should push "+
				"(a protected main branch rejecting a direct push is normal here, not an error)", args.ID, args.Status)})
	default:
		p.Resolve(mcp.Answer{Text: fmt.Sprintf(
			"ticket %s updated to %s, but committing it failed: %s", args.ID, args.Status, commitErr.Error())})
		return
	}
	m.appendEntry(entry{kind: eGood, body: fmt.Sprintf("ticket %s → %s", args.ID, args.Status)})
}

// --- rendering ---

// renderTicketBoard renders the whole board as cheap markdown: one block per
// ticket, its frontmatter fields as a summary line, then its body (the
// brief). ReadTickets and /tickets both go through this, so the two can never
// disagree about what the board says.
func renderTicketBoard(ts []tickets.Ticket) string {
	if len(ts) == 0 {
		return "no tickets yet — .acy/tickets is empty"
	}
	var b strings.Builder
	for i, t := range ts {
		if i > 0 {
			b.WriteString("\n\n---\n\n")
		}
		fmt.Fprintf(&b, "## %s: %s\n", t.ID, t.Title)
		fmt.Fprintf(&b, "status: %s", t.Status)
		if t.Branch != "" {
			fmt.Fprintf(&b, " · branch: %s", t.Branch)
		}
		if t.PR != "" {
			fmt.Fprintf(&b, " · pr: %s", t.PR)
		}
		if len(t.DependsOn) > 0 {
			fmt.Fprintf(&b, " · depends_on: %s", strings.Join(t.DependsOn, ", "))
		}
		b.WriteString("\n")
		if body := strings.TrimSpace(t.Body); body != "" {
			b.WriteString("\n" + body + "\n")
		}
	}
	return b.String()
}

// ticketsReport is /tickets's body: the same board ReadTickets hands the
// architect, re-read so a human checking in sees the current board rather
// than whatever it looked like when the run started.
func (m *Model) ticketsReport() string {
	if m.tickets == nil {
		return "no ticket store configured in this session"
	}
	ts, err := m.tickets.List()
	if err != nil {
		return "could not read tickets: " + err.Error()
	}
	return renderTicketBoard(ts)
}
