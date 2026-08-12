package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/gate"
	"github.com/hweeks/always-click-yes/internal/mcp"
	"github.com/hweeks/always-click-yes/internal/tickets"
)

// fakeTicketStore answers only what tickets.go's tool handlers need, mirroring
// fakeFleetManager in fleet_test.go.
type fakeTicketStore struct {
	mu sync.Mutex

	list    []tickets.Ticket
	listErr error

	puts   []tickets.Ticket
	putErr error

	updates    []ticketUpdateCall
	updateErr  error
	commitErr  error
	commitMsgs []string
}

type ticketUpdateCall struct{ id, status, note, branch, pr, jira string }

func (f *fakeTicketStore) List() ([]tickets.Ticket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list, f.listErr
}

func (f *fakeTicketStore) Put(t tickets.Ticket) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, t)
	return f.putErr
}

func (f *fakeTicketStore) UpdateFields(id, status, note, branch, pr, jira string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, ticketUpdateCall{id, status, note, branch, pr, jira})
	return f.updateErr
}

func (f *fakeTicketStore) Commit(_ context.Context, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitMsgs = append(f.commitMsgs, msg)
	return f.commitErr
}

// ticketPending builds an in-flight ticket tool call the way the ask bridge
// would, mirroring fleetPending in fleet_test.go.
func ticketPending(tool, args string) (*mcp.Pending, <-chan mcp.Answer) {
	return mcp.NewPending(mcp.Request{Tool: tool, ToolUseID: "tu_tickets", Args: json.RawMessage(args)})
}

// --- ReadTickets ---

func TestReadTicketsRefusedWithoutStore(t *testing.T) {
	m := &Model{}
	p, reply := ticketPending(mcp.ToolReadTickets, `{}`)
	m.startReadTickets(p)
	if got := answer(t, reply); got != mcp.TicketsUnavailable {
		t.Errorf("answer = %q, want mcp.TicketsUnavailable", got)
	}
}

func TestReadTicketsRendersBoard(t *testing.T) {
	fake := &fakeTicketStore{list: []tickets.Ticket{
		{ID: "t1", Title: "add the ledger", Status: tickets.StatusInProgress, Branch: "agent/t1", Body: "do the thing"},
		{ID: "t2", Title: "wire the store", Status: tickets.StatusTodo, DependsOn: []string{"t1"}},
	}}
	m := &Model{tickets: fake}
	p, reply := ticketPending(mcp.ToolReadTickets, `{}`)
	m.startReadTickets(p)

	got := answer(t, reply)
	for _, want := range []string{"t1", "add the ledger", tickets.StatusInProgress, "agent/t1", "do the thing",
		"t2", "wire the store", tickets.StatusTodo, "depends_on: t1"} {
		if !strings.Contains(got, want) {
			t.Errorf("board = %q, missing %q", got, want)
		}
	}
}

func TestRenderTicketBoardIncludesJiraWhenSet(t *testing.T) {
	got := renderTicketBoard([]tickets.Ticket{
		{ID: "t1", Title: "add x", Status: tickets.StatusTodo, Jira: "ENG-1"},
	})
	if !strings.Contains(got, "jira: ENG-1") {
		t.Errorf("board = %q, want it to contain %q", got, "jira: ENG-1")
	}
}

func TestRenderTicketBoardOmitsJiraWhenUnset(t *testing.T) {
	got := renderTicketBoard([]tickets.Ticket{
		{ID: "t1", Title: "add x", Status: tickets.StatusTodo},
	})
	if strings.Contains(got, "jira:") {
		t.Errorf("board = %q, want no \"jira:\" substring", got)
	}
}

func TestReadTicketsRendersStackChain(t *testing.T) {
	fake := &fakeTicketStore{list: []tickets.Ticket{
		{ID: "a", Title: "base", Status: tickets.StatusTodo},
		{ID: "b", Title: "middle", Status: tickets.StatusTodo, StackOn: "a"},
		{ID: "c", Title: "leaf", Status: tickets.StatusTodo, StackOn: "b"},
	}}
	m := &Model{tickets: fake}
	p, reply := ticketPending(mcp.ToolReadTickets, `{}`)
	m.startReadTickets(p)

	got := answer(t, reply)
	if !strings.Contains(got, "a -> b -> c") {
		t.Errorf("board = %q, want the chain order a -> b -> c", got)
	}
	for _, want := range []string{"stack_on: a", "stack_on: b"} {
		if !strings.Contains(got, want) {
			t.Errorf("board = %q, missing per-ticket %q", got, want)
		}
	}
}

