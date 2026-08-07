package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
	"github.com/hweeks/always-click-yes/internal/fleet"
	"github.com/hweeks/always-click-yes/internal/mcp"
	"github.com/hweeks/always-click-yes/internal/state"
)

// The architect's fleet, from the UI's side.
//
// Dispatch hands one task to a local child and blocks the parent's turn on the
// ask socket until it reports. The fleet tools are the same idea stretched
// across machines: LaunchEngineer hands a whole ticket to a remote engineer
// and returns immediately (the engineer takes unbounded wall-clock), and Await
// is what blocks the architect's turn instead — one call per wait, react,
// repeat, rather than one call per task. Both still arrive as a *mcp.Pending
// on the same ask socket; only the tool name tells them apart.

// FleetManager is the subset of *fleet.Manager the model uses. Named here
// rather than imported wholesale so ui tests can supply a fake, matching how
// Dispatcher already does for the orchestrator.
//
// Held as an interface value — that is, a pointer — for the same reason
// Dispatcher is: Bubble Tea copies the Model by value on every Update, and
// copying a struct containing a mutex is the strings.Builder crash's cousin.
type FleetManager interface {
	Launch(ctx context.Context, req fleet.LaunchReq) (fleet.EngineerStatus, error)
	Events() <-chan fleet.Event
	Answer(engineerID, questionID, text string) error
	Statuses() []fleet.EngineerStatus
	Active() int
	Capacity() (used, total int)
	CancelAll(reason string)

	// Ledger and Resume are the resume seam: Ledger is what snapshot() persists
	// (mirroring Dispatcher's own Ledger), and Resume is what applyResume calls
	// to hand a restored ledger back so a still-running engineer re-attaches.
	Ledger() []state.Engineer
	Resume(ctx context.Context, entries []state.Engineer) error
}

type fleetMsg struct{ ev fleet.Event }

// fleetBufferCap bounds how many events ingestFleet holds while no Await is
// pending — see bufferFleetEvent.
const fleetBufferCap = 64

// waitFleet blocks on the next event from the fleet's unified stream. Created
// once and closed only at shutdown, so — like waitChild — this needs no
// generation counter: every event already names the engineer it belongs to.
func waitFleet(ch <-chan fleet.Event) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return fleetMsg{ev: ev}
	}
}

// --- LaunchEngineer ---

type launchEngineerArgs struct {
	Ticket    string  `json:"ticket"`
	Title     string  `json:"title"`
	Brief     string  `json:"brief"`
	Success   string  `json:"success"`
	Host      string  `json:"host"`
	BudgetUSD float64 `json:"budget_usd"`
}

// parseLaunchEngineer decodes a LaunchEngineer call, strictly: a missing
// required field fails with a self-describing message rather than launching
// an engineer against half a brief.
func parseLaunchEngineer(raw json.RawMessage) (launchEngineerArgs, error) {
	var a launchEngineerArgs
	if len(raw) == 0 {
		return a, errors.New("no arguments were given")
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("the arguments were not valid JSON: %w", err)
	}
	a.Ticket = strings.TrimSpace(a.Ticket)
	a.Title = strings.TrimSpace(a.Title)
	a.Brief = strings.TrimSpace(a.Brief)
	a.Success = strings.TrimSpace(a.Success)
	a.Host = strings.TrimSpace(a.Host)

	var missing []string
	if a.Ticket == "" {
		missing = append(missing, "ticket")
	}
	if a.Title == "" {
		missing = append(missing, "title")
	}
	if a.Brief == "" {
		missing = append(missing, "brief")
	}
	if a.Success == "" {
		missing = append(missing, "success")
	}
	if len(missing) > 0 {
		return a, fmt.Errorf("missing required field(s): %s", strings.Join(missing, ", "))
	}
	return a, nil
}

