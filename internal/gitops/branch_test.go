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

func TestCurrentBranch(t *testing.T) {
	run := hermeticRunner(t)

	t.Run("normal branch", func(t *testing.T) {
		dir := initRepo(t, run, "feature-x")
		got, err := CurrentBranch(context.Background(), run, dir)
		if err != nil {
			t.Fatalf("CurrentBranch: %v", err)
		}
		if got != "feature-x" {
			t.Fatalf("CurrentBranch = %q, want %q", got, "feature-x")
		}
	})

	t.Run("detached HEAD", func(t *testing.T) {
		dir := initRepo(t, run, "main")
		sha := strings.TrimSpace(mustRun(t, run, dir, "git", "rev-parse", "HEAD"))
		mustRun(t, run, dir, "git", "checkout", sha)

		got, err := CurrentBranch(context.Background(), run, dir)
		if err != nil {
			t.Fatalf("CurrentBranch: %v", err)
		}
		wantPrefix := "detached @ "
		if !strings.HasPrefix(got, wantPrefix) {
			t.Fatalf("CurrentBranch = %q, want prefix %q", got, wantPrefix)
		}
		if !strings.HasPrefix(sha, got[len(wantPrefix):]) {
			t.Fatalf("CurrentBranch = %q, short sha not a prefix of full sha %q", got, sha)
		}
	})

	t.Run("not a git repo", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := CurrentBranch(context.Background(), run, dir); err == nil {
			t.Fatalf("CurrentBranch in a non-repo dir: want error, got nil")
		}
	})
}