func TestReadTicketsEmptyBoard(t *testing.T) {
	fake := &fakeTicketStore{}
	m := &Model{tickets: fake}
	p, reply := ticketPending(mcp.ToolReadTickets, `{}`)
	m.startReadTickets(p)
	if got := answer(t, reply); !strings.Contains(got, "no tickets") {
		t.Errorf("answer = %q, want it to say the board is empty", got)
	}
}

func TestReadTicketsSurfacesError(t *testing.T) {
	fake := &fakeTicketStore{listErr: errors.New("tickets: reading .acy/tickets: boom")}
	m := &Model{tickets: fake}
	p, reply := ticketPending(mcp.ToolReadTickets, `{}`)
	m.startReadTickets(p)
	if got := answer(t, reply); !strings.Contains(got, "boom") {
		t.Errorf("answer = %q, want the store's error surfaced", got)
	}
}

// --- UpdateTicket ---

func TestUpdateTicketRefusedWithoutStore(t *testing.T) {
	m := &Model{}
	p, reply := ticketPending(mcp.ToolUpdateTicket, `{"id":"t1","status":"in-progress"}`)
	m.startUpdateTicket(p)
	if got := answer(t, reply); got != mcp.TicketsUnavailable {
		t.Errorf("answer = %q, want mcp.TicketsUnavailable", got)
	}
}

// Strict parsing: a missing required field names itself rather than silently
// no-op'ing.
func TestUpdateTicketMissingFieldsRefusesWithoutUpdating(t *testing.T) {
	fake := &fakeTicketStore{}
	m := &Model{tickets: fake}
	p, reply := ticketPending(mcp.ToolUpdateTicket, `{}`)
	m.startUpdateTicket(p)

	got := answer(t, reply)
	for _, want := range []string{"id", "status"} {
		if !strings.Contains(got, want) {
			t.Errorf("answer = %q, want it to name the missing field %q", got, want)
		}
	}
	if len(fake.updates) != 0 {
		t.Error("nothing should have been updated with missing fields")
	}
}

func TestUpdateTicketInvalidStatusRefusesWithoutUpdating(t *testing.T) {
	fake := &fakeTicketStore{}
	m := &Model{tickets: fake}
	p, reply := ticketPending(mcp.ToolUpdateTicket, `{"id":"t1","status":"done"}`)
	m.startUpdateTicket(p)

	if got := answer(t, reply); !strings.Contains(got, "invalid status") {
		t.Errorf("answer = %q, want it to name the invalid status", got)
	}
	if len(fake.updates) != 0 {
		t.Error("nothing should have been updated with an invalid status")
	}
}

func TestUpdateTicketUpdatesAndCommits(t *testing.T) {
	fake := &fakeTicketStore{}
	m := &Model{tickets: fake, ctx: context.Background()}
	p, reply := ticketPending(mcp.ToolUpdateTicket, `{"id":"t1","status":"in-review","note":"PR opened"}`)
	m.startUpdateTicket(p)

	if len(fake.updates) != 1 || fake.updates[0] != (ticketUpdateCall{"t1", "in-review", "PR opened", "", "", ""}) {
		t.Fatalf("update not applied: %+v", fake.updates)
	}
	if len(fake.commitMsgs) != 1 || !strings.Contains(fake.commitMsgs[0], "t1") || !strings.Contains(fake.commitMsgs[0], "in-review") {
		t.Fatalf("commit message = %+v, want it to name the ticket and status", fake.commitMsgs)
	}
	got := answer(t, reply)
	for _, want := range []string{"t1", "in-review"} {
		if !strings.Contains(got, want) {
			t.Errorf("answer = %q, missing %q", got, want)
		}
	}
	// The eGood milestone notice, plus the flow diagram it triggers.
	if len(m.entries) != 2 {
		t.Fatalf("want 2 transcript entries, got %d", len(m.entries))
	}
	if m.entries[1].kind != eFlow {
		t.Errorf("second entry kind = %v, want eFlow", m.entries[1].kind)
	}
}

