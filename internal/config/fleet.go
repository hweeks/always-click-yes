package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// FleetHost describes one machine an engineer can be launched on: the local
// machine when SSH is empty, or a remote target reached over ssh.
type FleetHost struct {
	Name         string `json:"name"`
	SSH          string `json:"ssh,omitempty"`      // ssh target, e.g. "user@host"; empty means local
	RepoPath     string `json:"repoPath,omitempty"` // required when SSH is set; defaults to the project dir when local
	MaxEngineers *int   `json:"maxEngineers,omitempty"`
	ACYBin       string `json:"acyBin,omitempty"`
	// Path is extra directories to prepend to PATH on this host, e.g.
	// ["/opt/homebrew/bin", "/home/you/.local/bin"]. Non-interactive ssh
	// (`ssh host cmd`) hands the remote command a minimal PATH — usually just
	// /usr/bin:/bin — so a binary claude or gh actually depend on that lives
	// somewhere like ~/.local/bin is simply not found, for the doctor checks
	// and for the detached engineer daemon (and everything it execs) alike.
	// Every entry must be an absolute path: it is spliced into a remote shell
	// command, so a "~" entry would never expand there, and a relative one
	// would resolve against whatever directory ssh happens to land in.
	Path []string `json:"path,omitempty"`
	// Rc is a shell rc file to source before every remote command on this
	// host, e.g. "~/.zshrc". On real hosts a fleet `path` extension is often
	// not enough on its own: claude and gh can depend on auth/env wiring only
	// the login shell's rc sets up, not just PATH. Non-empty must start with
	// "~/" or "/" — and unlike Path and RepoPath above, a leading "~" is
	// exactly the point here rather than a mistake: this string is never
	// spliced directly into a command, only ever handed to the remote zsh as
	// the argument of a `source` call (internal/fleet/remotepath.go's
	// rcWrap), so it is the remote shell that expands the tilde, not us.
	Rc string `json:"rc,omitempty"`
	// Shell overrides which shell Rc is sourced through, e.g. "bash" or
	// "fish". Empty means derive it from Rc's basename
	// (internal/fleet/remotepath.go's shellForRc): .zshrc/.zprofile/.zshenv
	// mean zsh, .bashrc/.bash_profile/.profile mean bash, anything
	// unrecognised falls back to sh. Set this when a host's rc file doesn't
	// follow that naming, rather than fighting the derivation.
	Shell string `json:"shell,omitempty"`
}

// FleetConfig is the optional "fleet" object in .acy.json: shared defaults
// for arch mode's engineers, and the hosts they can run on. A project with no
// "fleet" key parses exactly as it always has — File.Fleet stays nil, and
// nothing below runs.
type FleetConfig struct {
	BaseBranch         string   `json:"baseBranch,omitempty"`
	PRCap              *int     `json:"prCap,omitempty"`
	EngineerModel      string   `json:"engineerModel,omitempty"`
	EngineerChildModel string   `json:"engineerChildModel,omitempty"`
	EngineerEffort     string   `json:"engineerEffort,omitempty"`
	EngineerBudgetUSD  *float64 `json:"engineerBudgetUSD,omitempty"`
	RunBudgetUSD       *float64 `json:"runBudgetUSD,omitempty"`
	DeadmanHours       *float64 `json:"deadmanHours,omitempty"`
	TicketCommit       string   `json:"ticketCommit,omitempty"`
	StackMode          string   `json:"stackMode,omitempty"`
	// VerifyCommands are the commands acy itself runs in an engineer's
	// worktree after the run finishes — machine-collected evidence, in
	// contrast with the model's own claim of having run tests.
	// internal/engineer/drive.go's finalize is what reads this.
	VerifyCommands []string `json:"verifyCommands,omitempty"`
	// VerifyTimeoutSeconds is the per-command wall-clock ceiling for
	// VerifyCommands, read by the same place.
	VerifyTimeoutSeconds *int        `json:"verifyTimeoutSeconds,omitempty"`
	Hosts                []FleetHost `json:"hosts,omitempty"`
}

// Defaults for the fleet fields the .acy.json spec calls out by name.
const (
	defaultBaseBranch   = "main"
	defaultPRCap        = 4
	defaultDeadmanHours = 24.0
	defaultTicketCommit = "direct"
	defaultStackMode    = "ask"
	defaultMaxEngineers = 1
	defaultACYBin       = "acy"
	// defaultVerifyTimeoutSeconds is 15 minutes: go test -race ./... on a
	// loaded fleet host is not fast, which is why this isn't shorter.
	defaultVerifyTimeoutSeconds = 900
)

