package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/sourceoffer"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/version"
)

const (
	forkValue    = "https://example.invalid/fork"
	hostileValue = `https://example.invalid/a?b="><img src=x onerror=1>`
)

// setSourceURL points the build-time source URL at v for one test. It must be set
// BEFORE the server is constructed: both root-serving branches resolve the offer
// once, at construction, so no request can influence it and no request races it.
func setSourceURL(t *testing.T, v string) {
	t.Helper()
	old := sourceoffer.URL
	sourceoffer.URL = v
	t.Cleanup(func() { sourceoffer.URL = old })
}

// newRootServer builds a Server around st. holdfast ships no frontend, so there is one
// root-serving branch and this is the whole of it: the plain-text page RootHandler
// serves, mounted in the real router.
func newRootServer(t *testing.T, token string, st store.Store) *Server {
	t.Helper()
	ctx := context.Background()
	ctrl := NewController(ctx, func(context.Context) error { return nil }, discard())
	hub := NewHub(st, ctrl, discard())
	ctrl.SetOnChange(hub.Trigger)
	return New(ctx, config.Config{}, secret.NewValue(token), secret.Value{}, st, ctrl, hub, nil, discard())
}

// getRoot fetches / with NO credentials of any kind.
func getRoot(t *testing.T, srv *Server) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec.Code, rec.Body.String()
}

// plainTextOfferProblems is AC9's grader over the API-only body, which IS the offer
// there: the source URL in effect verbatim, preceded by the literal label, plus the
// licence name and the build identity, with no markup and no escaping.
func plainTextOfferProblems(body, wantURL string) []string {
	var out []string
	if !strings.Contains(body, sourceoffer.Label+": "+wantURL) {
		out = append(out, "the body does not carry the label immediately before the source URL in effect")
	}
	if !strings.Contains(body, sourceoffer.License) {
		out = append(out, "the body does not name the licence "+sourceoffer.License)
	}
	if !strings.Contains(body, version.String()) {
		out = append(out, "the body does not carry the build identity "+version.String())
	}
	for _, banned := range []string{"<a ", "&amp;", "&#34;", "&lt;", "&gt;"} {
		if strings.Contains(body, banned) {
			out = append(out, "the body carries markup or escaping ("+banned+"); it is plain text")
		}
	}
	return out
}

// The plain-text grader must BITE, or every AC9 assertion below is decoration.
func TestPlainTextOfferGrader_FailsAgainstEveryMutation(t *testing.T) {
	good := sourceoffer.Offer{SourceURL: forkValue, License: sourceoffer.License, Build: version.String()}.Text()
	if probs := plainTextOfferProblems(good, forkValue); probs != nil {
		t.Fatalf("the shipped rendering was reported as broken: %v", probs)
	}
	for name, mutant := range map[string]string{
		"the label is missing":    strings.Replace(good, sourceoffer.Label+": ", "Source: ", 1),
		"the URL is missing":      strings.Replace(good, forkValue, "", 1),
		"the licence is missing":  strings.Replace(good, sourceoffer.License, "", 1),
		"the identity is missing": strings.Replace(good, version.String(), "", 1),
		"the URL is escaped":      strings.Replace(good, forkValue, "https://example.invalid/fork&amp;", 1),
		"the URL became a link":   strings.Replace(good, forkValue, `<a href="`+forkValue+`">x</a>`, 1),
		"words between label and URL": strings.Replace(good, sourceoffer.Label+": "+forkValue,
			sourceoffer.Label+" is at "+forkValue, 1),
	} {
		if probs := plainTextOfferProblems(mutant, forkValue); probs == nil {
			t.Errorf("the grader passed a mutation that breaks the offer (%s): %q", name, mutant)
		}
	}
}

// AC9: the root path, which is the only root path holdfast has now that it ships no
// frontend, carries the source URL in effect, the licence name and the build identity.
// AC7: it is served to a request with no bearer token, whether or not the binary has
// a control token configured at all.
func TestAPIOnlyRoot_CarriesTheSourceOffer(t *testing.T) {
	for _, token := range []string{"", "a-configured-token"} {
		st := newStore(t)
		srv := newRootServer(t, token, st)
		code, body := getRoot(t, srv)
		if code != http.StatusOK {
			t.Fatalf("token=%q: GET / with no credentials: code %d, want 200", token, code)
		}
		if probs := plainTextOfferProblems(body, sourceoffer.Upstream); probs != nil {
			t.Errorf("token=%q: %v\nbody: %s", token, probs, body)
		}
		// The existing banner is still there: the offer was added to that page, not
		// substituted for it.
		if !strings.Contains(body, "/api/summary") {
			t.Errorf("token=%q: the API-only body lost its endpoint banner:\n%s", token, body)
		}
	}
}

// AC3 on the API-only branch: a fork build's body names the fork's tree verbatim and
// the upstream URL occurs nowhere in it (there, the body IS the offer).
func TestAPIOnlyRoot_ForkBuildNamesItsOwnTree(t *testing.T) {
	setSourceURL(t, forkValue)
	srv := newRootServer(t, "", newStore(t))
	code, body := getRoot(t, srv)
	if code != http.StatusOK {
		t.Fatalf("code %d, want 200", code)
	}
	if probs := plainTextOfferProblems(body, forkValue); probs != nil {
		t.Errorf("%v\nbody: %s", probs, body)
	}
	if strings.Contains(body, sourceoffer.Upstream) {
		t.Errorf("the upstream URL occurs in a fork build's API-only body:\n%s", body)
	}
}

