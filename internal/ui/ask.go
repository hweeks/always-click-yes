package ui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/mcp"
)

// askOption is one selectable answer to a question.
type askOption struct {
	label       string
	description string
}

// askQuestion is a single question with its options and selection state.
type askQuestion struct {
	header      string
	question    string
	multiSelect bool
	options     []askOption
	selected    map[int]bool
	cursor      int
}

// askState is a question claude is blocked on, waiting for the human to answer.
//
// The block is real and it is not on claude's stdin: claude called an MCP tool, so
// it is waiting on the `acy mcp` child's JSON-RPC reply, and that child is waiting
// on pending. Resolving pending is the only thing that unblocks the turn — which is
// also why the panel is opened by the socket request rather than by the tool_use
// event. The event tells us a question was asked; only the request can answer it.
type askState struct {
	pending   *mcp.Pending
	questions []askQuestion
	qIdx      int

	// deadline auto-skips the question. Set only in AUTO-RUN, where the human has
	// by assumption walked away and a question that blocks forever would strand the
	// run — the exact failure acy exists to prevent. Zero in PLAN, where someone is
	// sitting right there.
	deadline time.Time
}

// parseAsk decodes an AskUserQuestion tool input into interactive state. It
// returns false when the shape is unrecognized so the caller can fall back to a
// plain tool rendering. The shape it accepts is the one mcp.askSchema advertises.
func parseAsk(raw json.RawMessage) (*askState, bool) {
	var in struct {
		Questions []struct {
			Question    string `json:"question"`
			Header      string `json:"header"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal(raw, &in) != nil || len(in.Questions) == 0 {
		return nil, false
	}
	a := &askState{}
	for _, q := range in.Questions {
		aq := askQuestion{
			header:      q.Header,
			question:    q.Question,
			multiSelect: q.MultiSelect,
			selected:    map[int]bool{},
		}
		for _, o := range q.Options {
			aq.options = append(aq.options, askOption{label: o.Label, description: o.Description})
		}
		if len(aq.options) == 0 {
			continue
		}
		a.questions = append(a.questions, aq)
	}
	if len(a.questions) == 0 {
		return nil, false
	}
	return a, true
}

// openAsk raises the picker for a question arriving on the ask socket. A shape we
// cannot parse is answered immediately rather than opening an empty panel the user
// could never dismiss — claude is blocked on the reply either way, so there is no
// version of this where we simply ignore it.
func (m *Model) openAsk(p *mcp.Pending) {
	a, ok := parseAsk(p.Req.Args)
	if !ok {
		alog.Printf("mcp: unparseable ask args, answering with a no-op: %s", string(p.Req.Args))
		p.Resolve(mcp.Answer{Text: mcp.SupervisorGone})
		m.appendEntry(entry{kind: eWarn, body: "⚠ " + m.agentProse() + " asked a question acy could not read — told it to use its best judgment"})
		return
	}
	a.pending = p
	if m.phase == PhaseAutoRun {
		a.deadline = m.now.Add(m.countdown)
	}
	m.ask = a
	// An Ask blocks a real claude turn and, in AUTO-RUN, is on its own
	// auto-skip countdown — it outranks the queue editor (see activeSurface
	// in present.go), and closing the editor outright rather than just
	// letting it render underneath is what guarantees nothing is left for a
	// keystroke to reach but the panel actually on screen.
	m.queueOpen = false
	m.appendEntry(entry{kind: eMeta, body: "❓ " + m.agentProse() + " is asking a question — answer below"})
}

// handleAskKey drives the AskUserQuestion panel. Enter confirms the current
// question and advances (or submits on the last one); space toggles a choice for
// multi-select; Esc skips all questions.
func (m *Model) handleAskKey(msg tea.KeyPressMsg) tea.Cmd {
	if m.ask == nil {
		return nil
	}
	q := &m.ask.questions[m.ask.qIdx]
	switch msg.String() {
	case "up", "k":
		if q.cursor > 0 {
			q.cursor--
		}
	case "down", "j":
		if q.cursor < len(q.options)-1 {
			q.cursor++
		}
	case "space": // v2's String() spells the space bar out; v1 returned " "
		if q.multiSelect {
			q.selected[q.cursor] = !q.selected[q.cursor]
		}
	case "esc":
		return m.submitAsk(true)
	case "enter":
		if !q.multiSelect {
			q.selected = map[int]bool{q.cursor: true}
		} else if len(q.selected) == 0 {
			q.selected[q.cursor] = true // require at least one
		}
		if m.ask.qIdx < len(m.ask.questions)-1 {
			m.ask.qIdx++
			return nil
		}
		return m.submitAsk(false)
	}
	return nil
}

// askRemaining returns the time left before the open question auto-skips, or 0 if
// it has no deadline (PLAN) or none is open.
func (m *Model) askRemaining() time.Duration {
	if m.ask == nil || m.ask.deadline.IsZero() {
		return 0
	}
	return max(m.ask.deadline.Sub(m.now), 0)
}

// expireAsk auto-skips a question whose countdown has elapsed. Called on every
// tick, beside expireDue.
func (m *Model) expireAsk() {
	if m.ask == nil || m.ask.deadline.IsZero() || m.now.Before(m.ask.deadline) {
		return
	}
	alog.Printf("mcp: auto-skip question after countdown")
	m.appendEntry(entry{kind: eWarn, body: "⏳ no answer in time — telling " + m.agentProse() + " to use its best judgment"})
	m.submitAsk(true)
}

// abandonAsk answers an open question when the session it belongs to is going away
// (stream closed, or the driver swapped out on arming). The `acy mcp` child is
// almost certainly dead already — it is in the driver's process group — but the
// panel must come down regardless, or it sits there eating every keystroke.
func (m *Model) abandonAsk() {
	if m.ask == nil {
		return
	}
	if m.ask.pending != nil {
		m.ask.pending.Resolve(mcp.Answer{Text: mcp.SupervisorGone})
	}
	m.ask = nil
}

// submitAsk sends the collected answers back over the ask socket, unblocking the
// turn. A skipped panel returns a neutral answer so claude proceeds rather than
// stalling.
func (m *Model) submitAsk(skipped bool) tea.Cmd {
	a := m.ask
	m.ask = nil
	if a == nil {
		return nil
	}
	var lines []string
	if skipped {
		lines = append(lines, "(user skipped the questions — proceed with your best judgment)")
	} else {
		for _, q := range a.questions {
			var picks []string
			for i := range q.options {
				if q.selected[i] {
					picks = append(picks, q.options[i].label)
				}
			}
			label := q.header
			if label == "" {
				label = q.question
			}
			lines = append(lines, fmt.Sprintf("%s: %s", label, strings.Join(picks, ", ")))
		}
	}
	answer := strings.Join(lines, "\n")

	if a.pending == nil {
		return nil
	}
	// The turn is blocked on this answer. If the mcp child has already died there
	// is nothing left to unblock, so say so rather than parking on "working…"
	// forever with no explanation.
	select {
	case <-a.pending.Done():
		alog.Printf("ask: the question's session is gone; answer discarded")
		m.appendEntry(entry{kind: eWarn, body: "⚠ could not deliver the answer — that session has ended"})
		m.status = "answer not delivered"
		return nil
	default:
	}
	a.pending.Resolve(mcp.Answer{Text: answer})

	if !skipped {
		m.appendEntry(entry{kind: eYou, body: "↳ answered:\n" + answer})
	}
	m.beginTurn()
	return nil
}
