package fleet

import (
	"os"
	"testing"

	"github.com/hweeks/always-click-yes/internal/config"
)

func TestForHostLocal(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name         string
		h            config.FleetHost
		wantACYBin   string // "" means "resolves to os.Executable() or acy"
		wantRepoPath string // "" means "resolves to cwd"
	}{
		{
			name:         "acyBin and repoPath set",
			h:            config.FleetHost{Name: "local", ACYBin: "acy", RepoPath: "/srv/repo"},
			wantACYBin:   "acy",
			wantRepoPath: "/srv/repo",
		},
		{
			name:         "no repoPath defaults to cwd",
			h:            config.FleetHost{Name: "local", ACYBin: "acy"},
			wantACYBin:   "acy",
			wantRepoPath: wd,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := ForHost(tc.h)
			lt, ok := tr.(*localTransport)
			if !ok {
				t.Fatalf("ForHost(%+v) = %T, want *localTransport", tc.h, tr)
			}
			if lt.acyBin != tc.wantACYBin {
				t.Errorf("acyBin = %q, want %q", lt.acyBin, tc.wantACYBin)
			}
			if lt.clonePath != tc.wantRepoPath {
				t.Errorf("clonePath = %q, want %q", lt.clonePath, tc.wantRepoPath)
			}
		})
	}
}

// An empty ACYBin resolves to this process's own binary, same as
// NewLocalTransport's default — ForHost must not invent a different rule.
func TestForHostLocalEmptyACYBinResolvesToThisBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("os.Executable unavailable in this environment")
	}
	tr := ForHost(config.FleetHost{Name: "local", RepoPath: "/srv/repo"})
	lt, ok := tr.(*localTransport)
	if !ok {
		t.Fatalf("ForHost = %T, want *localTransport", tr)
	}
	if lt.acyBin != exe {
		t.Errorf("acyBin = %q, want %q", lt.acyBin, exe)
	}
}

func TestForHostSSH(t *testing.T) {
	cases := []struct {
		name       string
		h          config.FleetHost
		wantACYBin string
	}{
		{
			name:       "acyBin set",
			h:          config.FleetHost{Name: "box1", SSH: "user@box1", ACYBin: "/opt/acy", RepoPath: "/srv/repo"},
			wantACYBin: "/opt/acy",
		},
		{
			name:       "acyBin empty defaults to acy on remote PATH",
			h:          config.FleetHost{Name: "box2", SSH: "box2", RepoPath: "/srv/repo"},
			wantACYBin: "acy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := ForHost(tc.h)
			st, ok := tr.(*sshTransport)
			if !ok {
				t.Fatalf("ForHost(%+v) = %T, want *sshTransport", tc.h, tr)
			}
			if st.target != tc.h.SSH {
				t.Errorf("target = %q, want %q", st.target, tc.h.SSH)
			}
			if st.acyBin != tc.wantACYBin {
				t.Errorf("acyBin = %q, want %q", st.acyBin, tc.wantACYBin)
			}
			if st.clonePath != tc.h.RepoPath {
				t.Errorf("clonePath = %q, want %q", st.clonePath, tc.h.RepoPath)
			}
		})
	}
}

// FleetHost.Path must reach the sshTransport unchanged — that's what makes
// it available to sshArgs when Start/Attach build the remote command.
func TestForHostSSHCarriesPath(t *testing.T) {
	h := config.FleetHost{Name: "box1", SSH: "user@box1", RepoPath: "/srv/repo", Path: []string{"/opt/homebrew/bin"}}
	tr := ForHost(h)
	st, ok := tr.(*sshTransport)
	if !ok {
		t.Fatalf("ForHost(%+v) = %T, want *sshTransport", h, tr)
	}
	if len(st.path) != 1 || st.path[0] != "/opt/homebrew/bin" {
		t.Errorf("path = %v, want [/opt/homebrew/bin]", st.path)
	}
}

// FleetHost.Rc must reach the sshTransport unchanged, same as Path above —
// that's what makes it available to sshArgs when Start/Attach build the
// remote command.
func TestForHostSSHCarriesRc(t *testing.T) {
	h := config.FleetHost{Name: "box1", SSH: "user@box1", RepoPath: "/srv/repo", Rc: "~/.zshrc"}
	tr := ForHost(h)
	st, ok := tr.(*sshTransport)
	if !ok {
		t.Fatalf("ForHost(%+v) = %T, want *sshTransport", h, tr)
	}
	if st.rc != "~/.zshrc" {
		t.Errorf("rc = %q, want ~/.zshrc", st.rc)
	}
}