// UpdateTicket accepts optional branch and pr and threads them through to
// the store untouched — whether they get left alone or overwritten on an
// omit is UpdateFields' call, not the UI's.
func TestUpdateTicketPassesBranchAndPRWhenSet(t *testing.T) {
	fake := &fakeTicketStore{}
	m := &Model{tickets: fake, ctx: context.Background()}
	p, reply := ticketPending(mcp.ToolUpdateTicket,
		`{"id":"t1","status":"in-progress","branch":"agent/t1","pr":"https://example.com/pr/1"}`)
	m.startUpdateTicket(p)
	answer(t, reply)

	if len(fake.updates) != 1 || fake.updates[0] != (ticketUpdateCall{"t1", "in-progress", "", "agent/t1", "https://example.com/pr/1", ""}) {
		t.Fatalf("update not applied with branch/pr: %+v", fake.updates)
	}
}

// UpdateTicket accepts an optional jira and threads it through to the store
// untouched, the same as branch and pr.
func TestUpdateTicketPassesJiraWhenSet(t *testing.T) {
	fake := &fakeTicketStore{}
	m := &Model{tickets: fake, ctx: context.Background()}
	p, reply := ticketPending(mcp.ToolUpdateTicket,
		`{"id":"t1","status":"in-progress","jira":"ENG-2"}`)
	m.startUpdateTicket(p)
	answer(t, reply)

	if len(fake.updates) != 1 || fake.updates[0].jira != "ENG-2" {
		t.Fatalf("update not applied with jira: %+v", fake.updates)
	}
}

func TestUpdateTicketOmittedBranchAndPRPassThroughEmpty(t *testing.T) {
	fake := &fakeTicketStore{}
	m := &Model{tickets: fake, ctx: context.Background()}
	p, reply := ticketPending(mcp.ToolUpdateTicket, `{"id":"t1","status":"merged"}`)
	m.startUpdateTicket(p)
	answer(t, reply)

	if len(fake.updates) != 1 || fake.updates[0].branch != "" || fake.updates[0].pr != "" {
		t.Fatalf("update with no branch/pr should pass them through empty: %+v", fake.updates)
	}
}

// A push failure is not a failure of the call: the status change and the
// local commit both already landed. The answer must say so, and must tell
// the model to have the human push rather than reporting an error.
func TestUpdateTicketPushFailureStillSucceeds(t *testing.T) {
	fake := &fakeTicketStore{commitErr: fmt.Errorf("%w: exit status 1", tickets.ErrPushFailed)}
	m := &Model{tickets: fake, ctx: context.Background()}
	p, reply := ticketPending(mcp.ToolUpdateTicket, `{"id":"t1","status":"merged"}`)
	m.startUpdateTicket(p)

	got := answer(t, reply)
	for _, want := range []string{"t1", "merged", "push failed", "human should push"} {
		if !strings.Contains(got, want) {
			t.Errorf("answer = %q, missing %q", got, want)
		}
	}
	// The eGood milestone notice, plus the flow diagram it triggers.
	if len(m.entries) != 2 {
		t.Fatalf("want 2 transcript entries even though the push failed, got %d", len(m.entries))
	}
}

func TestUpdateTicketSurfacesUpdateError(t *testing.T) {
	fake := &fakeTicketStore{updateErr: errors.New(`tickets: "t9": not found`)}
	m := &Model{tickets: fake, ctx: context.Background()}
	p, reply := ticketPending(mcp.ToolUpdateTicket, `{"id":"t9","status":"blocked","note":"waiting"}`)
	m.startUpdateTicket(p)
	if got := answer(t, reply); !strings.Contains(got, "not found") {
		t.Errorf("answer = %q, want the store's error surfaced", got)
	}
	if len(fake.commitMsgs) != 0 {
		t.Error("a failed update must not still commit")
	}
}

func TestUpdateTicketSurfacesCommitError(t *testing.T) {
	fake := &fakeTicketStore{commitErr: errors.New("tickets: git commit: exit status 1")}
	m := &Model{tickets: fake, ctx: context.Background()}
	p, reply := ticketPending(mcp.ToolUpdateTicket, `{"id":"t1","status":"todo"}`)
	m.startUpdateTicket(p)
	if got := answer(t, reply); !strings.Contains(got, "committing it failed") {
		t.Errorf("answer = %q, want it to say committing failed (not a push failure)", got)
	}
	if len(m.entries) != 0 {
		t.Error("a non-push commit failure should not append a success transcript entry")
	}
}

