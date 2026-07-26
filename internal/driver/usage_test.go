package driver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// loadResults decodes the captured result events. They are five real turns from
// one acy run on this repo, in order, spanning a --resume.
func loadResults(t *testing.T) []Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "result_events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		ev, err := Decode(line)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		out = append(out, ev)
	}
	if len(out) != 5 {
		t.Fatalf("want 5 result events, got %d", len(out))
	}
	return out
}

func TestDecodeUsage(t *testing.T) {
	first := loadResults(t)[0]

	if first.Usage == nil {
		t.Fatal("usage did not decode")
	}
	want := Usage{
		InputTokens:              22,
		OutputTokens:             14862,
		CacheCreationInputTokens: 74130,
		CacheReadInputTokens:     474322,
	}
	if *first.Usage != want {
		t.Errorf("usage = %+v, want %+v", *first.Usage, want)
	}
	// The context this turn actually carried — the number that exposes a
	// conversation growing without bound.
	if got, want := first.Usage.ContextSize(), 22+74130+474322; got != want {
		t.Errorf("ContextSize() = %d, want %d", got, want)
	}
	mu, ok := first.ModelUsage["claude-fable-5"]
	if !ok {
		t.Fatalf("no modelUsage for the main model, got keys %v", keysOf(first.ModelUsage))
	}
	if mu.ContextWindow != 1_000_000 {
		t.Errorf("ContextWindow = %d, want 1000000", mu.ContextWindow)
	}
}

// TestUsageIsPerTurnAndModelUsageIsPerProcess pins the one distinction that
// makes token accounting easy to get silently wrong: usage must be ADDED,
// modelUsage must be ASSIGNED. Swap them and every number stays plausible.
//
// The fixture proves it rather than asserting it by fiat. Turns 0-2 are one
// process, and their per-turn cache reads sum exactly to that process's
// cumulative modelUsage figure. Turn 3 then resets the cumulative to its own
// per-turn value — a --resume started a fresh process — even though the session
// id never changes, because --resume keeps the id in -p mode.
func TestUsageIsPerTurnAndModelUsageIsPerProcess(t *testing.T) {
	events := loadResults(t)

	const model = "claude-fable-5"
	var runTotal, procTotal int64
	for i, ev := range events {
		cum := ev.ModelUsage[model].CacheReadInputTokens
		turn := int64(ev.Usage.CacheReadInputTokens)

		// A process boundary is where the cumulative figure drops back to this
		// turn's own count.
		if int64(cum) == turn {
			procTotal = 0
		}
		procTotal += turn
		runTotal += turn

		if procTotal != int64(cum) {
			t.Errorf("turn %d: per-turn usage summed to %d, but modelUsage reports %d cumulative",
				i, procTotal, cum)
		}
	}

	// Every session id is the same, so a session id alone can never tell you
	// where one process ended and the next began.
	for _, ev := range events {
		if ev.SessionID != events[0].SessionID {
			t.Fatal("fixture no longer spans a single session id")
		}
	}

	// The measured run total this whole refactor exists to drive down.
	if want := int64(8_697_690); runTotal != want {
		t.Errorf("run cache-read total = %d, want %d", runTotal, want)
	}
}

// TestUsageAbsentIsHarmless: not every result carries usage (an aborted turn
// may not), and a nil Usage must not panic the accumulator.
func TestUsageAbsentIsHarmless(t *testing.T) {
	ev, err := Decode([]byte(`{"type":"result","stop_reason":"end_turn"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Usage != nil {
		t.Fatalf("want nil usage, got %+v", ev.Usage)
	}
	if got := ev.Usage.ContextSize(); got != 0 {
		t.Errorf("ContextSize() on nil = %d, want 0", got)
	}
}

// TestDecodeStructuredOutput covers the --json-schema return path a dispatched
// child reports through.
func TestDecodeStructuredOutput(t *testing.T) {
	ev, err := Decode([]byte(`{"type":"result","structured_output":{"outcome":"completed","summary":"did it"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.StructuredOutput) == 0 {
		t.Fatal("structured_output did not decode")
	}
	var got struct {
		Outcome string `json:"outcome"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(ev.StructuredOutput, &got); err != nil {
		t.Fatal(err)
	}
	if got.Outcome != "completed" || got.Summary != "did it" {
		t.Errorf("structured output = %+v", got)
	}
}

func keysOf(m map[string]ModelUsage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
