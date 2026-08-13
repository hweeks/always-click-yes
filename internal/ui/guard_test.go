package ui

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/gate"
)

func TestMergeGuardVerdict(t *testing.T) {
	stdProtected := map[string]bool{"main": true, "master": true}

	cases := []struct {
		name      string
		tool      string
		command   string
		protected map[string]bool
		wantDeny  bool
	}{
		{"gh pr merge with id", "Bash", "gh pr merge 1", stdProtected, true},
		{"gh pr merge auto", "Bash", "gh pr merge --auto", stdProtected, true},
		{"gh api merges path", "Bash", "gh api repos/o/r/merges", stdProtected, true},
		{"push to main", "Bash", "git push origin main", stdProtected, true},
		{"push refspec to main", "Bash", "git push origin HEAD:main", stdProtected, true},
		{"push plus main", "Bash", "git push origin +main", stdProtected, true},
		{"force push to master", "Bash", "git push --force origin master", stdProtected, true},
		{"delete-push refs/heads/main", "Bash", "git push origin :refs/heads/main", stdProtected, true},
		{"push refspec to refs/heads/main", "Bash", "git push origin HEAD:refs/heads/main", stdProtected, true},
		{"custom trunk", "Bash", "git push origin trunk", map[string]bool{"trunk": true}, true},
		{"chained deny after allow", "Bash", "echo hi && gh pr merge 1", stdProtected, true},
		{"multi refspec main last", "Bash", "git push origin acy/work main", stdProtected, true},
		{"multi refspec main first", "Bash", "git push origin main acy/work", stdProtected, true},
		{"multi refspec refs/heads/main non-first", "Bash", "git push origin acy/work refs/heads/main", stdProtected, true},
		{"push --all", "Bash", "git push --all origin", stdProtected, true},
		{"push --mirror", "Bash", "git push --mirror origin", stdProtected, true},

		{"push feature branch", "Bash", "git push origin acy/whatever", stdProtected, false},
		{"multi refspec both feature branches", "Bash", "git push origin acy/a acy/b", stdProtected, false},
		{"push -u HEAD", "Bash", "git push -u origin HEAD", stdProtected, false},
		{"bare git push", "Bash", "git push", stdProtected, false},
		{"git merge abort", "Bash", "git merge --abort", stdProtected, false},
		{"git merge origin/main", "Bash", "git merge origin/main", stdProtected, false},
		{"gh pr create", "Bash", "gh pr create", stdProtected, false},
		{"gh pr view", "Bash", "gh pr view 3", stdProtected, false},
		{"gh stack view", "Bash", "gh stack view --json", stdProtected, false},
		{"gh stack push", "Bash", "gh stack push", stdProtected, false},
		{"non-bash tool", "Write", "gh pr merge 1", stdProtected, false},
		{"empty command", "Bash", "", stdProtected, false},
		{"custom trunk unaffected by main", "Bash", "git push origin main", map[string]bool{"trunk": true}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]string{"command": tc.command})
			deny, reason := mergeGuardVerdict(tc.tool, raw, tc.protected)
			if deny != tc.wantDeny {
				t.Errorf("mergeGuardVerdict(%q, %q) deny = %v, want %v (reason=%q)",
					tc.tool, tc.command, deny, tc.wantDeny, reason)
			}
			if deny && reason == "" {
				t.Error("denied with no reason")
			}
		})
	}
}

// A matching Bash call must be resolved as a deny straight out of enqueue,
// never queued for a countdown. Every existing gate test used Bash commands
// that don't match the guard, so this is the one that actually exercises the
// deny path end to end.
func TestEnqueueDeniesMergeToProtectedBranch(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.now = time.Unix(1_000_000, 0)

	p, ch := bashPending("git push origin main")
	m.enqueue(p)

	if len(m.pending) != 0 {
		t.Fatalf("want 0 pending (denied outright), got %d", len(m.pending))
	}
	select {
	case d := <-ch:
		if d.Behavior != gate.Deny {
			t.Fatalf("want deny, got %+v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("no decision — claude is still blocked on the hook")
	}
}

// A non-matching Bash command is unaffected by the guard and still counts down
// as normal.
func TestEnqueueStillGatesOrdinaryBash(t *testing.T) {
	m := New(nil, Config{Countdown: 30 * time.Second})
	m.now = time.Unix(1_000_000, 0)

	p, ch := bashPending("git push origin acy/whatever")
	m.enqueue(p)

	if len(m.pending) != 1 {
		t.Fatalf("want 1 pending, got %d", len(m.pending))
	}
	select {
	case d := <-ch:
		t.Fatalf("resolved immediately (%+v); should be counting down", d)
	default:
	}
}