// startLaunchEngineer answers a LaunchEngineer call from the architect.
//
// Refusing here, at the moment of the call, is what lets the plan-phase
// system prompt stay silent about arming — the model finds out by trying,
// once — exactly the reasoning startDispatch already relies on.
func (m *Model) startLaunchEngineer(p *mcp.Pending) {
	switch {
	case m.fleet == nil:
		p.Resolve(mcp.Answer{Text: mcp.FleetUnavailable})
		m.appendEntry(entry{kind: eWarn, body: "an engineer launch was requested, but no fleet is wired in this session"})
		return
	case m.phase != PhaseAutoRun:
		p.Resolve(mcp.Answer{Text: mcp.LaunchNotArmed})
		alog.Printf("fleet: launch refused — the run is not armed")
		m.appendEntry(entry{kind: eMeta, body: "↯ launch declined — press Ctrl+G to arm the run"})
		return
	}

	args, err := parseLaunchEngineer(p.Req.Args)
	if err != nil {
		p.Resolve(mcp.Answer{Text: "LaunchEngineer could not be read: " + err.Error() +
			". Nothing was launched. Fix the arguments and call it again."})
		return
	}

	st, launchErr := m.fleet.Launch(m.ctx, fleet.LaunchReq{
		Ticket: args.Ticket, Title: args.Title, Brief: args.Brief, Success: args.Success,
		Host: args.Host, BudgetUSD: args.BudgetUSD,
	})
	m.syncFleet()
	used, total := m.fleetCapUsed, m.fleetCapTotal

	// A full fleet (or an unknown/full pinned host) is a normal answer, not a
	// failure: the architect just tried to launch beyond capacity, and the
	// per-host usage below is what tells it to Await instead of retrying blind.
	if launchErr != nil {
		p.Resolve(mcp.Answer{Text: fmt.Sprintf(
			"LaunchEngineer did not start: %s (fleet capacity %d/%d in use). Await for a slot to free up, or pick a different host.",
			launchErr.Error(), used, total)})
		m.appendEntry(entry{kind: eWarn, body: "✗ launch declined: " + launchErr.Error()})
		return
	}

	p.Resolve(mcp.Answer{Text: fmt.Sprintf(
		"launched %s on host %s, branch %s (fleet capacity %d/%d in use)",
		st.EngineerID, st.Host, st.Branch, used, total)})
	m.appendEntry(entry{kind: eTool, title: "launch " + st.EngineerID, body: launchBody(st)})
	// Without this, a crash between here and the next turn-end/arm persist
	// (dispatch.go's startDispatch does the local-child equivalent) would
	// resume with an empty engineer ledger — nothing for Manager.Resume to
	// re-attach, and the engineer's own progress and eventual Result would be
	// silently orphaned.
	m.persist()
}

func launchBody(st fleet.EngineerStatus) string {
	return fmt.Sprintf("ticket %s · host %s · branch %s", st.Ticket, st.Host, st.Branch)
}

// --- Await ---

// startAwait answers an Await call: the oldest buffered event if one is
// already waiting, an immediate refusal if there is nothing that could ever
// produce one, or — the common case — it holds p until the next fleet event
// resolves it from ingestFleet.
func (m *Model) startAwait(p *mcp.Pending) {
	switch {
	case m.fleet == nil:
		p.Resolve(mcp.Answer{Text: mcp.FleetUnavailable})
		return
	case m.phase != PhaseAutoRun:
		p.Resolve(mcp.Answer{Text: mcp.LaunchNotArmed})
		return
	}

	if len(m.fleetBuf) > 0 {
		ev := m.fleetBuf[0]
		m.fleetBuf = m.fleetBuf[1:]
		p.Resolve(mcp.Answer{Text: fleetEventText(ev)})
		return
	}
	if m.fleet.Active() == 0 {
		p.Resolve(mcp.Answer{Text: mcp.AwaitNothingRunning})
		return
	}
	// Held, like a gate holds — resolved from ingestFleet when the next event
	// lands, or abandoned if this session ends first (see abandonFleetAwait).
	m.fleetAwait = p
}

// --- AnswerEngineer ---

type answerEngineerArgs struct {
	EngineerID string `json:"engineer_id"`
	QuestionID string `json:"question_id"`
	Answer     string `json:"answer"`
}

func parseAnswerEngineer(raw json.RawMessage) (answerEngineerArgs, error) {
	var a answerEngineerArgs
	if len(raw) == 0 {
		return a, errors.New("no arguments were given")
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("the arguments were not valid JSON: %w", err)
	}
	a.EngineerID = strings.TrimSpace(a.EngineerID)
	a.QuestionID = strings.TrimSpace(a.QuestionID)
	a.Answer = strings.TrimSpace(a.Answer)

	var missing []string
	if a.EngineerID == "" {
		missing = append(missing, "engineer_id")
	}
	if a.QuestionID == "" {
		missing = append(missing, "question_id")
	}
	if a.Answer == "" {
		missing = append(missing, "answer")
	}
	if len(missing) > 0 {
		return a, fmt.Errorf("missing required field(s): %s", strings.Join(missing, ", "))
	}
	return a, nil
}

