package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/store"
)

// The PER-FILE surface: the ledger search, and the withheld paths an operator records,
// reads and removes.
//
// Two claims run through all of it and are graded separately from the happy paths, because
// they are the ones that keep this surface inside the authorization line S0080 drew: every
// action here either READS the ledger or WITHHOLDS a path, and nothing it can do creates,
// modifies or deletes a media file, offers one to the encoder, or writes the configuration
// file.

// perFileRoot is a library root with a couple of media files in it, so a case can prove
// that invoking every action left the filesystem exactly as it found it.
func perFileRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"alpha.mkv", "bravo.mkv"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("media bytes for "+name), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	return root
}

// perFileHarness is newHarness with a real library root configured, which the path rule
// needs: a per-file action names a path, and "inside a configured root" is the whole of
// what makes one acceptable.
type perFileHarness struct {
	*harness
	root string
	// configPath is a configuration file on disk beside the library. Nothing on this
	// surface may write it, and a case proves that by hashing it either side.
	configPath string
}

func newPerFileHarness(t *testing.T, token string) *perFileHarness {
	t.Helper()
	root := perFileRoot(t)
	h := newHarness(t, token)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("library_roots:\n  - "+root+"\nencoder: cpu\ncrf: 22\n"), 0o644); err != nil {
		t.Fatalf("writing the configuration file: %v", err)
	}
	// Rebuild the server with a configuration that has the root, since the routes are
	// built in New.
	ctx := context.Background()
	// An empty read token leaves the reads OPEN, which is what this harness assumed before
	// server_read_token existed: every case here is about the CONTROL token.
	h.srv = New(ctx, config.Config{LibraryRoots: []string{root}}, secret.NewValue(token), secret.Value{},
		h.st, h.ctrl, h.hub, nil, nil, discard())
	return &perFileHarness{harness: h, root: root, configPath: cfgPath}
}

// do issues one request against the surface and returns the status and the body.
func do(t *testing.T, ts *httptest.Server, method, path, token, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, string(raw)
}

func pathBody(p string) string {
	raw, _ := json.Marshal(map[string]string{"path": p})
	return string(raw)
}

// seedTerminalRows writes n done rows under root and returns their paths.
//
// The names are zero-padded, which is load-bearing rather than tidy: the ledger orders by
// transition time and then by path, and every row a test writes lands in the same second,
// so what a capped read ships is the first `cap` paths in order. Padding makes that order
// the numeric one, so a case can name a row it KNOWS the capped view does not hold.
func seedTerminalRows(t *testing.T, st *store.SQLite, root string, n int) []string {
	t.Helper()
	ctx := context.Background()
	var paths []string
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("%s/%04d.mkv", root, i)
		fp := strconv.Itoa(i) + ":" + strconv.Itoa(i)
		mustClaim(t, st, p, fp)
		if err := st.Finish(ctx, p, fp, store.Done, &store.Outcome{Encoder: "cpu"}, 3); err != nil {
			t.Fatalf("Finish %s: %v", p, err)
		}
		paths = append(paths, p)
	}
	return paths
}

// --- criterion 3: the search reaches rows the capped view does not hold ------------

func TestLedgerSearch_ReturnsRowsBeyondTheCap(t *testing.T) {
	h := newPerFileHarness(t, "secret")
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	// More terminal rows than the history view will ever ship, so some of them are beyond
	// what it can reach at all.
	seeded := seedTerminalRows(t, h.st, "/lib/deep", historyLimit+40)

	// WHICH row is beyond the cap is asked of the capped view itself rather than assumed
	// from the order rows were written in: the view orders by transition time and then by
	// path, and a seeding run that crosses a second boundary reverses which end of the
	// list falls off. So the view is read, and a seeded path it did NOT ship is the
	// subject. Without this the search below proves only that a query returns a row.
	var hist struct {
		History []jobDTO `json:"history"`
	}
	getJSON(t, ts.URL+"/api/history", &hist)
	if len(hist.History) != historyLimit {
		t.Fatalf("the capped view shipped %d rows, want the cap of %d", len(hist.History), historyLimit)
	}
	shipped := map[string]bool{}
	for _, j := range hist.History {
		shipped[j.Path] = true
	}
	beyondTheCap := ""
	for _, p := range seeded {
		if !shipped[p] {
			beyondTheCap = p
			break
		}
	}
	if beyondTheCap == "" {
		t.Fatalf("every seeded row is inside the capped history view, so reaching one proves nothing")
	}

	code, body := do(t, ts, http.MethodGet, "/api/search?path="+beyondTheCap, "secret", "")
	if code != http.StatusOK {
		t.Fatalf("the ledger search answered %d: %s", code, body)
	}
	var got searchResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("the search response is not JSON (%v): %s", err, body)
	}
	if len(got.Results) != 1 || got.Results[0].Path != beyondTheCap {
		t.Fatalf("the search for %q returned %d row(s) (%+v), want the one row the capped view cannot reach",
			beyondTheCap, len(got.Results), got.Results)
	}

	// And it REPORTS HOW MANY MATCHED, over the whole ledger rather than over what it
	// shipped: a term matching every seeded row counts every one of them.
	code, body = do(t, ts, http.MethodGet, "/api/search?path=/lib/deep/", "secret", "")
	if code != http.StatusOK {
		t.Fatalf("the ledger search answered %d: %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("the search response is not JSON (%v): %s", err, body)
	}
	if !got.Total.Available || got.Total.Count == nil {
		t.Fatalf("the search reported no match count at all: %+v", got.Total)
	}
	if *got.Total.Count != int64(historyLimit+40) {
		t.Errorf("the search counted %d matching rows, want %d - the count must be over the ledger and not over what it shipped",
			*got.Total.Count, historyLimit+40)
	}
	if got.Total.Covers == "" {
		t.Error("the match count states no set, so a reader cannot tell what it counted")
	}
	if len(got.Results) > searchLimit {
		t.Errorf("the search shipped %d rows, past its own cap of %d", len(got.Results), searchLimit)
	}
}

