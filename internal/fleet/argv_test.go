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
	got := sshArgs("user@box1", "/opt/acy", []string{"engineer", "start", "--clone", "/srv/repo"})
	want := []string{
		"-o", "BatchMode=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"user@box1",
		"--",
		"/opt/acy",
		"engineer", "start", "--clone", "/srv/repo",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sshArgs = %v, want %v", got, want)
	}
}

func TestSSHArgsAttach(t *testing.T) {
	got := sshArgs("box2", "acy", attachArgs("e1", 7))
	want := []string{
		"-o", "BatchMode=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"box2",
		"--",
		"acy",
		"engineer", "attach", "e1", "--from", "7",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sshArgs = %v, want %v", got, want)
	}
}
