package gitops

import (
	"context"
	"strings"
	"testing"
)

func TestBranchName(t *testing.T) {
	cases := []struct {
		name        string
		ticket      string
		title       string
		wantPrefix  string
		wantContain string
	}{
		{
			name:        "simple",
			ticket:      "ENG-123",
			title:       "Fix the flaky test",
			wantPrefix:  "acy/eng-123-",
			wantContain: "fix-the-flaky-test",
		},
		{
			name:       "unicode title",
			ticket:     "ENG-9",
			title:      "Añadir soporte para 日本語",
			wantPrefix: "acy/eng-9-",
		},
		{
			name:       "empty title",
			ticket:     "ENG-1",
			title:      "",
			wantPrefix: "acy/eng-1-",
		},
		{
			name:       "empty ticket",
			ticket:     "",
			title:      "Some change",
			wantPrefix: "acy/task-",
		},
		{
			name:       "long title",
			ticket:     "ENG-42",
			title:      strings.Repeat("very long descriptive title segment ", 10),
			wantPrefix: "acy/eng-42-",
		},
		{
			name:       "long ticket",
			ticket:     strings.Repeat("x", 200),
			title:      "short",
			wantPrefix: "acy/",
		},
		{
			name:       "punctuation only title",
			ticket:     "ENG-1",
			title:      "!!! ??? ***",
			wantPrefix: "acy/eng-1-",
		},
	}

	run := hermeticRunner(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BranchName(tc.ticket, tc.title)

			if len(got) > maxRefLen {
				t.Fatalf("BranchName(%q, %q) = %q, len %d exceeds maxRefLen %d", tc.ticket, tc.title, got, len(got), maxRefLen)
			}
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Fatalf("BranchName(%q, %q) = %q, want prefix %q", tc.ticket, tc.title, got, tc.wantPrefix)
			}
			if tc.wantContain != "" && !strings.Contains(got, tc.wantContain) {
				t.Fatalf("BranchName(%q, %q) = %q, want it to contain %q", tc.ticket, tc.title, got, tc.wantContain)
			}

			if _, err := run(context.Background(), "", "git", "check-ref-format", "--branch", got); err != nil {
				t.Fatalf("BranchName(%q, %q) = %q is not a valid git ref: %v", tc.ticket, tc.title, got, err)
			}
		})
	}
}
