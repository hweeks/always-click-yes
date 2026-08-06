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