// --- criterion 6: no token configured, no search and no row ------------------------

func TestLedgerSearch_IsDisabledWithoutAToken(t *testing.T) {
	h := newPerFileHarness(t, "") // no control token configured
	ts := httptest.NewServer(h.srv)
	defer ts.Close()
	seedTerminalRows(t, h.st, "/lib/deep", 3)

	// The same refusal the existing control endpoints give, with no token configured.
	wantCode, wantBody := do(t, ts, http.MethodPost, "/api/rescan", "", "")
	if wantCode != http.StatusForbidden {
		t.Fatalf("the fixture is wrong: rescan with no token answered %d, want 403", wantCode)
	}

	for _, tc := range []struct{ what, method, path, body string }{
		{"the ledger search", http.MethodGet, "/api/search?path=/lib/deep/", ""},
		{"the withheld-path list", http.MethodGet, "/api/exclusions", ""},
	} {
		code, body := do(t, ts, tc.method, tc.path, "", tc.body)
		if code != http.StatusForbidden {
			t.Errorf("%s answered %d with no token configured, want the same 403 the control endpoints give: %s",
				tc.what, code, body)
		}
		if strings.TrimSpace(body) != strings.TrimSpace(wantBody) {
			t.Errorf("%s refuses with %q; the control endpoints refuse with %q. The refusal must be the SAME one, "+
				"or an operator learns two different things about one switch", tc.what, strings.TrimSpace(body), strings.TrimSpace(wantBody))
		}
		// AND SERVES NO ROW. A refusal that leaked a path would be the unauthenticated
		// read this endpoint is gated to avoid.
		if strings.Contains(body, "/lib/deep/") {
			t.Errorf("%s served a media path inside its refusal: %s", tc.what, body)
		}
		if strings.Contains(body, `"results"`) || strings.Contains(body, `"exclusions"`) {
			t.Errorf("%s served a result set inside its refusal: %s", tc.what, body)
		}
	}

	// A correct-looking bearer changes nothing: with no token configured the control
	// surface is OFF, not merely locked.
	code, _ := do(t, ts, http.MethodGet, "/api/search?path=/lib/deep/", "anything", "")
	if code != http.StatusForbidden {
		t.Errorf("the ledger search answered %d to a bearer token on a server with control disabled, want 403", code)
	}
}

// --- criterion 8: a withholding survives a restart ---------------------------------

