package server

// POST /api/scan (S0093). This endpoint is the only NETWORK-REACHABLE entry point into a
// pipeline that ends in the deletion of an original, so the suite is written around the
// two properties that carry the whole risk and can only be established here:
//
//   - a submitted path is RESOLVED before it is judged, and a path that does not resolve
//     beneath a configured library root never reaches the queue at all;
//   - a refusal is a refusal in substance and not only in the status line: nothing was
//     enqueued, nothing was processed, no ledger row was written.
//
// The eligibility decision under test is the REAL one, over a real tree - engine.Submissions
// wired to a real engine and a real store - because the whole point of it is that the
// endpoint has no rules of its own. A substitute would grade the substitute.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/store"
)

// ---- harness -----------------------------------------------------------------

// scanHarness is a server wired to a REAL submission queue over a REAL library root and an
// EMPTY ledger. The ledger has to start empty: most of these cases assert that a refusal
// wrote no row, and a seeded store would make "no row" unassertable.
type scanHarness struct {
	srv  *Server
	ctrl *Controller
	subs *engine.Submissions
	eng  *engine.Engine
	st   *store.SQLite
	root string
	ts   *httptest.Server
	// scanStarted receives once per whole-library scan the controller starts, so a
	// refusal can be shown not to have started one.
	scanStarted chan struct{}
}

// newScanHarness builds it. The queue's pool is NOT started, so an accepted path sits in
// the channel: every case here is about admission, and the cases about processing are in
// internal/engine where the pipeline is.
func newScanHarness(t *testing.T, token string) *scanHarness {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Config{
		LibraryRoots: []string{root},
		VideoExts:    []string{"mkv", "mp4"},
		MaxFailures:  3,
	}
	// A prober with no binaries and an encoder that refuses: nothing here may process a
	// file, and if a change ever let one through, the encoder says so rather than
	// quietly encoding in a unit test.
	eng := engine.New(cfg, probe.New("", ""), engine.EncoderFunc(
		func(context.Context, string, string, *probe.VideoProps) error {
			t.Error("the admission suite reached the ENCODER; nothing here may process a file")
			return nil
		}), st, discard())

	h := &scanHarness{
		root: root, st: st, eng: eng,
		scanStarted: make(chan struct{}, 8),
	}
	ctx := context.Background()
	h.ctrl = NewController(ctx, func(ctx context.Context) error {
		h.scanStarted <- struct{}{}
		return nil
	}, discard())
	hub := NewHub(st, h.ctrl, discard())
	h.ctrl.SetOnChange(hub.Trigger)
	// Room for more than one request's worth, so a case about the path COUNT is never
	// answered by the queue's capacity instead.
	h.subs = eng.NewSubmissions(1, engine.DefaultSubmissionQueue)
	h.srv = New(ctx, cfg, secret.NewValue(token), secret.Value{}, st, h.ctrl, hub, nil, nil, discard())
	h.srv.SetSubmissions(h.subs)
	h.ts = httptest.NewServer(h.srv)
	t.Cleanup(h.ts.Close)
	return h
}

// file writes a real file under the library root and returns its path.
func (h *scanHarness) file(t *testing.T, rel string) string {
	t.Helper()
	p := filepath.Join(h.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// postScan sends a request body to the endpoint with the given headers and returns the
// status and raw body.
func (h *scanHarness) postScan(t *testing.T, body string, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.ts.URL+"/api/scan", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/scan: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	return resp.StatusCode, string(raw)
}

// submitAs is the ordinary call: a bearer token and a JSON path list.
func (h *scanHarness) submitAs(t *testing.T, token string, paths ...string) (int, scanResponse) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"paths": paths})
	if err != nil {
		t.Fatal(err)
	}
	code, raw := h.postScan(t, string(body), map[string]string{"Authorization": "Bearer " + token})
	var out scanResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("the response was not the documented shape (%d): %q", code, raw)
	}
	return code, out
}

// assertNothingWasTaken is what makes a refusal a refusal: nothing queued, nothing
// processed, and no ledger row at all. A 4xx that had already enqueued the path would be a
// refusal in the status line only, which is the failure mode these criteria name.
func (h *scanHarness) assertNothingWasTaken(t *testing.T) {
	t.Helper()
	if n := len(h.subs.Results()); n != 0 {
		t.Errorf("%d submissions were processed by a request that was refused", n)
	}
	if got := h.subs.Pending(); got != 0 {
		t.Errorf("%d path(s) were enqueued by a request that was refused", got)
	}
	rows, err := h.st.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a refused request wrote %d ledger row(s): %+v", len(rows), rows)
	}
	select {
	case <-h.scanStarted:
		t.Error("a refused request started a whole-library scan")
	case <-time.After(100 * time.Millisecond):
	}
}

