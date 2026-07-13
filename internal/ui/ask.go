package ui

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hweeks/always-click-yes/internal/alog"
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

// askState is a pending AskUserQuestion tool call the user is answering. Because
// the tool call arrives mid-turn (no result event until answered), the panel
// blocks input until submitAsk sends the tool_result back.
type askState struct {
	toolUseID string
	questions []askQuestion
	qIdx      int
}

// parseAsk decodes an AskUserQuestion tool input into interactive state. It
// returns false when the shape is unrecognized so the caller can fall back to a
// plain tool rendering.
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

// handleAskKey drives the AskUserQuestion panel. Enter confirms the current
// question and advances (or submits on the last one); space toggles a choice for
// multi-select; Esc skips all questions.
func (m *Model) handleAskKey(msg tea.KeyMsg) tea.Cmd {
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
	case " ":
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

// submitAsk sends the collected answers back as the tool_result, unblocking the
// turn. A skipped panel returns a neutral answer so claude proceeds.
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
	if m.drv != nil {
		// The turn is blocked on this tool_result. If the write fails there is
		// nothing left to unblock it, so say so rather than parking on "working…"
		// forever with no explanation.
		if err := m.drv.SendToolResult(a.toolUseID, answer); err != nil {
			alog.Printf("ask: send tool_result failed: %v", err)
			m.appendEntry(entry{kind: eWarn, body: "⚠ could not send the answer — the session may be dead: " + err.Error()})
			m.status = "answer not delivered"
			return nil
		}
	}
	m.appendEntry(entry{kind: eYou, body: "↳ answered:\n" + answer})
	m.status = "working…"
	m.processing = true
	return nil
}
