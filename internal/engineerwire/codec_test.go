package engineerwire

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hweeks/always-click-yes/internal/state"
)

// stampWant applies the same stamping Marshal does (type, and for Hello
// protocol_version) so a round-tripped value can be compared against the
// input as sent, not as constructed.
func stampWant(msg any) any {
	switch m := msg.(type) {
	case Spec:
		m.Type = TypeSpec
		return m
	case Answer:
		m.Type = TypeAnswer
		return m
	case Cancel:
		m.Type = TypeCancel
		return m
	case Hello:
		m.Type = TypeHello
		m.ProtocolVersion = ProtocolVersion
		return m
	case Event:
		m.Type = TypeEvent
		return m
	case Question:
		m.Type = TypeQuestion
		return m
	case Result:
		m.Type = TypeResult
		return m
	default:
		return msg
	}
}

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		msg  any
	}{
		{"spec", Spec{
			Ticket: "ACY-1", Title: "add the wire types", Brief: "build internal/engineerwire",
			Success: "go test ./internal/engineerwire/ passes", BaseBranch: "main", Branch: "agent/wire",
			Model: "sonnet", ChildModel: "sonnet", ChildEffort: "high", BudgetUSD: 10, DeadmanHours: 4,
			StackTrunk: "main",
		}},
		{"answer", Answer{QuestionID: "q1", Text: "sqlite, it's simplest"}},
		{"cancel", Cancel{Reason: "superseded by a newer spec"}},
		{"hello", Hello{EngineerID: "e-1", ACYVersion: "1.5.0", Host: "mbp.local", PID: 4242}},
		{"event", Event{
			Kind: EventPhase, Text: "starting", CostUSD: 0.01,
			Tokens: state.Tokens{Input: 10, Output: 20, CacheCreate: 1, CacheRead: 100},
		}},
		{"question", Question{
			QuestionID: "q1",
			Questions: []AskQuestion{{
				Question: "sqlite or postgres?", Header: "Storage", MultiSelect: false,
				Options: []AskOption{{Label: "sqlite", Description: "simplest"}, {Label: "postgres"}},
			}},
		}},
		{"result", Result{
			Outcome: "completed", Summary: "added the ledger", Branch: "agent/wire",
			PRURL: "https://example.com/pr/1", CostUSD: 1.23,
			Tokens: state.Tokens{CacheRead: 100}, Files: []string{"a.go", "b.go"},
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line, err := Marshal(c.msg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if !strings.HasSuffix(string(line), "\n") {
				t.Fatalf("Marshal did not terminate the line: %q", line)
			}
			if strings.Count(string(line), "\n") != 1 {
				t.Fatalf("Marshal produced more than one line: %q", line)
			}

			got, err := NewDecoder(strings.NewReader(string(line))).Decode()
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}

			want := stampWant(c.msg)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("round-trip mismatch:\n got  = %#v\n want = %#v", got, want)
			}
		})
	}
}

func TestMarshalRejectsUnknownType(t *testing.T) {
	_, err := Marshal(struct{ Type string }{Type: "bogus"})
	if err == nil {
		t.Fatal("expected an error for a type Marshal does not recognize")
	}
}

func TestDecodeRejectsUnknownType(t *testing.T) {
	_, err := NewDecoder(strings.NewReader(`{"type":"bogus"}` + "\n")).Decode()
	if err == nil {
		t.Fatal("expected an error for an unknown wire type")
	}
}

func TestDecodeMultipleLines(t *testing.T) {
	var buf strings.Builder
	for _, msg := range []any{
		Hello{EngineerID: "e1"},
		Event{Kind: EventLog, Text: "one"},
		Event{Kind: EventLog, Text: "two"},
	} {
		line, err := Marshal(msg)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		buf.Write(line)
	}

	dec := NewDecoder(strings.NewReader(buf.String()))
	var got []any
	for {
		msg, err := dec.Decode()
		if err != nil {
			break
		}
		got = append(got, msg)
	}
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	if h, ok := got[0].(Hello); !ok || h.EngineerID != "e1" {
		t.Errorf("first message = %#v, want the hello", got[0])
	}
}

// TestDecodeSpecWithStackTrunkField proves forward compatibility for the
// stack_trunk field: a spec line carrying it (as an architect built on a
// newer acy would send when stacking engineers) decodes cleanly into Spec,
// with StackTrunk populated rather than silently dropped.
func TestDecodeSpecWithStackTrunkField(t *testing.T) {
	line := `{"type":"spec","ticket":"ACY-2","title":"stacked task","brief":"do the thing",` +
		`"success":"tests pass","base_branch":"agent/other-engineer","branch":"agent/mine",` +
		`"stack_trunk":"main"}` + "\n"

	got, err := NewDecoder(strings.NewReader(line)).Decode()
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	spec, ok := got.(Spec)
	if !ok {
		t.Fatalf("Decode returned %T, want Spec", got)
	}
	if spec.StackTrunk != "main" {
		t.Errorf("StackTrunk = %q, want %q", spec.StackTrunk, "main")
	}
}

// TestDecodeSpecIgnoresUnknownField proves the general compatibility claim
// that lets the wire evolve without a protocol bump: decodeLine does not set
// json.Decoder's DisallowUnknownFields, so a spec line carrying a field an
// older (or simply different) engineer binary doesn't recognize decodes
// successfully instead of erroring, with the unknown field just dropped.
func TestDecodeSpecIgnoresUnknownField(t *testing.T) {
	line := `{"type":"spec","ticket":"ACY-3","title":"future task","brief":"do the thing",` +
		`"success":"tests pass","base_branch":"main","branch":"agent/mine",` +
		`"totally_unknown_field":"whatever"}` + "\n"

	got, err := NewDecoder(strings.NewReader(line)).Decode()
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if _, ok := got.(Spec); !ok {
		t.Fatalf("Decode returned %T, want Spec", got)
	}
}
