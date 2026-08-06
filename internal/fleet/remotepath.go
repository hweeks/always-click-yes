package fleet

import (
	"path/filepath"
	"strings"
)

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

// shellForRc derives the login shell that owns rc from its basename: the
// zsh-family dotfiles mean zsh, the bash-family (including plain .profile)
// mean bash, and anything unrecognised falls back to sh — the one shell
// POSIX guarantees exists, rather than assuming the zsh a developer's own
// machine happens to have. This is what fixes rcWrap hardcoding zsh: a host
// configured with an rc file that belongs to some other shell (or a host
// with no zsh installed at all, e.g. plain Ubuntu) used to fail outright
// with "zsh: command not found" no matter which shell rc actually named.
func shellForRc(rc string) string {
	switch filepath.Base(rc) {
	case ".zshrc", ".zprofile", ".zshenv":
		return "zsh"
	case ".bashrc", ".bash_profile", ".profile":
		return "bash"
	default:
		return "sh"
	}
}

// shellFor picks the shell rcWrap sources rc through: override
// (FleetHost.Shell) when the operator set one, else shellForRc's derivation.
func shellFor(rc, override string) string {
	if override != "" {
		return override
	}
	return shellForRc(rc)
}

// rcWrap wraps inner — the composition pathPreamble/quoteArgv already build
// — in `<shell> -c 'source <rc> >/dev/null 2>&1; <inner>'`, so a host's rc
// file (FleetHost.Rc) runs before inner does, or returns inner unchanged
// when rc is empty. shell is shellFor's pick — FleetHost.Shell when set,
// else derived from rc's basename — never a hardcoded assumption. This is
// what makes real hosts work where a fleet `path` entry alone is not
// enough: claude and gh can depend on auth/env wiring only the login
// shell's rc sets up.
//
// rc itself is spliced in unquoted, on purpose: it is validated to start
// with "~/" or "/" (config.FleetHost.Rc), and unlike a fleet `path` entry —
// which is spliced straight into the remote command with no shell of its
// own standing between it and ssh — this string is only ever the argument
// of a `source` call the remote shell itself interprets, so a leading "~"
// is exactly the case that needs the remote shell to expand it, quoting it
// away would defeat the entire point. inner, by contrast, may already
// contain single quotes from quoteArgv, which is why the whole `source ...;
// inner` line is shellQuote'd as one string rather than being spliced in
// raw.
func rcWrap(rc, shell, inner string) string {
	if rc == "" {
		return inner
	}
	return shellFor(rc, shell) + " -c " + shellQuote("source "+rc+" >/dev/null 2>&1; "+inner)
}
