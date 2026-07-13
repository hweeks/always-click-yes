package ui

import "testing"

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in   string
		name string
		args string
		ok   bool
	}{
		{"/help", "help", "", true},
		{"  /resume  ", "resume", "", true},
		{"/resume abc123", "resume", "abc123", true},
		{"/model claude-opus-4-8", "model", "claude-opus-4-8", true},
		{"/RESUME Abc", "resume", "Abc", true}, // name lowercased, args preserved
		{"just text", "", "", false},
		{"", "", "", false},
		{"/", "", "", false},
		{"not/a/command", "", "", false},
	}
	for _, c := range cases {
		name, args, ok := parseCommand(c.in)
		if ok != c.ok || name != c.name || args != c.args {
			t.Errorf("parseCommand(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, name, args, ok, c.name, c.args, c.ok)
		}
	}
}