// --- CreateTicket ---

func TestCreateTicketRefusedWithoutStore(t *testing.T) {
	m := &Model{}
	p, reply := ticketPending(mcp.ToolCreateTicket, `{"id":"t1","title":"add x","brief":"do the thing"}`)
	m.startCreateTicket(p)
	if got := answer(t, reply); got != mcp.TicketsUnavailable {
		t.Errorf("answer = %q, want mcp.TicketsUnavailable", got)
	}
}

// Strict parsing: a missing required field names itself rather than silently
// no-op'ing, and nothing gets written.
func TestCreateTicketMissingFieldsRefusesWithoutCreating(t *testing.T) {
	fake := &fakeTicketStore{}
	m := &Model{tickets: fake}
	p, reply := ticketPending(mcp.ToolCreateTicket, `{}`)
	m.startCreateTicket(p)

	got := answer(t, reply)
	for _, want := range []string{"id", "title", "brief"} {
		if !strings.Contains(got, want) {
			t.Errorf("answer = %q, want it to name the missing field %q", got, want)
		}
	}
	if len(fake.puts) != 0 {
		t.Error("nothing should have been created with missing fields")
	}
}

func TestCreateTicketDuplicateIDRefusesWithoutCreating(t *testing.T) {
	fake := &fakeTicketStore{list: []tickets.Ticket{{ID: "t1", Title: "existing", Status: tickets.StatusTodo}}}
	m := &Model{tickets: fake, ctx: context.Background()}
	p, reply := ticketPending(mcp.ToolCreateTicket, `{"id":"t1","title":"add x","brief":"do the thing"}`)
	m.startCreateTicket(p)

	got := answer(t, reply)
	if !strings.Contains(got, "already exists") {
		t.Errorf("answer = %q, want it to refuse the duplicate id", got)
	}
	if len(fake.puts) != 0 {
		t.Error("nothing should have been created for a duplicate id")
	}
	if len(fake.commitMsgs) != 0 {
		t.Error("a refused create must not still commit")
	}
}

func TestCreateTicketCreatesAndCommits(t *testing.T) {
	fake := &fakeTicketStore{}
	m := &Model{tickets: fake, ctx: context.Background()}
	p, reply := ticketPending(mcp.ToolCreateTicket,
		`{"id":"t1","title":"add x","brief":"do the thing","depends_on":["t0"]}`)
	m.startCreateTicket(p)

	if len(fake.puts) != 1 {
		t.Fatalf("want 1 ticket put, got %d", len(fake.puts))
	}
	got := fake.puts[0]
	if got.ID != "t1" || got.Title != "add x" || got.Body != "do the thing" || got.Status != tickets.StatusTodo {
		t.Errorf("ticket put = %+v", got)
	}
	if len(got.DependsOn) != 1 || got.DependsOn[0] != "t0" {
		t.Errorf("ticket put DependsOn = %v, want [t0]", got.DependsOn)
	}
	if len(fake.commitMsgs) != 1 || !strings.Contains(fake.commitMsgs[0], "t1") || !strings.Contains(fake.commitMsgs[0], "created") {
		t.Fatalf("commit message = %+v, want it to name the ticket and say created", fake.commitMsgs)
	}
	if reply2 := answer(t, reply); !strings.Contains(reply2, "t1") {
		t.Errorf("answer = %q, missing the ticket id", reply2)
	}
	// The eGood milestone notice, plus the flow diagram it triggers.
	if len(m.entries) != 2 {
		t.Fatalf("want 2 transcript entries, got %d", len(m.entries))
	}
	if m.entries[1].kind != eFlow {
		t.Errorf("second entry kind = %v, want eFlow", m.entries[1].kind)
	}
}

// CreateTicket accepts an optional jira and passes it through to the store's
// Put untouched, the same as depends_on and stack_on.
func TestCreateTicketPassesJira(t *testing.T) {
	fake := &fakeTicketStore{}
	m := &Model{tickets: fake, ctx: context.Background()}
	p, reply := ticketPending(mcp.ToolCreateTicket,
		`{"id":"t1","title":"add x","brief":"do the thing","jira":"ENG-1"}`)
	m.startCreateTicket(p)
	answer(t, reply)

	if len(fake.puts) != 1 || fake.puts[0].Jira != "ENG-1" {
		t.Fatalf("ticket put Jira = %+v, want ENG-1", fake.puts)
	}
}

