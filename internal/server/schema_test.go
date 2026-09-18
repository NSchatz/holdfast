package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/version"
)

// fetchSchema fetches the surface document from a live server and parses it.
func fetchSchema(t *testing.T, base string) (Document, []byte) {
	t.Helper()
	resp, err := http.Get(base + SchemaPath)
	if err != nil {
		t.Fatalf("GET %s: %v", SchemaPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", SchemaPath, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s answered %d: %s", SchemaPath, resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); ct != mediaJSON {
		t.Fatalf("GET %s content type is %q, want %q", SchemaPath, ct, mediaJSON)
	}
	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the served document is not JSON: %v", err)
	}
	return doc, raw
}

// routedSet walks a router and returns the "METHOD path" set it actually routes.
func routedSet(t *testing.T, routes chi.Routes) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	if err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out[method+" "+route] = true
		return nil
	}); err != nil {
		t.Fatalf("walking the router: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the walk found no routes at all, so nothing below compares anything")
	}
	return out
}

func documentSet(doc Document) map[string]bool {
	out := map[string]bool{}
	for _, ep := range doc.Endpoints {
		out[ep.Method+" "+ep.Path] = true
	}
	return out
}

// TestSchemaEndpoint_ServesItsOwnFormatVersionAndEveryRoutedEndpoint grades [AC-1]: a
// running server answers GET /api/schema with a document naming its own format and that
// format's version, the holdfast version the binary reports, and every endpoint it routes
// with the status codes, media types and body shapes each answers with.
func TestSchemaEndpoint_ServesItsOwnFormatVersionAndEveryRoutedEndpoint(t *testing.T) {
	h := newHarness(t, "")
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	doc, _ := fetchSchema(t, ts.URL)

	if doc.Schema != SchemaFormat {
		t.Errorf("schema = %q, want %q - the document must name its own format", doc.Schema, SchemaFormat)
	}
	if doc.SchemaVersion != SchemaFormatVersion {
		t.Errorf("schema_version = %q, want %q", doc.SchemaVersion, SchemaFormatVersion)
	}
	if doc.HoldfastVersion != version.Version {
		t.Errorf("holdfast_version = %q, want %q - the document names the version THIS binary reports",
			doc.HoldfastVersion, version.Version)
	}
	if len(doc.Endpoints) == 0 {
		t.Fatal("the document lists no endpoint at all")
	}

	kinds := map[string]bool{
		kindObject: true, kindArray: true, kindMap: true, kindString: true,
		kindInteger: true, kindNumber: true, kindBoolean: true,
		kindText: true, kindEmpty: true, kindStream: true, kindShape: true,
	}
	for _, ep := range doc.Endpoints {
		if ep.Method == "" || ep.Path == "" {
			t.Errorf("an endpoint carries method %q path %q", ep.Method, ep.Path)
		}
		if len(ep.Responses) == 0 {
			t.Errorf("%s %s lists no status code it can return", ep.Method, ep.Path)
		}
		for _, r := range ep.Responses {
			if r.Status < 100 || r.Status > 599 {
				t.Errorf("%s %s lists status %d", ep.Method, ep.Path, r.Status)
			}
			if !kinds[r.Body.Kind] {
				t.Errorf("%s %s %d declares body kind %q, which is not in the format's vocabulary",
					ep.Method, ep.Path, r.Status, r.Body.Kind)
			}
			// A body with no bytes carries no media type, and that is the only case: every
			// response that writes anything says what it writes it as.
			if r.MediaType == "" && r.Body.Kind != kindEmpty {
				t.Errorf("%s %s %d declares no media type for a %s body",
					ep.Method, ep.Path, r.Status, r.Body.Kind)
			}
		}
	}
}