// ruleFor returns the rule token the report carries for a submitted path.
func ruleFor(t *testing.T, resp scanResponse, path string) string {
	t.Helper()
	for _, r := range resp.Results {
		if r.Path == path {
			if r.Accepted {
				return ""
			}
			if r.Detail == "" {
				t.Errorf("%s was refused under %q with no detail; a rule token alone does not tell an "+
					"operator what was seen", path, r.Rule)
			}
			return r.Rule
		}
	}
	t.Fatalf("the per-path report says nothing at all about %s: %+v", path, resp.Results)
	return ""
}

// ---- AC1: the token gate ------------------------------------------------------

// With NO control token configured the endpoint is disabled outright, exactly as
// rescan/pause/resume are. That is the shipped posture, and it is the one a proxied
// deployment runs on: the controls are off until an operator opts in.
func TestScanEndpoint_IsDisabledWithoutAToken(t *testing.T) {
	h := newScanHarness(t, "") // no control token configured
	film := h.file(t, "film.mkv")

	for _, auth := range []map[string]string{
		nil,
		{"Authorization": "Bearer anything-at-all"},
	} {
		body, _ := json.Marshal(map[string]any{"paths": []string{film}})
		code, raw := h.postScan(t, string(body), auth)
		if code != http.StatusForbidden {
			t.Fatalf("POST /api/scan with no token configured answered %d, want 403 (body %q)", code, raw)
		}
	}
	h.assertNothingWasTaken(t)
}

// A proxy identity header is NEVER authorization here. A forward-auth proxy in front of
// holdfast adds these, and a surface that read one would be a control surface a forgeable
// header can start an encode on. Only a matching bearer token is authorization.
func TestScanEndpoint_ProxyIdentityHeadersAreNotAuthorization(t *testing.T) {
	for _, set := range proxyIdentityHeaderSets() {
		t.Run(set.name, func(t *testing.T) {
			h := newScanHarness(t, "a-real-control-token")
			film := h.file(t, "film.mkv")
			body, _ := json.Marshal(map[string]any{"paths": []string{film}})

			headers := map[string]string{}
			for k, v := range set.headers {
				headers[k] = v
			}
			code, raw := h.postScan(t, string(body), headers)
			if code != http.StatusUnauthorized {
				t.Fatalf("POST /api/scan with %s answered %d, want 401 (body %q)", set.name, code, raw)
			}
			h.assertNothingWasTaken(t)

			// And a WRONG bearer token stays wrong however the request is dressed.
			headers["Authorization"] = "Bearer not-the-token"
			code, raw = h.postScan(t, string(body), headers)
			if code != http.StatusUnauthorized {
				t.Fatalf("POST /api/scan with a wrong bearer and %s answered %d, want 401 (body %q)",
					set.name, code, raw)
			}
			h.assertNothingWasTaken(t)

			// Anti-vacuity: the RIGHT token is accepted, so the 401s above are about
			// authorization and not about a body or a path this harness refuses anyway.
			code, resp := h.submitAs(t, "a-real-control-token", film)
			if code != http.StatusAccepted || resp.Accepted != 1 {
				t.Fatalf("the correct token answered %d with %d accepted; the 401s prove nothing if "+
					"nothing is ever accepted", code, resp.Accepted)
			}
		})
	}
}

// ---- AC2 / AC2a: the path is resolved before it is judged ---------------------

