package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/sourceoffer"
	"github.com/NSchatz/holdfast/internal/webui"
)

// The credentials this suite uses. readTok and ctrlTok are deliberately the SAME LENGTH
// and differ in more than one byte, so a case that presented one where the other was
// wanted cannot pass by accident of shape.
const (
	readTok = "read-token-aaaaaaaaaaaaaaaaaaaa"
	ctrlTok = "ctrl-token-bbbbbbbbbbbbbbbbbbbb"
)

// readPaths are the four read endpoints this key gates, spelled once so no case can
// silently cover three of them.
var readPaths = []string{"/api/summary", "/api/queue", "/api/history", "/api/events"}

// seededPaths are the media paths newStore puts in the ledger. A 401 body must carry
// neither, and an authorized 200 must carry at least one - which is what stops the
// "no path leaked" assertions from passing against an endpoint that returns nothing.
var seededPaths = []string{"/lib/active.mkv", "/lib/done.mkv"}

// get issues one request, optionally with an Authorization header, and returns the
// response with its body already read. /api/events never completes on its own, so every
// request carries a deadline and the SSE case reads what arrived before it fired.
func get(t *testing.T, base, path, authorization string) (*http.Response, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("NewRequest(%s): %v", path, err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// An open SSE stream returns a context deadline here, which is the stream STAYING
	// open - the success shape for that endpoint, not a failure. Whatever arrived before
	// the deadline is the body under test.
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// carriesASeededPath reports whether a body names any of the ledger's media paths. It is
// the anti-vacuity half of every "no path leaked" assertion here: an endpoint that
// returned nothing would satisfy the leak check too. /api/queue carries the active row,
// /api/history the terminal one, so the two are looked for together rather than by name.
func carriesASeededPath(body string) bool {
	for _, p := range seededPaths {
		if strings.Contains(body, p) {
			return true
		}
	}
	return false
}

// assertNoLibraryPath fails naming the response that disclosed a media path. This is the
// whole point of the gate: a refusal that leaks the thing it is refusing access to has
// refused nothing.
func assertNoLibraryPath(t *testing.T, where, body string) {
	t.Helper()
	for _, p := range seededPaths {
		if strings.Contains(body, p) {
			t.Errorf("%s disclosed the library path %q:\n%s", where, p, body)
		}
	}
}

// TestReadEndpoints_RequireTheReadTokenWhenSet is the whole read gate, in both directions
// the spec grades: the four /api reads are gated when server_read_token is set and open
// when it is not, AND the dashboard root is served with no credential either way, because
// the page is not this key's to gate.
func TestReadEndpoints_RequireTheReadTokenWhenSet(t *testing.T) {
	// The shipped default. Every existing install is this case, and it must not move: no
	// credential, the same 200 and the same body the daemon served before this key
	// existed.
	t.Run("empty read token serves every read endpoint with no credential", func(t *testing.T) {
		h := newHarness(t, "")
		ts := httptest.NewServer(h.srv)
		defer ts.Close()
		for _, p := range readPaths {
			resp, body := get(t, ts.URL, p, "")
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s with no read token configured = %d, want 200", p, resp.StatusCode)
			}
			// Anti-vacuity: the endpoint answered with the LEDGER, not with an empty
			// shell that would also satisfy a "no path leaked" assertion below.
			if p != "/api/summary" && !carriesASeededPath(body) {
				t.Errorf("GET %s returned no seeded media path, so the open case proves nothing:\n%s", p, body)
			}
		}
	})

	// The gate itself. No Authorization header at all is the shape an unaware client
	// arrives in, and it is the one the 401 has to be exactly right about.
	t.Run("a set read token refuses an unauthenticated read", func(t *testing.T) {
		h := newHarnessWith(t, "", readTok, nil)
		ts := httptest.NewServer(h.srv)
		defer ts.Close()
		for _, p := range readPaths {
			resp, body := get(t, ts.URL, p, "")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("GET %s with a read token set and no credential = %d, want 401", p, resp.StatusCode)
			}
			if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
				t.Errorf("GET %s: WWW-Authenticate = %q, want a Bearer challenge", p, got)
			}
			assertNoLibraryPath(t, "the 401 for "+p, body)
			// Nor may the refusal disclose what would have been accepted.
			if strings.Contains(body, readTok) {
				t.Errorf("the 401 for %s echoed the configured read token:\n%s", p, body)
			}
		}
	})

	// The SSE stream is the one endpoint where "refused" and "refused before writing
	// anything" are different outcomes: handleEvents writes a full snapshot the moment it
	// runs, so a gate placed INSIDE it would push every queued and historical path to the
	// client it was about to refuse.
	t.Run("an unauthenticated /api/events is refused before any SSE data", func(t *testing.T) {
		h := newHarnessWith(t, "", readTok, nil)
		ts := httptest.NewServer(h.srv)
		defer ts.Close()
		resp, body := get(t, ts.URL, "/api/events", "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET /api/events with no credential = %d, want 401", resp.StatusCode)
		}
		if strings.Contains(body, "event: snapshot") {
			t.Errorf("the refused stream still wrote a snapshot frame:\n%s", body)
		}
		if strings.Contains(body, ": ping") {
			t.Errorf("the refused stream still wrote a heartbeat:\n%s", body)
		}
		assertNoLibraryPath(t, "the refused /api/events stream", body)
	})

	// The correct credential must be TRANSPARENT: the same status, the same content type
	// and the same body an ungated server answers with. Both servers read the same store,
	// so a difference is the gate's doing and nothing else's.
	t.Run("the configured read token is answered exactly as an open server answers", func(t *testing.T) {
		st := newStore(t)
		open := newHarnessOn(t, st, "", "", nil)
		gated := newHarnessOn(t, st, "", readTok, nil)
		openTS, gatedTS := httptest.NewServer(open.srv), httptest.NewServer(gated.srv)
		defer openTS.Close()
		defer gatedTS.Close()

		for _, p := range []string{"/api/summary", "/api/queue", "/api/history"} {
			openResp, openBody := get(t, openTS.URL, p, "")
			gatedResp, gatedBody := get(t, gatedTS.URL, p, "Bearer "+readTok)
			if gatedResp.StatusCode != openResp.StatusCode {
				t.Errorf("GET %s: gated status %d, open status %d", p, gatedResp.StatusCode, openResp.StatusCode)
			}
			if got, want := gatedResp.Header.Get("Content-Type"), openResp.Header.Get("Content-Type"); got != want {
				t.Errorf("GET %s: gated Content-Type %q, open %q", p, got, want)
			}
			if !sameJSONIgnoringNow(t, openBody, gatedBody) {
				t.Errorf("GET %s answered differently through the gate:\n open: %s\ngated: %s", p, openBody, gatedBody)
			}
			// Anti-vacuity: two identical EMPTY bodies would compare equal too. The row
			// endpoints must have returned a real ledger path. (/api/summary is
			// aggregates only and carries none by design, so it is exempt.)
			if p != "/api/summary" && !carriesASeededPath(openBody+gatedBody) {
				t.Errorf("GET %s returned no seeded path, so the comparison proves nothing:\n%s", p, gatedBody)
			}
		}

		// The stream, which has no comparable body: the credentialled client gets the
		// snapshot frame the refused one did not.
		resp, body := get(t, gatedTS.URL, "/api/events", "Bearer "+readTok)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /api/events with the read token = %d, want 200", resp.StatusCode)
		}
		if !strings.Contains(body, "event: snapshot") {
			t.Errorf("the authorized stream wrote no snapshot frame:\n%s", body)
		}
	})

	// Every way a credential can be wrong, on every gated endpoint. The two boundary
	// cases are the ones a hand-rolled comparison gets wrong: a value that is a PROPER
	// PREFIX of the token and a value the token is a proper prefix OF both pass a
	// `strings.HasPrefix` and both must be refused.
	t.Run("a malformed or wrong credential is refused", func(t *testing.T) {
		h := newHarnessWith(t, "", readTok, nil)
		ts := httptest.NewServer(h.srv)
		defer ts.Close()
		for _, tc := range []struct {
			name   string
			header string
		}{
			{"no header at all", ""},
			{"a non-Bearer scheme", "Basic " + readTok},
			{"a Bearer with an empty value", "Bearer "},
			{"a Bearer with only whitespace", "Bearer    "},
			{"the bare token with no scheme", readTok},
			{"a proper prefix of the token", "Bearer " + readTok[:len(readTok)-1]},
			{"a value the token is a proper prefix of", "Bearer " + readTok + "x"},
			{"the same length, differing in one byte", "Bearer " + readTok[:len(readTok)-1] + "X"},
			{"the empty string as a scheme-less value", "Bearer\t"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				for _, p := range readPaths {
					resp, body := get(t, ts.URL, p, tc.header)
					if resp.StatusCode != http.StatusUnauthorized {
						t.Errorf("GET %s with %s = %d, want 401", p, tc.name, resp.StatusCode)
					}
					assertNoLibraryPath(t, "the 401 for "+p+" with "+tc.name, body)
				}
			})
		}
	})

	// One Authorization header cannot carry two values, so the operator holding the MORE
	// privileged credential must not be locked out of the LESS privileged surface.
	t.Run("the control token is accepted on every read endpoint", func(t *testing.T) {
		h := newHarnessWith(t, ctrlTok, readTok, nil)
		ts := httptest.NewServer(h.srv)
		defer ts.Close()
		for _, p := range readPaths {
			resp, _ := get(t, ts.URL, p, "Bearer "+ctrlTok)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s with the CONTROL token = %d, want 200", p, resp.StatusCode)
			}
		}
		// Anti-vacuity: the gate is still a gate for this server. A third value, neither
		// token, is still refused.
		for _, p := range readPaths {
			resp, _ := get(t, ts.URL, p, "Bearer neither-of-the-two-configured-values")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("GET %s with an unrelated token = %d, want 401", p, resp.StatusCode)
			}
		}
	})

	// An EMPTY control token must never be a credential. Without the guard in
	// requireReadToken a constant-time compare of "" against "" reports EQUAL, and a
	// daemon with the controls disabled - the shipped default - would serve its gated
	// read API to any request that sent no Authorization header at all.
	t.Run("an empty control token is not a credential for a read", func(t *testing.T) {
		h := newHarnessWith(t, "", readTok, nil)
		ts := httptest.NewServer(h.srv)
		defer ts.Close()
		for _, p := range readPaths {
			for _, header := range []string{"", "Bearer ", "Bearer"} {
				resp, _ := get(t, ts.URL, p, header)
				if resp.StatusCode != http.StatusUnauthorized {
					t.Errorf("GET %s with %q and an EMPTY control token = %d, want 401", p, header, resp.StatusCode)
				}
			}
		}
	})

	// The dashboard root, which this key deliberately does NOT gate. A browser sends no
	// Bearer header on a navigation, so gating the page here would serve a login-less 401
	// to every operator who opened it; the page is the cookie-from-a-login-form item's.
	// Asserted as SAME BYTES rather than merely 200, because a gate that served a
	// different page to an uncredentialled client would also be 200.
	t.Run("the dashboard root is served with no credential while a read token is set", func(t *testing.T) {
		st := newStore(t)
		ui := func() http.Handler { return webui.HandlerFor(sourceoffer.Current()) }
		open := newHarnessOn(t, st, "", "", ui())
		gated := newHarnessOn(t, st, "", readTok, ui())
		openTS, gatedTS := httptest.NewServer(open.srv), httptest.NewServer(gated.srv)
		defer openTS.Close()
		defer gatedTS.Close()

		openResp, openBody := get(t, openTS.URL, "/", "")
		gatedResp, gatedBody := get(t, gatedTS.URL, "/", "")
		if gatedResp.StatusCode != http.StatusOK {
			t.Fatalf("GET / with a read token set and no credential = %d, want 200", gatedResp.StatusCode)
		}
		if openResp.StatusCode != http.StatusOK {
			t.Fatalf("GET / with no read token = %d, want 200", openResp.StatusCode)
		}
		if gatedBody != openBody {
			t.Errorf("the dashboard root served different bytes with a read token set:\n gated len %d\n open  len %d",
				len(gatedBody), len(openBody))
		}
		if len(gatedBody) == 0 {
			t.Error("the dashboard root served an empty body, so the comparison proves nothing")
		}
		if got := gatedResp.Header.Get("WWW-Authenticate"); got != "" {
			t.Errorf("the dashboard root sent a Bearer challenge (%q) - it is not this key's to gate", got)
		}
	})

	// ... and every other path the UI handler owns, not merely "/". This build inlines
	// its CSS and script into one document, so there is no second asset path to fetch
	// today; the case asserts about the ROUTE anyway, because the claim is that the gate
	// does not reach under the UI handler and a future asset route must not have to
	// rediscover that.
	t.Run("an embedded asset path is served with no credential too", func(t *testing.T) {
		const asset = "/assets/app.js"
		stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, "asset:"+r.URL.Path)
		})
		st := newStore(t)
		gated := newHarnessOn(t, st, "", readTok, stub)
		ts := httptest.NewServer(gated.srv)
		defer ts.Close()
		for _, p := range []string{"/", asset, "/assets/app.css"} {
			resp, body := get(t, ts.URL, p, "")
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s with a read token set and no credential = %d, want 200", p, resp.StatusCode)
			}
			if body != "asset:"+p {
				t.Errorf("GET %s returned %q, want the UI handler's own body", p, body)
			}
		}
	})
}

