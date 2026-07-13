// Package judge runs an independent, one-shot `claude` session that reads an
// approved plan plus the working session's final message and decides whether the
// plan is complete.
//
// Using a fresh session (no --resume, no hooks, tools disabled) keeps the verdict
// independent of the session that did the work: the model can't grade its own
// homework, and the completion check never pollutes the working session's context.
package judge

import (
	"context"
	"fmt"
	"strings"

	"github.com/hweeks/always-click-yes/internal/driver"
)

// Verdict classifies a completion check.
type Verdict int

const (
	VerdictUnclear  Verdict = iota // no sentinel found (or the judge failed)
	VerdictDone                    // every step reported complete
	VerdictContinue                // work remains
)

func (v Verdict) String() string {
	switch v {
	case VerdictDone:
		return "DONE"
	case VerdictContinue:
		return "CONTINUE"
	default:
		return "UNCLEAR"
	}
}

// Options configures the one-shot judge subprocess.
type Options struct {
	Bin   string // claude binary (default "claude")
	Cwd   string // working directory (the same repo the run operates in)
	Model string // --model for the judge (optional; empty = claude's default)
}

// ParseVerdict scans judge output for the STATUS sentinel. DONE wins ties.
func ParseVerdict(text string) Verdict {
	up := strings.ToUpper(text)
	if strings.Contains(up, "STATUS: DONE") || strings.Contains(up, "STATUS:DONE") {
		return VerdictDone
	}
	if strings.Contains(up, "STATUS: CONTINUE") || strings.Contains(up, "STATUS:CONTINUE") {
		return VerdictContinue
	}
	return VerdictUnclear
}

// Prompt builds the self-contained judgment prompt from the approved plan and the
// working session's final message.
func Prompt(plan, lastMsg string) string {
	plan = strings.TrimSpace(plan)
	if plan == "" {
		plan = "(no explicit plan was captured; judge from the final message alone)"
	}
	lastMsg = strings.TrimSpace(lastMsg)
	if lastMsg == "" {
		lastMsg = "(the agent produced no closing text)"
	}
	return "You are an independent reviewer. You did NOT do this work — judge it objectively.\n\n" +
		"An agent was executing the APPROVED PLAN below, and its FINAL MESSAGE follows. " +
		"Decide whether every step of the plan has been completed. " +
		"Base your judgment only on the text provided; do not use any tools.\n\n" +
		"Reply on a single line with EXACTLY one of:\n" +
		"  STATUS: DONE                     — if every step of the plan is complete\n" +
		"  STATUS: CONTINUE <what remains>  — if any step is unfinished\n\n" +
		"=== APPROVED PLAN ===\n" + plan + "\n\n" +
		"=== AGENT'S FINAL MESSAGE ===\n" + lastMsg + "\n"
}

// Assess launches a one-shot judge session and returns its verdict plus the raw
// reply text. It reuses the streaming driver with a fresh session, plan
// permission mode, and tools disabled, so it is a single fast read-only turn with
// no side effects.
func Assess(ctx context.Context, opts Options, plan, lastMsg string) (Verdict, string, error) {
	d := driver.New(driver.Options{
		Bin:            opts.Bin,
		Cwd:            opts.Cwd,
		Model:          opts.Model,
		PermissionMode: "plan",
		ExtraArgs:      []string{"--tools", ""}, // no tools -> no side effects
	})
	if err := d.Start(ctx); err != nil {
		return VerdictUnclear, "", fmt.Errorf("start judge: %w", err)
	}
	defer d.Stop()

	if err := d.Send(Prompt(plan, lastMsg)); err != nil {
		return VerdictUnclear, "", fmt.Errorf("send judge prompt: %w", err)
	}

	var reply strings.Builder
	for ev := range d.Events() {
		if ev.Type == driver.TypeAssistant && ev.Message != nil {
			for _, b := range ev.Message.Blocks() {
				if b.Type == driver.BlockText && b.Text != "" {
					reply.WriteString(b.Text)
					reply.WriteByte('\n')
				}
			}
		}
		if ev.IsTurnEnd() {
			break
		}
	}

	text := reply.String()
	return ParseVerdict(text), text, nil
}
