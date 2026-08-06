package fleet

import (
	"reflect"
	"testing"
)

func TestStartArgs(t *testing.T) {
	got := startArgs("/srv/repo")
	want := []string{"engineer", "start", "--clone", "/srv/repo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("startArgs = %v, want %v", got, want)
	}
}

func TestAttachArgs(t *testing.T) {
	cases := []struct {
		name       string
		engineerID string
		fromSeq    int64
		want       []string
	}{
		{"from 1", "e123", 1, []string{"engineer", "attach", "e123", "--from", "1"}},
		{"resume mid-stream", "e123", 42, []string{"engineer", "attach", "e123", "--from", "42"}},
		{"zero", "e999", 0, []string{"engineer", "attach", "e999", "--from", "0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := attachArgs(tc.engineerID, tc.fromSeq)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("attachArgs(%q, %d) = %v, want %v", tc.engineerID, tc.fromSeq, got, tc.want)
			}
		})
	}
}

func TestSSHArgs(t *testing.T) {
	cases := []struct {
		name         string
		target       string
		acyBin       string
		engineerArgs []string
		dirs         []string
		want         []string
	}{
		{
			name:         "start, no path — unquoted argv, unchanged",
			target:       "user@box1",
			acyBin:       "/opt/acy",
			engineerArgs: []string{"engineer", "start", "--clone", "/srv/repo"},
			want: []string{
				"-o", "BatchMode=yes",
				"-o", "ServerAliveInterval=15",
				"-o", "ServerAliveCountMax=4",
				"user@box1",
				"--",
				"/opt/acy",
				"engineer", "start", "--clone", "/srv/repo",
			},
		},
		{
			name:         "attach, no path — unquoted argv, unchanged",
			target:       "box2",
			acyBin:       "acy",
			engineerArgs: attachArgs("e1", 7),
			want: []string{
				"-o", "BatchMode=yes",
				"-o", "ServerAliveInterval=15",
				"-o", "ServerAliveCountMax=4",
				"box2",
				"--",
				"acy",
				"engineer", "attach", "e1", "--from", "7",
			},
		},
		{
			name:         "start, one path dir — wrapped as a single export PATH; exec string",
			target:       "user@box1",
			acyBin:       "/opt/acy",
			engineerArgs: []string{"engineer", "start", "--clone", "/srv/repo"},
			dirs:         []string{"/opt/homebrew/bin"},
			want: []string{
				"-o", "BatchMode=yes",
				"-o", "ServerAliveInterval=15",
				"-o", "ServerAliveCountMax=4",
				"user@box1",
				"--",
				"export PATH='/opt/homebrew/bin':$PATH; exec '/opt/acy' 'engineer' 'start' '--clone' '/srv/repo'",
			},
		},
		{
			name:         "attach, multiple path dirs — joined in order, still one string",
			target:       "box2",
			acyBin:       "acy",
			engineerArgs: attachArgs("e1", 7),
			dirs:         []string{"/opt/homebrew/bin", "/home/box2/.local/bin"},
			want: []string{
				"-o", "BatchMode=yes",
				"-o", "ServerAliveInterval=15",
				"-o", "ServerAliveCountMax=4",
				"box2",
				"--",
				"export PATH='/opt/homebrew/bin':'/home/box2/.local/bin':$PATH; exec 'acy' 'engineer' 'attach' 'e1' '--from' '7'",
			},
		},
		{
			name:         "path dir containing a space is quoted intact",
			target:       "box3",
			acyBin:       "acy",
			engineerArgs: []string{"engineer", "start", "--clone", "/srv/repo"},
			dirs:         []string{"/Users/has space/bin"},
			want: []string{
				"-o", "BatchMode=yes",
				"-o", "ServerAliveInterval=15",
				"-o", "ServerAliveCountMax=4",
				"box3",
				"--",
				"export PATH='/Users/has space/bin':$PATH; exec 'acy' 'engineer' 'start' '--clone' '/srv/repo'",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sshArgs(tc.target, tc.acyBin, tc.engineerArgs, tc.dirs)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("sshArgs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSSHDoctorArgs(t *testing.T) {
	cases := []struct {
		name string
		dirs []string
		cmd  string
		args []string
		want []string
	}{
		{
			name: "no path — still quoted (doctor always quotes)",
			cmd:  "sh",
			args: []string{"-c", "command -v claude"},
			want: []string{
				"-o", "BatchMode=yes",
				"-o", "ServerAliveInterval=15",
				"-o", "ServerAliveCountMax=4",
				"box1",
				"--",
				"'sh' '-c' 'command -v claude'",
			},
		},
		{
			name: "one path dir — export PATH prefix",
			dirs: []string{"/opt/homebrew/bin"},
			cmd:  "claude",
			args: []string{"auth", "status", "--json"},
			want: []string{
				"-o", "BatchMode=yes",
				"-o", "ServerAliveInterval=15",
				"-o", "ServerAliveCountMax=4",
				"box1",
				"--",
				"export PATH='/opt/homebrew/bin':$PATH; exec 'claude' 'auth' 'status' '--json'",
			},
		},
		{
			name: "multiple path dirs, in order",
			dirs: []string{"/opt/homebrew/bin", "/home/box1/.local/bin"},
			cmd:  "gh",
			args: []string{"auth", "status"},
			want: []string{
				"-o", "BatchMode=yes",
				"-o", "ServerAliveInterval=15",
				"-o", "ServerAliveCountMax=4",
				"box1",
				"--",
				"export PATH='/opt/homebrew/bin':'/home/box1/.local/bin':$PATH; exec 'gh' 'auth' 'status'",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sshDoctorArgs("box1", tc.dirs, tc.cmd, tc.args)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("sshDoctorArgs = %v, want %v", got, tc.want)
			}
		})
	}
}
