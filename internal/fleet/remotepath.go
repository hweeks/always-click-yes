package fleet

import "strings"

// pathPreamble returns the `export PATH=...; exec ` prefix that puts dirs
// ahead of whatever PATH the command that follows would otherwise see, or ""
// when dirs is empty. Non-interactive ssh (`ssh host cmd`) hands the remote
// command a minimal PATH — typically just /usr/bin:/bin — so a binary that
// lives somewhere like ~/.local/bin or /opt/homebrew/bin is invisible both
// to the command itself and to anything it execs afterward. That second
// part is what makes this matter beyond doctor's own probes: a detached
// engineer daemon launched this way passes its environment straight to its
// children (claude, gh, git), so the fix has to live in the command ssh
// actually runs, not in some wrapper around it.
//
// Each directory is shell-quoted individually — a fleet host's path entries
// are operator-configured, but a directory name is still not something to
// splice into a shell command unquoted.
func pathPreamble(dirs []string) string {
	if len(dirs) == 0 {
		return ""
	}
	quoted := make([]string, len(dirs))
	for i, d := range dirs {
		quoted[i] = shellQuote(d)
	}
	return "export PATH=" + strings.Join(quoted, ":") + ":$PATH; exec "
}

// quoteArgv shell-quotes every element of argv and joins them with a space.
// This is the single string ssh ends up forwarding to the remote shell
// either way — passed as one argument or several, ssh concatenates a
// multi-argument remote command with spaces before it ever reaches the
// wire — so composing that string ourselves, quoted, is what keeps an
// argument containing a space intact instead of coming apart into extra
// positional parameters on the other end.
func quoteArgv(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}