func (m *Model) startAnswerEngineer(p *mcp.Pending) {
	if m.fleet == nil {
		p.Resolve(mcp.Answer{Text: mcp.FleetUnavailable})
		return
	}
	args, err := parseAnswerEngineer(p.Req.Args)
	if err != nil {
		p.Resolve(mcp.Answer{Text: "AnswerEngineer could not be read: " + err.Error()})
		return
	}
	if err := m.fleet.Answer(args.EngineerID, args.QuestionID, args.Answer); err != nil {
		p.Resolve(mcp.Answer{Text: "AnswerEngineer failed: " + err.Error()})
		return
	}
	p.Resolve(mcp.Answer{Text: "answer delivered to " + args.EngineerID})
	m.appendEntry(entry{kind: eYou, body: fmt.Sprintf("↳ answered %s's question %s:\n%s", args.EngineerID, args.QuestionID, args.Answer)})
}

// --- FleetStatus ---

func (m *Model) startFleetStatus(p *mcp.Pending) {
	if m.fleet == nil {
		p.Resolve(mcp.Answer{Text: mcp.FleetUnavailable})
		return
	}
	m.syncFleet()
	p.Resolve(mcp.Answer{Text: fleetStatusTable(m.engineers, m.fleetCapUsed, m.fleetCapTotal)})
}

// --- ingest: the fleet's unified event stream ---

// ingestFleet folds one fleet event into the transcript, the status mirror,
// and — the reason Await exists — either resolves a held Await immediately or
// buffers the event for the next one.
func (m *Model) ingestFleet(ev fleet.Event) {
	m.syncFleet()
	m.appendEntry(fleetEntry(ev))

	// Terminal events, mirroring ingestChild's KindFinished/KindFailed: an
	// engineer's Result (or a Cancel/failure) is exactly the state a crash
	// must not lose, since Manager.Ledger()'s State/Outcome/PRURL is what a
	// resumed run reads to know the engineer is already done rather than
	// re-attaching it for nothing.
	if ev.Kind == fleet.KindResult || ev.Kind == fleet.KindFailed {
		m.persist()
	}

	if m.fleetAwait != nil {
		p := m.fleetAwait
		m.fleetAwait = nil
		p.Resolve(mcp.Answer{Text: fleetEventText(ev)})
		return
	}
	m.fleetBuf = bufferFleetEvent(m.fleetBuf, ev)
}

// bufferFleetEvent appends ev, evicting the oldest KindProgress event first if
// the buffer is already at capacity. A question, a result, or a pr event is
// never evicted to make room — mirroring the tradeoff fleet.Manager.emit
// already makes on its own channel: a lost progress line costs a transcript
// entry, a lost question, result, or PR merge/close corrupts the architect's
// view of the fleet.
func bufferFleetEvent(buf []fleet.Event, ev fleet.Event) []fleet.Event {
	if len(buf) >= fleetBufferCap {
		for i, b := range buf {
			if b.Kind == fleet.KindProgress {
				buf = append(buf[:i], buf[i+1:]...)
				break
			}
		}
	}
	return append(buf, ev)
}

// syncFleet pulls the fleet's own ledger into the model, mirroring
// syncChildTotals: Frame and /fleet read this mirror rather than calling back
// into the manager themselves.
func (m *Model) syncFleet() {
	if m.fleet == nil {
		return
	}
	m.engineers = m.fleet.Statuses()
	m.fleetActive = m.fleet.Active()
	m.fleetCapUsed, m.fleetCapTotal = m.fleet.Capacity()
}

// abandonFleetAwait answers a held Await when the session it belongs to is
// going away (stream closed, or the driver swapped out on arming/resume),
// mirroring abandonAsk. There is no local resource to release — the
// engineers themselves are remote and keep running regardless — so this only
// unblocks the architect's own turn rather than tearing anything down.
func (m *Model) abandonFleetAwait() {
	if m.fleetAwait == nil {
		return
	}
	m.fleetAwait.Resolve(mcp.Answer{Text: mcp.SupervisorGone})
	m.fleetAwait = nil
}

// cancelFleet stops every engineer still running.
//
// Deliberately NOT wired to Esc/interject the way cancelDispatches is: a
// local dispatch is a child process this session owns outright, but an
// engineer is durable remote work in its own worktree that the human may
// still want running after interrupting the architect's own turn to redirect
// it. Only an explicit /quit reaches here; fleet.Manager.Close (called by
// whoever owns the manager, on process exit) is the other place cancellation
// happens, which is why quitting still leaves nothing orphaned.
func (m *Model) cancelFleet(reason string) {
	if m.fleet == nil || m.fleet.Active() == 0 {
		return
	}
	m.appendEntry(entry{kind: eWarn, body: "cancelling running engineers — " + reason})
	m.fleet.CancelAll(reason)
}