func TestExclude_PersistsAcrossRestart(t *testing.T) {
	root := perFileRoot(t)
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	target := filepath.Join(root, "alpha.mkv")

	// A first daemon: open the store, stand the server up, record the withholding.
	first, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	srv1 := serverOver(t, first, root, "secret")
	ts1 := httptest.NewServer(srv1)
	code, body := do(t, ts1, http.MethodPost, "/api/exclusions", "secret", pathBody(target))
	if code != http.StatusOK {
		t.Fatalf("recording the withholding answered %d: %s", code, body)
	}
	var rec excludeResponse
	if err := json.Unmarshal([]byte(body), &rec); err != nil {
		t.Fatalf("the response is not JSON (%v): %s", err, body)
	}
	if !rec.Changed || rec.Path != target {
		t.Fatalf("recording the withholding reported %+v, want the path newly withheld", rec)
	}
	ts1.Close()
	if err := first.Close(); err != nil {
		t.Fatalf("closing the first store: %v", err)
	}

	// THE RESTART. A new process would do exactly this: open the same file again.
	second, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopening the store: %v", err)
	}
	defer func() { _ = second.Close() }()
	ts2 := httptest.NewServer(serverOver(t, second, root, "secret"))
	defer ts2.Close()

	code, body = do(t, ts2, http.MethodGet, "/api/exclusions", "secret", "")
	if code != http.StatusOK {
		t.Fatalf("reading the withheld paths after a restart answered %d: %s", code, body)
	}
	var list exclusionsResponse
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("the response is not JSON (%v): %s", err, body)
	}
	if len(list.Exclusions) != 1 || list.Exclusions[0].Path != target {
		t.Fatalf("after a restart the withheld paths read back as %+v, want the one that was recorded", list.Exclusions)
	}
	if list.Exclusions[0].CreatedAt == 0 {
		t.Error("the withholding records no time, so nobody can tell how long a file has been held out")
	}
	// The engine's own read agrees, which is what actually holds the file out.
	held, err := second.PathIsExcluded(context.Background(), target)
	if err != nil || !held {
		t.Errorf("after a restart the engine's own read says the path is not withheld: held=%v err=%v", held, err)
	}
}

// serverOver wires a Server over one store and one library root, with nothing else
// standing up: these cases are about the per-file surface and not about the scanner.
func serverOver(t *testing.T, st *store.SQLite, root, token string) *Server {
	t.Helper()
	ctx := context.Background()
	ctrl := NewController(ctx, func(context.Context) error { return nil }, discard())
	hub := NewHub(st, ctrl, discard())
	return New(ctx, config.Config{LibraryRoots: []string{root}}, secret.NewValue(token), secret.Value{},
		st, ctrl, hub, nil, nil, discard())
}

// --- criterion 12: a withholding is removable and the path is eligible again --------

