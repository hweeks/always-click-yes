package judge

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseVerdict(t *testing.T) {
	cases := map[string]Verdict{
		"all good. STATUS: DONE": VerdictDone,
		"status: done":           VerdictDone,
		"Not yet. STATUS: CONTINUE and I'll keep going": VerdictContinue,
		"STATUS:CONTINUE":                     VerdictContinue,
		"no sentinel here":                    VerdictUnclear,
		"STATUS: DONE beats STATUS: CONTINUE": VerdictDone,
	}
	for in, want := range cases {
		if got := ParseVerdict(in); got != want {
			t.Errorf("ParseVerdict(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPromptIncludesInputs(t *testing.T) {
	p := Prompt("PLAN-STEP-ALPHA", "FINAL-MSG-BETA")
	if !strings.Contains(p, "PLAN-STEP-ALPHA") {
		t.Error("prompt should embed the plan")
	}
	if !strings.Contains(p, "FINAL-MSG-BETA") {
		t.Error("prompt should embed the final message")
	}
	if !strings.Contains(p, "STATUS: DONE") || !strings.Contains(p, "STATUS: CONTINUE") {
		t.Error("prompt should instruct the sentinel replies")
	}
}

func TestPromptHandlesEmpty(t *testing.T) {
	p := Prompt("   ", "")
	if strings.Contains(p, "=== APPROVED PLAN ===\n\n") {
		t.Error("empty plan should be replaced with a placeholder")
	}
	if !strings.Contains(p, "no explicit plan") || !strings.Contains(p, "no closing text") {
		t.Error("expected placeholders for empty plan and message")
	}
}

// TestLiveAssess drives a real one-shot judge. Costs a few cents and needs auth,
// so it is opt-in via ACY_LIVE=1.
//
//	ACY_LIVE=1 go test ./internal/judge/ -run TestLiveAssess -v
func TestLiveAssess(t *testing.T) {
	if os.Getenv("ACY_LIVE") == "" {
		t.Skip("set ACY_LIVE=1 to run the live judge test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	plan := "Step 1: create hello.txt. Step 2: create world.txt."
	done := "I created both hello.txt and world.txt. Every step is finished."
	v, reply, err := Assess(ctx, Options{Model: "sonnet"}, plan, done)
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	t.Logf("verdict=%v reply=%q", v, reply)
	if v != VerdictDone {
		t.Fatalf("verdict = %v, want DONE (reply: %q)", v, reply)
	}

	partial := "I created hello.txt. I still need to create world.txt."
	v, reply, err = Assess(ctx, Options{Model: "sonnet"}, plan, partial)
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	t.Logf("verdict=%v reply=%q", v, reply)
	if v != VerdictContinue {
		t.Fatalf("verdict = %v, want CONTINUE (reply: %q)", v, reply)
	}
}