// TestSchemaDocument_ListsTheRoutedPathsAndNoOthers grades [AC-2]: the document is the
// router read back. A route present at construction appears in it, a route absent from
// construction is absent from it, and nothing hand-maintained stands between the two.
func TestSchemaDocument_ListsTheRoutedPathsAndNoOthers(t *testing.T) {
	h := newHarness(t, "tok")
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	doc, _ := fetchSchema(t, ts.URL)
	routed := routedSet(t, h.srv.mux)
	described := documentSet(doc)

	for r := range routed {
		if !described[r] {
			t.Errorf("the router routes %q and the served document does not list it", r)
		}
	}
	for d := range described {
		if !routed[d] {
			t.Errorf("the served document lists %q and the router does not route it", d)
		}
	}

	// The one route New makes conditional is the proof that the list is derived rather
	// than declared: build the same server with and without the Prometheus handler and
	// the document moves with the construction.
	withMetrics, err := New(context.Background(), config.Config{}, secret.Value{}, secret.Value{},
		nil, nil, nil, http.NotFoundHandler(), discard()).Surface()
	if err != nil {
		t.Fatalf("surface with metrics: %v", err)
	}
	withoutMetrics, err := New(context.Background(), config.Config{}, secret.Value{}, secret.Value{},
		nil, nil, nil, nil, discard()).Surface()
	if err != nil {
		t.Fatalf("surface without metrics: %v", err)
	}
	if !documentSet(withMetrics)["GET /metrics"] {
		t.Error("a server constructed WITH the Prometheus handler does not list /metrics")
	}
	if documentSet(withoutMetrics)["GET /metrics"] {
		t.Error("a server constructed WITHOUT the Prometheus handler still lists /metrics, " +
			"so the document is not being read off the router")
	}
}

// TestSchemaDocument_ServedBytesAreTheGateBytes grades [AC-3]: the bytes the server serves
// and the bytes the diff gate generates from the same build are identical, so the gate can
// never grade a surface the server does not serve. Determinism is asserted with them: a
// document that reordered between renderings would make every diff a false positive.
func TestSchemaDocument_ServedBytesAreTheGateBytes(t *testing.T) {
	// Constructed exactly as ReferenceSurface constructs it, which is the full surface the
	// gate and the baseline are about.
	srv := New(context.Background(), config.Config{}, secret.Value{}, secret.Value{},
		nil, nil, nil, http.NotFoundHandler(), discard())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	_, served := fetchSchema(t, ts.URL)
	gate, err := ReferenceSurfaceJSON()
	if err != nil {
		t.Fatalf("the gate could not generate the surface: %v", err)
	}
	if !bytes.Equal(served, gate) {
		t.Errorf("the served document and the gate's document differ.\nserved:\n%s\ngate:\n%s", served, gate)
	}

	again, err := ReferenceSurfaceJSON()
	if err != nil {
		t.Fatalf("second generation: %v", err)
	}
	if !bytes.Equal(gate, again) {
		t.Error("two generations from the same build produced different bytes, so no diff against " +
			"a baseline could be believed")
	}
}