// A push failure is not a failure of the call: the ticket file and the local
// commit both already landed. The answer must say so, and must tell the
// model to have the human push rather than reporting an error.
func TestCreateTicketPushFailureStillSucceeds(t *testing.T) {
	fake := &fakeTicketStore{commitErr: fmt.Errorf("%w: exit status 1", tickets.ErrPushFailed)}
	m := &Model{tickets: fake, ctx: context.Background()}
	p, reply := ticketPending(mcp.ToolCreateTicket, `{"id":"t1","title":"add x","brief":"do the thing"}`)
	m.startCreateTicket(p)

	got := answer(t, reply)
	for _, want := range []string{"t1", "push failed", "human should push"} {
		if !strings.Contains(got, want) {
			t.Errorf("answer = %q, missing %q", got, want)
		}
	}
	// The eGood milestone notice, plus the flow diagram it triggers.
	if len(m.entries) != 2 {
		t.Fatalf("want 2 transcript entries even though the push failed, got %d", len(m.entries))
	}
}

func TestCreateTicketPassesStackOn(t *testing.T) {
	fake := &fakeTicketStore{}
	m := &Model{tickets: fake, ctx: context.Background()}
	p, reply := ticketPending(mcp.ToolCreateTicket,
		`{"id":"t2","title":"add y","brief":"do the other thing","stack_on":"t1"}`)
	m.startCreateTicket(p)

	if len(fake.puts) != 1 {
		t.Fatalf("want 1 ticket put, got %d", len(fake.puts))
	}
	if got := fake.puts[0].StackOn; got != "t1" {
		t.Errorf("ticket put StackOn = %q, want %q", got, "t1")
	}
	if reply2 := answer(t, reply); !strings.Contains(reply2, "t2") {
		t.Errorf("answer = %q, missing the ticket id", reply2)
	}
}

func TestCreateTicketSurfacesStackOnPutError(t *testing.T) {
	fake := &fakeTicketStore{putErr: errors.New(`tickets: ticket "t2" would create a stack_on cycle`)}
	m := &Model{tickets: fake, ctx: context.Background()}
	p, reply := ticketPending(mcp.ToolCreateTicket,
		`{"id":"t2","title":"add y","brief":"do the other thing","stack_on":"t1"}`)
	m.startCreateTicket(p)
	if got := answer(t, reply); !strings.Contains(got, "stack_on cycle") {
		t.Errorf("answer = %q, want the store's stack_on error surfaced", got)
	}
	if len(fake.commitMsgs) != 0 {
		t.Error("a failed put must not still commit")
	}
}

func TestCreateTicketSurfacesPutError(t *testing.T) {
	fake := &fakeTicketStore{putErr: errors.New(`tickets: invalid id "T1": must match [a-z0-9-]+`)}
	m := &Model{tickets: fake, ctx: context.Background()}
	p, reply := ticketPending(mcp.ToolCreateTicket, `{"id":"T1","title":"add x","brief":"do the thing"}`)
	m.startCreateTicket(p)
	if got := answer(t, reply); !strings.Contains(got, "must match") {
		t.Errorf("answer = %q, want the store's error surfaced", got)
	}
	if len(fake.commitMsgs) != 0 {
		t.Error("a failed put must not still commit")
	}
}

func TestCreateTicketSurfacesCommitError(t *testing.T) {
	fake := &fakeTicketStore{commitErr: errors.New("tickets: git commit: exit status 1")}
	m := &Model{tickets: fake, ctx: context.Background()}
	p, reply := ticketPending(mcp.ToolCreateTicket, `{"id":"t1","title":"add x","brief":"do the thing"}`)
	m.startCreateTicket(p)
	if got := answer(t, reply); !strings.Contains(got, "committing it failed") {
		t.Errorf("answer = %q, want it to say committing failed (not a push failure)", got)
	}
	if len(m.entries) != 0 {
		t.Error("a non-push commit failure should not append a success transcript entry")
	}
}

// --- /tickets ---