// --- rendering ---

// fleetEntry renders one fleet event into the transcript. Progress bodies go
// through eTool so clampLines caps them at maxLines like any other tool
// output — an engineer can narrate for a long time, and a terminal has a
// fixed viewport.
func fleetEntry(ev fleet.Event) entry {
	switch ev.Kind {
	case fleet.KindStarted:
		return entry{kind: eMeta, body: fmt.Sprintf("▶ %s launched · %s · host %s · branch %s",
			ev.EngineerID, ev.Ticket, ev.Host, ev.Status.Branch)}

	case fleet.KindProgress:
		text := ""
		if ev.Progress != nil {
			text = ev.Progress.Text
		}
		return entry{kind: eTool, title: "engineer " + ev.EngineerID, body: text}

	case fleet.KindQuestion:
		qid, text := "", ""
		if ev.Question != nil {
			qid, text = ev.Question.QuestionID, questionsText(ev.Question.Questions)
		}
		return entry{kind: eWarn, body: fmt.Sprintf("❓ engineer %s asks (question_id %s): %s", ev.EngineerID, qid, text)}

	case fleet.KindResult:
		kind := eGood
		body := fmt.Sprintf("■ %s finished", ev.EngineerID)
		if ev.Result != nil {
			if ev.Result.Outcome != "completed" {
				kind = eWarn
			}
			body += " — " + ev.Result.Outcome + "\n" + ev.Result.Summary
			if ev.Result.PRURL != "" {
				body += "\nPR: " + ev.Result.PRURL
			}
			body += fmt.Sprintf("\n$%.4f", ev.Result.CostUSD)
			if len(ev.Result.Verification) > 0 {
				vs := summarizeVerification(ev.Result.Verification)
				body += "\nverification: " + vs.countLine
				if vs.anyFailed() {
					lines := make([]string, 0, len(vs.failed)+1)
					for _, c := range vs.failed {
						lines = append(lines, fmt.Sprintf("  FAILED: %s (exit %d)", c.Name, c.ExitCode))
					}
					if vs.moreFailed > 0 {
						lines = append(lines, fmt.Sprintf("  ...and %d more", vs.moreFailed))
					}
					lines[len(lines)-1] += " (see journal for output)"
					body += "\n" + strings.Join(lines, "\n")
					// Checked directly, not inferred from Outcome: finalize
					// (internal/engineer) already flips Outcome to "failed"
					// when a check fails, but that lives in a different
					// package — a later change there must not silently make
					// a red verification result render green here.
					kind = eWarn
				}
			}
		}
		return entry{kind: kind, body: body}

	case fleet.KindReconnected:
		return entry{kind: eMeta, body: fmt.Sprintf("↺ %s reconnected · gap %ds · attempt %d", ev.EngineerID, ev.Gap, ev.Attempt)}

	case fleet.KindFailed:
		body := fmt.Sprintf("✗ %s failed", ev.EngineerID)
		if ev.Err != nil {
			body += ": " + ev.Err.Error()
		}
		return entry{kind: eToolErr, body: body}

	case fleet.KindPR:
		if ev.PR == nil {
			return entry{kind: eMeta, body: "PR event: unrecognized"}
		}
		return entry{kind: eMeta, body: fmt.Sprintf("PR %s: %s (%s)", prVerb(ev.PR.State), ev.PR.URL, ev.PR.Head)}

	case fleet.KindStack:
		if ev.Stack == nil {
			return entry{kind: eMeta, body: "stack event: unrecognized"}
		}
		if ev.Stack.Err != nil {
			if ev.Stack.Branch != "" {
				return entry{kind: eWarn, body: fmt.Sprintf(
					"stack %s conflict on %s — needs a human, will not auto-resolve: %v",
					ev.Stack.Op, ev.Stack.Branch, ev.Stack.Err)}
			}
			return entry{kind: eToolErr, body: fmt.Sprintf("stack %s failed: %v", ev.Stack.Op, ev.Stack.Err)}
		}
		if ev.Stack.Op == "link" {
			return entry{kind: eMeta, body: "stack linked: " + strings.Join(ev.Stack.Branches, " -> ")}
		}
		return entry{kind: eMeta, body: "stack synced against trunk"}
	}
	return entry{kind: eMeta, body: fmt.Sprintf("engineer %s: unrecognized event", ev.EngineerID)}
}

