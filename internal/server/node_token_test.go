package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/NSchatz/holdfast/internal/node"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/version"
)

// The node credential's separation (docs/design/nodes.md#leases): `node_token` opens the
// lease endpoints and nothing else, and no other token opens them.

const nodeTok = "node-token-91d4e7"

// nodeHarness is a server with all four credentials configured and a real lease hub wired
// behind the node gate.
type nodeHarness struct {
	*harness
	ts  *httptest.Server
	hub *node.Hub
}

func newNodeHarness(t *testing.T, nodeToken string) *nodeHarness {
	t.Helper()
	h := newHarnessWith(t, hookControlTok, hookReadTok)
	h.srv.SetWebhookToken(secret.NewValue(hookTok))
	if nodeToken != "" {
		h.srv.SetNodeToken(secret.NewValue(nodeToken))
	}
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	hub := node.New(node.Options{Ledger: h.st, BaseCtx: ctx, Version: version.Version,
		LongPoll: 30 * time.Millisecond, Log: discard()})
	if _, err := hub.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	hub.Ready()
	h.srv.SetNodes(hub)
	ts := httptest.NewServer(h.srv)
	t.Cleanup(ts.Close)
	return &nodeHarness{harness: h, ts: ts, hub: hub}
}

// routed is every method and route the router serves, read from the router itself.
func (h *nodeHarness) routed(t *testing.T) []string {
	t.Helper()
	var served []string
	var routes chi.Routes = h.srv.mux
	if err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		served = append(served, method+" "+route)
		return nil
	}); err != nil {
		t.Fatalf("walking the routes: %v", err)
	}
	sort.Strings(served)
	return served
}

