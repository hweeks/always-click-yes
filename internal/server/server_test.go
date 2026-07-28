package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/hub"
	"github.com/hweeks/always-click-yes/internal/session"
	"github.com/hweeks/always-click-yes/internal/state"
	"github.com/hweeks/always-click-yes/internal/ui"
)

// The hub under these tests is built the way internal/ui and internal/hub build
// theirs: no driver, no launcher, injected fakes only. Nothing can reach a
// claude process, which is what makes an accepted action and a refused one both
// reachable — SetModel needs nothing, Arm needs a session that does not exist.

const (
	// settle is how long a test waits for something that should already have
	// happened. Generous, because a loaded -race build is slow.
	settle = 5 * time.Second

	testToken = "test-token-not-a-real-one"
)

// testServer wires a Server over a hub and hands back a live httptest.Server.
// The routes are the real ones — httptest only supplies the listener — so the
// middleware order under test is the middleware order in production.
func testServer(t *testing.T, opts Options) (*httptest.Server, *hub.Hub, *Server) {
	t.Helper()
	h := hub.New(ui.New(nil, ui.Config{}))
	t.Cleanup(h.Close)

	if opts.Token == "" {
		opts.Token = testToken
	}
	s, err := New(h, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The listener New bound is not the one httptest will use; close it so the
	// test leaves no stray port behind, and serve the same handler over
	// httptest's instead.
	t.Cleanup(func() { _ = s.Close() })

	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return ts, h, s
}

// get issues a GET, optionally with a bearer token ("" = no Authorization
// header at all, which is a different thing from an empty one).
func get(t *testing.T, ts *httptest.Server, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// postAction posts a raw body to /api/action with the good token.
func postAction(t *testing.T, ts *httptest.Server, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/action", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /api/action: %v", err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func decode[T any](t *testing.T, res *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatalf("decoding a %d response: %v", res.StatusCode, err)
	}
	return v
}

// The token is the whole defence against everything else running on this
// machine, so every /api/ route has to want it — and /healthz has to not, since
// a parent process checks liveness before it has done anything else.
func TestAuth(t *testing.T) {
	ts, _, _ := testServer(t, Options{})

	cases := []struct {
		name  string
		path  string
		token string
		want  int
	}{
		{"frame with no token", "/api/frame", "", http.StatusUnauthorized},
		{"frame with the wrong token", "/api/frame", "not-the-token", http.StatusUnauthorized},
		// Same length as the real one: a comparison that bailed on the first
		// differing byte would still pass the case above, so this is the one that
		// says the check is on the value and not on the shape.
		{"frame with a same-length wrong token", "/api/frame", strings.Repeat("x", len(testToken)), http.StatusUnauthorized},
		{"frame with the right token", "/api/frame", testToken, http.StatusOK},
		{"sessions with no token", "/api/sessions", "", http.StatusUnauthorized},
		{"css with no token", "/api/highlight.css", "", http.StatusUnauthorized},
		{"healthz needs none", "/healthz", "", http.StatusOK},
		{"healthz tolerates one", "/healthz", testToken, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := get(t, ts, tc.path, tc.token)
			if res.StatusCode != tc.want {
				t.Errorf("GET %s = %d, want %d", tc.path, res.StatusCode, tc.want)
			}
		})
	}
}

// /healthz is the one route an unauthenticated caller can reach. It must
// therefore say nothing about the run — and above all not echo the token it
// does not require.
func TestHealthzLeaksNothing(t *testing.T) {
	ts, _, _ := testServer(t, Options{})
	res := get(t, ts, "/healthz", "")

	body := decode[map[string]string](t, res)
	if body["status"] != "ok" {
		t.Errorf("healthz = %v, want a status", body)
	}
	for k, v := range body {
		if strings.Contains(v, testToken) {
			t.Errorf("healthz leaked the token in %q", k)
		}
	}
}

// The frame route is the read seam on the wire: the same bytes the hub built,
// decodable back into a ui.Frame.
func TestFrameRoute(t *testing.T) {
	ts, _, _ := testServer(t, Options{})
	res := get(t, ts, "/api/frame", testToken)

	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	f := decode[ui.Frame](t, res)
	if f.Phase != "PLAN" {
		t.Errorf("phase = %q, want the run's phase", f.Phase)
	}
	// Every list field is an array, never null — the property Frame promises and
	// a client relies on.
	if f.Entries == nil || f.Gates == nil || f.Queue == nil {
		t.Errorf("a list field came back null: entries=%v gates=%v queue=%v", f.Entries, f.Gates, f.Queue)
	}
}

// An action the model accepts, and one it refuses, over the same route.
//
// The refusal is the interesting half: 200, with the verdict in the body. A
// refusal is a domain answer — the client is describing a run it last saw a
// moment ago — and the reason string is exactly what it needs either way.
func TestActionRoute(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantOK     bool
		wantReason string
	}{
		{
			name:       "an accepted action round-trips",
			body:       `{"kind":"setModel","name":"opus"}`,
			wantStatus: http.StatusOK,
			wantOK:     true,
			wantReason: "opus",
		},
		{
			// Arming needs a session id claude has not emitted (there is no driver
			// at all here), so the model legitimately says no.
			name:       "a refused action is still a 200",
			body:       `{"kind":"arm"}`,
			wantStatus: http.StatusOK,
			wantOK:     false,
			wantReason: "no session id yet",
		},
		{
			name:       "malformed JSON is a 400",
			body:       `{"kind":`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "not even JSON is a 400",
			body:       `arm please`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "an unknown kind is a 400",
			body:       `{"kind":"teleport"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "a missing kind is a 400",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, _, _ := testServer(t, Options{})
			res := postAction(t, ts, tc.body)
			if res.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.wantStatus)
			}
			if tc.wantStatus != http.StatusOK {
				// A failure body is the one error shape every route uses.
				if e := decode[map[string]string](t, res)["error"]; e == "" {
					t.Error("a 4xx came back with no error message")
				}
				return
			}
			got := decode[ui.ActionResult](t, res)
			if got.Accepted != tc.wantOK {
				t.Errorf("accepted = %v, want %v (reason %q)", got.Accepted, tc.wantOK, got.Reason)
			}
			if !strings.Contains(got.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", got.Reason, tc.wantReason)
			}
		})
	}
}

// An accepted action really reaches the model, rather than being acknowledged
// and dropped: the next frame carries it.
func TestActionReachesTheModel(t *testing.T) {
	ts, _, _ := testServer(t, Options{})

	if res := postAction(t, ts, `{"kind":"setModel","name":"haiku"}`); res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	f := decode[ui.Frame](t, get(t, ts, "/api/frame", testToken))
	if len(f.Entries) == 0 {
		t.Fatal("the frame has no entries")
	}
	if last := f.Entries[len(f.Entries)-1].Body; !strings.Contains(last, "haiku") {
		t.Errorf("newest entry = %q, want it to record the model change", last)
	}
}

// The stream: connect, change something, receive a frame event carrying the new
// revision. The id is the hub's rev, which is how a client tells a gap from a
// quiet run.
func TestEventsStream(t *testing.T) {
	ts, _, _ := testServer(t, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	events := make(chan sseEvent, 8)
	go readSSE(res.Body, events)

	// The priming frame: a client that connects mid-run renders immediately.
	first := waitFor(t, events, "the priming frame")
	if first.event != "frame" || first.id == 0 {
		t.Fatalf("first event = %+v, want a frame carrying an id", first)
	}
	if f := frameOf(t, first); f.Phase == "" {
		t.Errorf("the primed frame has no phase: %s", first.data)
	}

	// Now change something and watch it arrive.
	if res := postAction(t, ts, `{"kind":"setModel","name":"opus"}`); res.StatusCode != http.StatusOK {
		t.Fatalf("the action was not accepted: %d", res.StatusCode)
	}
	next := waitFor(t, events, "the frame the action produced")
	if next.id <= first.id {
		t.Errorf("id = %d, want it past the priming frame's %d", next.id, first.id)
	}
	if !strings.Contains(next.data, "opus") {
		t.Errorf("the new frame does not mention the change: %s", next.data)
	}

	// And the client's context is the client hanging up: the stream ends, which
	// the reader reports by closing its channel. Anything already buffered is
	// drained on the way — what is being asserted is that the channel closes at
	// all, not that nothing was in flight when it did.
	cancel()
	deadline := time.After(settle)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the stream outlived the client's cancelled context")
		}
	}
}

// A preflight from a webview is granted; one from a page on the web is not, and
// gets no grant header at all — the absence is the answer.
func TestCORSPreflight(t *testing.T) {
	ts, _, _ := testServer(t, Options{AllowOrigins: []string{"http://localhost:5173"}})

	cases := []struct {
		name       string
		origin     string
		wantStatus int
		wantGrant  bool
	}{
		{"a vscode webview", "vscode-webview://abc", http.StatusNoContent, true},
		{"another vscode webview", "vscode-webview://0f1e2d3c-dead-beef", http.StatusNoContent, true},
		{"an explicitly allowed dev origin", "http://localhost:5173", http.StatusNoContent, true},
		{"a page on the web", "https://evil.example", http.StatusForbidden, false},
		// Near-misses, because a prefix check is exactly where an origin like
		// this would sneak through.
		{"an origin that merely mentions the scheme", "https://vscode-webview.evil.example", http.StatusForbidden, false},
		{"an http origin naming the scheme in its path", "http://evil.example/vscode-webview://", http.StatusForbidden, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodOptions, ts.URL+"/api/action", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Access-Control-Request-Method", "POST")
			req.Header.Set("Access-Control-Request-Headers", "authorization")
			res, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = res.Body.Close() }()

			if res.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", res.StatusCode, tc.wantStatus)
			}
			grant := res.Header.Get("Access-Control-Allow-Origin")
			if !tc.wantGrant {
				if grant != "" {
					t.Errorf("Access-Control-Allow-Origin = %q, want none at all", grant)
				}
				return
			}
			if grant != tc.origin {
				t.Errorf("Access-Control-Allow-Origin = %q, want the origin reflected", grant)
			}
			if grant == "*" {
				t.Error("a wildcard grant would open this port to every page in the browser")
			}
			if h := res.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(strings.ToLower(h), "authorization") {
				t.Errorf("Access-Control-Allow-Headers = %q, want authorization — without it the token cannot be sent", h)
			}
			if m := res.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(m, "POST") {
				t.Errorf("Access-Control-Allow-Methods = %q, want POST", m)
			}
		})
	}
}

// A preflight carries no Authorization header — the browser is asking whether it
// may send one — so answering it must not go through the auth check.
func TestCORSPreflightNeedsNoToken(t *testing.T) {
	ts, _, _ := testServer(t, Options{})

	req, err := http.NewRequest(http.MethodOptions, ts.URL+"/api/frame", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "vscode-webview://abc")
	req.Header.Set("Access-Control-Request-Method", "GET")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode == http.StatusUnauthorized {
		t.Fatal("the preflight was refused for want of a token the browser cannot send yet")
	}
}

// A real (non-preflight) request from an allowed origin gets the grant, and one
// from an unknown origin gets none — even when it carries a valid token, which
// is the case that matters: the token is the defence, and CORS is what stops a
// *page* from ever getting to use one.
func TestCORSOnRealRequests(t *testing.T) {
	ts, _, _ := testServer(t, Options{})

	for _, tc := range []struct {
		origin    string
		wantGrant string
	}{
		{"vscode-webview://abc", "vscode-webview://abc"},
		{"https://evil.example", ""},
	} {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/frame", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("Authorization", "Bearer "+testToken)
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()

		if got := res.Header.Get("Access-Control-Allow-Origin"); got != tc.wantGrant {
			t.Errorf("origin %s: grant = %q, want %q", tc.origin, got, tc.wantGrant)
		}
		if v := res.Header.Get("Vary"); !strings.Contains(v, "Origin") {
			t.Errorf("origin %s: Vary = %q, want it to name Origin", tc.origin, v)
		}
	}
}

// Both themes are servable, as CSS, and they differ — the dark one is the same
// dracula the terminal highlights with, and a client swapping this one document
// is how it follows the editor from dark to light.
func TestHighlightCSS(t *testing.T) {
	ts, _, _ := testServer(t, Options{})

	var bodies []string
	for _, theme := range []string{"dark", "light", ""} {
		path := "/api/highlight.css"
		if theme != "" {
			path += "?theme=" + theme
		}
		res := get(t, ts, path, testToken)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("theme %q: status = %d", theme, res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
			t.Errorf("theme %q: content-type = %q, want text/css", theme, ct)
		}
		body := readAll(t, res)
		if !strings.Contains(body, "chroma") {
			t.Errorf("theme %q: body carries no chroma classes:\n%s", theme, body)
		}
		bodies = append(bodies, body)
	}
	if bodies[0] == bodies[1] {
		t.Error("the dark and light stylesheets are identical")
	}
	if bodies[2] != bodies[0] {
		t.Error("no theme should mean dark, the same one acy run uses")
	}

	if res := get(t, ts, "/api/highlight.css?theme=puce", testToken); res.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown theme = %d, want 400", res.StatusCode)
	}
}

// The picker rows come from the injected lister, through ui.SessionRows — the
// same call the terminal's /resume makes, so the two front ends cannot show
// different lists.
func TestSessionsRoute(t *testing.T) {
	mod := time.Unix(1_700_000_000, 0)
	ts, _, _ := testServer(t, Options{
		Sessions: func() ([]session.Info, error) {
			return []session.Info{
				{ID: "parent-1", ModTime: mod, Summary: "port the parser"},
				{ID: "child-a", ModTime: mod, Summary: "a dispatched task"},
				{ID: "plain", ModTime: mod, Summary: "a plain claude chat"},
			}, nil
		},
		LoadState: func(id string) (state.Snapshot, bool, error) {
			if id != "parent-1" {
				return state.Snapshot{}, false, nil
			}
			return state.Snapshot{
				Phase: "AUTO-RUN", Dispatches: 3, CostSettled: 2.5,
				Tasks: []state.Task{{ID: "t1", SessionID: "child-a"}},
			}, true, nil
		},
	})

	rows := decode[[]ui.SessionRow](t, get(t, ts, "/api/sessions", testToken))
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (the dispatched child is hidden): %+v", len(rows), rows)
	}
	if rows[0].ID != "parent-1" || rows[0].Label != "AUTO-RUN · 3 tasks · $2.50" {
		t.Errorf("row 0 = %+v, want the supervised run with its label", rows[0])
	}
	if rows[1].Label != "" {
		t.Errorf("row 1 = %+v, want no label for a session acy never supervised", rows[1])
	}
}

// No lister is not an error: a run with nothing to resume answers with an empty
// list, and a client renders an empty picker rather than a failure.
func TestSessionsRouteWithoutALister(t *testing.T) {
	ts, _, _ := testServer(t, Options{})
	rows := decode[[]ui.SessionRow](t, get(t, ts, "/api/sessions", testToken))
	if len(rows) != 0 {
		t.Errorf("rows = %+v, want an empty list", rows)
	}
}

func TestSessionsRouteWhenListingFails(t *testing.T) {
	ts, _, _ := testServer(t, Options{
		Sessions: func() ([]session.Info, error) { return nil, errors.New("no project dir") },
	})
	if res := get(t, ts, "/api/sessions", testToken); res.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", res.StatusCode)
	}
}

// The listener is loopback-only and the token is real randomness. Both are the
// difference between "a local tool" and "a service on the network".
func TestNewBindsLoopbackAndMintsAToken(t *testing.T) {
	h := hub.New(ui.New(nil, ui.Config{}))
	t.Cleanup(h.Close)

	s, err := New(h, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if !strings.HasPrefix(s.URL(), "http://127.0.0.1:") {
		t.Errorf("URL = %q, want a loopback address", s.URL())
	}
	if len(s.Token()) != 2*tokenBytes {
		t.Errorf("token is %d chars, want %d (256 bits, hex)", len(s.Token()), 2*tokenBytes)
	}

	// A second server mints a different token; a constant would be worse than
	// none, since it would look like a secret.
	s2, err := New(h, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if s2.Token() == s.Token() {
		t.Error("two servers minted the same token")
	}
}

// Binding anywhere but loopback is refused rather than warned about: this
// server hands out the ability to approve tool calls on this machine.
func TestNewRefusesANonLoopbackAddress(t *testing.T) {
	h := hub.New(ui.New(nil, ui.Config{}))
	t.Cleanup(h.Close)

	for _, addr := range []string{"0.0.0.0:0", "192.168.1.10:0", "example.com:80"} {
		if _, err := New(h, Options{Addr: addr}); err == nil {
			t.Errorf("New(%q) was allowed; it must not be", addr)
		}
	}
	// An empty host means "any interface" to net.Listen, and must be read as
	// loopback here rather than passed through.
	s, err := New(h, Options{Addr: ":0"})
	if err != nil {
		t.Fatalf("New(\":0\"): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if !strings.HasPrefix(s.URL(), "http://127.0.0.1:") {
		t.Errorf("URL = %q, want loopback", s.URL())
	}
}

// Close is safe twice, which is what lets a caller defer it and still call it on
// a shutdown path.
func TestCloseTwice(t *testing.T) {
	h := hub.New(ui.New(nil, ui.Config{}))
	t.Cleanup(h.Close)
	s, err := New(h, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	if err := s.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// A live server really serves: this is the one test that goes through New's own
// listener rather than httptest's, so the Start/URL/Close path a caller uses is
// exercised end to end.
func TestStartServesOnItsOwnListener(t *testing.T) {
	h := hub.New(ui.New(nil, ui.Config{}))
	t.Cleanup(h.Close)
	s, err := New(h, Options{Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.Start()

	res, err := http.Get(s.URL() + "/healthz") //nolint:noctx // a loopback health check in a test
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
}

// --- SSE reading helpers -----------------------------------------------------

type sseEvent struct {
	id    int
	event string
	data  string
}

// readSSE parses the stream into events and closes ch when it ends. It is
// deliberately minimal — enough of the format to assert on, no more — and
// ignores comment lines, which is what a heartbeat is.
func readSSE(r io.Reader, ch chan<- sseEvent) {
	defer close(ch)
	sc := bufio.NewScanner(r)
	var cur sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.event != "" {
				ch <- cur
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, ":"): // a comment: the heartbeat
		case strings.HasPrefix(line, "id: "):
			cur.id, _ = strconv.Atoi(strings.TrimPrefix(line, "id: "))
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func waitFor(t *testing.T, ch <-chan sseEvent, what string) sseEvent {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatalf("the stream ended while waiting for %s", what)
		}
		return e
	case <-time.After(settle):
		t.Fatalf("timed out after %s waiting for %s", settle, what)
		return sseEvent{}
	}
}

func frameOf(t *testing.T, e sseEvent) ui.Frame {
	t.Helper()
	var f ui.Frame
	if err := json.Unmarshal([]byte(e.data), &f); err != nil {
		t.Fatalf("event data does not unmarshal as a frame: %v\n%s", err, e.data)
	}
	return f
}

func readAll(t *testing.T, res *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	return string(b)
}
