package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/hub"
	"github.com/hweeks/always-click-yes/internal/server"
	"github.com/hweeks/always-click-yes/internal/state"
	"github.com/hweeks/always-click-yes/internal/supervisor"
)

// `acy serve` is `acy run` without a terminal.
//
// Same supervisor, wired by the same NewSupervisor: the same gate socket, the
// same PreToolUse hook settings, the same MCP bridge, the same orchestrator, the
// same persistence. What changes is who drives it — internal/hub instead of
// tea.NewProgram — and that frames and actions travel over HTTP instead of over
// a screen and a keyboard.
//
// It takes every run setting `run` takes, from the same registration
// (addRunFlags), because a client that can only start acy one way must not be
// stuck with a lesser version of it. And it prints exactly one line to stdout —
// the URL and the token — so the process that launched it can connect without
// scraping logs.

// serveEndpoint is that line. It is a contract with whatever launched acy, so
// the field names are protocol: a VS Code extension parses this to know where to
// point its webview and what token to send.
type serveEndpoint struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func newServeCmd() *cobra.Command {
	var f supervisor.Flags
	var port int
	var token string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the supervisor headless and serve it over HTTP for a webview client",
		Long: "serve runs exactly the supervisor `acy run` does — the same gate, the same\n" +
			"hook, the same dispatched children — with no terminal UI. It listens on\n" +
			"127.0.0.1 (port 0 by default, so the kernel picks one), requires a bearer\n" +
			"token on every /api/ request, and prints one line of JSON to stdout as soon\n" +
			"as the listener is up:\n" +
			"\n" +
			"    {\"url\":\"http://127.0.0.1:54321\",\"token\":\"…\"}\n" +
			"\n" +
			"Nothing else is written to stdout, so a parent process can parse that line\n" +
			"directly. See docs/webui-protocol.md for the routes it serves.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return serveSupervisor(cmd.Context(), f, port, token, cmd.Flags().Changed)
		},
	}
	// The same registration `run` uses, so the two cannot drift.
	addRunFlags(cmd, &f)
	cmd.Flags().IntVar(&port, "port", 0, "TCP port to listen on (0 = let the kernel pick a free one); always bound to 127.0.0.1")
	cmd.Flags().StringVar(&token, "token", "", "bearer token required on every /api/ request; empty mints a fresh 256-bit one")
	return cmd
}

// serveSupervisor is runSupervisor's headless sibling.
func serveSupervisor(ctx context.Context, f supervisor.Flags, port int, token string, changed func(string) bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// acy's own signal handling, because there is no Bubble Tea program here to
	// take Ctrl+C for us and nothing above main installs one. Cancelling the
	// context is also what stops the claude processes: NewSupervisor threads it
	// into every driver it launches.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := supervisor.OverlayFileConfig(&f, changed); err != nil {
		return err
	}

	// The two stamps that make this a served run rather than a terminal one.
	//
	// RenderHTML is what fills Frame.Entries[].html: the webview renders no
	// markdown and highlights no code — its CSP forbids inline styles and a
	// second markdown stack would be a second implementation of the transcript —
	// so the fragments have to be produced here. Nothing else sets this, so
	// without it every html field is empty and the client has nothing to show.
	f.RenderHTML = true
	// No terminal: no alternate screen to enter, and deliberately no
	// term.DrainInput. Draining stdin is a fix for a *terminal* that answered a
	// previous process's capability query; here stdin may be a pipe the parent
	// process is using, and reading from it would be taking bytes that are not
	// ours.
	f.AltScreen = false

	sup, err := supervisor.NewSupervisor(ctx, f)
	if err != nil {
		return err
	}
	defer sup.Close()
	// Old snapshots are worth keeping (you may want to resume last week's run) but
	// not forever. Best-effort, exactly as `run` does it.
	defer func() { _ = state.Prune(supervisor.MaxSnapshots) }()

	h := hub.New(sup.Model)
	defer h.Close()

	srv, err := server.New(h, server.Options{
		// The port is all a caller gets to choose. The host is not a setting: this
		// server hands out the ability to approve tool calls on this machine.
		Addr:  net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Token: token,
		// The picker's own sources, so the webview's resume list is the terminal's.
		Sessions:  sup.Sessions,
		LoadState: sup.LoadState,
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := srv.Close(); err != nil {
			alog.Printf("serve: shutting the server down: %v", err)
		}
	}()
	srv.Start()

	if err := printEndpoint(os.Stdout, srv.URL(), srv.Token()); err != nil {
		return err
	}
	alog.Printf("serve: %s", srv.URL())

	// Two ways this ends, and both are ordinary: the person interrupted it, or
	// the run itself finished (/quit, a Quit action, the model's own exit).
	select {
	case <-ctx.Done():
		alog.Printf("serve: signal received, shutting down")
	case <-h.Done():
		alog.Printf("serve: the run ended")
	}
	// The deferred closes then run in the only order that is safe: the server
	// first, so no handler is mid-action; then the hub, so the model has stopped;
	// then the supervisor's own resources — the gate socket, the MCP bridge, the
	// generated settings, the log.
	return nil
}

// printEndpoint writes the single machine-readable line, and nothing else ever
// goes to stdout before it. Encoded rather than fmt'd so a token or a URL can
// never break the line it travels on.
func printEndpoint(w io.Writer, url, token string) error {
	line, err := json.Marshal(serveEndpoint{URL: url, Token: token})
	if err != nil {
		return fmt.Errorf("serve: encoding the endpoint line: %w", err)
	}
	if _, err := fmt.Fprintln(w, string(line)); err != nil {
		return fmt.Errorf("serve: printing the endpoint line: %w", err)
	}
	return nil
}
