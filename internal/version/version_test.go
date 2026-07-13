package version

import (
	"runtime/debug"
	"testing"
)

func TestStampWins(t *testing.T) {
	t.Cleanup(func() { stamped = "" })
	stamped = "v1.2.3"

	if got := String(); got != "v1.2.3" {
		t.Errorf("String() = %q, want the release stamp v1.2.3", got)
	}
}

// A local `go build` records no module version, so the VCS revision is all we have.
func TestDevel(t *testing.T) {
	tests := []struct {
		name     string
		settings []debug.BuildSetting
		want     string
	}{
		{
			name:     "clean tree",
			settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "a396321579a6cafe1234"}},
			want:     "(devel)-a396321579a6",
		},
		{
			name: "dirty tree",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "a396321579a6cafe1234"},
				{Key: "vcs.modified", Value: "true"},
			},
			want: "(devel)-a396321579a6-dirty",
		},
		{
			name:     "no vcs info",
			settings: nil,
			want:     "(devel)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := devel(tt.settings); got != tt.want {
				t.Errorf("devel() = %q, want %q", got, tt.want)
			}
		})
	}
}

// String must never return empty: it goes straight into `acy --version`.
func TestStringNeverEmpty(t *testing.T) {
	if String() == "" {
		t.Error("String() returned empty")
	}
}