func TestExclusionCanBeRemovedAndThePathIsEligibleAgain(t *testing.T) {
	h := newPerFileHarness(t, "secret")
	ts := httptest.NewServer(h.srv)
	defer ts.Close()
	target := filepath.Join(h.root, "alpha.mkv")
	ctx := context.Background()

	if code, body := do(t, ts, http.MethodPost, "/api/exclusions", "secret", pathBody(target)); code != http.StatusOK {
		t.Fatalf("recording the withholding answered %d: %s", code, body)
	}
	if held, err := h.st.PathIsExcluded(ctx, target); err != nil || !held {
		t.Fatalf("the fixture is wrong: the path is not withheld (held=%v err=%v), so removing it proves nothing", held, err)
	}

	// The list offers the path back, which is what a per-entry removal is addressed by.
	code, body := do(t, ts, http.MethodGet, "/api/exclusions", "secret", "")
	if code != http.StatusOK {
		t.Fatalf("reading the withheld paths answered %d: %s", code, body)
	}
	var list exclusionsResponse
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("the response is not JSON (%v): %s", err, body)
	}
	if len(list.Exclusions) != 1 || list.Exclusions[0].Path != target {
		t.Fatalf("the withheld paths read back as %+v, want the one that was recorded", list.Exclusions)
	}

	code, body = do(t, ts, http.MethodDelete, "/api/exclusions", "secret", pathBody(list.Exclusions[0].Path))
	if code != http.StatusOK {
		t.Fatalf("removing the withholding answered %d: %s", code, body)
	}
	var rec excludeResponse
	if err := json.Unmarshal([]byte(body), &rec); err != nil {
		t.Fatalf("the response is not JSON (%v): %s", err, body)
	}
	if !rec.Changed {
		t.Errorf("removing a withholding that was in force reported no change: %+v", rec)
	}

	// ELIGIBLE AGAIN: the engine's own read is what decides that, and it is the read the
	// next scan makes.
	if held, err := h.st.PathIsExcluded(ctx, target); err != nil || held {
		t.Errorf("the path is still withheld after the record was removed: held=%v err=%v", held, err)
	}
	code, body = do(t, ts, http.MethodGet, "/api/exclusions", "secret", "")
	if code != http.StatusOK {
		t.Fatalf("reading the withheld paths answered %d: %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("the response is not JSON (%v): %s", err, body)
	}
	if len(list.Exclusions) != 0 {
		t.Errorf("the withheld paths still hold %+v after the only record was removed", list.Exclusions)
	}

	// Removing a withholding that is not there changes nothing and says so, rather than
	// reporting a success against an empty set.
	code, body = do(t, ts, http.MethodDelete, "/api/exclusions", "secret", pathBody(target))
	if code != http.StatusOK {
		t.Fatalf("removing a withholding that was not there answered %d: %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &rec); err != nil {
		t.Fatalf("the response is not JSON (%v): %s", err, body)
	}
	if rec.Changed {
		t.Error("removing a withholding that was not there reported a change")
	}
}

// --- criterion 14: token gating on the mutating per-file actions --------------------

func TestExcludeEndpoint_IsDisabledWithoutAToken(t *testing.T) {
	target := "alpha.mkv"

	t.Run("no token configured: refused as disabled, and nothing is recorded", func(t *testing.T) {
		h := newPerFileHarness(t, "")
		ts := httptest.NewServer(h.srv)
		defer ts.Close()
		p := filepath.Join(h.root, target)

		for _, method := range []string{http.MethodPost, http.MethodDelete} {
			code, body := do(t, ts, method, "/api/exclusions", "", pathBody(p))
			if code != http.StatusForbidden {
				t.Errorf("%s /api/exclusions answered %d with no token configured, want 403: %s", method, code, body)
			}
			if !strings.Contains(body, "control disabled") {
				t.Errorf("%s /api/exclusions refuses with %q, which does not say control is disabled", method, strings.TrimSpace(body))
			}
		}
		// A bearer changes nothing: with no token configured the surface is OFF.
		if code, _ := do(t, ts, http.MethodPost, "/api/exclusions", "anything", pathBody(p)); code != http.StatusForbidden {
			t.Errorf("a bearer token reached the exclude endpoint on a server with control disabled: %d", code)
		}
		assertNoWithholding(t, h.st, p)
	})

	t.Run("token configured, wrong or absent bearer: unauthorized, and nothing is recorded", func(t *testing.T) {
		h := newPerFileHarness(t, "secret")
		ts := httptest.NewServer(h.srv)
		defer ts.Close()
		p := filepath.Join(h.root, target)

		for _, bearer := range []string{"", "wrong", "secre", "secrett"} {
			for _, method := range []string{http.MethodPost, http.MethodDelete} {
				code, body := do(t, ts, method, "/api/exclusions", bearer, pathBody(p))
				if code != http.StatusUnauthorized {
					t.Errorf("%s /api/exclusions with bearer %q answered %d, want 401: %s", method, bearer, code, body)
				}
				if strings.Contains(body, "secret") {
					t.Errorf("the refusal names what would have been accepted: %s", body)
				}
			}
		}
		assertNoWithholding(t, h.st, p)

		// And the right one works, so the gate is a gate and not a wall.
		if code, body := do(t, ts, http.MethodPost, "/api/exclusions", "secret", pathBody(p)); code != http.StatusOK {
			t.Fatalf("the correct bearer was refused (%d): %s", code, body)
		}
		if held, err := h.st.PathIsExcluded(context.Background(), p); err != nil || !held {
			t.Errorf("the correct bearer recorded nothing: held=%v err=%v", held, err)
		}
	})
}

func assertNoWithholding(t *testing.T, st *store.SQLite, path string) {
	t.Helper()
	held, err := st.PathIsExcluded(context.Background(), path)
	if err != nil {
		t.Fatalf("PathIsExcluded: %v", err)
	}
	if held {
		t.Errorf("a refused request recorded a withholding for %q", path)
	}
	list, err := st.ExcludedPaths(context.Background())
	if err != nil {
		t.Fatalf("ExcludedPaths: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("a refused request left %d withholding(s) behind: %+v", len(list), list)
	}
}

// --- criterion 15: a path this build cannot accept is refused with a reason ---------

func TestExclude_RefusesAPathItCannotAccept(t *testing.T) {
	h := newPerFileHarness(t, "secret")
	ts := httptest.NewServer(h.srv)
	defer ts.Close()
	ctx := context.Background()

	// One withholding IS in force throughout, so "left the exclusions in force unchanged"
	// is a claim with something to be unchanged.
	keep := filepath.Join(h.root, "bravo.mkv")
	if code, body := do(t, ts, http.MethodPost, "/api/exclusions", "secret", pathBody(keep)); code != http.StatusOK {
		t.Fatalf("seeding a withholding answered %d: %s", code, body)
	}
	before, err := h.st.ExcludedPaths(ctx)
	if err != nil {
		t.Fatalf("ExcludedPaths: %v", err)
	}

	for _, tc := range []struct{ what, body string }{
		{"an empty path", pathBody("")},
		{"a path of blanks", pathBody("   ")},
		{"a relative path", pathBody("alpha.mkv")},
		{"a path outside every configured library root", pathBody("/etc/passwd")},
		{"a path that merely shares a prefix with a root", pathBody(h.root + "-old/alpha.mkv")},
		{"a path escaping the root with ..", pathBody(h.root + "/../elsewhere.mkv")},
		{"a path carrying a newline", pathBody(filepath.Join(h.root, "a\nb.mkv"))},
		{"a path carrying a tab", pathBody(filepath.Join(h.root, "a\tb.mkv"))},
		{"a path longer than any path this system can hold", pathBody("/" + strings.Repeat("a", maxPathBytes+1))},
		{"a body that is not JSON at all", "this is not JSON"},
		{"a body with no path field", `{"paths":["/x"]}`},
	} {
		for _, method := range []string{http.MethodPost, http.MethodDelete} {
			code, body := do(t, ts, method, "/api/exclusions", "secret", tc.body)
			if code != http.StatusBadRequest {
				t.Errorf("%s: %s /api/exclusions answered %d, want 400: %s", tc.what, method, code, body)
			}
			if strings.TrimSpace(body) == "" {
				t.Errorf("%s: the refusal states no reason at all", tc.what)
			}
		}
	}

	after, err := h.st.ExcludedPaths(ctx)
	if err != nil {
		t.Fatalf("ExcludedPaths: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("the withheld paths went from %d to %d across a run of refusals: %+v", len(before), len(after), after)
	}
	for i := range before {
		if before[i].Path != after[i].Path || before[i].CreatedAt != after[i].CreatedAt {
			t.Errorf("a refusal moved a withholding in force: %+v -> %+v", before[i], after[i])
		}
	}

	// The ledger search refuses an empty term for the same reason, naming it.
	if code, body := do(t, ts, http.MethodGet, "/api/search?path=", "secret", ""); code != http.StatusBadRequest {
		t.Errorf("the ledger search answered %d to an empty term, want 400: %s", code, body)
	}
}

// --- criterion 10: no action here writes the configuration file --------------------

// everyPerFileAction is the WHOLE surface this spec adds, in one place, so a case that
// says "every action" is actually driven against every one of them.
func everyPerFileAction(root string) []struct{ what, method, path, body string } {
	target := filepath.Join(root, "alpha.mkv")
	return []struct{ what, method, path, body string }{
		{"the ledger search", http.MethodGet, "/api/search?path=alpha", ""},
		{"the withheld-path list", http.MethodGet, "/api/exclusions", ""},
		{"recording a withholding", http.MethodPost, "/api/exclusions", pathBody(target)},
		{"recording the same withholding again", http.MethodPost, "/api/exclusions", pathBody(target)},
		{"removing a withholding", http.MethodDelete, "/api/exclusions", pathBody(target)},
		{"removing one that is not there", http.MethodDelete, "/api/exclusions", pathBody(target)},
		{"a refused path", http.MethodPost, "/api/exclusions", pathBody("/etc/passwd")},
	}
}

func TestPerFileActions_NeverWriteTheConfigFile(t *testing.T) {
	h := newPerFileHarness(t, "secret")
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	before := fileDigest(t, h.configPath)
	beforeInfo, err := os.Stat(h.configPath)
	if err != nil {
		t.Fatalf("stat the configuration file: %v", err)
	}

	for _, a := range everyPerFileAction(h.root) {
		code, body := do(t, ts, a.method, a.path, "secret", a.body)
		if code >= 500 {
			t.Fatalf("%s answered %d: %s", a.what, code, body)
		}
		if got := fileDigest(t, h.configPath); got != before {
			t.Fatalf("%s changed the configuration file. Nothing on this surface may write it, ever", a.what)
		}
	}

	afterInfo, err := os.Stat(h.configPath)
	if err != nil {
		t.Fatalf("stat the configuration file: %v", err)
	}
	if fileDigest(t, h.configPath) != before {
		t.Error("the configuration file is not byte-for-byte what it was before the per-file actions ran")
	}
	if !afterInfo.ModTime().Equal(beforeInfo.ModTime()) || afterInfo.Size() != beforeInfo.Size() {
		t.Errorf("the configuration file was rewritten with the same bytes: size %d -> %d, mtime %v -> %v",
			beforeInfo.Size(), afterInfo.Size(), beforeInfo.ModTime(), afterInfo.ModTime())
	}

	// THE INSTRUMENT'S OWN PROOF, and this criterion needs it more than any other here.
	// Every assertion above passes trivially against a build with no per-file surface at
	// all, so "it did not write the file" is only worth something once the reading is
	// shown to notice a write. One byte, through the same comparison.
	if err := os.WriteFile(h.configPath, []byte("crf: 23\n"), 0o644); err != nil {
		t.Fatalf("rewriting the configuration file: %v", err)
	}
	if fileDigest(t, h.configPath) == before {
		t.Fatal("the configuration reading passed a file that was rewritten, so every assertion above measured nothing")
	}
	// And the action count is not zero either: a loop over an empty list asserts nothing.
	if len(everyPerFileAction(h.root)) < 5 {
		t.Fatalf("the surface enumerates %d actions; this criterion is about EVERY action it exposes",
			len(everyPerFileAction(h.root)))
	}
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

// --- criterion 13: nothing here reaches a media file or the encoder ----------------

// fsSnapshot is every file under root, with its size and its content digest. It is what
// "created, modified and deleted no media file" is decided against.
func fsSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[p] = fmt.Sprintf("%d:%x", len(raw), sha256.Sum256(raw))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return out
}

// eligible is the set of files under root the engine's own withholding read would let
// through. It is the set this criterion is about: no action may ADD to it.
func eligible(t *testing.T, st *store.SQLite, root string) map[string]bool {
	t.Helper()
	ctx := context.Background()
	out := map[string]bool{}
	for p := range fsSnapshot(t, root) {
		held, err := st.PathIsExcluded(ctx, p)
		if err != nil {
			t.Fatalf("PathIsExcluded(%s): %v", p, err)
		}
		if !held {
			out[p] = true
		}
	}
	return out
}

func TestPerFileActions_OfferNothingToTheEncoder(t *testing.T) {
	h := newPerFileHarness(t, "secret")
	ts := httptest.NewServer(h.srv)
	defer ts.Close()
	ctx := context.Background()
	seedTerminalRows(t, h.st, h.root, 3)

	// A PARKED file, in the library, alongside the rest. It is the sharpest shape this
	// criterion has: what holds a parked file out of the encoder is its ATTEMPT COUNT and
	// nothing else, so a withholding recorded over its row and then removed would hand it
	// straight back - a surface re-opening a decision, which is the authorization line
	// `requeue` stays a local command to keep. Recorded through the same claim-then-finish
	// pair a real run writes, so it is the accounting that parks it and not a hand-set
	// column.
	parked := filepath.Join(h.root, "parked.mkv")
	if err := os.WriteFile(parked, []byte("media bytes for parked.mkv"), 0o644); err != nil {
		t.Fatalf("writing the parked fixture: %v", err)
	}
	const parkedBound = 3
	for i := 0; i < parkedBound; i++ {
		ok, err := h.st.Claim(ctx, parked, "9:9", "seed", parkedBound, store.DecisionInputs{})
		if err != nil || !ok {
			t.Fatalf("parking %s: attempt %d claimed=%v err=%v", parked, i+1, ok, err)
		}
		if err := h.st.Finish(ctx, parked, "9:9", store.Failed,
			&store.Outcome{Reason: "a simulated failure"}, parkedBound); err != nil {
			t.Fatalf("parking %s: %v", parked, err)
		}
	}
	if ok, err := h.st.Claim(ctx, parked, "9:9", "seed", parkedBound, store.DecisionInputs{}); err != nil || ok {
		t.Fatalf("the parked fixture is still claimable, so the case below proves nothing: claimed=%v err=%v", ok, err)
	}

	filesBefore := fsSnapshot(t, h.root)
	eligibleBefore := eligible(t, h.st, h.root)
	ledgerBefore := ledgerSnapshot(t, h.st)
	if len(filesBefore) == 0 || len(eligibleBefore) == 0 {
		t.Fatal("the fixture holds no files, so nothing below is a claim about anything")
	}

	// Every action the surface exposes, plus the add-and-remove over the PARKED path that
	// would re-open it if a withholding could clobber a row.
	actions := append(everyPerFileAction(h.root), []struct{ what, method, path, body string }{
		{"withholding a parked path", http.MethodPost, "/api/exclusions", pathBody(parked)},
		{"releasing a parked path", http.MethodDelete, "/api/exclusions", pathBody(parked)},
	}...)

	for _, a := range actions {
		code, body := do(t, ts, a.method, a.path, "secret", a.body)
		if code >= 500 {
			t.Fatalf("%s answered %d: %s", a.what, code, body)
		}

		// (1) No media file was created, modified or deleted.
		if got := fsSnapshot(t, h.root); !sameFiles(filesBefore, got) {
			t.Fatalf("%s changed the library: %v -> %v", a.what, keysOf(filesBefore), keysOf(got))
		}

		// (2) Nothing became eligible that was not already. Withholding only ever takes a
		// file OUT, so the set may shrink and may return to what it was, and may never
		// grow past it.
		for p := range eligible(t, h.st, h.root) {
			if !eligibleBefore[p] {
				t.Errorf("%s made %q eligible to the encoder when it was not before", a.what, p)
			}
		}

		// (3) No terminal row moved. A row is a DECISION the engine enforces through
		// Claim, so re-opening or deleting one is exactly how a surface hands a file back
		// to the encoder without touching a file itself - and it is what `requeue`,
		// `restore` and `resolve` stay local commands in order to do.
		if got := ledgerSnapshot(t, h.st); !sameRows(ledgerBefore, got) {
			t.Fatalf("%s moved the ledger, which is how a file is handed back to the encoder without touching it:\n%v\n%v",
				a.what, ledgerBefore, got)
		}
	}

	// The parked row is still parked, asked of the store's own answer rather than inferred
	// from the snapshot above: after a withholding was recorded over it and removed again,
	// the attempt count that holds it out is the count it had, and the engine still refuses
	// to claim it.
	if ok, err := h.st.Claim(ctx, parked, "9:9", "seed", parkedBound, store.DecisionInputs{}); err != nil || ok {
		t.Errorf("the parked path became claimable after a withholding was recorded over it and removed: claimed=%v err=%v", ok, err)
	}

	// And the surface really did DO something, or every assertion above is vacuous.
	if code, body := do(t, ts, http.MethodPost, "/api/exclusions", "secret", pathBody(filepath.Join(h.root, "alpha.mkv"))); code != http.StatusOK {
		t.Fatalf("the surface refused the action this case rests on (%d): %s", code, body)
	}
	held, err := h.st.PathIsExcluded(ctx, filepath.Join(h.root, "alpha.mkv"))
	if err != nil || !held {
		t.Fatalf("the surface recorded nothing, so the case proved nothing: held=%v err=%v", held, err)
	}
	// Withholding SHRINKS the eligible set, which is the direction that is allowed.
	after := eligible(t, h.st, h.root)
	if len(after) >= len(eligibleBefore) {
		t.Errorf("recording a withholding did not take the path out of the eligible set: %d -> %d",
			len(eligibleBefore), len(after))
	}
}

// ledgerSnapshot is every row in the ledger, rendered as text, so a case can say "no row
// moved" without asserting on a struct that grows a field every phase.
func ledgerSnapshot(t *testing.T, st *store.SQLite) []string {
	t.Helper()
	rows, err := st.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%s|%s|%d|%s", r.Path, r.Status, r.FailCount, r.Outcome.Reason))
	}
	sort.Strings(out)
	return out
}

func sameRows(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameFiles(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- the graders' own proof: each bites against the mutation it exists for ----------

// The safety assertions above are the ones a later change is most likely to defeat
// silently, so each is driven against a world that breaks exactly the property it
// measures. A check nobody tries to defeat is a check nobody knows works.
func TestPerFileGraders_BiteAgainstTheMutationEachExistsFor(t *testing.T) {
	h := newPerFileHarness(t, "secret")
	root := h.root

	t.Run("the filesystem reading notices a media file that changed", func(t *testing.T) {
		before := fsSnapshot(t, root)
		if err := os.WriteFile(filepath.Join(root, "alpha.mkv"), []byte("different bytes"), 0o644); err != nil {
			t.Fatalf("rewriting a media file: %v", err)
		}
		if sameFiles(before, fsSnapshot(t, root)) {
			t.Error("the filesystem reading passed a library whose file was rewritten")
		}
	})

	t.Run("the filesystem reading notices a media file that appeared", func(t *testing.T) {
		before := fsSnapshot(t, root)
		if err := os.WriteFile(filepath.Join(root, "charlie.mkv"), []byte("new"), 0o644); err != nil {
			t.Fatalf("writing a media file: %v", err)
		}
		if sameFiles(before, fsSnapshot(t, root)) {
			t.Error("the filesystem reading passed a library that gained a file")
		}
	})

	t.Run("the ledger reading notices a terminal row that was re-opened", func(t *testing.T) {
		seeded := seedTerminalRows(t, h.st, "/lib/mut", 2)
		before := ledgerSnapshot(t, h.st)
		if ok, err := h.st.Reopen(context.Background(), seeded[0], "0:0", true); err != nil || !ok {
			t.Fatalf("Reopen: ok=%v err=%v", ok, err)
		}
		if err := h.st.Finish(context.Background(), seeded[0], "0:0", store.Failed,
			&store.Outcome{Reason: "moved"}, 3); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		if sameRows(before, ledgerSnapshot(t, h.st)) {
			t.Error("the ledger reading passed a ledger whose row moved")
		}
	})

	t.Run("the configuration digest notices a byte", func(t *testing.T) {
		before := fileDigest(t, h.configPath)
		raw, err := os.ReadFile(h.configPath)
		if err != nil {
			t.Fatalf("reading the configuration file: %v", err)
		}
		if err := os.WriteFile(h.configPath, append(raw, []byte("crf: 23\n")...), 0o644); err != nil {
			t.Fatalf("rewriting the configuration file: %v", err)
		}
		if fileDigest(t, h.configPath) == before {
			t.Error("the configuration digest passed a file that was appended to")
		}
	})

	t.Run("the eligibility reading notices a path that became eligible", func(t *testing.T) {
		ctx := context.Background()
		p := filepath.Join(root, "bravo.mkv")
		if _, err := h.st.ExcludePath(ctx, p); err != nil {
			t.Fatalf("ExcludePath: %v", err)
		}
		before := eligible(t, h.st, root)
		if before[p] {
			t.Fatal("the fixture is wrong: the withheld path still reads as eligible")
		}
		if _, err := h.st.UnexcludePath(ctx, p); err != nil {
			t.Fatalf("UnexcludePath: %v", err)
		}
		if !eligible(t, h.st, root)[p] {
			t.Error("the eligibility reading did not notice a path that became eligible again")
		}
	})
}

// --- the path rule, one refusal at a time ------------------------------------------

// The path rule decides what may be written into the daemon's state, so it is exercised
// directly as well as over HTTP: a refusal that only ever runs behind a handler is one
// nobody can see the shape of.
func TestAcceptPath_TakesOnlyAPathInsideAConfiguredRoot(t *testing.T) {
	s := &Server{cfg: config.Config{LibraryRoots: []string{"/media/films", "/media/shows"}}}
	for _, tc := range []struct {
		in   string
		want string // "" means refused
	}{
		{"/media/films/a.mkv", "/media/films/a.mkv"},
		{"/media/films/nested/dir/a.mkv", "/media/films/nested/dir/a.mkv"},
		{"/media/films", "/media/films"},
		{"/media/films/./a.mkv", "/media/films/a.mkv"},
		{"/media/shows/b.mkv", "/media/shows/b.mkv"},
		{"/media/films-old/a.mkv", ""},
		{"/media/filmsx", ""},
		{"/media/films/../../etc/passwd", ""},
		{"/etc/passwd", ""},
		{"relative.mkv", ""},
		{"", ""},
		{"   ", ""},
		{"/media/films/a\x00.mkv", ""},
	} {
		got, err := s.acceptPathValue(tc.in)
		if tc.want == "" {
			if err == nil {
				t.Errorf("acceptPathValue(%q) accepted it as %q, want a refusal", tc.in, got)
			} else if !strings.Contains(err.Error(), "nothing changed") {
				t.Errorf("acceptPathValue(%q) refuses with %q, which does not say nothing changed", tc.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("acceptPathValue(%q) refused it: %v", tc.in, err)
		} else if got != tc.want {
			t.Errorf("acceptPathValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// With no root configured, nothing is inside one.
	none := &Server{cfg: config.Config{}}
	if _, err := none.acceptPathValue("/media/films/a.mkv"); err == nil {
		t.Error("a daemon with no library root accepted a path as being inside one")
	}
}

// The response body is JSON the page can read, and the one sentence about where a
// withholding LIVES travels with it: a client that renders the list without the sentence
// would leave an operator looking for the record in a file that does not mention it.
func TestExclusions_CarryTheRuntimeStateSentence(t *testing.T) {
	h := newPerFileHarness(t, "secret")
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	for _, tc := range []struct{ method, body string }{
		{http.MethodGet, ""},
		{http.MethodPost, pathBody(filepath.Join(h.root, "alpha.mkv"))},
		{http.MethodDelete, pathBody(filepath.Join(h.root, "alpha.mkv"))},
	} {
		_, body := do(t, ts, tc.method, "/api/exclusions", "secret", tc.body)
		if !strings.Contains(body, "runtime state") {
			t.Errorf("%s /api/exclusions does not say the withholding is runtime state: %s", tc.method, body)
		}
		if !strings.Contains(body, "configuration file") {
			t.Errorf("%s /api/exclusions does not say the configuration file is not written: %s", tc.method, body)
		}
	}
	// And the notice names no configuration key, because there is none to name.
	for _, forbidden := range []string{"exclude_paths", "include_paths"} {
		if strings.Contains(runtimeStateNotice, forbidden) {
			t.Errorf("the notice names %q, a key this build does not accept", forbidden)
		}
	}
}

// A body larger than the surface will read is refused rather than buffered.
func TestExclude_RefusesAnOversizedBody(t *testing.T) {
	h := newPerFileHarness(t, "secret")
	ts := httptest.NewServer(h.srv)
	defer ts.Close()
	big := bytes.Repeat([]byte("x"), maxPerFileBody*2)
	code, _ := do(t, ts, http.MethodPost, "/api/exclusions", "secret", string(big))
	if code != http.StatusBadRequest {
		t.Errorf("an oversized body answered %d, want 400", code)
	}
}
