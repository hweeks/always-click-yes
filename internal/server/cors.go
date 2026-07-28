package server

import (
	"net/http"
	"slices"
	"strings"
)

// Cross-origin, deliberately as small as it can be while still working.
//
// The client this exists for is a VS Code webview. It is served from
// vscode-webview://<uuid> and fetches http://127.0.0.1:<port>, which is a
// different origin by every rule the browser has — and because every /api/*
// request carries an Authorization header, each one is a "non-simple" request
// that the browser preflights with an OPTIONS first. Answer that preflight or
// nothing works at all: the browser never sends the real request, and the
// failure surfaces in the webview's console rather than in acy's log.
//
// So the surface is exactly:
//
//   - Access-Control-Allow-Origin is *reflected*, only for an origin we
//     recognise. Never `*` — a wildcard would tell every page on the machine's
//     browser that it may talk to this port, and the only thing left standing
//     between them and a run would be the token.
//   - OPTIONS is answered for those origins with the methods and the
//     `authorization` header the client is asking to send.
//   - Anything else gets no CORS headers whatsoever. Not a 200 with a missing
//     grant, not a helpful message: the absence *is* the answer, and the browser
//     enforces it.
//
// The token remains the real defence. CORS is a browser-side rule and cannot
// stop a program with a socket; what it stops is a *page* — evil.example cannot
// preflight successfully, cannot read a response, and never had the token that
// was printed to the process that launched acy. Credentials are deliberately not
// allowed either: acy authenticates with a header a client must be given, never
// with an ambient cookie a page would carry automatically.

// vscodeWebviewScheme is the origin a VS Code webview presents. The authority
// after it is a per-webview uuid that changes every time one is created, so
// there is nothing to pin but the scheme.
const vscodeWebviewScheme = "vscode-webview://"

// corsMethods and corsHeaders are what the API actually uses. Listing more would
// be describing a surface that does not exist.
const (
	corsMethods = "GET, POST, OPTIONS"
	corsHeaders = "authorization, content-type"
	corsMaxAge  = "600" // seconds a browser may cache the preflight
)

// originAllowed decides whether an Origin gets a CORS grant.
func (s *Server) originAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	// Prefix, not equality: the uuid is per webview. The scheme is the part that
	// matters — no page on the web can claim it, since a browser sets Origin
	// itself and only a VS Code webview is ever served from there.
	if strings.HasPrefix(origin, vscodeWebviewScheme) {
		return true
	}
	return slices.Contains(s.opts.AllowOrigins, origin)
}

// cors reflects the grant for a recognised origin and answers its preflights.
//
// It wraps everything, including /healthz, because a browser preflights by URL
// and not by whether the route happens to need a credential.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := s.originAllowed(origin)
		if origin != "" {
			// The response differs by Origin, so anything caching it has to key on
			// that. Set even when the origin is refused: a cached "no grant" served
			// back to an allowed origin would break the webview.
			w.Header().Add("Vary", "Origin")
		}
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}

		// A preflight is an OPTIONS carrying Access-Control-Request-Method. A bare
		// OPTIONS is not one and falls through to the mux, which answers 405.
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if !allowed {
				// No CORS headers at all — and no hint about why. The browser will
				// block the real request either way; there is nothing here for a page
				// that is probing to learn.
				writeError(w, http.StatusForbidden, "origin not allowed")
				return
			}
			w.Header().Set("Access-Control-Allow-Methods", corsMethods)
			w.Header().Set("Access-Control-Allow-Headers", corsHeaders)
			w.Header().Set("Access-Control-Max-Age", corsMaxAge)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
