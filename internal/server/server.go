// Package server puts a supervised run on the wire.
//
// internal/hub is the runtime — one ui.Model, one goroutine, a stream of frames
// — and this is the transport in front of it: the frame projection over HTTP,
// the action vocabulary over POST, and the stylesheet the server-rendered
// transcript needs to be legible. The client it exists for is a VS Code webview,
// which cannot reach into a Go process and must not be a second implementation
// of the UI.
//
// Nothing here understands the run. Every route is a thin translation of
// something docs/webui-protocol.md already specifies: GET a frame, POST an
// action, subscribe to frames. A refusal from the model is a 200 carrying the
// refusal, because the client asked a legitimate question and got a legitimate
// answer — see handleAction.
//
// # Where the security actually lives
//
// Two things, and it is worth being precise about which does what.
//
// The listener binds loopback only, so nothing off this machine can reach it at
// all. The bearer token then defends against everything else on this machine
// (another user's process, a page in the browser): every /api/* route requires
// it, compared in constant time. It is minted per run and printed once, on
// stdout, to the parent process that launched acy.
//
// CORS grants nothing. It exists because a webview is genuinely cross-origin —
// it is served from vscode-webview://<uuid> and fetches http://127.0.0.1:<port>
// — so a request carrying an Authorization header triggers a preflight that
// something has to answer. The grant is reflected only for origins we recognise,
// never `*`, and a page that is not one of them gets no CORS headers whatsoever:
// its fetch fails in the browser before it can read a byte, and it never had the
// token to begin with. See cors.go.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hweeks/always-click-yes/internal/alog"
	"github.com/hweeks/always-click-yes/internal/htmlrender"
	"github.com/hweeks/always-click-yes/internal/hub"
	"github.com/hweeks/always-click-yes/internal/session"
	"github.com/hweeks/always-click-yes/internal/state"
	"github.com/hweeks/always-click-yes/internal/ui"
)

const (
	// maxActionBytes bounds a POSTed action. Generous, because Submit carries
	// whatever a person pasted into the composer — a plan document is not a
	// suspicious request — and still a bound, because an unbounded read from a
	// socket is how a local process turns a supervisor into a memory hog.
	maxActionBytes = 1 << 20

	// shutdownGrace is how long Close waits for in-flight handlers before it
	// stops being polite. The SSE streams are already being told to leave by
	// their own channel, so this only covers a request mid-write.
	shutdownGrace = 2 * time.Second

	// tokenBytes is 256 bits of crypto/rand. The token is the only thing between
	// another local process and a session that can run tools.
	tokenBytes = 32
)

// Options configures a Server. The zero value is usable: it listens on
// 127.0.0.1:0 with a freshly minted token and serves an empty session list.
type Options struct {
	// Addr is the listen address. Empty means 127.0.0.1:0 — loopback, and a port
	// the kernel picks, which is what a parent process reading URL off stdout
	// wants. A non-loopback host is refused rather than quietly bound: this
	// server hands out the ability to run tools on this machine.
	Addr string

	// Token is the bearer token every /api/* route requires. Empty mints one.
	Token string

	// AllowOrigins are extra browser origins allowed to preflight, beyond the
	// vscode-webview:// scheme. For developing a client against a dev server;
	// production needs none, and there is deliberately no wildcard.
	AllowOrigins []string

	// Sessions and LoadState build the resume picker's rows, exactly as the
	// terminal's /resume does — they are the same two functions ui.Config takes.
	// Nil Sessions means the picker route answers with an empty list rather than
	// an error: a run with no session lister has nothing to resume, which is not
	// a failure.
	Sessions  func() ([]session.Info, error)
	LoadState func(id string) (state.Snapshot, bool, error)
}

// Server is the HTTP front door for one hub.
type Server struct {
	hub  *hub.Hub
	opts Options

	token string
	ln    net.Listener
	srv   *http.Server

	// closing is how a parked SSE handler learns the server is going away. Without
	// it Shutdown would wait out its grace period on every open stream, since a
	// stream's whole job is to not finish.
	closing   chan struct{}
	closeOnce sync.Once

	startOnce sync.Once
}

