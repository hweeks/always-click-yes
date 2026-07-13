// Package version reports which build is running, for `acy --version` and bug reports.
package version

import (
	"runtime/debug"
	"strings"
)

// stamped is injected at release-build time with
// -ldflags "-X github.com/hweeks/always-click-yes/internal/version.stamped=v1.2.3".
// Release binaries are built from a plain checkout, where the toolchain records no
// module version, so the tag has to be passed in. It stays empty for `go install`,
// which records the real module version in the build info instead.
var stamped string

// String returns the most specific version available: the release stamp, else the
// module version the toolchain embeds (a tag, or a v0.0.0-<date>-<sha> pseudo-version
// for an untagged commit — a plain `go build` stamps this from VCS too, suffixed
// "+dirty" for a modified tree), else the bare revision. That last fallback only
// happens when there is no VCS info to read: -buildvcs=false, or a source tarball.
func String() string {
	if stamped != "" {
		return stamped
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return devel(bi.Settings)
}

// devel renders a local build from the VCS settings the toolchain stamps in.
func devel(settings []debug.BuildSetting) string {
	var rev string
	var dirty bool
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "(devel)"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	var b strings.Builder
	b.WriteString("(devel)-")
	b.WriteString(rev)
	if dirty {
		b.WriteString("-dirty")
	}
	return b.String()
}