// prVerb turns a PREvent's raw gh state into the past-tense verb the
// transcript and Await's text both read naturally with.
func prVerb(state string) string {
	switch state {
	case "open":
		return "opened"
	case "merged":
		return "merged"
	case "closed":
		return "closed"
	default:
		return state
	}
}

// fleetEventText is what Await (or the buffer it drains) hands back to the
// architect. Unlike fleetEntry, which is prose for a human watching, this has
// to carry every id the model needs to act — engineer_id to launch another or
// Answer, question_id to answer this one, the PR url to reference in Finish.
func fleetEventText(ev fleet.Event) string {
	switch ev.Kind {
	case fleet.KindStarted:
		return fmt.Sprintf("engineer_id %s started on ticket %s (host %s, branch %s)",
			ev.EngineerID, ev.Ticket, ev.Host, ev.Status.Branch)

	case fleet.KindProgress:
		if ev.Progress == nil {
			return fmt.Sprintf("engineer_id %s: progress", ev.EngineerID)
		}
		return fmt.Sprintf("engineer_id %s (%s): %s", ev.EngineerID, ev.Progress.Kind, ev.Progress.Text)

	case fleet.KindQuestion:
		if ev.Question == nil {
			return fmt.Sprintf("engineer_id %s asked a question that could not be read", ev.EngineerID)
		}
		return fmt.Sprintf("engineer_id %s asks (question_id %s): %s — answer with AnswerEngineer",
			ev.EngineerID, ev.Question.QuestionID, questionsText(ev.Question.Questions))

	case fleet.KindResult:
		if ev.Result == nil {
			return fmt.Sprintf("engineer_id %s finished with no result", ev.EngineerID)
		}
		text := fmt.Sprintf("engineer_id %s %s — %s", ev.EngineerID, ev.Result.Outcome, ev.Result.Summary)
		if ev.Result.PRURL != "" {
			text += "\npr_url " + ev.Result.PRURL
		}
		text += fmt.Sprintf("\ncost_usd %.4f", ev.Result.CostUSD)
		if len(ev.Result.Verification) > 0 {
			vs := summarizeVerification(ev.Result.Verification)
			text += "\nverification " + vs.countLine
			if vs.anyFailed() {
				lines := make([]string, 0, len(vs.failed)+1)
				for _, c := range vs.failed {
					lines = append(lines, fmt.Sprintf("verification FAILED: %s (exit %d)", c.Name, c.ExitCode))
				}
				if vs.moreFailed > 0 {
					lines = append(lines, fmt.Sprintf("verification ...and %d more", vs.moreFailed))
				}
				lines[len(lines)-1] += " (see journal for output)"
				text += "\n" + strings.Join(lines, "\n")
			}
		}
		return text

	case fleet.KindReconnected:
		return fmt.Sprintf("engineer_id %s reconnected after a %ds gap (attempt %d)", ev.EngineerID, ev.Gap, ev.Attempt)

	case fleet.KindFailed:
		text := fmt.Sprintf("engineer_id %s failed", ev.EngineerID)
		if ev.Err != nil {
			text += ": " + ev.Err.Error()
		}
		return text

	case fleet.KindPR:
		if ev.PR == nil {
			return "a PR event could not be read"
		}
		return fmt.Sprintf("pr %s: %s (head %s)", prVerb(ev.PR.State), ev.PR.URL, ev.PR.Head)

	case fleet.KindStack:
		if ev.Stack == nil {
			return "a stack event could not be read"
		}
		if ev.Stack.Err != nil {
			if ev.Stack.Branch != "" {
				return fmt.Sprintf(
					"stack %s conflict on branch %s — this needs a human to rebase by hand; do not retry automatically",
					ev.Stack.Op, ev.Stack.Branch)
			}
			return fmt.Sprintf("stack %s failed: %v", ev.Stack.Op, ev.Stack.Err)
		}
		if ev.Stack.Op == "link" {
			return "stack linked: " + strings.Join(ev.Stack.Branches, " -> ")
		}
		return "stack synced against trunk"
	}
	return fmt.Sprintf("engineer_id %s: unrecognized event", ev.EngineerID)
}

// verifyFailedListCap bounds how many failing checks fleetEventText and
// fleetEntry name individually — enough to point at the culprits without a
// run with dozens of failing checks ballooning either rendering.
const verifyFailedListCap = 3