// New binds the listener and builds the routes. It does not serve yet — Start
// does — so a caller can learn URL and Token, and print them, before the first
// request can arrive.
//
// Binding here rather than in Start is what makes "the port is up" and "the
// endpoint line has been printed" the same moment: an error binding is returned
// to the caller instead of turning up asynchronously in a goroutine nobody is
// watching.
func New(h *hub.Hub, opts Options) (*Server, error) {
	if h == nil {
		return nil, errors.New("server: nil hub")
	}
	addr, err := loopbackAddr(opts.Addr)
	if err != nil {
		return nil, err
	}
	token := opts.Token
	if token == "" {
		if token, err = newToken(); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("server: listen on %s: %w", addr, err)
	}

	s := &Server{hub: h, opts: opts, token: token, ln: ln, closing: make(chan struct{})}
	s.srv = &http.Server{
		Handler: s.routes(),
		// No WriteTimeout on purpose: /api/events is a stream that stays open for
		// the length of a run, and a write deadline would sever it on a schedule.
		// ReadHeaderTimeout is the one that still applies — it bounds a connection
		// that opens and then says nothing.
		ReadHeaderTimeout: 10 * time.Second,
	}
	alog.Printf("server: listening on %s", ln.Addr())
	return s, nil
}

// Start begins serving. It returns immediately; the server runs until Close.
func (s *Server) Start() {
	s.startOnce.Do(func() {
		go func() {
			if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				alog.Printf("server: serve stopped: %v", err)
			}
		}()
	})
}

// URL is where the server is listening, ready to be handed to a client.
func (s *Server) URL() string { return "http://" + s.ln.Addr().String() }

// Token is the bearer token every /api/* route requires.
func (s *Server) Token() string { return s.token }

// Close stops the server and waits for its handlers, and is safe to call twice.
//
// It does not touch the hub: this server did not start the run and does not get
// to end it. The caller that owns both closes them in that order.
func (s *Server) Close() error {
	s.closeOnce.Do(func() { close(s.closing) })

	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err := s.srv.Shutdown(ctx)
	if err != nil {
		// A handler that would not leave within the grace period gets its
		// connection cut. Better a severed stream than a supervisor that will not
		// exit when a person pressed Ctrl+C.
		_ = s.srv.Close()
	}
	return err
}

// routes is the whole surface. The middleware order is load-bearing: CORS
// outermost, because a browser preflight arrives *without* the Authorization
// header it is asking permission to send, and an auth check in front of it would
// answer 401 to a request that is only asking whether it may try.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/frame", s.handleFrame)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/action", s.handleAction)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("GET /api/highlight.css", s.handleHighlightCSS)
	return s.cors(s.auth(mux))
}