func TestTicketsSlashCommandRendersBoard(t *testing.T) {
	fake := &fakeTicketStore{list: []tickets.Ticket{{ID: "t1", Title: "add x", Status: tickets.StatusTodo}}}
	m := New(nil, Config{Tickets: fake})
	before := len(m.entries)

	if cmd := m.runCommand("tickets", ""); cmd != nil {
		t.Error("runCommand(tickets) should not return a tea.Cmd")
	}
	if len(m.entries) != before+1 {
		t.Fatalf("entries = %d, want %d (one board appended)", len(m.entries), before+1)
	}
	got := m.entries[len(m.entries)-1].body
	if !strings.Contains(got, "t1") || !strings.Contains(got, "add x") {
		t.Errorf("tickets report = %q", got)
	}
}

func TestTicketsSlashCommandWithoutStore(t *testing.T) {
	m := New(nil, Config{})
	m.runCommand("tickets", "")
	got := m.entries[len(m.entries)-1].body
	if !strings.Contains(got, "no ticket store") {
		t.Errorf("tickets report = %q, want it to say no store is configured", got)
	}
}

// --- flow diagram ---

// countKind counts how many entries carry the given kind, so a dedupe test
// can compare a before/after count rather than assuming a fixed index.
func countKind(entries []entry, k ekind) int {
	n := 0
	for _, e := range entries {
		if e.kind == k {
			n++
		}
	}
	return n
}

func TestUpdateTicketMilestoneEmitsFlowDiagram(t *testing.T) {
	fake := &fakeTicketStore{list: []tickets.Ticket{{ID: "t1", Title: "add x", Status: tickets.StatusTodo}}}
	m := &Model{tickets: fake, ctx: context.Background()}
	p, reply := ticketPending(mcp.ToolUpdateTicket, `{"id":"t1","status":"in-progress"}`)
	m.startUpdateTicket(p)
	answer(t, reply)

	if got := countKind(m.entries, eFlow); got != 1 {
		t.Fatalf("eFlow entries = %d, want 1", got)
	}
}

// A milestone that leaves the board's shape unchanged — here, a second
// UpdateTicket call to the same status the fake store already reports — must
// not print the same diagram again.
func TestUpdateTicketMilestoneDedupesUnchangedBoard(t *testing.T) {
	fake := &fakeTicketStore{list: []tickets.Ticket{{ID: "t1", Title: "add x", Status: tickets.StatusInProgress}}}
	m := &Model{tickets: fake, ctx: context.Background()}

	p1, reply1 := ticketPending(mcp.ToolUpdateTicket, `{"id":"t1","status":"in-progress"}`)
	m.startUpdateTicket(p1)
	answer(t, reply1)
	if got := countKind(m.entries, eFlow); got != 1 {
		t.Fatalf("eFlow entries after first update = %d, want 1", got)
	}

	p2, reply2 := ticketPending(mcp.ToolUpdateTicket, `{"id":"t1","status":"in-progress"}`)
	m.startUpdateTicket(p2)
	answer(t, reply2)
	if got := countKind(m.entries, eFlow); got != 1 {
		t.Fatalf("eFlow entries after an unchanged-board update = %d, want still 1", got)
	}
}

// --- /flow ---

func TestFlowSlashCommandWithoutStore(t *testing.T) {
	m := New(nil, Config{})
	if cmd := m.runCommand("flow", ""); cmd != nil {
		t.Error("runCommand(flow) should not return a tea.Cmd")
	}
	got := m.entries[len(m.entries)-1].body
	if !strings.Contains(got, "no ticket store") {
		t.Errorf("flow report = %q, want it to say no store is configured", got)
	}
}

func TestFlowSlashCommandRendersDiagram(t *testing.T) {
	fake := &fakeTicketStore{list: []tickets.Ticket{{ID: "t1", Title: "add x", Status: tickets.StatusTodo}}}
	m := New(nil, Config{Tickets: fake})
	before := len(m.entries)

	if cmd := m.runCommand("flow", ""); cmd != nil {
		t.Error("runCommand(flow) should not return a tea.Cmd")
	}
	if len(m.entries) != before+1 {
		t.Fatalf("entries = %d, want %d (one flow diagram appended)", len(m.entries), before+1)
	}
	got := m.entries[len(m.entries)-1]
	if got.kind != eFlow {
		t.Errorf("entry kind = %v, want eFlow", got.kind)
	}
	if got.raw == "" {
		t.Error("raw should carry the mermaid source, not be empty")
	}
	if !strings.Contains(got.body, "```mermaid") {
		t.Errorf("body = %q, want it to contain a fenced mermaid block", got.body)
	}
}