// resolve fills in every default and validates the fleet config, using dir —
// the project directory LoadFile was called with — as a local host's default
// repoPath. It mutates f in place; path is only for error messages.
func (f *FleetConfig) resolve(dir, path string) error {
	if f.BaseBranch == "" {
		f.BaseBranch = defaultBaseBranch
	}

	if f.PRCap == nil {
		n := defaultPRCap
		f.PRCap = &n
	} else if *f.PRCap < 0 {
		return fmt.Errorf("%s: fleet.prCap must be zero or greater", path)
	}

	if f.EngineerBudgetUSD != nil && *f.EngineerBudgetUSD < 0 {
		return fmt.Errorf("%s: fleet.engineerBudgetUSD must be zero or greater", path)
	}
	if f.RunBudgetUSD != nil && *f.RunBudgetUSD < 0 {
		return fmt.Errorf("%s: fleet.runBudgetUSD must be zero or greater", path)
	}

	if f.DeadmanHours == nil {
		h := defaultDeadmanHours
		f.DeadmanHours = &h
	} else if *f.DeadmanHours < 0 {
		return fmt.Errorf("%s: fleet.deadmanHours must be zero or greater", path)
	}

	switch f.TicketCommit {
	case "":
		f.TicketCommit = defaultTicketCommit
	case "direct", "none":
	default:
		return fmt.Errorf("%s: fleet.ticketCommit must be \"direct\" or \"none\", got %q", path, f.TicketCommit)
	}

	// The default is deliberately "ask", not "off": existing arch users get
	// one extra planning question after this change lands. That's
	// intentional, not a bug — don't "fix" it by silently defaulting to
	// "off".
	switch f.StackMode {
	case "":
		f.StackMode = defaultStackMode
	case "off", "ask", "chain":
	default:
		return fmt.Errorf("%s: fleet.stackMode must be \"off\", \"ask\", or \"chain\", got %q", path, f.StackMode)
	}

	// encoding/json leaves f.VerifyCommands nil when "verifyCommands" is
	// absent from the source JSON entirely, but gives a non-nil, zero-length
	// slice for an explicit "verifyCommands": []. That's exactly what lets
	// this nil check tell "absent" (default the four commands below) apart
	// from "explicit []" (disable verification). A later refactor to
	// *[]string, or swapping this for a len(f.VerifyCommands) == 0 check,
	// would silently collapse that distinction — don't make either change
	// without re-reading this comment.
	if f.VerifyCommands == nil {
		f.VerifyCommands = []string{
			"go build ./...",
			"go test -race ./...",
			"gofmt -l .",
			"golangci-lint run ./...",
		}
	}
	for i, cmd := range f.VerifyCommands {
		if strings.TrimSpace(cmd) == "" {
			return fmt.Errorf("%s: fleet.verifyCommands[%d] must not be empty", path, i)
		}
	}

	if f.VerifyTimeoutSeconds == nil {
		n := defaultVerifyTimeoutSeconds
		f.VerifyTimeoutSeconds = &n
	} else if *f.VerifyTimeoutSeconds <= 0 {
		return fmt.Errorf("%s: fleet.verifyTimeoutSeconds must be greater than zero", path)
	}

	seen := make(map[string]bool, len(f.Hosts))
	for i := range f.Hosts {
		h := &f.Hosts[i]
		if h.Name == "" {
			return fmt.Errorf("%s: fleet.hosts[%d] is missing a name", path, i)
		}
		if seen[h.Name] {
			return fmt.Errorf("%s: fleet.hosts has a duplicate name %q", path, h.Name)
		}
		seen[h.Name] = true

		if h.SSH != "" && h.RepoPath == "" {
			return fmt.Errorf("%s: fleet.hosts[%q] sets ssh but no repoPath", path, h.Name)
		}
		if h.SSH == "" && h.RepoPath == "" {
			h.RepoPath = dir
		}

		if h.MaxEngineers == nil {
			n := defaultMaxEngineers
			h.MaxEngineers = &n
		} else if *h.MaxEngineers <= 0 {
			return fmt.Errorf("%s: fleet.hosts[%q].maxEngineers must be greater than zero", path, h.Name)
		}

		if h.ACYBin == "" {
			h.ACYBin = defaultACYBin
		}

		for j, p := range h.Path {
			if !filepath.IsAbs(p) {
				return fmt.Errorf("%s: fleet.hosts[%q].path[%d] must be an absolute path, got %q (a \"~\" entry won't expand where this runs)", path, h.Name, j, p)
			}
		}

		if h.Rc != "" && !strings.HasPrefix(h.Rc, "~/") && !strings.HasPrefix(h.Rc, "/") {
			return fmt.Errorf("%s: fleet.hosts[%q].rc must start with \"~/\" or \"/\", got %q", path, h.Name, h.Rc)
		}
	}
	return nil
}