// TestSchemaDocument_DescribesEveryJSONReadBodyAsAClosedFieldSet grades [AC-4]: a live
// response from each JSON read endpoint validates against the shape the served document
// declares for it, and each of those bodies is declared as a closed set of named fields
// rather than as an object of unconstrained keys.
func TestSchemaDocument_DescribesEveryJSONReadBodyAsAClosedFieldSet(t *testing.T) {
	h := newHarness(t, "")
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	doc, _ := fetchSchema(t, ts.URL)

	for _, path := range []string{"/api/summary", "/api/queue", "/api/history"} {
		body := getBody(t, ts.URL+path)

		// The declaration itself: a closed set of named fields, each with a type. An
		// object of unconstrained keys would make the validation below pass while
		// describing nothing, which is H4's property lost rather than met.
		shape := declaredBody(t, doc, http.MethodGet, path, http.StatusOK)
		if shape.Kind != kindObject {
			t.Errorf("%s declares a %q body; a read endpoint's body must be a closed object", path, shape.Kind)
		}
		if len(shape.Fields) == 0 {
			t.Errorf("%s declares an object with no named fields, which constrains nothing", path)
		}
		for _, f := range shape.Fields {
			if f.Name == "" || f.Type.Kind == "" {
				t.Errorf("%s declares a field with no name or no type: %+v", path, f)
			}
		}

		bad, err := doc.ValidateResponse(http.MethodGet, path, http.StatusOK, body)
		if err != nil {
			t.Fatalf("%s: the document could not be used to check the response: %v", path, err)
		}
		if len(bad) != 0 {
			t.Errorf("%s: the live response does not match the served document:\n  %s",
				path, strings.Join(bad, "\n  "))
		}
	}

	// The validator BITES, in both directions, or the pass above is worth nothing.
	t.Run("an undeclared field on the wire is found", func(t *testing.T) {
		mutated := dropDeclaredField(t, doc, "/api/queue", "queue")
		bad, err := mutated.ValidateResponse(http.MethodGet, "/api/queue", http.StatusOK,
			getBody(t, ts.URL+"/api/queue"))
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if len(bad) == 0 {
			t.Error("a document that declares no `queue` field found a response carrying one valid")
		}
	})
	t.Run("a required field the response omits is found", func(t *testing.T) {
		bad, err := doc.ValidateResponse(http.MethodGet, "/api/queue", http.StatusOK, []byte(`{}`))
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if len(bad) == 0 {
			t.Error("an empty body validated against a document declaring required fields")
		}
	})
}

// TestReadResponseTypes_EncodeTheBytesTheMapLiteralsDid grades [AC-4]'s standing
// requirement that giving a handler a named response type changes nothing a caller sees.
// The named types exist so the document has a closed field set to describe; a caller must
// not be able to tell.
func TestReadResponseTypes_EncodeTheBytesTheMapLiteralsDid(t *testing.T) {
	jobs := []jobDTO{{Path: "/lib/a.mkv", Status: "done", Profile: ""}}
	total := rowTotalDTO{Available: true, Covers: "terminal", Cap: 200}
	now := time.Now().Unix()

	for _, tc := range []struct {
		name    string
		literal any
		named   any
	}{
		{
			name:    "GET /api/queue",
			literal: map[string]any{"queue": jobs, "now": now, "queue_total": total},
			named:   queueResponse{Now: now, Queue: jobs, QueueTotal: total},
		},
		{
			name:    "GET /api/history",
			literal: map[string]any{"history": jobs, "history_total": total},
			named:   historyResponse{History: jobs, HistoryTotal: total},
		},
		{
			name:    "POST /api/rescan",
			literal: map[string]any{"started": true, "reason": "", "paused": false, "scanning": true},
			named:   rescanResponse{Paused: false, Reason: "", Scanning: true, Started: true},
		},
		{
			name:    "POST /api/pause",
			literal: map[string]any{"paused": true, "scanning": false},
			named:   controlToggleResponse{Paused: true, Scanning: false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			was, err := json.Marshal(tc.literal)
			if err != nil {
				t.Fatal(err)
			}
			is, err := json.Marshal(tc.named)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(was, is) {
				t.Errorf("the named type changed the bytes on the wire.\nwas: %s\nis:  %s", was, is)
			}
		})
	}
}

// TestSchemaEndpoint_RefusesAMethodItDoesNotServe grades [AC-5]: a method the schema
// endpoint does not serve is answered 405, and never falls through to the root page.
func TestSchemaEndpoint_RefusesAMethodItDoesNotServe(t *testing.T) {
	h := newHarness(t, "")
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, err := http.NewRequest(method, ts.URL+SchemaPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, SchemaPath, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s answered %d, want 405", method, SchemaPath, resp.StatusCode)
		}
		if strings.Contains(string(body), "holdfast API is running") {
			t.Errorf("%s %s fell through to the root page: %q", method, SchemaPath, body)
		}
	}
}