// AC6 on the API-only branch of AC9 (binding advisory F13 on verdict-spec-2, F16 on
// verdict-spec-3): a rejected value must not reach a root response on EITHER branch.
// The daemon refuses before any listener exists - there is no listener here to refuse
// and no exit code to return, so the same accept test refuses in place. This is the
// observation F16 says bullet 6's API-only half is satisfiable by.
func TestAPIOnlyRoot_RefusesEveryRejectedValue(t *testing.T) {
	for _, bad := range []string{"", "   ", "not-a-url", "javascript:alert(1)", "//example.com"} {
		old := sourceoffer.URL
		sourceoffer.URL = bad
		srv := newRootServer(t, "", newStore(t))
		code, body := getRoot(t, srv)
		if code != http.StatusServiceUnavailable {
			t.Errorf("source URL %q: code %d, want 503", bad, code)
		}
		// A refusal, not an offer, and never a silent fall back to upstream.
		if strings.Contains(body, sourceoffer.Label+": ") {
			t.Errorf("source URL %q: the refusal body is an offer:\n%s", bad, body)
		}
		if strings.Contains(body, sourceoffer.Upstream) {
			t.Errorf("source URL %q: the refusal fell back to naming upstream:\n%s", bad, body)
		}
		sourceoffer.URL = old
	}
}

// AC6's converse on the same branch (binding advisory F10): an absolute http/https
// value that is NOT clean under RFC 3986 is served, not refused.
func TestAPIOnlyRoot_ServesAnHTMLSignificantValue(t *testing.T) {
	setSourceURL(t, hostileValue)
	srv := newRootServer(t, "", newStore(t))
	code, body := getRoot(t, srv)
	if code != http.StatusOK {
		t.Fatalf("code %d, want 200 - an absolute http/https value is served, never refused for being unclean", code)
	}
	if probs := plainTextOfferProblems(body, hostileValue); probs != nil {
		t.Errorf("%v\nbody: %s", probs, body)
	}
}

// --- AC8: every open read endpoint failing ------------------------------------

var errStoreDown = errors.New("job store is unreachable")

// failingStore fails every open read the API can make. The embedded nil interface
// means any method the read endpoints do NOT call would panic rather than quietly
// return a zero value, so this cannot silently stop covering a new endpoint.
type failingStore struct{ store.Store }

func (failingStore) Summary(context.Context) (map[store.Status]int, error) { return nil, errStoreDown }
func (failingStore) List(context.Context, []store.Status, int) ([]store.Job, error) {
	return nil, errStoreDown
}
func (failingStore) ReclaimedTotal(context.Context) (int64, error) { return 0, errStoreDown }
func (failingStore) Aggregates(context.Context) store.Aggregates {
	return store.Aggregates{
		Outcomes:     store.Breakdown{Err: errStoreDown},
		SkipsByGuard: store.Breakdown{Err: errStoreDown},
		SizeRatio:    store.Spread{Err: errStoreDown},
		EncodeMs:     store.Spread{Err: errStoreDown},
		VmafMean:     store.Spread{Err: errStoreDown},
		VmafMin:      store.Spread{Err: errStoreDown},
	}
}

// AC8: with every open read endpoint failing, the source offer is still present on
// the served page and carries the SAME values it carries when they succeed.
//
// It holds by construction rather than by retry or cache: the offer is substituted
// into the served bytes when the handler is built, from a value stamped into the
// binary, so no store, no endpoint and no request is on its path (binding advisory
// F17). This test proves that claim rather than asserting it - it fails the store
// hard, shows all three open read endpoints returning 500, and compares the offer
// byte for byte against the one a healthy server serves.
func TestSourceOffer_SurvivesEveryOpenReadEndpointFailing(t *testing.T) {
	setSourceURL(t, forkValue)

	healthy := newRootServer(t, "", newStore(t))
	codeH, bodyH := getRoot(t, healthy)
	if codeH != http.StatusOK {
		t.Fatalf("healthy server: code %d, want 200", codeH)
	}

	broken := newRootServer(t, "", failingStore{})
	// Every open read endpoint is down - the precondition of this test.
	for _, ep := range []string{"/api/summary", "/api/queue", "/api/history"} {
		rec := httptest.NewRecorder()
		broken.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ep, nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s: code %d, want 500 (the precondition of this test)", ep, rec.Code)
		}
	}
	codeB, bodyB := getRoot(t, broken)
	if codeB != http.StatusOK {
		t.Fatalf("broken server: GET / code %d, want 200 - the offer must survive", codeB)
	}
	// The root body IS the offer here, so it is compared whole.
	if bodyB != bodyH {
		t.Errorf("the root response changed when the read endpoints failed.\n broken: %s\nhealthy: %s",
			bodyB, bodyH)
	}
	if !strings.Contains(bodyB, forkValue) {
		t.Errorf("the offer lost the source URL in effect: %s", bodyB)
	}
}

// The root page is the root page and nothing else: with no frontend there is no catch-all
// under "/", so a stray asset request 404s rather than being answered with the root body.
// A handler that served the same page for every path would make the offer assertions above
// pass for paths that carry no offer obligation at all.
func TestRoot_ServesOnlyTheRootPath(t *testing.T) {
	srv := newRootServer(t, "", newStore(t))
	for _, p := range []string{"/assets/app.js", "/index.html", "/anything"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: code %d, want 404 - holdfast serves no asset tree", p, rec.Code)
		}
	}
}