// auth requires a bearer token on everything under /api/.
//
// /healthz is deliberately outside it: a parent process needs to know the server
// is up, that is not a secret, and a health check that needs a credential is one
// more thing to get wrong at startup. It says nothing about the run.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && !s.authorized(r) {
			// WWW-Authenticate so a client library knows what was wanted; no detail
			// about what was wrong with the token it sent.
			w.Header().Set("WWW-Authenticate", `Bearer realm="acy"`)
			writeError(w, http.StatusUnauthorized, "a bearer token is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorized compares the presented token in constant time. A byte-by-byte
// comparison that returns early leaks the token one character at a time to
// anything that can time a local request, which is every process on this machine.
func (s *Server) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	got := strings.TrimSpace(h[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

// handleHealth answers that the process is alive and nothing else. No token, and
// no secrets in the body: this is the one route an unauthenticated caller can
// reach, so it must not become a place to learn the run's phase, its cwd, or its
// token.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleFrame answers with the current frame, built on demand.
func (s *Server) handleFrame(w http.ResponseWriter, _ *http.Request) {
	u := s.hub.Current()
	if u.JSON == nil {
		// The model has not produced a frame yet. Not an error in the request, so
		// not a 4xx; the client should try again.
		writeError(w, http.StatusServiceUnavailable, "no frame yet")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// The revision the body is, so a client can correlate a fetched frame with the
	// event stream's ids without parsing the body first.
	w.Header().Set("X-Acy-Rev", fmt.Sprint(u.Rev))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(u.JSON)
}

// handleAction decodes one action, performs it, and returns the model's verdict.
//
// The status codes here are the whole design of this route:
//
//   - 400 for a body that is not an action — malformed JSON, or a kind this
//     build has no vocabulary for. The client sent something meaningless.
//   - 200 for an action the model *refused*. A refusal is a domain answer, not a
//     transport failure: the client is describing a run it last saw a moment ago,
//     and gates auto-approve, turns end, phases move. It needs the reason string
//     either way, and an error status would push callers into treating "that gate
//     is already gone" as a bug in the connection.
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	var a ui.Action
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxActionBytes))
	if err := dec.Decode(&a); err != nil {
		writeError(w, http.StatusBadRequest, "malformed action: "+err.Error())
		return
	}
	if !a.Kind.Valid() {
		writeError(w, http.StatusBadRequest, "unknown action kind "+strconv.Quote(string(a.Kind)))
		return
	}
	writeJSON(w, http.StatusOK, s.hub.Do(a))
}

// handleSessions serves the resume picker's rows.
//
// It builds them with ui.SessionRows — the same call the terminal's /resume
// picker makes — so the two front ends cannot offer different lists, hide
// different children, or disagree about which runs acy supervised.
func (s *Server) handleSessions(w http.ResponseWriter, _ *http.Request) {
	if s.opts.Sessions == nil {
		writeJSON(w, http.StatusOK, []ui.SessionRow{})
		return
	}
	list, err := s.opts.Sessions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "listing sessions: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ui.SessionRows(list, s.opts.LoadState))
}

// handleHighlightCSS serves the theme.
//
// A transcript fragment carries class names and no colors — the webview's CSP
// forbids inline styles, and a client that switches from dark to light must not
// have to re-render entries it already has. So the palette lives in exactly one
// document, and this is it. A server that serves frames without serving this has
// shipped an unreadable transcript.
func (s *Server) handleHighlightCSS(w http.ResponseWriter, r *http.Request) {
	var theme htmlrender.Theme
	switch r.URL.Query().Get("theme") {
	case "", "dark":
		theme = htmlrender.ThemeDark
	case "light":
		theme = htmlrender.ThemeLight
	default:
		writeError(w, http.StatusBadRequest, `theme must be "dark" or "light"`)
		return
	}
	css, err := htmlrender.StyleSheet(theme)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(css))
}

// loopbackAddr normalises a listen address and refuses anything that is not
// loopback.
//
// This server exposes the ability to approve tool calls on this machine. Binding
// it to 0.0.0.0 because a config said so would put that on the network, so a
// non-loopback host is an error rather than a warning nobody reads.
func loopbackAddr(addr string) (string, error) {
	if addr == "" {
		return "127.0.0.1:0", nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("server: address %q must be host:port: %w", addr, err)
	}
	if host == "" || host == "localhost" {
		return net.JoinHostPort("127.0.0.1", port), nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("server: %q is not a loopback address; acy serves 127.0.0.1 only", host)
	}
	return net.JoinHostPort(host, port), nil
}

// newToken mints 256 bits of randomness, hex-encoded so it survives a JSON line,
// a shell variable and an HTTP header unchanged.
func newToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("server: generating a token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// writeJSON is the one place a response body is encoded, so every route answers
// with the same content type and the same failure behaviour.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// Nothing useful left to say to the client — the header is not written
		// yet, so a 500 is still possible, and the log is where this belongs.
		alog.Printf("server: marshalling a %d response: %v", status, err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError answers with the same JSON shape every failure uses, so a client
// has one error decoder rather than one per route.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
