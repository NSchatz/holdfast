// Package webui serves the holdfast web UI. The UI is a single self-contained
// HTML page (vanilla JS, inline CSS, no external/CDN assets) embedded into the
// binary via go:embed, so `holdfast serve` ships one binary with the dashboard
// baked in. The page is a READ-AND-CONTROL view over the API — it holds no state of
// its own; the YAML config and the SQLite store remain the sources of truth.
package webui

import (
	"bytes"
	_ "embed"
	"net/http"

	"github.com/NSchatz/holdfast/internal/sourceoffer"
)

//go:embed index.html
var indexHTML []byte

// offerMarker is the ONE placeholder in index.html that the resolved source offer
// replaces, once, when the handler is constructed. The offer is rendered SERVER-SIDE
// and baked into the document that goes on the wire, rather than fetched by the
// page's own JavaScript, for three reasons that are each a requirement rather than a
// preference: the offer must survive every API endpoint failing (it is in the static
// bytes, so no request can remove it), the page must keep its no-HTML-string-sink
// render idiom (nothing assigns a string to an HTML sink), and the response
// Content-Security-Policy must not be relaxed to let it in.
const offerMarker = "<!--holdfast:source-offer-->"

// csp is the response Content-Security-Policy, byte for byte. A tight CSP: the page
// is fully self-contained, so nothing but its own inline script/style is ever allowed
// to load — defence in depth for a tool that may sit on a home LAN.
// `require-trusted-types-for 'script'` enforces Trusted Types (TRANSCODE-15): the page
// renders rows as DOM nodes and never assigns a string to an HTML sink, so this turns
// that discipline into a browser-enforced guarantee — a regression that string-builds
// from an attacker-influencable media path would throw, not silently reintroduce a
// sink. The source offer (LICENSE-3) is rendered into the served document server-side
// and needs no relaxation of any directive here.
const csp = "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; require-trusted-types-for 'script'"

// render returns the served document for one resolved offer: the embedded page with
// the offer substituted for its marker.
func render(o sourceoffer.Offer) []byte {
	return bytes.Replace(indexHTML, []byte(offerMarker), []byte(o.HTML()), 1)
}

// Handler returns an http.Handler that serves the embedded dashboard at "/" (and
// only "/": any other path under the catch-all 404s rather than serving the app
// shell for, say, a stray asset request). It is mounted behind chi's "/*" route.
//
// The source URL this binary was built with is validated here as well as at startup,
// and the same accept test does both - so a handler built in process with a rejected
// value REFUSES rather than serving an offer that names one. In the daemon the
// refusal has already happened: cmd/holdfast resolves the offer before it creates any
// listener, and hands the resolved value to HandlerFor.
func Handler() http.Handler {
	o, err := sourceoffer.Resolve()
	if err != nil {
		return refusingHandler(err)
	}
	return HandlerFor(o)
}

// HandlerFor is Handler for an already-resolved offer. The document is rendered once,
// at construction: the offer is fixed for the life of the process (it is stamped into
// the binary), so every root response is the same bytes.
func HandlerFor(o sourceoffer.Offer) http.Handler {
	doc := render(o)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", csp)
		_, _ = w.Write(doc)
	})
}

// refusingHandler serves nothing at the root when the build's source URL was
// rejected. It is a refusal, never a fall back to the upstream URL: a page that
// quietly named upstream as the source of a modified binary would be a worse failure
// than no page. The body states why and names the rejected value, and is deliberately
// not an offer - it carries no link, no licence name and no identity.
func refusingHandler(err error) http.Handler {
	msg := "holdfast refuses to serve the dashboard: " + err.Error() + "\n"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, msg, http.StatusServiceUnavailable)
	})
}