// /flow is an explicit request, unlike the milestone emission — it must
// print every time regardless of whether the board changed in between.
func TestFlowSlashCommandAlwaysPrintsEvenWhenUnchanged(t *testing.T) {
	fake := &fakeTicketStore{list: []tickets.Ticket{{ID: "t1", Title: "add x", Status: tickets.StatusTodo}}}
	m := New(nil, Config{Tickets: fake})

	m.runCommand("flow", "")
	m.runCommand("flow", "")

	if got := countKind(m.entries, eFlow); got != 2 {
		t.Fatalf("eFlow entries after two /flow calls = %d, want 2", got)
	}
}

// --- routing ---

// The ask socket must dispatch all three ticket tools through the real
// askMsg switch, not just through calling the handler directly — mirroring
// TestAskMsgRoutesFleetTools.
func TestAskMsgRoutesTicketTools(t *testing.T) {
	fake := &fakeTicketStore{list: []tickets.Ticket{{ID: "t1", Title: "x", Status: tickets.StatusTodo}}}
	m := New(nil, Config{Tickets: fake})
	m.ctx = context.Background()

	p, reply := ticketPending(mcp.ToolReadTickets, `{}`)
	next, _ := m.Update(askMsg{p})
	m = next.(Model)
	if got := answer(t, reply); !strings.Contains(got, "t1") {
		t.Fatalf("askMsg did not route ReadTickets: %q", got)
	}

	p, reply = ticketPending(mcp.ToolUpdateTicket, `{"id":"t1","status":"in-progress"}`)
	next, _ = m.Update(askMsg{p})
	m = next.(Model)
	answer(t, reply)
	if len(fake.updates) != 1 {
		t.Fatal("askMsg did not route UpdateTicket to startUpdateTicket")
	}

	p, reply = ticketPending(mcp.ToolCreateTicket, `{"id":"t2","title":"y","brief":"do y"}`)
	next, _ = m.Update(askMsg{p})
	m = next.(Model)
	answer(t, reply)
	if len(fake.puts) != 1 {
		t.Fatal("askMsg did not route CreateTicket to startCreateTicket")
	}
}

// The PreToolUse hook matches "*", so without this the ticket tools would
// raise a countdown that ticks invisibly behind the ask-socket answer acy
// already gave — mirrors TestFleetToolsAreIntercepted.
func TestTicketToolsAreIntercepted(t *testing.T) {
	for _, tool := range []string{mcp.ToolReadTickets, mcp.ToolUpdateTicket, mcp.ToolCreateTicket} {
		t.Run(tool, func(t *testing.T) {
			m := New(nil, Config{Countdown: 30 * time.Second})
			m.now = time.Now()

			p, decisions := pendingFrom(mcp.Qualified(tool), "parent-sess")
			m.enqueue(p)

			select {
			case d := <-decisions:
				if d.Behavior != gate.Allow {
					t.Errorf("behavior = %v, want allow", d.Behavior)
				}
			default:
				t.Fatalf("%s raised a countdown; acy answers it itself over the ask socket", tool)
			}
			if len(m.pending) != 0 {
				t.Errorf("%d gates queued for %s, want 0", len(m.pending), tool)
			}
		})
	}
}

// --- Frame ---

func TestFrameCarriesTickets(t *testing.T) {
	fake := &fakeTicketStore{list: []tickets.Ticket{
		{ID: "t1", Title: "add x", Status: tickets.StatusInReview, PR: "https://example.com/pr/1"},
	}}
	m := &Model{tickets: fake}
	m.refreshTicketCache()

	fr := m.Frame()
	if len(fr.Tickets) != 1 {
		t.Fatalf("want 1 ticket in the frame, got %d", len(fr.Tickets))
	}
	tk := fr.Tickets[0]
	if tk.ID != "t1" || tk.Title != "add x" || tk.Status != tickets.StatusInReview || tk.PRURL != "https://example.com/pr/1" {
		t.Errorf("frame ticket = %+v", tk)
	}
}

