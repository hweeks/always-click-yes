package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/driver"
)

// rateLimitEvent builds the rate_limit_event that drives the "rate limit
// reached" prose in ingest() — see model.go's TypeRateLimit case.
func rateLimitEvent() driver.Event {
	return driver.Event{
		Type: driver.TypeRateLimit,
		RateLimitInfo: &driver.RateLimitInfo{
			Status:   "rejected",
			ResetsAt: time.Now().Unix(),
		},
	}
}

// claudeEntry returns the first eClaude entry in the transcript, so a test can
// render it in isolation rather than scraping the whole joined viewport.
func claudeEntry(t *testing.T, m *Model) entry {
	for _, e := range m.entries {
		if e.kind == eClaude {
			return e
		}
	}
	t.Fatal("no eClaude entry in transcript")
	return entry{}
}

// With Config.Agent set to "codex", both registers must say so: the
// transcript badge in its lowercase form, the rate-limit notice in its
// capitalized prose form.
func TestAgentLabelCodex(t *testing.T) {
	m := New(nil, Config{Agent: "codex"})

	m.ingest(assistantEvent("hello"))
	out := renderEntry(claudeEntry(t, &m), 60, 10)
	if !strings.Contains(out, "codex") {
		t.Errorf("badge should read codex, got:\n%s", out)
	}
	if strings.Contains(out, "claude") {
		t.Errorf("badge should not still say claude, got:\n%s", out)
	}

	m.ingest(rateLimitEvent())
	if !strings.Contains(m.transcript(), "Codex rate limit reached") {
		t.Errorf("rate-limit notice should say Codex, got transcript:\n%s", m.transcript())
	}
}

// Config left at its zero value — every caller that predates the Agent field
// — must still read exactly as it always has: "claude" and "Claude".
func TestAgentLabelDefaultsToClaude(t *testing.T) {
	m := New(nil, Config{})

	m.ingest(assistantEvent("hello"))
	out := renderEntry(claudeEntry(t, &m), 60, 10)
	if !strings.Contains(out, "claude") {
		t.Errorf("badge should default to claude, got:\n%s", out)
	}

	m.ingest(rateLimitEvent())
	if !strings.Contains(m.transcript(), "Claude rate limit reached") {
		t.Errorf("rate-limit notice should default to Claude, got transcript:\n%s", m.transcript())
	}
}
