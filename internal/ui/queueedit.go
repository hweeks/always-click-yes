package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// handleQueueEditKey drives the /queue edit overlay: up/down move the cursor,
// Ctrl+X drops the selected message outright, Enter pulls it back into the
// composer to edit and resend, Esc closes the overlay without touching
// anything. It mirrors handlePickKey's shape — the picker's counterpart for
// the /resume list.
//
// Enter refuses while the composer already holds text rather than clobbering
// a draft the user is mid-way through — the queue is not going anywhere, so
// the right answer is to ask them to deal with what's in the box first.
func (m *Model) handleQueueEditKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "up", "k":
		if m.queueCursor > 0 {
			m.queueCursor--
		}
	case "down", "j":
		if m.queueCursor < len(m.queued)-1 {
			m.queueCursor++
		}
	case "esc":
		// The keyboard is a client like any other: this only takes the overlay
		// down, and does not swallow the key that follows it.
		m.queueOpen = false
	case "ctrl+x":
		id := m.queued[m.queueCursor].id
		m.applyAction(QueueRemove(id), nil)
	case "enter":
		if strings.TrimSpace(m.input.Value()) != "" {
			m.appendEntry(entry{kind: eWarn, body: "the composer already has text — send or clear it before pulling in a queued message"})
			break
		}
		id := m.queued[m.queueCursor].id
		text := m.queued[m.queueCursor].text
		if m.raise(QueueRemove(id)).Accepted {
			m.input.SetValue(text)
			m.queueOpen = false
		}
	}
	return nil
}
