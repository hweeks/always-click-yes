package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/hweeks/always-click-yes/internal/driver"
)

// newlineKeys are the three spellings bound to the composer's InsertNewline.
// Each is asserted separately because they arrive by different routes: ctrl+j
// and alt+enter are ordinary keys every terminal can send, while shift+enter
// only exists as a distinct key under the Kitty keyboard protocol — a v2 program
// negotiates it, a v1 one could not see the modifier at all.
var newlineKeys = map[string]tea.KeyPressMsg{
	"shift+enter": {Code: tea.KeyEnter, Mod: tea.ModShift},
	"alt+enter":   {Code: tea.KeyEnter, Mod: tea.ModAlt},
	"ctrl+j":      {Code: 'j', Mod: tea.ModCtrl},
}

// planParagraphs is more paragraphs than the composer has rows, which is the
// whole point: a plan document is longer than the box is tall.
const planParagraphs = maxInputRows * 3

// A newline key must keep inserting newlines past the height of the box.
// bubbles' MaxHeight is not only the visible-row cap it reads as — the textarea
// refuses InsertNewline once the value holds MaxHeight *logical* lines — so
// setting it to maxInputRows made the ninth newline a silent no-op and flattened
// a multi-paragraph message into one run-on line.
func TestNewlineKeysBuildAValueTallerThanTheBox(t *testing.T) {
	for name, press := range newlineKeys {
		t.Run(name, func(t *testing.T) {
			m := sizedModel(t)
			for i := range planParagraphs {
				m = typeInto(m, fmt.Sprintf("para %d", i))
				tm, _ := m.Update(press)
				m = tm.(Model)
			}

			value := m.input.Value()
			if got, want := strings.Count(value, "\n"), planParagraphs; got != want {
				t.Fatalf("%s inserted %d newlines, want %d:\n%q", name, got, want, value)
			}
			// The tail is what a logical-line cap eats: the early paragraphs are
			// under the limit and would survive even with the bug.
			last := fmt.Sprintf("para %d", planParagraphs-1)
			if !strings.Contains(value, last) {
				t.Fatalf("%s lost the tail of the message (%q missing):\n%q", name, last, value)
			}
			// However tall the value gets, the box does not: layout owns the
			// visible height and the textarea scrolls inside it.
			if m.input.Height() != maxInputRows {
				t.Errorf("composer is %d rows, want the %d-row cap", m.input.Height(), maxInputRows)
			}
		})
	}
}

// The composer must stay clamped no matter how long the value is — that is the
// half of the old MaxHeight that we still want, now enforced by layout alone.
func TestComposerHeightStaysClampedForALongValue(t *testing.T) {
	const height = 30
	m := New(nil, Config{})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: height})
	m = tm.(Model)

	for _, n := range []int{maxInputRows - 1, maxInputRows, maxInputRows * 5, maxInputRows * 50} {
		m.input.SetValue(strings.TrimRight(strings.Repeat("a line of the plan\n", n), "\n"))
		m.layout()

		want := min(n, maxInputRows)
		if m.input.Height() != want {
			t.Errorf("%d-line value: composer is %d rows, want %d", n, m.input.Height(), want)
		}
		if got := lipgloss.Height(m.input.View()); got != want {
			t.Errorf("%d-line value: composer renders %d rows, want %d", n, got, want)
		}
		if got := lipgloss.Height(m.View().Content); got != height {
			t.Errorf("%d-line value: frame is %d lines, want %d", n, got, height)
		}
	}
}

// Splitting the modified variants out of the send binding must not disturb plain
// Enter: it still sends the composer to claude and still clears the box.
func TestPlainEnterStillSends(t *testing.T) {
	sent := &strings.Builder{}
	m := sizedModel(t)
	m.drv = driver.NewWithWriter(driver.Options{}, nopCloser{sent})

	m = typeAndSend(t, m, "ship it")

	if !strings.Contains(sent.String(), "ship it") {
		t.Errorf("Enter did not send; the driver saw:\n%s", sent.String())
	}
	if m.input.Value() != "" {
		t.Errorf("composer = %q, want it cleared by the send", m.input.Value())
	}
}

func TestFailedSendLeavesComposerIntact(t *testing.T) {
	m := sizedModel(t)
	m.drv = driver.NewWithWriter(driver.Options{}, failingWriteCloser{err: errors.New("broken pipe")})
	m.input.SetValue("do not lose this")

	tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = tm.(Model)

	if got := m.input.Value(); got != "do not lose this" {
		t.Errorf("composer = %q, want failed message preserved", got)
	}
	if m.processing {
		t.Error("failed send must not begin a turn")
	}
	if !strings.Contains(lastBody(&m), "send failed") {
		t.Errorf("failure was not visible: %q", lastBody(&m))
	}
}

