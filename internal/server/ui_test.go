package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/sourceoffer"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/ui"
	"github.com/NSchatz/holdfast/internal/version"
)

const (
	uiScript     = "index-AAAA.js"
	uiScriptBody = "console.log('holdfast')"
)

// uiTree is a built UI in miniature: a page carrying the offer slot and naming its
// script, and the script.
func uiTree() fstest.MapFS {
	return fstest.MapFS{
		ui.IndexName: {Data: []byte(`<!doctype html><html><head><script type="module" src="/assets/` + uiScript +
			`"></script></head><body><div id="app"></div>` + ui.OfferSlot + `</body></html>`)},
		"assets/" + uiScript:    {Data: []byte(uiScriptBody)},
		"assets/index-BBBB.css": {Data: []byte("body{}")},
	}
}

// newUIServer is newRootServer with a web UI wired the way `serve` wires one: loaded
// once, with the source offer in effect, before the first request.
func newUIServer(t *testing.T, st store.Store) *Server {
	t.Helper()
	srv := newRootServer(t, "", st)
	offer, err := sourceoffer.Resolve()
	if err != nil {
		t.Fatalf("the source offer under test is refused: %v", err)
	}
	site, err := ui.Load(uiTree(), offer)
	if err != nil {
		t.Fatalf("loading the test UI: %v", err)
	}
	srv.SetUI(site)
	return srv
}

func getAs(t *testing.T, srv *Server, path string, accept ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, a := range accept {
		req.Header.Add("Accept", a)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// What a browser sends on a navigation.
const browserAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

// A request that asks for HTML gets the UI's page at "/", and that page carries the AGPL
// section 13 offer: the source URL in effect - the fork's, not upstream's - shown and
// linked behind the literal label, with the licence and the build identity.
func TestUI_RootServesThePageWithTheSourceOfferToARequestForHTML(t *testing.T) {
	setSourceURL(t, forkValue)
	srv := newUIServer(t, newStore(t))
	rec := getAs(t, srv, "/", browserAccept)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / asking for HTML: code %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type %q, want text/html; charset=utf-8", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<div id="app"></div>`,
		`/assets/` + uiScript,
		sourceoffer.Label + `: <a href="` + forkValue + `" rel="noopener noreferrer">` + forkValue + `</a>`,
		sourceoffer.License,
		version.String(),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, sourceoffer.Upstream) {
		t.Errorf("the page of a build whose source is %s names upstream:\n%s", forkValue, body)
	}
	if strings.Contains(body, ui.OfferSlot) {
		t.Errorf("the page was served with its offer slot still empty:\n%s", body)
	}
}

// The page is sent under a policy that lets it load from this server and nowhere else,
// with no inline script, and it is the one file a browser must ask for again.
func TestUI_ThePageIsSentUnderItsSecurityHeaders(t *testing.T) {
	srv := newUIServer(t, newStore(t))
	h := getAs(t, srv, "/", browserAccept).Header()
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'self'",
		"frame-ancestors 'none'", "base-uri 'none'", "form-action 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("Content-Security-Policy %q lacks %q", csp, want)
		}
	}
	for _, never := range []string{"unsafe-inline", "unsafe-eval", "http:", "https:", "*"} {
		if strings.Contains(csp, never) {
			t.Errorf("Content-Security-Policy %q carries %q", csp, never)
		}
	}
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"Cache-Control":          "no-cache",
		"Vary":                   "Accept",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// A hostile source URL - accepted at build time, because the accept test is the scheme
// prefix alone - reaches the page as text and as an attribute value, never as markup.
func TestUI_AHostileSourceURLIsEscapedInThePage(t *testing.T) {
	setSourceURL(t, hostileValue)
	srv := newUIServer(t, newStore(t))
	body := getAs(t, srv, "/", "text/html").Body.String()
	if strings.Contains(body, "<img") || strings.Contains(body, `b="><`) {
		t.Fatalf("the source URL reached the page as markup:\n%s", body)
	}
	if !strings.Contains(body, "&lt;img src=x onerror=1&gt;") {
		t.Errorf("the page does not carry the source URL in effect, escaped:\n%s", body)
	}
}

// Every request that does not ask for HTML by name gets the plain-text page, byte for
// byte what a server with no UI answers: the UI changes nothing for a client that was
// reading "/" before there was one, and the plain-text offer stays served.
func TestUI_ARequestThatDoesNotAskForHTMLGetsThePlainTextPageUnchanged(t *testing.T) {
	setSourceURL(t, forkValue)
	st := newStore(t)
	_, want := getRoot(t, newRootServer(t, "", st))
	if probs := plainTextOfferProblems(want, forkValue); probs != nil {
		t.Fatalf("the precondition of this test fails - the page of a server with no UI: %v", probs)
	}
	srv := newUIServer(t, st)
	for name, accept := range map[string][]string{
		"no Accept header":          nil,
		"curl's wildcard":           {"*/*"},
		"JSON":                      {"application/json"},
		"plain text":                {"text/plain"},
		"HTML refused by quality":   {"text/html;q=0, */*"},
		"HTML with a broken q":      {"text/html;q=high"},
		"HTML with a q above one":   {"text/html;q=7"},
		"any text":                  {"text/*"},
		"something that is no MIME": {"text/htmlx, html"},
	} {
		rec := getAs(t, srv, "/", accept...)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: code %d, want 200", name, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
			t.Errorf("%s: Content-Type %q, want the plain-text page", name, ct)
		}
		if got := rec.Body.String(); got != want {
			t.Errorf("%s: the body differs from the page of a server with no UI.\n got: %s\nwant: %s", name, got, want)
		}
		if rec.Header().Get("Vary") != "Accept" {
			t.Errorf("%s: no Vary: Accept, so a cache may hand this answer to a browser", name)
		}
		if rec.Header().Get("Content-Security-Policy") != "" {
			t.Errorf("%s: the page's policy was sent with the plain-text page", name)
		}
	}
}

