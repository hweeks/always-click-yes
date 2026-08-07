package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/fleet"
	"github.com/hweeks/always-click-yes/internal/gitops"
	"github.com/hweeks/always-click-yes/internal/mcp"
)

// --- AssembleStack ---
//
// LaunchEngineer/stack_on builds a stack forward, one launch at a time, by
// hand-declaring a parent at launch. AssembleStack is the reverse: several
// tickets are launched wide, independently, against trunk, and once each has
// finished with its own open PR, this tool folds their branches into one
// stack after the fact — the breadth-then-depth loop ArchSystemPromptFor
// describes. See mcp.assembleStackDescription for the load-bearing
// constraint this cannot check for itself: the tickets must have been
// genuinely code-independent, because folding PRs together cannot make a
// verification real that wasn't real when each engineer's own PR opened.

type assembleStackArgs struct {
	Tickets []string `json:"tickets"`
}

// parseAssembleStack decodes an AssembleStack call, strictly — mirroring
// parseLaunchEngineer: a missing or empty ticket id fails with a
// self-describing message rather than assembling a partial stack.
func parseAssembleStack(raw json.RawMessage) (assembleStackArgs, error) {
	var a assembleStackArgs
	if len(raw) == 0 {
		return a, errors.New("no arguments were given")
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("the arguments were not valid JSON: %w", err)
	}
	if len(a.Tickets) == 0 {
		return a, errors.New("missing required field: tickets")
	}
	for i, t := range a.Tickets {
		t = strings.TrimSpace(t)
		if t == "" {
			return a, fmt.Errorf("tickets[%d] is empty", i)
		}
		a.Tickets[i] = t
	}
	return a, nil
}

// findLatestByTicket returns the most recently launched status for ticket, or
// ok=false. Mirrors fleet.Manager.findByTicketLocked's "most recent wins"
// rule — that method is private to package fleet, so this is a deliberate,
// small duplication of its search rather than an import.
func findLatestByTicket(statuses []fleet.EngineerStatus, ticket string) (fleet.EngineerStatus, bool) {
	for i := len(statuses) - 1; i >= 0; i-- {
		if statuses[i].Ticket == ticket {
			return statuses[i], true
		}
	}
	return fleet.EngineerStatus{}, false
}

// resolveAssembleTicket checks one ticket id against the fleet's ledger,
// returning the specific refusal AssembleStack should give the architect —
// in the fixed order the ticket specifies — or ok=true with the status to
// use.
func resolveAssembleTicket(statuses []fleet.EngineerStatus, ticket string) (fleet.EngineerStatus, string) {
	st, found := findLatestByTicket(statuses, ticket)
	switch {
	case !found:
		return st, fmt.Sprintf("no engineer has been launched for ticket %q", ticket)
	case st.State != fleet.StateDone:
		return st, fmt.Sprintf("ticket %q has not finished yet (state %s)", ticket, st.State)
	case st.Branch == "":
		// Defensive only: a done engineer always has a branch recorded by the
		// time it reaches this state, since Launch assigns one before the
		// engineer ever starts.
		return st, fmt.Sprintf("ticket %q has no branch recorded", ticket)
	case st.PRURL == "":
		return st, fmt.Sprintf("ticket %q finished with no open PR — a branch that was never pushed cannot be assembled", ticket)
	case st.StackID != "":
		return st, fmt.Sprintf("ticket %q is already part of stack %s — assembly builds a new chain, it does not restructure an existing one", ticket, st.StackID)
	}
	return st, ""
}

// looksLikeLeaseRejection reports whether err is a `gh stack push` rejection
// caused by a per-branch --force-with-lease mismatch, rather than some other
// push failure. gh-stack pushes every branch with --force-with-lease, so a
// rejection means a branch has commits this run never observed — someone
// (or some other process) touched an acy/* branch after this run last saw
// it. That is a human problem: retrying blind would force-push over
// whatever they just added.
func looksLikeLeaseRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "stale info") || strings.Contains(msg, "force-with-lease")
}

