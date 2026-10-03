package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The web UI drives exactly the controls this router offers, so what the router does not
// serve the UI cannot reach. `restore` overwrites a library file with older bytes and
// `requeue` re-opens a row a terminal decision already answered: both are LOCAL commands
// by ratified operator decision (docs/design/web-ui.md#controls), and neither word may
// appear in any route pattern this server registers, under any method, at any depth.
//
// The walk is over the ROUTER, not over a list of paths somebody thought to try, so a
// route added tomorrow is judged the day it lands.
func TestRoutes_NoRestoreOrRequeueRouteIsRegistered(t *testing.T) {
	h := newHarness(t, "tok")
	if h.srv.mux == nil {
		t.Fatal("the server holds no chi router, so its routes cannot be enumerated and this check would pass over anything")
	}

	var served []string
	err := chi.Walk(h.srv.mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		served = append(served, method+" "+route)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the routes: %v", err)
	}

	for _, route := range served {
		for _, word := range []string{"restore", "requeue"} {
			if strings.Contains(strings.ToLower(route), word) {
				t.Errorf("the HTTP surface registers %q, whose pattern contains %q. That command is local "+
					"only: it changes what happens to a library file, and no credential this API issues "+
					"authorises that", route, word)
			}
		}
	}

	// Anti-vacuity: the walk saw the routes the web UI's controls use, in every group it
	// reaches into, so "no such route" is a finding about the real surface.
	for _, want := range []string{
		"GET /api/summary", "GET /api/queue", "GET /api/history", "GET /api/health",
		"POST /api/pause", "POST /api/resume", "POST /api/rescan", "POST /api/scan",
		"GET /api/exclusions/", "POST /api/exclusions/", "DELETE /api/exclusions/",
	} {
		found := false
		for _, route := range served {
			if route == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the walk did not see %q, which this router serves and the web UI calls; it is not "+
				"enumerating the real surface: %v", want, served)
		}
	}
}

// The check above must be able to fail. A router carrying such a route is walked the same
// way and the same test for the two words finds it.
func TestRoutes_TheRestoreAndRequeueWalkBites(t *testing.T) {
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Get("/summary", func(http.ResponseWriter, *http.Request) {})
		r.Route("/jobs", func(r chi.Router) {
			r.Post("/{id}/Requeue", func(http.ResponseWriter, *http.Request) {})
		})
		r.Group(func(r chi.Router) {
			r.Put("/undo/restore", func(http.ResponseWriter, *http.Request) {})
		})
	})
	hits := 0
	if err := chi.Walk(r, func(_, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		low := strings.ToLower(route)
		if strings.Contains(low, "restore") || strings.Contains(low, "requeue") {
			hits++
		}
		return nil
	}); err != nil {
		t.Fatalf("walking the routes: %v", err)
	}
	if hits != 2 {
		t.Fatalf("the walk found %d of the 2 planted routes; a nested or grouped route is escaping it", hits)
	}
}