// acceptsHTML, over the shapes an Accept header takes.
func TestAcceptsHTML(t *testing.T) {
	for _, tc := range []struct {
		accept []string
		want   bool
	}{
		{nil, false},
		{[]string{""}, false},
		{[]string{"*/*"}, false},
		{[]string{"text/*"}, false},
		{[]string{"application/json"}, false},
		{[]string{"text/html"}, true},
		{[]string{"TEXT/HTML"}, true},
		{[]string{" text/html ; q=0.5 "}, true},
		{[]string{"application/xhtml+xml"}, true},
		{[]string{browserAccept}, true},
		{[]string{"application/json", "text/html"}, true},
		{[]string{"application/json, text/html;level=1;Q=0.1"}, true},
		{[]string{"text/html;q=0"}, false},
		{[]string{"text/html;q=0.0, text/plain"}, false},
		{[]string{"text/html;q=abc"}, false},
		{[]string{"text/html;q=-1"}, false},
		{[]string{"text/html;q=1.5"}, false},
		{[]string{"text/html;q"}, true},
		{[]string{"text/htmlx"}, false},
		{[]string{"xtext/html"}, false},
	} {
		if got := acceptsHTML(tc.accept); got != tc.want {
			t.Errorf("acceptsHTML(%q) = %v, want %v", tc.accept, got, tc.want)
		}
	}
}

// An asset the build holds is served as what it is, cacheable for good; nothing else
// under the prefix exists, and no path there reaches the page or anything outside the
// build.
func TestUI_AssetsAreServedByNameAndNothingElseIs(t *testing.T) {
	srv := newUIServer(t, newStore(t))
	rec := getAs(t, srv, "/assets/"+uiScript)
	if rec.Code != http.StatusOK || rec.Body.String() != uiScriptBody {
		t.Fatalf("GET /assets/%s: code %d body %q", uiScript, rec.Code, rec.Body.String())
	}
	for name, want := range map[string]string{
		"Content-Type":           "text/javascript; charset=utf-8",
		"Cache-Control":          "public, max-age=31536000, immutable",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if ct := getAs(t, srv, "/assets/index-BBBB.css").Header().Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Errorf("the stylesheet is sent as %q", ct)
	}
	for _, p := range []string{
		"/assets/", "/assets/missing.js", "/assets/index.html", "/assets/../index.html",
		"/assets/%2e%2e/index.html", "/assets/" + uiScript + "/", "/assets//" + uiScript,
		"/index.html", "/assets", "/anything", "/.gitkeep",
	} {
		rec := getAs(t, srv, p, browserAccept)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: code %d, want 404", p, rec.Code)
		}
		if strings.Contains(rec.Body.String(), `<div id="app">`) {
			t.Errorf("GET %s was answered with the page", p)
		}
	}
}

// With no UI wired - a build that embeds none, or one whose UI was refused - the asset
// route answers 404 and the root answers the plain-text page even to a browser.
func TestUI_WithNoUIWiredABrowserGetsThePlainTextPage(t *testing.T) {
	setSourceURL(t, forkValue)
	srv := newRootServer(t, "", newStore(t))
	rec := getAs(t, srv, "/", browserAccept)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("GET / asking for HTML with no UI: code %d, Content-Type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if probs := plainTextOfferProblems(rec.Body.String(), forkValue); probs != nil {
		t.Errorf("the plain-text page lost the offer: %v", probs)
	}
	if code := getAs(t, srv, "/assets/"+uiScript).Code; code != http.StatusNotFound {
		t.Errorf("GET /assets/%s with no UI: code %d, want 404", uiScript, code)
	}
}

// The page and its assets need no credential in any configuration, and the read gate
// still stands in front of every read: the page carries no library datum, and the UI
// holds nothing the API does not gate.
func TestUI_ThePageIsOpenAndTheReadGateStillStands(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	ctrl := NewController(ctx, func(context.Context) error { return nil }, discard())
	hub := NewHub(st, ctrl, discard())
	ctrl.SetOnChange(hub.Trigger)
	gated := New(ctx, config.Config{}, secret.Value{}, secret.NewValue("read-token-for-the-ui-test"), st, ctrl, hub, nil, discard())
	offer, err := sourceoffer.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	site, err := ui.Load(uiTree(), offer)
	if err != nil {
		t.Fatal(err)
	}
	gated.SetUI(site)
	if rec := getAs(t, gated, "/", browserAccept); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), sourceoffer.Label) {
		t.Errorf("GET / with a read token configured and none sent: code %d", rec.Code)
	}
	if code := getAs(t, gated, "/assets/"+uiScript).Code; code != http.StatusOK {
		t.Errorf("GET an asset with a read token configured and none sent: code %d, want 200", code)
	}
	for _, ep := range []string{"/api/summary", "/api/queue", "/api/history", "/api/health"} {
		if code := getAs(t, gated, ep, browserAccept).Code; code != http.StatusUnauthorized {
			t.Errorf("GET %s with no credential: code %d, want 401 - wiring a UI must not open a read", ep, code)
		}
	}
}
