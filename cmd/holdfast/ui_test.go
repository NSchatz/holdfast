package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/server"
	"github.com/NSchatz/holdfast/internal/sourceoffer"
	"github.com/NSchatz/holdfast/internal/ui"
)

// uiWiringServer is a real server with nothing behind it but the routes: the root path
// and the asset route answer without a store.
func uiWiringServer(t *testing.T) (*server.Server, *bytes.Buffer, *slog.Logger) {
	t.Helper()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := server.New(context.Background(), config.Config{}, secret.Value{}, secret.Value{}, nil, nil, nil, nil, log)
	return srv, &logs, log
}

func browserGet(t *testing.T, srv http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

const wiredPage = `<!doctype html><html><head><script type="module" src="/assets/app-1.js"></script></head><body><div id="app"></div>`

// The three things `serve` can find in the tree it embeds, each stated at startup and
// each ending in a root path that carries the source offer:
// a built UI is served to a browser; the placeholder alone leaves the plain-text page and
// says so at INFO; a UI that cannot carry the offer is served in NO part, says so at WARN
// with the reason, and leaves the plain-text page.
func TestWireUI_ServesAWholeBuildAndNoPartOfABrokenOne(t *testing.T) {
	cases := []struct {
		name      string
		tree      fstest.MapFS
		wantHTML  bool
		wantLevel string
		wantLog   string
	}{
		{
			name: "a built UI",
			tree: fstest.MapFS{
				ui.IndexName:      {Data: []byte(wiredPage + ui.OfferSlot + "</body></html>")},
				"assets/app-1.js": {Data: []byte("1")},
			},
			wantHTML: true, wantLevel: "level=INFO", wantLog: "web UI embedded",
		},
		{
			name:      "the placeholder alone",
			tree:      fstest.MapFS{".gitkeep": {Data: []byte("x")}},
			wantLevel: "level=INFO", wantLog: "no web UI in this build",
		},
		{
			name: "a page with no place for the offer",
			tree: fstest.MapFS{
				ui.IndexName:      {Data: []byte(wiredPage + "</body></html>")},
				"assets/app-1.js": {Data: []byte("1")},
			},
			wantLevel: "level=WARN", wantLog: "0 times, not once",
		},
		{
			name: "a page whose script is missing",
			tree: fstest.MapFS{
				ui.IndexName:        {Data: []byte(wiredPage + ui.OfferSlot + "</body></html>")},
				"assets/other-2.js": {Data: []byte("1")},
			},
			wantLevel: "level=WARN", wantLog: "not one build",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, logs, log := uiWiringServer(t)
			wireUI(srv, tc.tree, log)

			if !strings.Contains(logs.String(), tc.wantLevel) || !strings.Contains(logs.String(), tc.wantLog) {
				t.Errorf("startup log lacks %s / %q:\n%s", tc.wantLevel, tc.wantLog, logs.String())
			}
			if strings.Contains(logs.String(), "level=ERROR") {
				t.Errorf("an error-level record for a state the daemon continues in:\n%s", logs.String())
			}

			rec := browserGet(t, srv, "/")
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /: code %d", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, sourceoffer.Label) || !strings.Contains(body, sourceoffer.URL) {
				t.Errorf("the root response carries no source offer:\n%s", body)
			}
			gotHTML := strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html")
			if gotHTML != tc.wantHTML {
				t.Errorf("a browser got HTML = %v, want %v (Content-Type %q)", gotHTML, tc.wantHTML, rec.Header().Get("Content-Type"))
			}
			assetCode := browserGet(t, srv, "/assets/app-1.js").Code
			if tc.wantHTML && assetCode != http.StatusOK {
				t.Errorf("the page's script: code %d, want 200", assetCode)
			}
			if !tc.wantHTML && assetCode != http.StatusNotFound {
				t.Errorf("an asset of a UI that is not served: code %d, want 404 - no part of it is served", assetCode)
			}
		})
	}
}

// A source URL the startup check would refuse never reaches a page: nothing is wired,
// and the reason is logged.
func TestWireUI_ARefusedSourceOfferWiresNothing(t *testing.T) {
	old := sourceoffer.URL
	sourceoffer.URL = "not-a-url"
	t.Cleanup(func() { sourceoffer.URL = old })

	srv, logs, log := uiWiringServer(t)
	wireUI(srv, fstest.MapFS{
		ui.IndexName:      {Data: []byte(wiredPage + ui.OfferSlot + "</body></html>")},
		"assets/app-1.js": {Data: []byte("1")},
	}, log)
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "source offer") {
		t.Errorf("the refusal was not logged at WARN:\n%s", logs.String())
	}
	if code := browserGet(t, srv, "/assets/app-1.js").Code; code != http.StatusNotFound {
		t.Errorf("an asset was served (code %d) with the source offer refused", code)
	}
	if rec := browserGet(t, srv, "/"); strings.Contains(rec.Body.String(), `<div id="app">`) {
		t.Errorf("the page was served with the source offer refused:\n%s", rec.Body.String())
	}
}

// What this binary really embeds is wired by the same call `serve` makes, and whichever
// tree the test binary was built from, the root carries the offer afterwards.
func TestWireUI_TheEmbeddedTreeEndsInARootThatCarriesTheOffer(t *testing.T) {
	srv, logs, log := uiWiringServer(t)
	wireUI(srv, ui.Embedded(), log)
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("the tree this binary embeds is neither the placeholder nor a UI that can be served:\n%s", logs.String())
	}
	rec := browserGet(t, srv, "/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), sourceoffer.Label) {
		t.Errorf("GET /: code %d, body %s", rec.Code, rec.Body.String())
	}
}
