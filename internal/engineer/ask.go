package engineer

import (
	"encoding/json"
	"time"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/engineerwire"
	"github.com/hweeks/always-click-yes/internal/mcp"
)

// defaultAskTimeout is how long a question waits for Answer before it
// resolves itself with a best-judgment fallback. 15m gives an architect that
// is away from the keyboard a real chance to come back without leaving
// claude's turn blocked indefinitely.
const defaultAskTimeout = 15 * time.Minute

// askTimeoutFallback is what an unanswered question resolves with, modeled on
// mcp.SupervisorGone: claude's turn is blocked on this reply, so the
// alternative to a useless answer is a permanently hung turn.
const askTimeoutFallback = "(no answer arrived from the architect within the timeout — proceed with your best judgment)"

// pendingQuestion is one AskUserQuestion the engineer has escalated and not
// yet answered.
type pendingQuestion struct {
	p    *mcp.Pending
	stop chan struct{} // closed by Answer, so the timeout goroutine exits early
}

// interceptAsk is offered every AskUserQuestion request via
// supervisor.Flags.InterceptAsk. It always takes ownership (returns true):
// every Ask is escalated to the architect regardless of who raised it —
// children escalate too, per the wire protocol's "same question, no
// translation layer" contract.
func (c *Core) interceptAsk(p *mcp.Pending) bool {
	var payload struct {
		Questions []engineerwire.AskQuestion `json:"questions"`
	}
	if err := json.Unmarshal(p.Req.Args, &payload); err != nil {
		alog.Printf("engineer: ask: bad args for use_id=%s: %v", p.Req.ToolUseID, err)
	}

	qid := p.Req.ToolUseID
	if _, err := c.journal.Append(engineerwire.Question{
		QuestionID: qid,
		Questions:  payload.Questions,
	}); err != nil {
		alog.Printf("engineer: ask: journal append failed for %s: %v", qid, err)
	}

	pq := &pendingQuestion{p: p, stop: make(chan struct{})}
	c.mu.Lock()
	c.pending[qid] = pq
	c.mu.Unlock()

	go c.awaitQuestion(qid, pq)
	return true
}

// awaitQuestion resolves pq with the timeout fallback if nobody calls Answer
// first, and cleans up either way. It also stops waiting the moment the `acy
// mcp` child that raised the question disconnects (p.Done()) — there is no
// one left for an answer to reach at that point.
func (c *Core) awaitQuestion(qid string, pq *pendingQuestion) {
	timer := time.NewTimer(c.askTimeout)
	defer timer.Stop()
	select {
	case <-pq.stop:
	case <-pq.p.Done():
		c.dropPending(qid)
	case <-timer.C:
		alog.Printf("engineer: ask: %s timed out after %s, falling back", qid, c.askTimeout)
		pq.p.Resolve(mcp.Answer{Text: askTimeoutFallback})
		c.dropPending(qid)
	}
}

// Answer resolves the question named by questionID with text, as the
// architect would over the wire (engineerwire.Answer). It reports whether a
// question with that id was still outstanding.
func (c *Core) Answer(questionID, text string) bool {
	c.mu.Lock()
	pq, ok := c.pending[questionID]
	if ok {
		delete(c.pending, questionID)
	}
	c.mu.Unlock()
	if !ok {
		return false
	}
	pq.p.Resolve(mcp.Answer{Text: text})
	close(pq.stop)
	return true
}

func (c *Core) dropPending(qid string) {
	c.mu.Lock()
	delete(c.pending, qid)
	c.mu.Unlock()
}
