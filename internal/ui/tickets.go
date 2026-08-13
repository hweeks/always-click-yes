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
// memory — see phase.go's ArchSystemPromptFor — so keeping it current is the
// architect's job, not something acy infers on its behalf.

// TicketStore is the subset of *tickets.Store the model uses. Named here
// rather than imported wholesale so ui tests can supply a fake, mirroring how
// FleetManager already does for *fleet.Manager.
type TicketStore interface {
	List() ([]tickets.Ticket, error)
	Put(t tickets.Ticket) error
	UpdateFields(id, status, note, branch, pr, jira string) error
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
	Branch string `json:"branch"`
	PR     string `json:"pr"`
	Jira   string `json:"jira"`
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
	a.Branch = strings.TrimSpace(a.Branch)
	a.PR = strings.TrimSpace(a.PR)
	a.Jira = strings.TrimSpace(a.Jira)

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
	if err := m.tickets.UpdateFields(args.ID, args.Status, args.Note, args.Branch, args.PR, args.Jira); err != nil {
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
	case errors.Is(commitErr, tickets.ErrPushSkipped):
		p.Resolve(mcp.Answer{Text: fmt.Sprintf(
			"ticket %s updated to %s and committed locally, but not pushed: %s", args.ID, args.Status, commitErr.Error())})
	default:
		p.Resolve(mcp.Answer{Text: fmt.Sprintf(
			"ticket %s updated to %s, but committing it failed: %s", args.ID, args.Status, commitErr.Error())})
		return
	}
	m.appendEntry(entry{kind: eGood, body: fmt.Sprintf("ticket %s → %s", args.ID, args.Status)})
	m.emitFlowDiagram()
}

// --- CreateTicket ---

type createTicketArgs struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Brief     string   `json:"brief"`
	DependsOn []string `json:"depends_on"`
	StackOn   string   `json:"stack_on"`
	Jira      string   `json:"jira"`
}

// parseCreateTicket decodes a CreateTicket call, strictly: a missing required
// field names itself rather than silently no-op'ing. The id's shape
// ([a-z0-9-]+) is left to tickets.Store.Put, which already enforces it.
func parseCreateTicket(raw json.RawMessage) (createTicketArgs, error) {
	var a createTicketArgs
	if len(raw) == 0 {
		return a, errors.New("no arguments were given")
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("the arguments were not valid JSON: %w", err)
	}
	a.ID = strings.TrimSpace(a.ID)
	a.Title = strings.TrimSpace(a.Title)
	a.Brief = strings.TrimSpace(a.Brief)
	a.StackOn = strings.TrimSpace(a.StackOn)
	a.Jira = strings.TrimSpace(a.Jira)

	var missing []string
	if a.ID == "" {
		missing = append(missing, "id")
	}
	if a.Title == "" {
		missing = append(missing, "title")
	}
	if a.Brief == "" {
		missing = append(missing, "brief")
	}
	if len(missing) > 0 {
		return a, fmt.Errorf("missing required field(s): %s", strings.Join(missing, ", "))
	}
	return a, nil
}

// startCreateTicket answers a CreateTicket call: refuse a duplicate id, put
// the new ticket as todo, then commit. A push failure from Commit is not a
// failure of the call itself — the ticket file already landed locally — so
// it resolves as success with a note that the human should push, the same
// semantics as startUpdateTicket.
func (m *Model) startCreateTicket(p *mcp.Pending) {
	if m.tickets == nil {
		p.Resolve(mcp.Answer{Text: mcp.TicketsUnavailable})
		return
	}
	args, err := parseCreateTicket(p.Req.Args)
	if err != nil {
		p.Resolve(mcp.Answer{Text: "CreateTicket could not be read: " + err.Error() +
			". Nothing was created. Fix the arguments and call it again."})
		return
	}

	existing, err := m.tickets.List()
	if err != nil {
		p.Resolve(mcp.Answer{Text: "CreateTicket failed: " + err.Error()})
		return
	}
	for _, t := range existing {
		if t.ID == args.ID {
			p.Resolve(mcp.Answer{Text: fmt.Sprintf(
				"CreateTicket refused: ticket %q already exists. The board is the memory — updating an "+
					"existing ticket is UpdateTicket's job, not CreateTicket's.", args.ID)})
			return
		}
	}

	err = m.tickets.Put(tickets.Ticket{
		ID:        args.ID,
		Title:     args.Title,
		Status:    tickets.StatusTodo,
		Body:      args.Brief,
		DependsOn: args.DependsOn,
		StackOn:   args.StackOn,
		Jira:      args.Jira,
	})
	if err != nil {
		p.Resolve(mcp.Answer{Text: "CreateTicket failed: " + err.Error()})
		return
	}

	commitErr := m.tickets.Commit(m.ctx, fmt.Sprintf("ticket %s: created", args.ID))
	path := ticketFilePath(args.ID, args.Title)
	switch {
	case commitErr == nil:
		p.Resolve(mcp.Answer{Text: fmt.Sprintf("ticket %s created at %s", args.ID, path)})
	case errors.Is(commitErr, tickets.ErrPushFailed):
		p.Resolve(mcp.Answer{Text: fmt.Sprintf(
			"ticket %s created at %s and committed locally, but the push failed — the human should push "+
				"(a protected main branch rejecting a direct push is normal here, not an error)", args.ID, path)})
	case errors.Is(commitErr, tickets.ErrPushSkipped):
		p.Resolve(mcp.Answer{Text: fmt.Sprintf(
			"ticket %s created at %s and committed locally, but not pushed: %s", args.ID, path, commitErr.Error())})
	default:
		p.Resolve(mcp.Answer{Text: fmt.Sprintf(
			"ticket %s created at %s, but committing it failed: %s", args.ID, path, commitErr.Error())})
		return
	}
	m.appendEntry(entry{kind: eGood, body: fmt.Sprintf("ticket %s created", args.ID)})
	m.emitFlowDiagram()
}

