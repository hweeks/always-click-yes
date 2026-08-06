package fleet

import "testing"

func TestPathPreamble(t *testing.T) {
	cases := []struct {
		name string
		dirs []string
		want string
	}{
		{"empty is empty", nil, ""},
		{"one dir", []string{"/opt/homebrew/bin"}, "export PATH='/opt/homebrew/bin':$PATH; exec "},
		{
			"multiple dirs, in order",
			[]string{"/opt/homebrew/bin", "/home/box1/.local/bin"},
			"export PATH='/opt/homebrew/bin':'/home/box1/.local/bin':$PATH; exec ",
		},
		{
			"a dir containing a space is quoted intact",
			[]string{"/Users/has space/bin"},
			"export PATH='/Users/has space/bin':$PATH; exec ",
		},
		{
			"a dir containing a single quote is escaped",
			[]string{"/opt/o'brien/bin"},
			`export PATH='/opt/o'\''brien/bin':$PATH; exec `,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathPreamble(tc.dirs); got != tc.want {
				t.Errorf("pathPreamble(%v) = %q, want %q", tc.dirs, got, tc.want)
			}
		})
	}
}

func TestQuoteArgv(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"empty", nil, ""},
		{"single element", []string{"claude"}, "'claude'"},
		{"several elements", []string{"claude", "auth", "status", "--json"}, "'claude' 'auth' 'status' '--json'"},
		{"element with a space", []string{"command -v claude"}, "'command -v claude'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quoteArgv(tc.argv); got != tc.want {
				t.Errorf("quoteArgv(%v) = %q, want %q", tc.argv, got, tc.want)
			}
		})
	}
}

func TestRcWrap(t *testing.T) {
	cases := []struct {
		name  string
		rc    string
		shell string
		inner string
		want  string
	}{
		{"empty rc leaves inner unchanged", "", "", "true", "true"},
		{
			"empty rc leaves inner unchanged even with an explicit shell override (rc is what gates wrapping)",
			"", "bash", "true", "true",
		},
		{
			"empty rc leaves an already-quoted composition unchanged (regression: byte-identical to no rc configured)",
			"", "", "export PATH='/opt/homebrew/bin':$PATH; exec 'acy' 'engineer' 'start'",
			"export PATH='/opt/homebrew/bin':$PATH; exec 'acy' 'engineer' 'start'",
		},
		{
			"rc set, tilde path, zsh derived from .zshrc — unquoted so the remote shell expands it",
			"~/.zshrc", "", "true",
			"zsh -c 'source ~/.zshrc >/dev/null 2>&1; true'",
		},
		{
			"zsh derived from .zprofile",
			"~/.zprofile", "", "true",
			"zsh -c 'source ~/.zprofile >/dev/null 2>&1; true'",
		},
		{
			"zsh derived from .zshenv",
			"~/.zshenv", "", "true",
			"zsh -c 'source ~/.zshenv >/dev/null 2>&1; true'",
		},
		{
			"bash derived from .bashrc",
			"~/.bashrc", "", "true",
			"bash -c 'source ~/.bashrc >/dev/null 2>&1; true'",
		},
		{
			"bash derived from .bash_profile",
			"~/.bash_profile", "", "true",
			"bash -c 'source ~/.bash_profile >/dev/null 2>&1; true'",
		},
		{
			"bash derived from .profile",
			"~/.profile", "", "true",
			"bash -c 'source ~/.profile >/dev/null 2>&1; true'",
		},
		{
			"unrecognised basename falls back to sh, not zsh (an absolute path with no leading dot)",
			"/etc/profile", "", "true",
			"sh -c 'source /etc/profile >/dev/null 2>&1; true'",
		},
		{
			"unrecognised basename falls back to sh, not zsh (an arbitrary dotfile)",
			"~/.cshrc", "", "true",
			"sh -c 'source ~/.cshrc >/dev/null 2>&1; true'",
		},
		{
			"explicit shell override wins over derivation from a recognised bash rc",
			"~/.bashrc", "fish", "true",
			"fish -c 'source ~/.bashrc >/dev/null 2>&1; true'",
		},
		{
			"rc set, inner already single-quoted — the quotes get escaped for the outer wrap",
			"~/.zshrc", "", "'acy'",
			`zsh -c 'source ~/.zshrc >/dev/null 2>&1; '\''acy'\'''`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rcWrap(tc.rc, tc.shell, tc.inner); got != tc.want {
				t.Errorf("rcWrap(%q, %q, %q) = %q, want %q", tc.rc, tc.shell, tc.inner, got, tc.want)
			}
		})
	}
}

func TestShellForRc(t *testing.T) {
	cases := []struct {
		rc   string
		want string
	}{
		{"~/.zshrc", "zsh"},
		{"~/.zprofile", "zsh"},
		{"~/.zshenv", "zsh"},
		{"~/.bashrc", "bash"},
		{"~/.bash_profile", "bash"},
		{"~/.profile", "bash"},
		{"/etc/profile", "sh"},
		{"~/.cshrc", "sh"},
	}
	for _, tc := range cases {
		t.Run(tc.rc, func(t *testing.T) {
			if got := shellForRc(tc.rc); got != tc.want {
				t.Errorf("shellForRc(%q) = %q, want %q", tc.rc, got, tc.want)
			}
		})
	}
}

func TestShellFor(t *testing.T) {
	cases := []struct {
		name     string
		rc       string
		override string
		want     string
	}{
		{"no override derives from rc", "~/.zshrc", "", "zsh"},
		{"override wins even for a recognised rc", "~/.zshrc", "fish", "fish"},
		{"override wins for an unrecognised rc too", "/etc/profile", "dash", "dash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shellFor(tc.rc, tc.override); got != tc.want {
				t.Errorf("shellFor(%q, %q) = %q, want %q", tc.rc, tc.override, got, tc.want)
			}
		})
	}
}