// TestReadToken_IsNotAcceptedForAMutation: the two keys authorise different things, and
// the asymmetry is deliberate. The control token buys reads because one Authorization
// header cannot carry two values; a read token buys NO mutation, because "let me see the
// queue" is not "start encoding my library".
func TestReadToken_IsNotAcceptedForAMutation(t *testing.T) {
	mutations := []string{"/api/rescan", "/api/pause", "/api/resume"}

	t.Run("the read token is refused on every mutating endpoint", func(t *testing.T) {
		h := newHarnessWith(t, ctrlTok, readTok, nil)
		ts := httptest.NewServer(h.srv)
		defer ts.Close()
		for _, p := range mutations {
			resp, err := http.Post(ts.URL+p, "", nil)
			if err != nil {
				t.Fatalf("POST %s: %v", p, err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("POST %s with NO credential = %d, want 401", p, resp.StatusCode)
			}
			_ = body

			resp2, err := postWithToken(ts.URL+p, readTok)
			if err != nil {
				t.Fatalf("POST %s: %v", p, err)
			}
			_ = resp2.Body.Close()
			if resp2.StatusCode != http.StatusUnauthorized {
				t.Errorf("POST %s with the READ token = %d, want 401", p, resp2.StatusCode)
			}
		}
		// Nothing moved. A refusal that had already flipped the flag would be a refusal
		// in the response only.
		if h.ctrl.Paused() {
			t.Error("a refused POST /api/pause paused the engine anyway")
		}
		if h.ctrl.Scanning() {
			t.Error("a refused POST /api/rescan started a scan anyway")
		}

		// Anti-vacuity: the CONTROL token still works on the same endpoints, so the
		// refusals above are about the credential and not about the route.
		resp, err := postWithToken(ts.URL+"/api/pause", ctrlTok)
		if err != nil {
			t.Fatalf("POST /api/pause: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /api/pause with the CONTROL token = %d, want 200", resp.StatusCode)
		}
		if !h.ctrl.Paused() {
			t.Error("the authorized pause did not take effect")
		}
	})

	// The control-disabled behaviour is UNTOUCHED by this key. With no control token the
	// mutating endpoints answer 403 naming server_auth_token - whatever server_read_token
	// holds and whatever credential the request presents - because they are OFF, not
	// merely unauthorized, and an operator sent hunting for the wrong key is an operator
	// who configures the wrong thing.
	t.Run("with no control token the mutations stay disabled with 403", func(t *testing.T) {
		for _, readTokenValue := range []string{"", readTok} {
			h := newHarnessWith(t, "", readTokenValue, nil)
			ts := httptest.NewServer(h.srv)
			for _, p := range mutations {
				for _, credential := range []string{"", readTok, ctrlTok} {
					var resp *http.Response
					var err error
					if credential == "" {
						resp, err = http.Post(ts.URL+p, "", nil)
					} else {
						resp, err = postWithToken(ts.URL+p, credential)
					}
					if err != nil {
						t.Fatalf("POST %s: %v", p, err)
					}
					body, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode != http.StatusForbidden {
						t.Errorf("POST %s (read token %q, credential %q) = %d, want 403",
							p, readTokenValue, credential, resp.StatusCode)
					}
					if !strings.Contains(string(body), "server_auth_token") {
						t.Errorf("POST %s did not name server_auth_token in its refusal:\n%s", p, body)
					}
				}
			}
			if h.ctrl.Paused() || h.ctrl.Scanning() {
				t.Errorf("a disabled control surface still moved state (read token %q)", readTokenValue)
			}
			ts.Close()
		}
	})
}

// TestMetrics_IsNotGatedByTheReadToken records the decision as a test: /metrics is NOT
// gated, by this key or any other, and its reachability stays governed by metrics_enable
// alone. A scrape credential is the one thing a Prometheus deployment most often cannot
// supply, so gating it would break every existing scrape on upgrade - and the exposition
// names no file, which is the premise that makes that safe (asserted in internal/metrics).
func TestMetrics_IsNotGatedByTheReadToken(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, "holdfast_files_total{outcome=\"done\"} 1\n")
	})
	st := newStore(t)
	ctx := context.Background()
	ctrl := NewController(ctx, func(context.Context) error { return nil }, discard())
	hub := NewHub(st, ctrl, discard())

	for _, tc := range []struct{ name, readToken string }{
		{"with no read token", ""},
		{"with a read token set", readTok},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(ctx, configZero(), secret.Value{}, secret.NewValue(tc.readToken),
				st, ctrl, hub, nil, handler, discard())
			ts := httptest.NewServer(srv)
			defer ts.Close()
			resp, body := get(t, ts.URL, "/metrics", "")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /metrics %s and no credential = %d, want 200", tc.name, resp.StatusCode)
			}
			if !strings.Contains(body, "holdfast_files_total") {
				t.Errorf("GET /metrics %s returned no exposition:\n%s", tc.name, body)
			}
			if got := resp.Header.Get("WWW-Authenticate"); got != "" {
				t.Errorf("/metrics sent a Bearer challenge (%q) - it is governed by metrics_enable alone", got)
			}
		})
	}
}

// postWithToken issues a POST carrying a bearer credential.
func postWithToken(url, token string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return http.DefaultClient.Do(req)
}

// sameJSONIgnoringNow compares two JSON bodies with the one field that legitimately moves
// between two requests removed. /api/queue carries `now` so a client can age a row against
// the server's clock; every other field in these responses is read from the ledger.
func sameJSONIgnoringNow(t *testing.T, a, b string) bool {
	t.Helper()
	decode := func(s string) any {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatalf("the response is not JSON (%v):\n%s", err, s)
		}
		if m, ok := v.(map[string]any); ok {
			delete(m, "now")
		}
		return v
	}
	return reflect.DeepEqual(decode(a), decode(b))
}
