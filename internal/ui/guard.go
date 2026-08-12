package ui

import (
	"encoding/json"
	"regexp"
	"strings"
)

// The merge guard is a deterministic backstop against a model's own initiative
// expressed through a Bash string — "I'll just merge it myself" — not a
// sandbox. It is pure string matching over a command that has not run yet, so
// a determined or obfuscated invocation (a base64-encoded script, an alias, a
// wrapper binary) can walk right past it. That is an accepted gap, not an
// oversight: the guarantees that actually hold are structural, not textual.
//
// The load-bearing ones live elsewhere:
//   - The supervising session's own --tools registry has no Bash in it at all
//     (see AGENTS.md, "Why the parent cannot write"), so the parent can never
//     reach this path in the first place.
//   - internal/gitops only ever pushes the engineer's own branch, with a fixed
//     argv it chooses itself, in Go, deterministically — never through a model.
//
// This guard exists for the layer between those two: a child (or an engineer's
// own supervised session) that does have Bash, catching the common, unobfuscated
// case before it ever counts down. And because a remote engineer runs this exact
// same ui.Model through internal/supervisor, that engineer inherits this guard
// for free — there is no second copy to keep in sync.
//
// mergeGuardVerdict must never shell out, block, or perform I/O: gate.go's
// enqueue runs on the Bubble Tea update loop, and this is consulted on every
// call before a countdown is even considered.

// protectedBranches is the set of branch names a git push must never resolve
// to. main/master are always protected; m.trunk (the fleet's configured base
// branch, Config.Trunk) is added when set, since arch mode's trunk is often
// neither name.
func (m *Model) protectedBranches() map[string]bool {
	protected := map[string]bool{"main": true, "master": true}
	if m.trunk != "" {
		protected[m.trunk] = true
	}
	return protected
}

// mergeCommandSplitRE splits a shell command into independently-evaluated
// segments on the operators that chain distinct invocations together. A
// single Bash call can chain several commands, and a deny on any one of them
// denies the whole call.
var mergeCommandSplitRE = regexp.MustCompile(`&&|\|\||[;|]`)

// ghPRMergeRE matches `gh pr merge` as a subcommand, e.g. "gh pr merge 1" or
// "gh pr merge --auto".
var ghPRMergeRE = regexp.MustCompile(`\bgh\s+pr\s+merge\b`)

// ghAPIRE matches a `gh api` invocation.
var ghAPIRE = regexp.MustCompile(`\bgh\s+api\b`)

// mergesPathRE matches a `/merges` path component, as in `gh api
// repos/o/r/merges`.
var mergesPathRE = regexp.MustCompile(`/merges\b`)

// mergeGuardVerdict decides whether a tool call must be denied outright
// because it would merge a PR or push to a protected branch. Only the Bash
// tool is inspected — every other tool name returns false immediately — and
// the decision is made by string matching on the command text already
// sitting in rawInput, never by running anything.
func mergeGuardVerdict(tool string, rawInput json.RawMessage, protected map[string]bool) (deny bool, reason string) {
	if tool != "Bash" {
		return false, ""
	}
	var obj struct {
		Command string `json:"command"`
	}
	if len(rawInput) == 0 || json.Unmarshal(rawInput, &obj) != nil {
		return false, ""
	}
	for _, seg := range mergeCommandSplitRE.Split(obj.Command, -1) {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		if ghPRMergeRE.MatchString(seg) {
			return true, "gh pr merge is not allowed"
		}
		if ghAPIRE.MatchString(seg) && mergesPathRE.MatchString(seg) {
			return true, "gh api against a /merges endpoint is not allowed"
		}
		if branches, bulk, ok := pushDestinations(seg); ok {
			if bulk {
				return true, "git push --all/--mirror pushes every branch, including protected ones"
			}
			for _, branch := range branches {
				if protected[branch] {
					return true, "git push targets protected branch " + branch
				}
			}
		}
	}
	return false, ""
}

// pushDestinations resolves every branch a `git push` invocation would push
// to, purely from the command text — `git push` takes `[<refspec>...]`, so a
// single invocation can target several branches at once and each one must be
// checked, not just the first. It reports ok=false whenever the string alone
// does not resolve to any destination — a bare `git push`, or `git push -u
// origin HEAD` where the refspec is literally HEAD — rather than guessing.
//
// bulk=true means the invocation is `--all` or `--mirror`: per git-push(1)
// both push every local branch (refs/heads/*) to the remote regardless of
// any refspec, so they touch protected branches by definition and are always
// denied.
func pushDestinations(seg string) (branches []string, bulk bool, ok bool) {
	fields := strings.Fields(seg)
	if len(fields) < 2 || fields[0] != "git" || fields[1] != "push" {
		return nil, false, false
	}
	var positional []string
	for _, f := range fields[2:] {
		if f == "--all" || f == "--mirror" {
			bulk = true
			continue
		}
		if strings.HasPrefix(f, "-") {
			continue
		}
		positional = append(positional, f)
	}
	if bulk {
		return nil, true, true
	}
	if len(positional) < 2 {
		return nil, false, false
	}
	for _, refspec := range positional[1:] {
		refspec = strings.TrimPrefix(refspec, "+")
		dest := refspec
		if i := strings.Index(refspec, ":"); i >= 0 {
			dest = refspec[i+1:]
		}
		dest = strings.TrimPrefix(dest, "refs/heads/")
		if dest == "" || dest == "HEAD" {
			continue
		}
		branches = append(branches, dest)
	}
	if len(branches) == 0 {
		return nil, false, false
	}
	return branches, false, true
}