// verifyStatusOrder fixes the order status counts render in (passed before
// failed before skipped before timeout before error), so the same checks
// always summarize identically regardless of the order acy ran them in.
var verifyStatusOrder = []engineerwire.VerifyStatus{
	engineerwire.VerifyPassed,
	engineerwire.VerifyFailed,
	engineerwire.VerifySkipped,
	engineerwire.VerifyTimeout,
	engineerwire.VerifyError,
}

// verifySummary is the shared shape fleetEventText and fleetEntry both
// render a Result's Verification from, so the two views — the architect's
// terse context and the human transcript — can never disagree about how
// many checks count as which status or which ones made the failing list.
type verifySummary struct {
	// countLine is e.g. "2 passed, 1 failed, 1 skipped" — statuses with a
	// zero count are omitted.
	countLine string
	// failed holds up to verifyFailedListCap checks with Status == "failed",
	// in the order they appear in the input.
	failed []engineerwire.VerifyCheck
	// moreFailed is how many additional failing checks did not fit in failed.
	moreFailed int
}

// anyFailed reports whether at least one check failed — the signal a caller
// should use to make a failure unmissable, regardless of Outcome.
func (vs verifySummary) anyFailed() bool {
	return len(vs.failed) > 0 || vs.moreFailed > 0
}

// summarizeVerification counts checks by status and picks out up to
// verifyFailedListCap failing ones. Call with a non-empty checks; a nil or
// empty slice has nothing to summarize.
func summarizeVerification(checks []engineerwire.VerifyCheck) verifySummary {
	counts := make(map[engineerwire.VerifyStatus]int, len(verifyStatusOrder))
	var failed []engineerwire.VerifyCheck
	for _, c := range checks {
		counts[c.Status]++
		if c.Status == engineerwire.VerifyFailed {
			failed = append(failed, c)
		}
	}

	parts := make([]string, 0, len(verifyStatusOrder))
	for _, status := range verifyStatusOrder {
		if n := counts[status]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, status))
		}
	}

	vs := verifySummary{countLine: strings.Join(parts, ", ")}
	if len(failed) > verifyFailedListCap {
		vs.moreFailed = len(failed) - verifyFailedListCap
		failed = failed[:verifyFailedListCap]
	}
	vs.failed = failed
	return vs
}

// questionsText renders an engineer's escalated questions into one line per
// question: label, the question itself, and its options — everything
// AnswerEngineer needs the architect to have read before it replies.
func questionsText(qs []engineerwire.AskQuestion) string {
	parts := make([]string, 0, len(qs))
	for _, q := range qs {
		label := q.Header
		if label == "" {
			label = q.Question
		}
		opts := make([]string, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, o.Label)
		}
		parts = append(parts, fmt.Sprintf("%s: %s [%s]", label, q.Question, strings.Join(opts, ", ")))
	}
	return strings.Join(parts, "; ")
}

// engineerTicketWidth bounds the ticket column, mirroring taskTitleWidth.
const engineerTicketWidth = 24

// fleetStatusTable renders the fleet ledger: FleetStatus's answer and /fleet's
// report both go through this, so the two can never disagree about what the
// fleet looks like.
func fleetStatusTable(engineers []fleet.EngineerStatus, used, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "capacity %d/%d\n", used, total)
	if len(engineers) == 0 {
		b.WriteString("no engineers launched yet")
		return b.String()
	}

	row := func(id, ticket, host, state, outcome, cost string) {
		fmt.Fprintf(&b, "%-6s %-*s %-10s %-10s %-11s %10s\n", id, engineerTicketWidth, ticket, host, state, outcome, cost)
	}
	row("id", "ticket", "host", "state", "outcome", "cost")

	for _, e := range engineers {
		outcome := e.Outcome
		if outcome == "" {
			outcome = "—"
		}
		row(e.EngineerID, truncate(e.Ticket, engineerTicketWidth-1), e.Host, e.State, outcome, fmt.Sprintf("$%.4f", e.CostUSD))
		if e.PRURL != "" {
			fmt.Fprintf(&b, "       %s\n", e.PRURL)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// fleetReport is /fleet's body: the same table FleetStatus hands the
// architect, re-synced so a human checking in sees the current ledger rather
// than whatever the last event happened to leave behind.
func (m *Model) fleetReport() string {
	if m.fleet == nil {
		return "no fleet configured in this session"
	}
	m.syncFleet()
	return fleetStatusTable(m.engineers, m.fleetCapUsed, m.fleetCapTotal)
}
