package orchestrator

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var (
	errClosed       = errors.New("the supervisor is shutting down")
	errStreamClosed = errors.New("the child process ended without reporting")
	errCancelled    = errors.New("cancelled")
)

// dispatchSpec is a decoded Dispatch call. Its wire shape is mcp.DispatchSchema,
// which is what the parent is shown; this is the other half of that contract.
type dispatchSpec struct {
	Title       string   `json:"title"`
	Instruction string   `json:"instruction"`
	Context     []string `json:"context"`
	Success     string   `json:"success"`
	BudgetUSD   float64  `json:"budget_usd"`
}

func parseDispatch(raw json.RawMessage) (dispatchSpec, error) {
	var s dispatchSpec
	if len(raw) == 0 {
		return s, errors.New("no arguments were given")
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("the arguments were not valid JSON: %w", err)
	}
	s.Title = strings.TrimSpace(s.Title)
	s.Instruction = strings.TrimSpace(s.Instruction)
	s.Success = strings.TrimSpace(s.Success)
	if s.Instruction == "" {
		return s, errors.New("instruction is required and was empty")
	}
	if s.Title == "" {
		s.Title = clip(firstLine(s.Instruction), 60)
	}
	return s, nil
}

func firstLine(s string) string {
	head, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(head)
}

// taskID is short on purpose: it appears in the gate badge, in the transcript
// and in every report the parent reads, so it has to be cheap to carry.
func taskID(n int) string { return fmt.Sprintf("t%d", n) }

// newUUID is a v4 uuid. claude's --session-id requires one, and pre-assigning
// the child's id (rather than learning it from the init event) is what makes
// gate attribution race-free: a child cannot raise a permission request before
// acy knows which task it belongs to.
//
// Hand-rolled because the repo has no uuid dependency and this is twelve lines.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice, and a session id that is
		// merely unique-enough is better than a dead dispatch.
		return fmt.Sprintf("acy0%012x-0000-4000-8000-%012x", len(b), len(b))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