// TestSchemaEndpoint_IsOpenAndCarriesNothingButTheSurface grades [AC-6]: the document is
// served unauthenticated and identically whether or not a control token is configured, and
// it carries no credential value, no configured filesystem path and no per-file datum.
func TestSchemaEndpoint_IsOpenAndCarriesNothingButTheSurface(t *testing.T) {
	const (
		controlToken = "control-token-b6f1a2"
		readToken    = "read-token-9d4c7e"
		libraryRoot  = "/distinctive-library-root-4f2a"
	)
	st := newStore(t)
	build := func(token, read string) *Server {
		ctx := context.Background()
		ctrl := NewController(ctx, func(context.Context) error { return nil }, discard())
		hub := NewHub(st, ctrl, discard())
		cfg := config.Config{LibraryRoots: []string{libraryRoot}}
		return New(ctx, cfg, secret.NewValue(token), secret.NewValue(read), st, ctrl, hub, nil, discard())
	}

	openTS := httptest.NewServer(build("", ""))
	defer openTS.Close()
	gatedTS := httptest.NewServer(build(controlToken, readToken))
	defer gatedTS.Close()

	// No credential is sent to either, and both answer 200 with the same bytes.
	_, openBytes := fetchSchema(t, openTS.URL)
	_, gatedBytes := fetchSchema(t, gatedTS.URL)
	if !bytes.Equal(openBytes, gatedBytes) {
		t.Errorf("the document differs with a token configured.\nopen:\n%s\ngated:\n%s", openBytes, gatedBytes)
	}

	// The exposure bound. The ledger rows newStore seeds are real per-file data on this
	// server, and both tokens and the library root are real configuration on it.
	for _, forbidden := range []string{
		controlToken, readToken, libraryRoot,
		"/lib/active.mkv", "/lib/done.mkv",
	} {
		if bytes.Contains(openBytes, []byte(forbidden)) {
			t.Errorf("the surface document carries %q, which is not a path, method, status code, "+
				"media type, field name or field type", forbidden)
		}
	}

	// And the rows really are there to have leaked, so the assertion above is not passing
	// over an empty ledger.
	jobs, err := st.List(context.Background(), terminal, 10)
	if err != nil || len(jobs) == 0 {
		t.Fatalf("the ledger holds no terminal row, so the exposure check proved nothing: %v", err)
	}
}

// --- helpers ------------------------------------------------------------------

func getBody(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s answered %d: %s", url, resp.StatusCode, body)
	}
	return body
}

func declaredBody(t *testing.T, doc Document, method, path string, status int) Shape {
	t.Helper()
	for _, ep := range doc.Endpoints {
		if ep.Method != method || ep.Path != path {
			continue
		}
		for _, r := range ep.Responses {
			if r.Status == status {
				return r.Body
			}
		}
	}
	t.Fatalf("the document declares no %s %s %d", method, path, status)
	return Shape{}
}

// dropDeclaredField returns a copy of doc with one top-level field removed from an
// endpoint's 200 body, which is the stale-document case the validator exists to catch.
func dropDeclaredField(t *testing.T, doc Document, path, field string) Document {
	t.Helper()
	out := Document{Schema: doc.Schema, SchemaVersion: doc.SchemaVersion, HoldfastVersion: doc.HoldfastVersion}
	dropped := false
	for _, ep := range doc.Endpoints {
		if ep.Path == path && ep.Method == http.MethodGet {
			responses := make([]Response, 0, len(ep.Responses))
			for _, r := range ep.Responses {
				if r.Status == http.StatusOK {
					kept := make([]Field, 0, len(r.Body.Fields))
					for _, f := range r.Body.Fields {
						if f.Name == field {
							dropped = true
							continue
						}
						kept = append(kept, f)
					}
					r.Body.Fields = kept
				}
				responses = append(responses, r)
			}
			ep.Responses = responses
		}
		out.Endpoints = append(out.Endpoints, ep)
	}
	if !dropped {
		t.Fatalf("%s declares no field %q to drop, so the mutation proved nothing", path, field)
	}
	return out
}