// A path that does not lie at or beneath a configured library root is refused, NAMING the
// rule it broke - and the check is answered against the RESOLVED path, so neither a `..`
// climb nor a symbolic link out of the roots gets past it.
//
// This is the criterion that carries the blast radius. Everything downstream of the
// pipeline's door is licensed to rewrite the file it was handed, and a lexical root check
// would answer "/library/../etc/passwd" as being under /library.
func TestScanEndpoint_RejectsAPathOutsideTheRoots(t *testing.T) {
	const token = "tok"

	t.Run("plainly elsewhere", func(t *testing.T) {
		h := newScanHarness(t, token)
		outside := filepath.Join(t.TempDir(), "elsewhere.mkv")
		if err := os.WriteFile(outside, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, resp := h.submitAs(t, token, outside)
		if code != http.StatusBadRequest {
			t.Fatalf("a path outside every root answered %d, want a 400-class refusal", code)
		}
		if rule := ruleFor(t, resp, outside); rule != engine.RuleOutsideRoots {
			t.Errorf("the refusal names %q; want %q - a refusal must name the rule it broke",
				rule, engine.RuleOutsideRoots)
		}
		h.assertNothingWasTaken(t)
	})

	// AC2a, first half: `..` segments that climb out of a configured root. The path is
	// LEXICALLY under the root and really is not.
	t.Run("traversal", func(t *testing.T) {
		h := newScanHarness(t, token)
		// A real file just OUTSIDE the root, in the root's own parent, so the climb
		// reaches something rather than failing for want of a file.
		target := filepath.Join(filepath.Dir(h.root), "elsewhere.mkv")
		if err := os.WriteFile(target, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Built by concatenation, NOT filepath.Join: Join cleans, and a fixture whose ..
		// had already been resolved away would exercise nothing. This is the string a
		// caller actually sends.
		climb := h.root + "/../elsewhere.mkv"

		// And the case a LEXICAL root check cannot answer at all: the same climb taken
		// through a symbolic link, so filepath.Clean puts the result back INSIDE the
		// root while the real path is outside it. This one is refused only because the
		// path was resolved before the root check was asked.
		away := t.TempDir()
		if err := os.WriteFile(filepath.Join(away, "elsewhere.mkv"), []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
		hop := filepath.Join(h.root, "hop")
		if err := os.Symlink(away, hop); err != nil {
			t.Skipf("this filesystem does not support symbolic links: %v", err)
		}
		throughLink := hop + "/../elsewhere.mkv"
		if !strings.HasPrefix(filepath.Clean(throughLink), h.root+string(filepath.Separator)) {
			t.Fatalf("filepath.Clean(%q) is %q, which is NOT under the root - this fixture does not "+
				"exercise the gap between a lexical check and a resolved one",
				throughLink, filepath.Clean(throughLink))
		}

		for _, p := range []string{climb, throughLink} {
			if !strings.Contains(p, "..") {
				t.Fatalf("the fixture path %q carries no .. segment, so it exercises nothing", p)
			}
			code, resp := h.submitAs(t, token, p)
			if code != http.StatusBadRequest {
				t.Fatalf("%q answered %d, want a 400-class refusal: %+v", p, code, resp.Results)
			}
			if rule := ruleFor(t, resp, p); rule != engine.RuleOutsideRoots {
				t.Errorf("%q was refused under %q; want %q", p, rule, engine.RuleOutsideRoots)
			}
			h.assertNothingWasTaken(t)
		}
	})

	// AC2a, second half: a path that is lexically beneath a root and is a symbolic link
	// whose target resolves outside every root. config.Root.Contains is lexical and
	// cannot see this; only resolving first can.
	t.Run("symlink_out", func(t *testing.T) {
		h := newScanHarness(t, token)
		outside := filepath.Join(t.TempDir(), "real.mkv")
		if err := os.WriteFile(outside, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(h.root, "looks-local.mkv")
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("this filesystem does not support symbolic links: %v", err)
		}
		// The fixture really is the shape the criterion describes: lexically under the
		// root, and pointing out of it.
		if !strings.HasPrefix(link, h.root) {
			t.Fatalf("the link %q is not lexically under the root %q", link, h.root)
		}

		code, resp := h.submitAs(t, token, link)
		if code != http.StatusBadRequest {
			t.Fatalf("a symbolic link out of the roots answered %d, want a 400-class refusal", code)
		}
		if rule := ruleFor(t, resp, link); rule != engine.RuleOutsideRoots {
			t.Errorf("the refusal names %q; want %q", rule, engine.RuleOutsideRoots)
		}
		h.assertNothingWasTaken(t)

		// A link that stays INSIDE the roots is accepted, and is accepted AS ITS TARGET.
		// Without this the refusal above would be indistinguishable from "links are
		// refused", which is not what the criterion says.
		actual := h.file(t, "real-inside.mkv")
		inside := filepath.Join(h.root, "points-inside.mkv")
		if err := os.Symlink(actual, inside); err != nil {
			t.Fatal(err)
		}
		code, resp = h.submitAs(t, token, inside)
		if code != http.StatusAccepted {
			t.Fatalf("a symbolic link INSIDE the roots answered %d, want 202: %+v", code, resp.Results)
		}
		if resp.Results[0].Resolved != actual {
			t.Errorf("the accepted path resolved to %q, want %q - the pipeline is handed the path the "+
				"root check was answered against", resp.Results[0].Resolved, actual)
		}
	})
}

// A path naming something that is not a regular file is refused with the reason named, and
// enqueues nothing. A directory under a source name is the case that matters: it carries a
// video extension and a scan would not enumerate it either.
func TestScanEndpoint_RejectsAPathThatIsNotARegularFile(t *testing.T) {
	const token = "tok"
	h := newScanHarness(t, token)

	dir := filepath.Join(h.root, "season.mkv")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(h.root, "never-existed.mkv")
	dangling := filepath.Join(h.root, "dangling.mkv")
	if err := os.Symlink(filepath.Join(h.root, "nothing-here.mkv"), dangling); err != nil {
		t.Skipf("this filesystem does not support symbolic links: %v", err)
	}

	code, resp := h.submitAs(t, token, dir, missing, dangling)
	if code != http.StatusBadRequest {
		t.Fatalf("answered %d, want a 400-class refusal: %+v", code, resp.Results)
	}
	for _, p := range []string{dir, missing, dangling} {
		if rule := ruleFor(t, resp, p); rule != engine.RuleNotARegularFile {
			t.Errorf("%s was refused under %q; want %q", p, rule, engine.RuleNotARegularFile)
		}
	}
	h.assertNothingWasTaken(t)
}

// A path whose directory cannot be read is refused with a STATED reason, not enqueued and
// not answered 500. The distinction from "does not exist" is the point: an unreadable
// directory is a deployment fault an operator can fix, and reporting it as an absence would
// send them looking for a missing file.
func TestScanEndpoint_RejectsAPathItCannotResolve(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which may read a directory whatever its mode")
	}
	const token = "tok"
	h := newScanHarness(t, token)

	closed := filepath.Join(h.root, "closed")
	if err := os.MkdirAll(closed, 0o755); err != nil {
		t.Fatal(err)
	}
	film := filepath.Join(closed, "film.mkv")
	if err := os.WriteFile(film, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(closed, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(closed, 0o755) })
	// The fixture really is unreadable, or the case grades nothing.
	if _, err := os.ReadDir(closed); err == nil {
		t.Skip("this filesystem does not enforce directory permissions for this user")
	}

	code, resp := h.submitAs(t, token, film)
	if code == http.StatusInternalServerError {
		t.Fatalf("an unreadable directory answered 500; it is a refusal about the path, not a server fault")
	}
	if code != http.StatusBadRequest {
		t.Fatalf("answered %d, want a 400-class refusal: %+v", code, resp.Results)
	}
	if rule := ruleFor(t, resp, film); rule != engine.RuleUnresolvable {
		t.Errorf("the refusal names %q; want %q", rule, engine.RuleUnresolvable)
	}
	h.assertNothingWasTaken(t)
}

// ---- AC2b: not a file a scan would enumerate ---------------------------------

// A path that resolves beneath a root but is not a file a library scan would enumerate as
// a source is refused with a stated reason. These are the rules the enumeration applies,
// asked of a path: the extension, this tool's own working files, and the retention area.
func TestScanEndpoint_RejectsAPathAScanWouldNotEnumerate(t *testing.T) {
	const token = "tok"
	h := newScanHarness(t, token)

	cases := []struct {
		rel  string
		rule string
	}{
		{"notes.txt", engine.RuleNotAVideoFile},
		{"noextension", engine.RuleNotAVideoFile},
		{"film.__transcoding__.mkv", engine.RuleWorkingFile},
		{"film.__holdfast-replacement__.mkv", engine.RuleWorkingFile},
		{"film.abc123.__undo__.mkv", engine.RuleWorkingFile},
		{filepath.Join(engine.UndoDirName, "kept.deadbeef.__undo__.mkv"), engine.RuleRetentionArea},
		{filepath.Join(engine.UndoDirName, "plain.mkv"), engine.RuleRetentionArea},
	}
	paths := make([]string, 0, len(cases))
	for _, c := range cases {
		paths = append(paths, h.file(t, c.rel))
	}

	code, resp := h.submitAs(t, token, paths...)
	if code != http.StatusBadRequest {
		t.Fatalf("answered %d, want a 400-class refusal: %+v", code, resp.Results)
	}
	for i, c := range cases {
		if rule := ruleFor(t, resp, paths[i]); rule != c.rule {
			t.Errorf("%s was refused under %q; want %q", c.rel, rule, c.rule)
		}
	}
	h.assertNothingWasTaken(t)

	// Anti-vacuity: an ordinary source in the same directory IS accepted, so the
	// refusals above are about the rules and not about a harness that refuses everything.
	film := h.file(t, "film.mkv")
	if code, resp := h.submitAs(t, token, film); code != http.StatusAccepted || resp.Accepted != 1 {
		t.Fatalf("an ordinary source answered %d with %d accepted: %+v", code, resp.Accepted, resp.Results)
	}
}

// ---- AC3 / AC3a: enqueue, report, refuse -------------------------------------

// The handler returns BEFORE the accepted file has been processed. An HTTP handler held
// open across an encode would hold a connection for hours and would make the endpoint
// unusable from an *arr's webhook, which has its own timeout.
//
// It is established by the clock AND by the state: the response arrives while the queue
// still holds the path unprocessed.
func TestScanEndpoint_ReturnsBeforeProcessing(t *testing.T) {
	const token = "tok"
	h := newScanHarness(t, token)
	film := h.file(t, "film.mkv")

	start := time.Now()
	code, resp := h.submitAs(t, token, film)
	elapsed := time.Since(start)

	if code != http.StatusAccepted {
		t.Fatalf("answered %d, want 202: %+v", code, resp.Results)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the handler took %s to answer; it must not wait on the pipeline", elapsed)
	}
	// The path is in the QUEUE, not processed: the pool is not running in this harness,
	// so an answered 202 with the path still waiting is exactly the contract.
	if n := len(h.subs.Results()); n != 0 {
		t.Errorf("%d submissions had already been processed when the response arrived; the handler "+
			"answered after the pipeline rather than before it", n)
	}
	if got := h.subs.Pending(); got != 1 {
		t.Errorf("%d path(s) are waiting in the queue, want 1 - a 202 that enqueued nothing is a "+
			"silent no-op", got)
	}
}

// Every submitted path gets a line in the report, saying whether it was accepted and, where
// it was not, which rule rejected it. A caller integrating an *arr webhook needs to know
// which of its paths were taken without deriving it by subtraction - and the same request
// naming one path twice accepts it at most once.
func TestScanEndpoint_ReportsEveryPathAcceptedOrRejectedWithAReason(t *testing.T) {
	const token = "tok"
	h := newScanHarness(t, token)

	good := h.file(t, "good.mkv")
	alsoGood := h.file(t, "sub/also-good.mp4")
	notVideo := h.file(t, "notes.txt")
	outside := filepath.Join(t.TempDir(), "elsewhere.mkv")
	if err := os.WriteFile(outside, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}

	// good is named THREE times, in two spellings: the bare path, and the same file
	// reached through a symbolic link. Both resolve to one file, so one acceptance.
	link := filepath.Join(h.root, "another-name.mkv")
	if err := os.Symlink(good, link); err != nil {
		t.Skipf("this filesystem does not support symbolic links: %v", err)
	}
	submitted := []string{good, notVideo, alsoGood, good, outside, link}

	code, resp := h.submitAs(t, token, submitted...)
	if code != http.StatusAccepted {
		t.Fatalf("answered %d, want 202 (some paths were acceptable): %+v", code, resp.Results)
	}
	if len(resp.Results) != len(submitted) {
		t.Fatalf("the report carries %d lines for %d submitted paths; every path gets one",
			len(resp.Results), len(submitted))
	}
	for i, p := range submitted {
		if resp.Results[i].Path != p {
			t.Fatalf("report line %d is about %q, not the %q that was submitted at that position - the "+
				"report must be readable against the request", i, resp.Results[i].Path, p)
		}
	}
	if resp.Accepted != 2 {
		t.Errorf("%d paths were accepted, want 2 (good.mkv once, also-good.mp4 once): %+v",
			resp.Accepted, resp.Results)
	}
	if resp.Rejected != 4 {
		t.Errorf("%d paths were rejected, want 4: %+v", resp.Rejected, resp.Results)
	}
	if rule := ruleFor(t, resp, notVideo); rule != engine.RuleNotAVideoFile {
		t.Errorf("notes.txt was refused under %q; want %q", rule, engine.RuleNotAVideoFile)
	}
	if rule := ruleFor(t, resp, outside); rule != engine.RuleOutsideRoots {
		t.Errorf("a path outside the roots was refused under %q; want %q", rule, engine.RuleOutsideRoots)
	}
	// The duplicate and the second spelling are each reported, each as a duplicate,
	// rather than silently dropped.
	dupes := 0
	for _, r := range resp.Results {
		if r.Rule == ruleDuplicate {
			dupes++
		}
	}
	if dupes != 2 {
		t.Errorf("%d lines report a duplicate, want 2 (the repeated path and the second spelling of "+
			"it): %+v", dupes, resp.Results)
	}
	// And exactly two paths reached the queue, which is what "accepted at most once" means
	// in substance rather than in the report.
	if got := h.subs.Pending(); got != 2 {
		t.Errorf("%d paths reached the queue, want 2", got)
	}
}

// A malformed body is refused with 400, processes nothing and writes no ledger row. Each
// shape is a way a real caller gets it wrong, and none of them may be read as an empty
// but valid request.
func TestScanEndpoint_RefusesAMalformedBody(t *testing.T) {
	const token = "tok"
	for _, tc := range []struct{ name, body string }{
		{"not JSON at all", `{"paths": [`},
		{"empty body", ``},
		{"a JSON array rather than an object", `["/library/film.mkv"]`},
		{"a JSON string", `"/library/film.mkv"`},
		{"no paths key", `{"files": ["/library/film.mkv"]}`},
		{"paths is null", `{"paths": null}`},
		{"paths is not an array", `{"paths": "/library/film.mkv"}`},
		{"paths is empty", `{"paths": []}`},
		{"an entry that is a number", `{"paths": ["/library/film.mkv", 7]}`},
		{"an entry that is an object", `{"paths": [{"path": "/library/film.mkv"}]}`},
		{"an entry that is null", `{"paths": [null]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newScanHarness(t, token)
			h.file(t, "film.mkv")
			code, raw := h.postScan(t, tc.body, map[string]string{"Authorization": "Bearer " + token})
			if code != http.StatusBadRequest {
				t.Fatalf("answered %d, want 400 (body %q)", code, raw)
			}
			if strings.TrimSpace(raw) == "" {
				t.Error("the refusal said nothing; a caller has to be told what was wrong with the body")
			}
			h.assertNothingWasTaken(t)
		})
	}
}

// An oversized request is refused WHOLE, with a 4xx naming the maximum, and enqueues
// nothing. A partially-accepted oversized request is the failure this criterion names: a
// caller that got half its list taken and a 4xx back would have no way to tell which half.
func TestScanEndpoint_RefusesAnOversizedRequest(t *testing.T) {
	const token = "tok"

	t.Run("more paths than the per-request maximum", func(t *testing.T) {
		h := newScanHarness(t, token)
		// Every one of them is a real, acceptable file, so the refusal is about the count
		// and about nothing else - and so a partial acceptance would really have taken
		// work rather than refusing each path on its own merits.
		paths := make([]string, 0, MaxScanPaths+1)
		for i := 0; i <= MaxScanPaths; i++ {
			paths = append(paths, h.file(t, fmt.Sprintf("film%03d.mkv", i)))
		}
		body, _ := json.Marshal(map[string]any{"paths": paths})
		code, raw := h.postScan(t, string(body), map[string]string{"Authorization": "Bearer " + token})
		if code < 400 || code >= 500 {
			t.Fatalf("answered %d, want a 4xx refusal (body %q)", code, raw)
		}
		if !strings.Contains(raw, fmt.Sprint(MaxScanPaths)) {
			t.Errorf("the refusal does not name the maximum (%d): %q", MaxScanPaths, raw)
		}
		h.assertNothingWasTaken(t)

		// Anti-vacuity: exactly the maximum is accepted, so the refusal is at the bound
		// and not at some smaller number.
		code, resp := h.submitAs(t, token, paths[:MaxScanPaths]...)
		if code != http.StatusAccepted || resp.Accepted != MaxScanPaths {
			t.Fatalf("exactly %d paths answered %d with %d accepted", MaxScanPaths, code, resp.Accepted)
		}
	})

	t.Run("a body larger than the maximum", func(t *testing.T) {
		h := newScanHarness(t, token)
		h.file(t, "film.mkv")
		// One path, padded well past the body bound: the COUNT is fine, so only the size
		// rule can refuse this.
		huge := "/" + strings.Repeat("a", MaxScanBodyBytes+1024) + "/film.mkv"
		body, _ := json.Marshal(map[string]any{"paths": []string{huge}})
		if len(body) <= MaxScanBodyBytes {
			t.Fatalf("the fixture body is %d bytes, not over the %d-byte bound", len(body), MaxScanBodyBytes)
		}
		code, raw := h.postScan(t, string(body), map[string]string{"Authorization": "Bearer " + token})
		if code < 400 || code >= 500 {
			t.Fatalf("answered %d, want a 4xx refusal (body %q)", code, raw)
		}
		if !strings.Contains(raw, fmt.Sprint(MaxScanBodyBytes)) {
			t.Errorf("the refusal does not name the maximum body size (%d): %q", MaxScanBodyBytes, raw)
		}
		h.assertNothingWasTaken(t)
	})
}

// A queue that cannot take the work says so, per path, rather than blocking the handler or
// dropping paths silently. Which paths were NOT taken is the only thing a caller needs in
// order to retry correctly.
func TestScanEndpoint_SaysSoWhenItCannotTakeTheWork(t *testing.T) {
	const token = "tok"
	h := newScanHarness(t, token)
	// A queue with room for exactly one, so the second acceptable path cannot be taken.
	h.subs = h.eng.NewSubmissions(1, 1)
	h.srv.SetSubmissions(h.subs)

	first := h.file(t, "first.mkv")
	second := h.file(t, "second.mkv")

	start := time.Now()
	code, resp := h.submitAs(t, token, first, second)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the handler took %s; a full queue must not block it", elapsed)
	}
	if code < 400 {
		t.Fatalf("answered %d; a request whose work could not be taken must not read as success", code)
	}
	if rule := ruleFor(t, resp, second); rule != ruleQueueFull {
		t.Errorf("the path that could not be taken is reported under %q; want %q", rule, ruleQueueFull)
	}
	if !resp.Results[0].Accepted {
		t.Errorf("the path that WAS taken is not reported as accepted: %+v", resp.Results[0])
	}
	if n := len(h.subs.Results()); n != 0 {
		t.Errorf("%d submissions were processed; the pool is not running in this harness", n)
	}
	if got := h.subs.Pending(); got != 1 {
		t.Errorf("%d paths reached the queue, want exactly the 1 it had room for", got)
	}
}

// ---- AC5a: no route beyond scan ----------------------------------------------

// The HTTP surface gains POST /api/scan and NOTHING else. requeue, restore and resolve are
// LOCAL commands by ratified operator decision, because each re-offers, overwrites or
// disposes of a media file on an operator's say-so - and POST /api/scan must not become a
// network-reachable spelling of any of them.
//
// The route set is read from the ROUTER, so a route added tomorrow is covered on the day
// it lands rather than when somebody remembers to add it to a list.
func TestScanEndpoint_AddsNoRouteBeyondScan(t *testing.T) {
	h := newScanHarness(t, "tok")
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

	// The whole API surface, as it stands with this work in. A route added without being
	// named here reds, which is the point: the decision this pins is about what the
	// surface IS, not about which words a route happens to contain.
	//
	// The four S0094 routes are named here because that item's criteria admit exactly
	// them, and each is on the right side of the line this test draws. `GET /api/search`
	// is a READ of the ledger, token-gated only because it serves per-file rows the capped
	// reads never have. The three `/api/exclusions` routes only ever WITHHOLD a path from
	// the pipeline or stop withholding one: the direction is out, never in, so none of them
	// is a network-reachable spelling of requeue, restore or resolve - S0094 criterion 13
	// is the assertion that holds that shut, and criterion 12 is why the removal exists.
	// The trailing slashes are chi's own rendering of a subrouter mounted at /exclusions.
	want := map[string]bool{
		"GET /api/summary": true, "GET /api/queue": true, "GET /api/history": true,
		"GET /api/events":  true,
		"POST /api/rescan": true, "POST /api/scan": true,
		"POST /api/pause": true, "POST /api/resume": true,
		"GET /api/search":         true,
		"GET /api/exclusions/":    true,
		"POST /api/exclusions/":   true,
		"DELETE /api/exclusions/": true,
	}
	for _, r := range served {
		if !strings.HasPrefix(r, "GET /api/") && !strings.HasPrefix(r, "POST /api/") {
			continue // the root page and /metrics are not the API surface
		}
		if !want[r] {
			t.Errorf("the HTTP surface serves %q, which this work did not put there and no criterion "+
				"admits. requeue, restore and resolve stay LOCAL commands by ratified operator decision", r)
		}
	}
	for r := range want {
		found := false
		for _, got := range served {
			if got == r {
				found = true
			}
		}
		if !found {
			t.Errorf("the router does not serve %q; the enumeration is not seeing the real surface", r)
		}
	}

	// And the shapes somebody would reach for are 404 or 405, asked with a VALID token so
	// the answer cannot be an authorization result in disguise.
	for _, path := range []string{
		"/api/requeue", "/api/restore", "/api/resolve", "/api/reopen",
		"/api/scan/requeue", "/api/jobs/requeue", "/requeue", "/restore",
	} {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			req, err := http.NewRequest(method, h.ts.URL+path, strings.NewReader(`{"paths":["/x.mkv"]}`))
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
				t.Errorf("%s %s answered %d; re-opening a row, restoring an original and resolving a "+
					"parked incident must not be reachable over HTTP at all", method, path, resp.StatusCode)
			}
		}
	}
}

// ---- AC5b: paused, and shutdown ----------------------------------------------

// Paused refuses with 409 and enqueues nothing, consistently with POST /api/rescan
// refusing while paused. Pause is the operator saying "feed no NEW files to the workers",
// and a targeted submission is new files.
func TestScanEndpoint_RefusesWhilePaused(t *testing.T) {
	const token = "tok"
	h := newScanHarness(t, token)
	film := h.file(t, "film.mkv")

	h.ctrl.Pause()
	body, _ := json.Marshal(map[string]any{"paths": []string{film}})
	code, raw := h.postScan(t, string(body), map[string]string{"Authorization": "Bearer " + token})
	if code != http.StatusConflict {
		t.Fatalf("POST /api/scan while paused answered %d, want 409 (body %q)", code, raw)
	}
	h.assertNothingWasTaken(t)

	// The same refusal POST /api/rescan gives, so the two are consistent rather than
	// merely both being refusals.
	rescanCode, _ := post(t, h.ts.URL+"/api/rescan", token)
	if rescanCode != http.StatusConflict {
		t.Fatalf("POST /api/rescan while paused answered %d; this criterion is written against its "+
			"refusal, so they must agree", rescanCode)
	}

	// And resume lets the same submission through, so the 409 is about the pause state
	// and not about the path or the body.
	h.ctrl.Resume()
	if code, resp := h.submitAs(t, token, film); code != http.StatusAccepted || resp.Accepted != 1 {
		t.Fatalf("after resume the same submission answered %d with %d accepted", code, resp.Accepted)
	}
}

// Shutdown joins work already in flight before the store handle is closed, and discards
// submissions that were never started without writing a ledger row for them.
//
// It runs the REAL queue against the REAL pipeline entry point, and holds one file inside
// it by blocking the engine's own Observer, which ProcessFile calls the moment it takes
// the claim. That is honestly in flight: the worker is inside ProcessFile, past the door.
// (The Observer contract requires a non-blocking implementation; this one deliberately
// breaks it, because "work that does not stop when the cancellation lands" is exactly the
// state the criterion is about. A substitute queue would grade the substitute's Wait.)
func TestScanEndpoint_ShutdownJoinsInFlightWorkAndDropsTheRest(t *testing.T) {
	root := t.TempDir()

	var paths []string
	for _, n := range []string{"a.mkv", "b.mkv", "c.mkv"} {
		p := filepath.Join(root, n)
		if err := os.WriteFile(p, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	cfg := config.Config{LibraryRoots: []string{root}, VideoExts: []string{"mkv"}, MaxFailures: 3}
	eng := engine.New(cfg, probe.New("", ""), engine.EncoderFunc(
		func(context.Context, string, string, *probe.VideoProps) error { return nil }), st, discard())

	entered := make(chan string, 4)
	release := make(chan struct{})
	eng.Observer = func(ev engine.Event) {
		if ev.Status != store.Probing {
			return
		}
		select {
		case entered <- ev.Path:
		default:
		}
		<-release // held, and deliberately deaf to the cancellation
	}

	ctx, cancel := context.WithCancel(context.Background())
	ctrl := NewController(ctx, func(context.Context) error { return nil }, discard())
	hub := NewHub(st, ctrl, discard())
	subs := eng.NewSubmissions(1, 8) // one worker, so the other two must wait
	srv := New(ctx, cfg, secret.NewValue("tok"), secret.Value{}, st, ctrl, hub, nil, nil, discard())
	srv.SetSubmissions(subs)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	runDone := make(chan struct{})
	go func() { defer close(runDone); subs.Run(ctx) }()

	body, _ := json.Marshal(map[string]any{"paths": paths})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/scan", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/scan: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/scan answered %d, want 202", resp.StatusCode)
	}

	// Wait for the first submission to be INSIDE the pipeline, past the claim.
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		cancel()
		close(release)
		<-runDone
		t.Fatal("no submission reached the pipeline")
	}

	// Shutdown lands with one file in flight and two still queued.
	cancel()
	waitDone := make(chan struct{})
	go func() { srv.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
		close(release)
		t.Fatal("Wait returned while a submission was still in flight; the caller would then close the " +
			"store handle out from under it")
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	select {
	case <-waitDone:
	case <-time.After(60 * time.Second):
		t.Fatal("Wait never returned after the in-flight work finished")
	}
	<-runDone

	// Only the one that was in flight was processed. Closing the store is safe now, and
	// closing it here is the ordering the criterion is about.
	if got := len(subs.Results()); got != 1 {
		t.Fatalf("%d submissions were processed; only the one already in flight should have been", got)
	}
	processed := subs.Results()[0].Path
	rows, err := st.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	for _, r := range rows {
		if r.Path != processed {
			t.Errorf("a submission that was never started carries a ledger row (%s: %s/%q): nothing "+
				"looked at that file, so there is no decision to record", r.Path, r.Status, r.Outcome.Reason)
		}
	}
	if err := st.Close(); err != nil {
		t.Errorf("closing the store after Wait: %v", err)
	}
}
