package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Requeue is CLI-ONLY, and this is the one thing in this package that says so.
//
// `holdfast requeue` changes what the engine will do to a media file, which is the same
// property that keeps `restore` off the HTTP surface: a mutating endpoint opens an
// authorization question the read-and-control API does not answer, and this API's
// mutating endpoints can only ever start a scan or toggle pause - none of them can touch
// a file. That is a ratified operator decision recorded in the umbrella's
// operator-decision-holdfast-backlog-review.md, and no later change may put requeue on
// this surface without reversing it explicitly.
//
// A test is what makes a decision like that survive. Prose in a comment is reversed by
// whoever did not read it; this reds.
func TestRoutes_NoRequeueEndpointExists(t *testing.T) {
	h := newHarness(t, "tok")

	// 1. Every route this router serves, from the ROUTER ITSELF rather than from a list
	// of paths a test author thought to try. A route added tomorrow is covered by this
	// walk on the day it lands.
	routes, ok := h.srv.mux.(chi.Routes)
	if !ok {
		t.Fatalf("the server's handler is not a chi router (%T), so its routes cannot be enumerated "+
			"and this check would pass over anything", h.srv.mux)
	}
	var served []string
	if err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		served = append(served, method+" "+route)
		return nil
	}); err != nil {
		t.Fatalf("walking the routes: %v", err)
	}
	if len(served) == 0 {
		t.Fatal("the walk found no routes at all, so it cannot have found a requeue one either")
	}
	for _, r := range served {
		if strings.Contains(strings.ToLower(r), "requeue") {
			t.Errorf("the HTTP surface serves %q. Requeue is CLI-only by ratified operator decision: "+
				"it changes what the engine will do to a media file, and a mutating endpoint opens an "+
				"authorization question this read-and-control API does not answer", r)
		}
	}
	// Anti-vacuity: the walk really does see this router's routes, so "no requeue route"
	// is a finding about the surface and not about an empty enumeration.
	var sawRescan bool
	for _, r := range served {
		if strings.Contains(r, "/api/rescan") {
			sawRescan = true
		}
	}
	if !sawRescan {
		t.Fatalf("the walk did not see /api/rescan, which this router certainly serves; it is not "+
			"enumerating the real surface: %v", served)
	}

	// 2. And the shapes somebody would actually reach for are 404, authenticated. The
	// walk above is the general answer; these are the specific ones, asked of a running
	// server with a valid control token so a 404 cannot be an authorization result in
	// disguise.
	ts := httptest.NewServer(h.srv)
	defer ts.Close()
	for _, path := range []string{
		"/api/requeue", "/api/jobs/requeue", "/api/queue/requeue", "/requeue",
	} {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			req, err := http.NewRequest(method, ts.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer tok")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s answered %d; requeue must not be reachable over HTTP at all",
					method, path, resp.StatusCode)
			}
		}
	}
}