func TestFrameTicketsEmptyWithoutStore(t *testing.T) {
	m := &Model{}
	fr := m.Frame()
	if fr.Tickets == nil {
		t.Error("Tickets must be [], never null, so a client never has to handle the two differently")
	}
	if len(fr.Tickets) != 0 {
		t.Errorf("want no tickets with no store wired, got %d", len(fr.Tickets))
	}
}

// The board redrawn as mermaid and ascii is a separate projection from the
// ticket list, kept current the same way — a read of the store, not a mirror.
func TestFrameCarriesFlow(t *testing.T) {
	fake := &fakeTicketStore{list: []tickets.Ticket{
		{ID: "t1", Title: "add x", Status: tickets.StatusInReview},
	}}
	m := &Model{tickets: fake}
	m.refreshTicketCache()

	fl := m.Frame().Flow
	if fl.Mermaid == "" || !strings.Contains(fl.Mermaid, "t1") {
		t.Errorf("flow.mermaid = %q, want the board's mermaid source", fl.Mermaid)
	}
	if fl.ASCII == "" || !strings.Contains(fl.ASCII, "t1") {
		t.Errorf("flow.ascii = %q, want the board's ascii lanes", fl.ASCII)
	}
}

// With no ticket store wired, flow is the zero value — both fields empty —
// mirroring how Tickets projects as no tickets.
func TestFrameFlowEmptyWithoutStore(t *testing.T) {
	m := &Model{}
	fl := m.Frame().Flow
	if fl.Mermaid != "" || fl.ASCII != "" {
		t.Errorf("flow = %+v, want the zero value with no store wired", fl)
	}
}

// internal/hub relies on an idle run's frame marshalling to identical bytes
// twice in a row, or it would never fall silent. Flow projects the cached
// mirror refreshTicketCache keeps rather than reading the board on every
// call, so this pins that two calls against an unchanged cache produce
// byte-identical JSON, not just equal-looking structs.
func TestFrameFlowIsByteStableAcrossRepeatedCalls(t *testing.T) {
	fake := &fakeTicketStore{list: []tickets.Ticket{
		{ID: "t1", Title: "add x", Status: tickets.StatusInProgress},
		{ID: "t2", Title: "add y", Status: tickets.StatusTodo, DependsOn: []string{"t1"}},
	}}
	m := New(nil, Config{Tickets: fake})
	m.now = frameTime

	first := mustMarshal(t, m.Frame())
	second := mustMarshal(t, m.Frame())
	if first != second {
		t.Fatalf("two consecutive Frame() calls on an unchanged board diverged:\n1: %s\n2: %s", first, second)
	}
}

// countingTicketStore wraps fakeTicketStore to count List() calls, so a test
// can assert Frame() makes none of its own rather than just inferring it from
// output shape.
type countingTicketStore struct {
	*fakeTicketStore
	listCalls int
}

func (c *countingTicketStore) List() ([]tickets.Ticket, error) {
	c.listCalls++
	return c.fakeTicketStore.List()
}

// Frame() must not touch the ticket store at all: it used to call List()
// twice a tick (once each for Tickets and Flow), so an idle run with a
// webview subscriber attached re-read and re-parsed every ticket file on
// disk twice every 120ms. The board only changes when CreateTicket or
// UpdateTicket changes it — see refreshTicketCache — so Frame projects the
// cached mirror instead. New()'s one-time seed read is allowed; nothing
// after it is.
func TestFrameDoesNotReadTicketStoreOnIdleTicks(t *testing.T) {
	counting := &countingTicketStore{fakeTicketStore: &fakeTicketStore{list: []tickets.Ticket{
		{ID: "t1", Title: "add x", Status: tickets.StatusInProgress},
	}}}
	m := New(nil, Config{Tickets: counting})
	m.now = frameTime
	seeded := counting.listCalls
	if seeded == 0 {
		t.Fatal("New() should have seeded the cache with one read")
	}

	for i := 1; i <= 5; i++ {
		m.now = frameTime.Add(time.Duration(i) * tickInterval)
		_ = m.Frame()
		_ = m.Frame().Tickets
		_ = m.Frame().Flow
	}

	if counting.listCalls != seeded {
		t.Fatalf("ticket store List() called %d times across 5 idle ticks (each rendering Frame, Tickets and Flow), want still %d — Frame must not read the store", counting.listCalls, seeded)
	}
}
