// Package webui serves the holdfast web UI. The UI is a single self-contained
// HTML page (vanilla JS, inline CSS, no external/CDN assets) embedded into the
// binary via go:embed, so `holdfast serve` ships one binary with the dashboard
// baked in. The page is a READ-AND-CONTROL view over the API — it holds no state of
// its own; the YAML config and the SQLite store remain the sources of truth.
package webui

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

// Handler returns an http.Handler that serves the embedded dashboard at "/" (and
// only "/": any other path under the catch-all 404s rather than serving the app
// shell for, say, a stray asset request). It is mounted behind chi's "/*" route.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// A tight CSP: the page is fully self-contained, so nothing but its own
		// inline script/style is ever allowed to load — defence in depth for a tool
		// that may sit on a home LAN. `require-trusted-types-for 'script'` enforces
		// Trusted Types (TRANSCODE-15): the page renders rows as DOM nodes and never
		// assigns a string to an HTML sink, so this turns that discipline into a
		// browser-enforced guarantee — a regression that string-builds from an
		// attacker-influencable media path would throw, not silently reintroduce a sink.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; require-trusted-types-for 'script'")
		_, _ = w.Write(indexHTML)
	})
}