// send issues one request for a routed "METHOD /path", with a lease id in place of {id}.
func (h *nodeHarness) send(t *testing.T, route, body string, auth func(*http.Request)) (int, string) {
	t.Helper()
	method, path, _ := strings.Cut(route, " ")
	path = strings.ReplaceAll(path, "{id}", "0123456789abcdef0123456789abcdef")
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	req, err := http.NewRequestWithContext(ctx, method, h.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if auth != nil {
		auth(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", route, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// An event stream never ends; the first kilobyte is all an assertion here reads.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return resp.StatusCode, string(raw)
}

func isNodeRoute(route string) bool {
	_, path, _ := strings.Cut(route, " ")
	return strings.HasPrefix(path, NodePathPrefix+"/")
}

// gatedElsewhere reports whether a route sits behind the read, control or webhook gate:
// every route under /api but the lease endpoints and the open surface document.
func gatedElsewhere(route string) bool {
	_, path, _ := strings.Cut(route, " ")
	return strings.HasPrefix(path, "/api/") && !isNodeRoute(route) && path != SchemaPath
}

const acquireJSON = `{"node":"node-a","version":"` + "%s" + `","slots":1,"mode":"mapped","encoders":["libx265"]}`

// TestNodeToken_CannotCallAControlOrReadEndpoint: the node token on EVERY read, control and
// webhook route is refused, and nothing it asked for happens. The routes are read from the
// router, so a route added tomorrow is covered on the day it lands.
func TestNodeToken_CannotCallAControlOrReadEndpoint(t *testing.T) {
	h := newNodeHarness(t, nodeTok)
	seen := map[string]bool{}
	for _, route := range h.routed(t) {
		if !gatedElsewhere(route) {
			continue
		}
		seen[route] = true
		for name, auth := range map[string]func(*http.Request){
			"as a bearer":             bearer(nodeTok),
			"as a basic password":     basic("holdfast", nodeTok),
			"as a lower-case bearer":  func(r *http.Request) { r.Header.Set("Authorization", "bearer "+nodeTok) },
			"as a bearer, in a query": func(r *http.Request) { bearer(nodeTok)(r); r.URL.RawQuery = "token=" + nodeTok },
		} {
			code, raw := h.send(t, route, `{"paths":["/x.mkv"],"path":"/x.mkv"}`, auth)
			if code != http.StatusUnauthorized {
				t.Errorf("%s with the node token %s = %d, want 401: %s", route, name, code, raw)
			}
			if strings.Contains(raw, nodeTok) {
				t.Errorf("%s echoed the node token: %s", route, raw)
			}
		}
	}
	// Anti-vacuity: the walk saw every group the node token must not open.
	for _, must := range []string{"GET /api/summary", "GET /api/events", "POST /api/pause", "POST /api/scan",
		"GET /api/search", "DELETE /api/exclusions/", "POST /api/webhook/sonarr", "PUT /api/webhook/radarr"} {
		if !seen[must] {
			t.Errorf("the walk did not try %s; it is not enumerating the real surface: %v", must, seen)
		}
	}
	if len(seen) < 17 {
		t.Errorf("the walk tried %d routes, fewer than the 17 this surface had when the test was written", len(seen))
	}
	// Nothing the node token asked for happened.
	if h.ctrl.Paused() || h.ctrl.Scanning() {
		t.Errorf("a refused request changed the controller: paused=%v scanning=%v", h.ctrl.Paused(), h.ctrl.Scanning())
	}
	if ex, err := h.st.ExcludedPaths(context.Background()); err != nil || len(ex) != 0 {
		t.Errorf("a refused request withheld a path: %v, %v", ex, err)
	}
	// And each of those refusals was about the CREDENTIAL: the group's own token is taken.
	for route := range seen {
		own := bearer(hookControlTok)
		if strings.HasPrefix(strings.SplitN(route, " ", 2)[1], WebhookPathPrefix) {
			own = bearer(hookTok)
		}
		if route == "POST /api/rescan" || route == "POST /api/pause" {
			continue // each would start a scan or pause this harness; TestAuth_MutatingEndpoints owns them
		}
		if code, raw := h.send(t, route, `{}`, own); code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Errorf("%s with its own group's token = %d: %s", route, code, raw)
		}
	}
}

// TestNodeToken_OtherTokensCannotCallNodeEndpoints: the control, read and webhook tokens,
// no credential and a near miss are all refused on every lease endpoint, and the node token
// is taken.
func TestNodeToken_OtherTokensCannotCallNodeEndpoints(t *testing.T) {
	h := newNodeHarness(t, nodeTok)
	var routes []string
	for _, route := range h.routed(t) {
		if isNodeRoute(route) {
			routes = append(routes, route)
		}
	}
	want := []string{
		"POST /api/node/v1/leases", "POST /api/node/v1/leases/{id}/complete", "POST /api/node/v1/leases/{id}/fail",
		"POST /api/node/v1/leases/{id}/heartbeat", "PUT /api/node/v1/leases/{id}/output",
	}
	if fmt.Sprint(routes) != fmt.Sprint(want) {
		t.Fatalf("the router serves the lease routes %v, want %v", routes, want)
	}
	body := fmt.Sprintf(acquireJSON, version.Version)
	for _, route := range routes {
		for name, auth := range map[string]func(*http.Request){
			"no credential":                       nil,
			"the control token":                   bearer(hookControlTok),
			"the read token":                      bearer(hookReadTok),
			"the webhook token":                   bearer(hookTok),
			"the webhook token as basic password": basic("sonarr", hookTok),
			"the node token as basic password":    basic("node", nodeTok),
			"an empty bearer":                     bearer(""),
			"the node token with a suffix":        bearer(nodeTok + "x"),
			"a prefix of the node token":          bearer(nodeTok[:len(nodeTok)-1]),
			"the node token under another scheme": func(r *http.Request) { r.Header.Set("Authorization", "Token "+nodeTok) },
			"the node token in X-Api-Key":         func(r *http.Request) { r.Header.Set("X-Api-Key", nodeTok) },
			"the node token in the query string": func(r *http.Request) {
				r.URL.RawQuery = "token=" + nodeTok + "&access_token=" + nodeTok
			},
		} {
			code, raw := h.send(t, route, body, auth)
			if code != http.StatusUnauthorized {
				t.Errorf("%s with %s = %d, want 401: %s", route, name, code, raw)
			}
			for _, tok := range []string{nodeTok, hookControlTok, hookReadTok, hookTok} {
				if strings.Contains(raw, tok) {
					t.Errorf("%s with %s echoed a credential: %s", route, name, raw)
				}
			}
		}
	}
	// No refused request reached the lease ledger.
	if live, err := h.st.LiveLeases(context.Background()); err != nil || len(live) != 0 {
		t.Errorf("a refused request left %d lease rows (%v)", len(live), err)
	}

	// The node token is taken on each, and reaches the hub: an acquire with no work waiting
	// answers 204 after the long-poll, and a call on a lease that does not exist 404.
	if code, raw := h.send(t, "POST "+NodePathPrefix+node.RouteLeases, body, bearer(nodeTok)); code != http.StatusNoContent {
		t.Errorf("an acquire with the node token = %d, want 204 from the hub: %s", code, raw)
	}
	for _, route := range routes[1:] {
		code, raw := h.send(t, route, `{"epoch":1,"reason":"why"}`, func(r *http.Request) {
			bearer(nodeTok)(r)
			r.Header.Set(node.EpochHeader, "1")
			r.Header.Set("Content-Digest", "sha-256=:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=:")
		})
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Errorf("%s with the node token = %d, want it past the gate: %s", route, code, raw)
		}
	}
}

// TestNodeToken_UnsetAnswers403NamingTheKey: with no node_token the whole group is off,
// whatever credential is presented, the way control is without its token.
func TestNodeToken_UnsetAnswers403NamingTheKey(t *testing.T) {
	h := newNodeHarness(t, "")
	n := 0
	for _, route := range h.routed(t) {
		if !isNodeRoute(route) {
			continue
		}
		n++
		for name, auth := range map[string]func(*http.Request){
			"no credential":     nil,
			"an empty bearer":   bearer(""),
			"the control token": bearer(hookControlTok),
			"the read token":    bearer(hookReadTok),
			"the webhook token": bearer(hookTok),
		} {
			code, raw := h.send(t, route, fmt.Sprintf(acquireJSON, version.Version), auth)
			if code != http.StatusForbidden {
				t.Errorf("%s with %s = %d, want 403: %s", route, name, code, raw)
			}
			if !strings.Contains(raw, "node_token") {
				t.Errorf("the 403 on %s does not name node_token: %s", route, raw)
			}
		}
	}
	if n != 5 {
		t.Errorf("the walk found %d lease routes, want 5", n)
	}
	// The other groups answer exactly as they do without this key.
	if code, _ := h.send(t, "GET /api/summary", "", bearer(hookReadTok)); code != http.StatusOK {
		t.Errorf("GET /api/summary with the read token = %d with node_token unset, want 200", code)
	}
}

// TestNodeEndpoints_AnswersMatchTheSurfaceDocument: a lease run end to end through the
// real router and the node gate, each live answer validated against the document.
func TestNodeEndpoints_AnswersMatchTheSurfaceDocument(t *testing.T) {
	h := newNodeHarness(t, nodeTok)
	doc, err := h.srv.Surface()
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	declared := map[string]string{}
	for _, ep := range doc.Endpoints {
		if strings.HasPrefix(ep.Path, NodePathPrefix) {
			var codes []int
			for _, r := range ep.Responses {
				codes = append(codes, r.Status)
			}
			declared[ep.Method+" "+ep.Path] = fmt.Sprint(codes)
		}
	}
	for key, want := range map[string]string{
		"POST /api/node/v1/leases":                "[200 204 400 401 403 405 409 503]",
		"POST /api/node/v1/leases/{id}/heartbeat": "[200 400 401 403 404 405 410 500 503]",
		"PUT /api/node/v1/leases/{id}/output":     "[200 400 401 403 404 405 408 409 410 411 413 500 503]",
		"POST /api/node/v1/leases/{id}/complete":  "[200 400 401 403 404 405 409 410 500 503]",
		"POST /api/node/v1/leases/{id}/fail":      "[200 400 401 403 404 405 410 500 503]",
	} {
		if declared[key] != want {
			t.Errorf("%s declares %s, want %s", key, declared[key], want)
		}
	}
	if len(declared) != 5 {
		t.Errorf("the document declares %d lease endpoints, want 5: %v", len(declared), declared)
	}

	call := func(method, route, id string, body []byte, hdr map[string]string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, h.ts.URL+NodePathPrefix+strings.ReplaceAll(route, "{id}", id), bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+nodeTok)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusNoContent {
			// Declared with no body, which is not JSON to validate: it must carry none.
			if len(raw) != 0 || resp.Header.Get("Retry-After") == "" {
				t.Errorf("%s %s 204 carries a body %q or no Retry-After", method, route, raw)
			}
			return resp.StatusCode, raw
		}
		bad, err := doc.ValidateResponse(method, NodePathPrefix+route, resp.StatusCode, raw)
		if err != nil {
			t.Fatalf("%s %s %d: %v", method, route, resp.StatusCode, err)
		}
		if len(bad) != 0 {
			t.Errorf("%s %s %d does not match the document: %v", method, route, resp.StatusCode, bad)
		}
		return resp.StatusCode, raw
	}
	acquire := []byte(fmt.Sprintf(acquireJSON, version.Version))

	// The engine's side: one job offered to whichever node asks.
	dir := t.TempDir()
	src := filepath.Join(dir, "film.mkv")
	if err := os.WriteFile(src, bytes.Repeat([]byte{'s'}, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(src)
	job := node.Job{Path: src, Key: "fp", Temp: filepath.Join(dir, "film.__transcoding__.mkv.holdfast-part"),
		Pre: []string{"-nostdin"}, Body: []string{"-c:v", "libx265"}, Encoder: "libx265",
		SourceSize: fi.Size(), SourceModTime: fi.ModTime(), ReservedBytes: fi.Size()}
	type result struct {
		res node.Result
		err error
	}
	done := make(chan result, 1)
	go func() {
		tk, err := h.hub.WaitDemand(context.Background())
		if err != nil {
			done <- result{err: err}
			return
		}
		res, err := h.hub.Encode(context.Background(), tk, job, nil)
		done <- result{res, err}
	}()

	code, raw := call("POST", node.RouteLeases, "", acquire, nil)
	var lease node.AcquireResponse
	if code != 200 || json.Unmarshal(raw, &lease) != nil {
		t.Fatalf("acquire through the server = %d %s, want a lease", code, raw)
	}
	out := []byte("the encoded output")
	digest := node.FormatDigest(sha256Sum(out))
	if code, raw := call("POST", node.RouteHeartbeat, lease.LeaseID, []byte(`{"epoch":1,"progress":0.5}`), nil); code != 200 {
		t.Fatalf("heartbeat = %d %s", code, raw)
	}
	if code, _ := call("POST", node.RouteHeartbeat, lease.LeaseID, []byte(`{"epoch":9}`), nil); code != 410 {
		t.Errorf("a heartbeat at a stale epoch = %d, want 410", code)
	}
	if code, _ := call("POST", node.RouteComplete, lease.LeaseID,
		[]byte(fmt.Sprintf(`{"epoch":1,"output_digest":%q,"source_digest":%q,"output_bytes":%d}`, digest, digest, len(out))), nil); code != 409 {
		t.Errorf("a completion before the upload = %d, want 409", code)
	}
	if code, _ := call("PUT", node.RouteOutput, lease.LeaseID, bytes.Repeat([]byte{'x'}, 1000),
		map[string]string{node.EpochHeader: "1", "Content-Digest": digest}); code != 413 {
		t.Errorf("an output as large as its source = %d, want 413", code)
	}
	if code, raw := call("PUT", node.RouteOutput, lease.LeaseID, out,
		map[string]string{node.EpochHeader: "1", "Content-Digest": digest}); code != 200 {
		t.Fatalf("upload = %d %s", code, raw)
	}
	if code, raw := call("POST", node.RouteComplete, lease.LeaseID,
		[]byte(fmt.Sprintf(`{"epoch":1,"output_digest":%q,"source_digest":%q,"output_bytes":%d}`, digest, digest, len(out))), nil); code != 200 {
		t.Fatalf("complete = %d %s", code, raw)
	}
	r := <-done
	if r.err != nil || r.res.OutputBytes != int64(len(out)) || r.res.OutputDigest != digest {
		t.Fatalf("the engine's call returned %+v, %v", r.res, r.err)
	}
	if got, err := os.ReadFile(job.Temp); err != nil || !bytes.Equal(got, out) {
		t.Errorf("the working file holds %q (%v), want the uploaded output", got, err)
	}
	if code, _ := call("POST", node.RouteFail, lease.LeaseID, []byte(`{"epoch":1,"reason":"late"}`), nil); code != 410 {
		t.Errorf("a fail after the completion = %d, want 410", code)
	}
	if code, _ := call("POST", node.RouteFail, "ffffffffffffffffffffffffffffffff", []byte(`{"epoch":1,"reason":"late"}`), nil); code != 404 {
		t.Errorf("a fail on an unknown lease = %d, want 404", code)
	}
	if code, _ := call("POST", node.RouteLeases, "", []byte(`{`), nil); code != 400 {
		t.Errorf("a malformed acquire = %d, want 400", code)
	}
	if code, _ := call("POST", node.RouteLeases, "", []byte(strings.Replace(string(acquire), version.Version, "another", 1)), nil); code != 409 {
		t.Errorf("an acquire at another version = %d, want 409", code)
	}
	if code, _ := call("POST", node.RouteLeases, "", acquire, nil); code != 204 {
		t.Errorf("an acquire with no work = %d, want 204", code)
	}
	h.srv.SetNodes(nil)
	if code, _ := call("POST", node.RouteLeases, "", acquire, nil); code != 503 {
		t.Errorf("an acquire with no hub wired = %d, want 503", code)
	}
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