// startAssembleStack answers an AssembleStack call from the architect.
//
// Every refusal below resolves p immediately and runs no gh/git command —
// only once every ticket has passed every check does this touch a worktree
// or the network.
func (m *Model) startAssembleStack(p *mcp.Pending) {
	switch {
	case m.fleet == nil:
		p.Resolve(mcp.Answer{Text: mcp.FleetUnavailable})
		return
	case m.phase != PhaseAutoRun:
		p.Resolve(mcp.Answer{Text: mcp.AssembleStackNotArmed})
		alog.Printf("assemble: refused — the run is not armed")
		m.appendEntry(entry{kind: eMeta, body: "↯ assemble declined — press Ctrl+G to arm the run"})
		return
	}

	args, err := parseAssembleStack(p.Req.Args)
	if err != nil {
		p.Resolve(mcp.Answer{Text: "AssembleStack could not be read: " + err.Error() +
			". Nothing was assembled. Fix the arguments and call it again."})
		return
	}

	if m.stackMode == "off" {
		p.Resolve(mcp.Answer{Text: fmt.Sprintf(
			"AssembleStack did not run: stacking is disabled for this fleet (fleet.stackMode is %q) — nothing was assembled.",
			m.stackMode)})
		return
	}
	if len(args.Tickets) < 2 {
		p.Resolve(mcp.Answer{Text: fmt.Sprintf(
			"AssembleStack requires at least 2 tickets, got %d. Nothing was assembled.", len(args.Tickets))})
		return
	}
	seen := make(map[string]bool, len(args.Tickets))
	for _, t := range args.Tickets {
		if seen[t] {
			p.Resolve(mcp.Answer{Text: fmt.Sprintf(
				"AssembleStack did not run: ticket %q appears more than once. Nothing was assembled.", t)})
			return
		}
		seen[t] = true
	}

	m.syncFleet()
	branches := make([]string, 0, len(args.Tickets))
	var topPRURL string
	for _, ticket := range args.Tickets {
		st, refusal := resolveAssembleTicket(m.engineers, ticket)
		if refusal != "" {
			p.Resolve(mcp.Answer{Text: "AssembleStack did not run: " + refusal + ". Nothing was assembled."})
			return
		}
		branches = append(branches, st.Branch)
		topPRURL = st.PRURL
	}

	trunk := m.trunk
	if trunk == "" {
		// Defensive only: `acy arch` always sets Config.Trunk from the
		// fleet's resolved BaseBranch.
		trunk = "main"
	}

	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	dir, cleanup, err := assembleWorktree(ctx, m.gitRunner, m.cwd, trunk)
	if err != nil {
		p.Resolve(mcp.Answer{Text: "AssembleStack failed: " + err.Error()})
		return
	}
	defer cleanup()

	n, err := gitops.StackAssemble(ctx, m.gitRunner, dir, trunk, branches)
	if err != nil {
		switch {
		case errors.Is(err, gitops.ErrStackConflict):
			p.Resolve(mcp.Answer{Text: err.Error() + "\n\nThis is a rebase conflict — stop and escalate to a " +
				"human to resolve it by hand. Do not retry automatically."})
		case looksLikeLeaseRejection(err):
			p.Resolve(mcp.Answer{Text: fmt.Sprintf(
				"AssembleStack failed: %s\n\nThis looks like a force-with-lease rejection: someone has local "+
					"commits on one of these branches that this run never saw. Escalate to a human rather than retrying.",
				err.Error())})
		default:
			p.Resolve(mcp.Answer{Text: "AssembleStack failed: " + err.Error()})
		}
		return
	}

	p.Resolve(mcp.Answer{Text: fmt.Sprintf(
		"assembled stack #%d from %d tickets (%s) — top PR: %s",
		n, len(args.Tickets), strings.Join(branches, " -> "), topPRURL)})
	m.appendEntry(entry{kind: eTool, title: fmt.Sprintf("assemble stack #%d", n),
		body: fmt.Sprintf("tickets %s · branches %s · top PR %s",
			strings.Join(args.Tickets, ", "), strings.Join(branches, " -> "), topPRURL)})
	// Mirrors startLaunchEngineer's crash-safety precedent: a crash between
	// here and the next persist would lose this transcript entry. There is
	// nothing else new to capture in the snapshot from this action alone —
	// the fleet's own ledger (StackID/StackBase per engineer) is unchanged,
	// since assembly is a GitHub-side operation this process does not track
	// per engineer — but persisting here keeps that guarantee uniform across
	// every fleet tool that mutates something real.
	m.persist()
}

// assembleWorktree creates a disposable worktree for AssembleStack's gh stack
// commands, detached at trunk's tip, and returns a cleanup that removes it.
//
// acy arch runs in the human's own checkout. gh stack init/rebase/push/link
// check branches out as they go, and doing that in the operator's working
// tree while they might be mid-edit is unacceptable — so every command that
// needs a checkout runs here instead, never with dir == clonePath.
//
// This duplicates a small piece of what a concurrent ticket's
// fleet.StackKeeper (a dedicated worktree owner, not yet on main) will also
// need. Unify the two once both have landed; for now this tool must not
// depend on code that isn't merged.
func assembleWorktree(ctx context.Context, run gitops.Runner, clonePath, trunk string) (dir string, cleanup func(), err error) {
	dir, err = os.MkdirTemp("", "acy-assemble-")
	if err != nil {
		return "", nil, fmt.Errorf("assemble: creating scratch worktree dir: %w", err)
	}
	startPoint := trunk
	if _, ferr := run(ctx, clonePath, "git", "fetch", "origin", trunk); ferr == nil {
		startPoint = "origin/" + trunk
	}
	if _, err = run(ctx, clonePath, "git", "worktree", "add", "--detach", dir, startPoint); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("assemble: git worktree add: %w", err)
	}
	cleanup = func() {
		if _, rmErr := run(ctx, clonePath, "git", "worktree", "remove", "--force", dir); rmErr != nil {
			alog.Printf("assemble: git worktree remove failed: %v", rmErr)
		}
		if _, pruneErr := run(ctx, clonePath, "git", "worktree", "prune"); pruneErr != nil {
			alog.Printf("assemble: git worktree prune failed: %v", pruneErr)
		}
		_ = os.RemoveAll(dir)
	}
	return dir, cleanup, nil
}