// ticketFilePath names the file CreateTicket just wrote, mirroring
// tickets.Store's own <id>-<slug>.md convention purely for the confirmation
// message — Put is what actually decides where the ticket lives.
func ticketFilePath(id, title string) string {
	return ".acy/tickets/" + id + "-" + ticketSlugify(title) + ".md"
}

func ticketSlugify(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 60 {
		slug = strings.Trim(slug[:60], "-")
	}
	if slug == "" {
		slug = "ticket"
	}
	return slug
}

// --- flow diagram ---

// flowBody renders the board as the ascii lanes followed by a fenced mermaid
// block — the shared shape emitFlowDiagram and /flow both append as an eFlow
// entry's body, so the two can never disagree about what a flow entry looks
// like. mermaid and ascii are returned separately so a caller can dedupe, set
// raw, or cache the projection without re-deriving either from the fenced
// body.
func flowBody(ts []tickets.Ticket) (body, mermaid, ascii string) {
	mermaid = tickets.Mermaid(ts)
	ascii = tickets.ASCII(ts)
	body = ascii + "\n\n```mermaid\n" + mermaid + "\n```"
	return body, mermaid, ascii
}

// projectTickets is Frame's Ticket projection of the board — the summary a
// client lists, not the full brief ReadTickets/UpdateTicket hand the model.
func projectTickets(ts []tickets.Ticket) []Ticket {
	out := make([]Ticket, 0, len(ts))
	for _, t := range ts {
		out = append(out, Ticket{ID: t.ID, Title: t.Title, Status: t.Status, PRURL: t.PR})
	}
	return out
}

// refreshTicketCache is the ticket board's only read path: it re-lists the
// store once and refreshes cachedTickets/cachedFlow, the mirror Frame
// projects — mirroring how syncFleet keeps a mirror for the fleet, except
// triggered by CreateTicket/UpdateTicket's own success rather than an event,
// because there is no other way the board changes during a run. That keeps
// Frame a genuine read of the model's own state: it runs once per tick
// (120ms) with a webview attached, and hitting disk on every one of those
// ticks — twice, once each for Tickets and Flow — was what made an idle run
// with no board change still re-read and re-parse every ticket file twice a
// tick and regenerate both diagrams from possibly-disagreeing reads.
//
// A nil store or a read error leaves the cache exactly as it was, matching
// how the callers already treat both as "skip silently, not fatal".
func (m *Model) refreshTicketCache() (ts []tickets.Ticket, body, mermaid string, err error) {
	if m.tickets == nil {
		return nil, "", "", nil
	}
	ts, err = m.tickets.List()
	if err != nil {
		return nil, "", "", err
	}
	var ascii string
	body, mermaid, ascii = flowBody(ts)
	m.cachedTickets = projectTickets(ts)
	m.cachedFlow = Flow{Mermaid: mermaid, ASCII: ascii}
	return ts, body, mermaid, nil
}

// emitFlowDiagram redraws the ticket flow into the transcript after a
// CreateTicket/UpdateTicket milestone. It is best-effort UI decoration, not
// the tool's answer — a nil store or a read error is silently skipped rather
// than surfaced — and it dedupes against the last diagram emitted so a
// milestone that leaves the board's shape unchanged (e.g. re-marking a
// ticket with the status it already had) does not print the same diagram
// twice. The cache refreshes unconditionally, even when deduped: a field the
// diagram does not draw (PR, branch, jira, note) can still have changed.
func (m *Model) emitFlowDiagram() {
	if m.tickets == nil {
		return
	}
	_, body, mermaid, err := m.refreshTicketCache()
	if err != nil {
		return
	}
	if mermaid == m.lastFlowDiagram {
		return
	}
	m.lastFlowDiagram = mermaid
	m.appendEntry(entry{kind: eFlow, body: body, raw: mermaid, lang: "mermaid"})
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
	if chains := tickets.StackChains(ts); len(chains) > 0 {
		for _, chain := range chains {
			fmt.Fprintf(&b, "stack: %s\n", strings.Join(chain, " -> "))
		}
		b.WriteString("\n")
	}
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
		if t.Jira != "" {
			fmt.Fprintf(&b, " · jira: %s", t.Jira)
		}
		if len(t.DependsOn) > 0 {
			fmt.Fprintf(&b, " · depends_on: %s", strings.Join(t.DependsOn, ", "))
		}
		if t.StackOn != "" {
			fmt.Fprintf(&b, " · stack_on: %s", t.StackOn)
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

// runFlowCommand answers /flow: an explicit request to redraw the ticket
// flow, so unlike emitFlowDiagram it always appends — it deliberately never
// touches m.lastFlowDiagram, which exists only to dedupe the automatic
// milestone emission.
func (m *Model) runFlowCommand() {
	if m.tickets == nil {
		m.appendEntry(entry{kind: eMeta, body: "no ticket store configured in this session"})
		return
	}
	ts, err := m.tickets.List()
	if err != nil {
		m.appendEntry(entry{kind: eMeta, body: "could not read tickets: " + err.Error()})
		return
	}
	body, mermaid, _ := flowBody(ts)
	m.appendEntry(entry{kind: eFlow, body: body, raw: mermaid, lang: "mermaid"})
}