// ...and the /command path in handleEnter, which is a different branch entirely
// and would fail silently by being forwarded to claude as a message.
func TestPlainEnterStillRunsSlashCommands(t *testing.T) {
	sent := &strings.Builder{}
	m := sizedModel(t)
	m.drv = driver.NewWithWriter(driver.Options{}, nopCloser{sent})

	m = typeAndSend(t, m, "/help")

	if !m.showHelp {
		t.Error("/help did not open the help overlay")
	}
	if sent.String() != "" {
		t.Errorf("/help was forwarded to claude:\n%s", sent.String())
	}
	if m.input.Value() != "" {
		t.Errorf("composer = %q, want it cleared by the command", m.input.Value())
	}
}

// The point of all of it: a multi-paragraph message reaches claude with its
// paragraphs, rather than as the run-on sentence a logical-line cap produced.
func TestAMultiParagraphMessageSendsWhole(t *testing.T) {
	sent := &strings.Builder{}
	m := sizedModel(t)
	m.drv = driver.NewWithWriter(driver.Options{}, nopCloser{sent})

	for i := range planParagraphs {
		m = typeInto(m, fmt.Sprintf("para %d", i))
		tm, _ := m.Update(newlineKeys["ctrl+j"])
		m = tm.(Model)
	}
	m = typeInto(m, "and finally")
	tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = tm.(Model)

	// One stdin line of JSON. Decode it before counting: acy's authoritative
	// runtime envelope has its own newlines and is deliberately not user text.
	wire := sent.String()
	for _, para := range []string{"para 0", fmt.Sprintf("para %d", planParagraphs-1), "and finally"} {
		if !strings.Contains(wire, para) {
			t.Errorf("%q never reached the driver:\n%s", para, wire)
		}
	}
	var payload struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(wire)), &payload); err != nil {
		t.Fatalf("decode driver payload: %v", err)
	}
	_, userText, ok := strings.Cut(payload.Message.Content, "</acy-runtime>\n\n")
	if !ok {
		t.Fatalf("driver payload has no runtime envelope:\n%s", wire)
	}
	if got, want := strings.Count(userText, "\n"), planParagraphs; got != want {
		t.Errorf("the driver saw %d newlines, want %d:\n%s", got, want, wire)
	}
}

// pastedPlan is the shape that used to arrive as one line: blank-line-separated
// paragraphs plus a list.
const pastedPlan = "# Plan\n\n1. read the file\n2. change it\n3. run the tests"

// A bracketed paste is its own message type in v2, not a run of key runes, so it
// can neither be mistaken for an Enter press nor be claimed by any of the
// interception branches ahead of the sub-component routing.
func TestMultiLinePasteLandsWhole(t *testing.T) {
	sent := &strings.Builder{}
	m := sizedModel(t)
	m.drv = driver.NewWithWriter(driver.Options{}, nopCloser{sent})

	tm, _ := m.Update(tea.PasteMsg{Content: pastedPlan})
	m = tm.(Model)

	if m.input.Value() != pastedPlan {
		t.Errorf("composer =\n%q\nwant\n%q", m.input.Value(), pastedPlan)
	}
	if sent.String() != "" {
		t.Errorf("the paste sent a message:\n%s", sent.String())
	}
	// The box grew to the pasted document rather than showing its last row only.
	if want := strings.Count(pastedPlan, "\n") + 1; m.input.Height() != want {
		t.Errorf("composer is %d rows, want %d — one per pasted line", m.input.Height(), want)
	}
}

// The same, with a countdown running. A pending gate is the state acy spends
// most of an armed run in, and the gate chords are the last interception before
// the composer — so if anything above the routing were going to eat a paste,
// this is where it would.
func TestMultiLinePasteLandsWhileAGateIsPending(t *testing.T) {
	m := sizedModel(t)
	m.countdown = 30 * time.Second
	m.now = time.Unix(1_000_000, 0)

	p, _ := bashPending("go test ./...")
	m.enqueue(p)
	if len(m.pending) != 1 {
		t.Fatalf("want one gate counting down, got %d", len(m.pending))
	}

	tm, _ := m.Update(tea.PasteMsg{Content: pastedPlan})
	m = tm.(Model)

	if m.input.Value() != pastedPlan {
		t.Errorf("composer =\n%q\nwant\n%q", m.input.Value(), pastedPlan)
	}
	if len(m.pending) != 1 {
		t.Errorf("the paste answered the gate (%d pending)", len(m.pending))
	}
}
