package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// serveOnlyFlags are the flags `serve` has that `run` has no use for: where to
// listen, and what token to demand. Everything else must exist on both.
var serveOnlyFlags = []string{"port", "token"}

// flagNames is every flag registered on a command, sorted.
func flagNames(fs *pflag.FlagSet) []string {
	var out []string
	fs.VisitAll(func(f *pflag.Flag) { out = append(out, f.Name) })
	slices.Sort(out)
	return out
}

// `serve` is `run` with a different front end, which means it has to accept the
// same run settings — model, countdown, child knobs, resume, all of it. A client
// that can only start acy through `serve` must not be stuck with a lesser acy.
//
// They share one registration (addRunFlags) precisely so they cannot drift, and
// this is the test that says so: a flag added to one and not the other would
// otherwise fail silently, since `acy serve --child-effort high` that parses
// nowhere simply does nothing.
func TestServeAndRunExposeTheSameRunFlags(t *testing.T) {
	run := flagNames(newRunCmd().Flags())
	serve := flagNames(newServeCmd().Flags())

	for _, name := range run {
		if !slices.Contains(serve, name) {
			t.Errorf("run has --%s and serve does not", name)
		}
	}
	for _, name := range serve {
		if slices.Contains(serveOnlyFlags, name) {
			continue
		}
		if !slices.Contains(run, name) {
			t.Errorf("serve has --%s and run does not — either share it or make it serve-only on purpose", name)
		}
	}
	for _, name := range serveOnlyFlags {
		if !slices.Contains(serve, name) {
			t.Errorf("serve should have --%s", name)
		}
	}
}

// The settings themselves have to match, not just the names: a --countdown that
// defaulted to 30s in one command and 5s in the other would be the same drift
// wearing a disguise.
func TestServeAndRunFlagDefaultsMatch(t *testing.T) {
	run := newRunCmd().Flags()
	serve := newServeCmd().Flags()

	run.VisitAll(func(f *pflag.Flag) {
		got := serve.Lookup(f.Name)
		if got == nil {
			return // reported by the test above
		}
		if got.DefValue != f.DefValue {
			t.Errorf("--%s defaults to %q on run and %q on serve", f.Name, f.DefValue, got.DefValue)
		}
		if got.Usage != f.Usage {
			t.Errorf("--%s is documented differently on run and serve", f.Name)
		}
	})
}

// The .acy.json overlay keys on cobra's Changed, so each command must own its
// own pflag instances: sharing them would make a flag set on one command look
// explicitly set on the other, and the file's value would stop applying.
func TestServeAndRunDoNotShareFlagInstances(t *testing.T) {
	run := newRunCmd()
	serve := newServeCmd()

	if err := serve.Flags().Set("model", "opus"); err != nil {
		t.Fatal(err)
	}
	if run.Flags().Changed("model") {
		t.Error("setting --model on serve marked it changed on run — the two share a FlagSet")
	}
	if !serve.Flags().Changed("model") {
		t.Error("serve's own --model did not register as changed")
	}
}

// The endpoint line is a contract with whatever launched acy: exactly one line,
// parseable as JSON, carrying the URL and the token and nothing else.
func TestPrintEndpoint(t *testing.T) {
	var out strings.Builder
	if err := printEndpoint(&out, "http://127.0.0.1:54321", "abc123"); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Fatalf("endpoint output is not exactly one line: %q", got)
	}
	var ep serveEndpoint
	if err := json.Unmarshal([]byte(strings.TrimSpace(got)), &ep); err != nil {
		t.Fatalf("the endpoint line does not parse as JSON: %v\n%s", err, got)
	}
	if ep.URL != "http://127.0.0.1:54321" || ep.Token != "abc123" {
		t.Errorf("endpoint = %+v, want the url and token it was given", ep)
	}
}

// serve is a documented sibling of run, not a hidden command: a client author
// has to be able to find it in --help.
func TestServeIsRegisteredAndVisible(t *testing.T) {
	var serve *cobra.Command
	for _, c := range Root().Commands() {
		if c.Name() == "serve" {
			serve = c
		}
	}
	if serve == nil {
		t.Fatal("Root() has no serve command")
	}
	if serve.Hidden {
		t.Error("serve is hidden; it is a supported mode, not an implementation detail")
	}
	if serve.Short == "" || serve.Long == "" {
		t.Error("serve should carry both a short and a long description")
	}
}
