package server

// The Sonarr and Radarr webhook intake (T28). The suite is written around the three things
// only this package can establish:
//
//   - a payload in each shape the arr really sends reaches the QUEUE, through the same
//     admission seam POST /api/scan uses, as the path holdfast sees the file by;
//   - the intake's credential opens the intake and nothing else, and nothing else opens it;
//   - a payload the intake does not recognise, or a path the engine refuses, queues nothing
//     and still answers the arr with a 2xx that says why.
//
// The eligibility decision is the REAL one - engine.Submissions over a real tree - and the
// payloads are the fixtures under testdata/webhook, written from the arr's payload classes.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/secret"
)

const (
	hookControlTok = "control-token-5b1e9a"
	hookReadTok    = "read-token-2f7c40"
	hookTok        = "webhook-token-c83d16"
)

// The files the fixtures name, as paths relative to the library root. The arr's view of
// each is the same path under "/", which is what the harness's path maps translate.
const (
	hookEp1   = "tv/Synthetic Series/Season 01/Synthetic Series - S01E01.mkv"
	hookEp2   = "tv/Synthetic Series/Season 01/Synthetic Series - S01E02.mkv"
	hookEp3   = "tv/Synthetic Series/Season 01/Synthetic Series - S01E03.mkv"
	hookEpOld = "tv/Synthetic Series/Season 01/Synthetic Series - S01E01 - old.mkv"
	hookFilm  = "movies/Synthetic Film (2002)/Synthetic Film (2002).mkv"
	hookFilmO = "movies/Synthetic Film (2002)/Synthetic Film (2002) - old.mkv"
)

// recordingSubs is the REAL submission queue with one addition: it remembers every path the
// queue took. It stands exactly where POST /api/scan's queue stands - the Submissions seam -
// so what it records is what the engine was handed.
type recordingSubs struct {
	*engine.Submissions
	mu    sync.Mutex
	taken []string
}

func (r *recordingSubs) Offer(resolved string) bool {
	ok := r.Submissions.Offer(resolved)
	if ok {
		r.mu.Lock()
		r.taken = append(r.taken, resolved)
		r.mu.Unlock()
	}
	return ok
}

func (r *recordingSubs) queued() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.taken...)
}

// webhookHarness is the scan harness - a real engine, a real queue, a real library root and
// an empty ledger - with the intake configured on top of it.
type webhookHarness struct {
	*scanHarness
	rec  *recordingSubs
	logs *lockedBuffer
}

// newWebhookHarness builds it with a control token, a read token and - when token is not
// empty - a webhook token, every fixture file present under the root, and the two path maps
// an install with /tv and /movies mounts in the arrs would write.
func newWebhookHarness(t *testing.T, token string) *webhookHarness {
	t.Helper()
	sh := newScanHarness(t, hookControlTok)
	h := &webhookHarness{scanHarness: sh, logs: &lockedBuffer{}}
	for _, rel := range []string{hookEp1, hookEp2, hookEp3, hookEpOld, hookFilm, hookFilmO} {
		sh.file(t, rel)
	}
	sh.srv.cfg.SonarrPathMap = config.PathMap{{From: filepath.Join(sh.root, "tv"), To: "/tv"}}
	sh.srv.cfg.RadarrPathMap = config.PathMap{{From: filepath.Join(sh.root, "movies"), To: "/movies"}}
	sh.srv.readToken = secret.NewValue(hookReadTok)
	if token != "" {
		sh.srv.SetWebhookToken(secret.NewValue(token))
	}
	h.rec = &recordingSubs{Submissions: sh.subs}
	sh.srv.SetSubmissions(h.rec)
	sh.srv.log = slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return h
}

// want is the path the queue must have been handed for a file under the root: resolved, as
// the engine resolves it.
func (h *webhookHarness) want(t *testing.T, rel string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(filepath.Join(h.root, rel))
	if err != nil {
		t.Fatalf("resolving %s: %v", rel, err)
	}
	return p
}

func hookFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "webhook", name))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	return raw
}

func bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func basic(user, password string) func(*http.Request) {
	return func(r *http.Request) { r.SetBasicAuth(user, password) }
}

// hookAnswer is one response from the intake.
type hookAnswer struct {
	code   int
	raw    string
	header http.Header
	body   webhookResponse
}

// send delivers one payload the way an arr does and returns the answer. A JSON answer is
// decoded STRICTLY: a field the response type does not declare fails the test.
func (h *webhookHarness) send(t *testing.T, method, app string, payload []byte, auth func(*http.Request)) hookAnswer {
	t.Helper()
	req := httptest.NewRequest(method, "/api/webhook/"+app, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if auth != nil {
		auth(req)
	}
	resp := h.serve(req)
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	out := hookAnswer{code: resp.StatusCode, raw: string(raw), header: resp.Header}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&out.body); err != nil {
			t.Fatalf("the response is not the documented shape (%d): %v\n%s", resp.StatusCode, err, raw)
		}
	}
	return out
}

// serve hands one request to the REAL server handler - the router, the gates and the
// handlers behind them - and returns what it answered.
func (h *webhookHarness) serve(req *http.Request) *http.Response {
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	return rec.Result()
}

// post is the ordinary delivery: POST with the webhook token as a bearer credential.
func (h *webhookHarness) post(t *testing.T, app string, payload []byte) hookAnswer {
	t.Helper()
	return h.send(t, http.MethodPost, app, payload, bearer(hookTok))
}

// assertQueued fails unless the queue was handed exactly these paths, in this order, and
// holds that many waiting.
func (h *webhookHarness) assertQueued(t *testing.T, want ...string) {
	t.Helper()
	got := h.rec.queued()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the queue was handed %q, want %q", got, want)
	}
	if n := h.subs.Pending(); n != len(want) {
		t.Errorf("%d path(s) are waiting in the queue, want %d", n, len(want))
	}
}

// skipRecords counts the "webhook queued nothing" log records written so far.
func (h *webhookHarness) skipRecords() int {
	return strings.Count(h.logs.String(), `msg="webhook queued nothing"`)
}

// assertQueuedNothing is the fail-safe answer in full: the status, the rule, a reason in
// words, an empty queue and ledger, and exactly one more skip record than before.
func (h *webhookHarness) assertQueuedNothing(t *testing.T, a hookAnswer, code int, rule string, recordsBefore int) {
	t.Helper()
	if a.code != code {
		t.Errorf("status = %d, want %d: %s", a.code, code, a.raw)
	}
	if a.body.Rule != rule {
		t.Errorf("rule = %q, want %q: %s", a.body.Rule, rule, a.raw)
	}
	if a.body.Reason == "" {
		t.Errorf("an answer that queued nothing gave no reason: %s", a.raw)
	}
	if a.body.Accepted != 0 {
		t.Errorf("accepted = %d on an answer that queued nothing", a.body.Accepted)
	}
	if a.body.Results == nil {
		t.Errorf("results is null, want an array: %s", a.raw)
	}
	for _, res := range a.body.Results {
		if res.Accepted || res.Resolved != "" {
			t.Errorf("a result is marked accepted on an answer that queued nothing: %+v", res)
		}
	}
	h.assertNothingWasTaken(t)
	if got := h.rec.queued(); len(got) != 0 {
		t.Errorf("the queue was handed %q by a request that queued nothing", got)
	}
	if got := h.skipRecords() - recordsBefore; got != 1 {
		t.Errorf("%d skip record(s) were logged, want exactly 1:\n%s", got, h.logs.String())
	}
}

// ---- the four payload shapes -------------------------------------------------

func TestWebhook_SonarrDownloadPerFileShapeQueuesTheFile(t *testing.T) {
	h := newWebhookHarness(t, hookTok)
	a := h.post(t, "sonarr", hookFixture(t, "sonarr-download-episodefile.json"))
	if a.code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", a.code, a.raw)
	}
	want := h.want(t, hookEp1)
	h.assertQueued(t, want)
	if a.body.App != "sonarr" || a.body.EventType != "Download" {
		t.Errorf("app, event_type = %q, %q, want sonarr, Download", a.body.App, a.body.EventType)
	}
	if a.body.Accepted != 1 || a.body.Rejected != 0 || a.body.Rule != "" || a.body.Reason != "" || a.body.Retryable {
		t.Errorf("the answer to a fully queued event carries a refusal: %s", a.raw)
	}
	if len(a.body.Results) != 1 {
		t.Fatalf("%d results, want 1: %s", len(a.body.Results), a.raw)
	}
	res := a.body.Results[0]
	if res.Path != "/"+hookEp1 {
		t.Errorf("result path = %q, want the path as the arr sent it", res.Path)
	}
	if res.Mapped != filepath.Join(h.root, hookEp1) {
		t.Errorf("result mapped = %q, want holdfast's view of the path", res.Mapped)
	}
	if !res.Accepted || res.Resolved != want || res.Rule != "" || res.Detail != "" || res.Retryable {
		t.Errorf("result = %+v, want accepted and resolved to %s", res, want)
	}
	// An accepted event is one record of its own, and not a skip record.
	if !strings.Contains(h.logs.String(), `msg="webhook accepted" app=sonarr event_type=Download accepted=1 rejected=0`) {
		t.Errorf("no accept record was logged:\n%s", h.logs.String())
	}
	if h.skipRecords() != 0 {
		t.Errorf("an accepted event logged a skip record:\n%s", h.logs.String())
	}
	// Admission only: nothing was processed and no ledger row was written by the handler.
	if n := len(h.subs.Results()); n != 0 {
		t.Errorf("%d submissions were processed inside the request", n)
	}
}

func TestWebhook_SonarrDownloadImportCompleteShapeQueuesEveryFile(t *testing.T) {
	h := newWebhookHarness(t, hookTok)
	a := h.post(t, "sonarr", hookFixture(t, "sonarr-download-importcomplete.json"))
	if a.code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", a.code, a.raw)
	}
	h.assertQueued(t, h.want(t, hookEp1), h.want(t, hookEp2), h.want(t, hookEp3))
	if a.body.EventType != "Download" || a.body.Accepted != 3 || a.body.Rejected != 0 {
		t.Errorf("event_type, accepted, rejected = %q, %d, %d, want Download, 3, 0", a.body.EventType, a.body.Accepted, a.body.Rejected)
	}
	if len(a.body.Results) != 3 {
		t.Fatalf("%d results, want one per file: %s", len(a.body.Results), a.raw)
	}
	for i, rel := range []string{hookEp1, hookEp2, hookEp3} {
		if res := a.body.Results[i]; res.Path != "/"+rel || !res.Accepted || res.Resolved != h.want(t, rel) {
			t.Errorf("results[%d] = %+v, want %s accepted", i, res, rel)
		}
	}
}

func TestWebhook_RadarrDownloadQueuesTheFile(t *testing.T) {
	h := newWebhookHarness(t, hookTok)
	a := h.post(t, "radarr", hookFixture(t, "radarr-download-moviefile.json"))
	if a.code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", a.code, a.raw)
	}
	want := h.want(t, hookFilm)
	h.assertQueued(t, want)
	if a.body.App != "radarr" || a.body.EventType != "Download" || a.body.Accepted != 1 || a.body.Rejected != 0 {
		t.Errorf("the answer does not describe one queued Radarr download: %s", a.raw)
	}
	if len(a.body.Results) != 1 || a.body.Results[0].Path != "/"+hookFilm || a.body.Results[0].Resolved != want {
		t.Errorf("results = %+v, want the one film accepted", a.body.Results)
	}
	if !strings.Contains(h.logs.String(), `msg="webhook accepted" app=radarr event_type=Download accepted=1 rejected=0`) {
		t.Errorf("no accept record was logged:\n%s", h.logs.String())
	}
}

// An upgrade is a Download with isUpgrade true. It also carries the files it REPLACED, under
// deletedFiles, each with a path: those are the old files and are never queued, even where
// one is still on disk and would pass every rule.
func TestWebhook_UpgradeArrivesAsDownloadWithIsUpgradeAndIsQueued(t *testing.T) {
	for _, tc := range []struct{ app, fixture, file, old string }{
		{"sonarr", "sonarr-download-upgrade.json", hookEp1, hookEpOld},
		{"radarr", "radarr-download-upgrade.json", hookFilm, hookFilmO},
	} {
		t.Run(tc.app, func(t *testing.T) {
			h := newWebhookHarness(t, hookTok)
			payload := hookFixture(t, tc.fixture)
			// The fixture really is the upgrade shape, and really names the old file.
			var shape struct {
				EventType    string `json:"eventType"`
				IsUpgrade    bool   `json:"isUpgrade"`
				DeletedFiles []struct {
					Path string `json:"path"`
				} `json:"deletedFiles"`
			}
			if err := json.Unmarshal(payload, &shape); err != nil {
				t.Fatal(err)
			}
			if shape.EventType != "Download" || !shape.IsUpgrade || len(shape.DeletedFiles) != 1 ||
				shape.DeletedFiles[0].Path != "/"+tc.old {
				t.Fatalf("the fixture is not an upgrade naming %s: %+v", tc.old, shape)
			}
			if _, err := os.Stat(filepath.Join(h.root, tc.old)); err != nil {
				t.Fatalf("the replaced file must exist for this case to mean anything: %v", err)
			}

			a := h.post(t, tc.app, payload)
			if a.code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202: %s", a.code, a.raw)
			}
			h.assertQueued(t, h.want(t, tc.file))
			if a.body.EventType != "Download" || a.body.Accepted != 1 || a.body.Rejected != 0 || len(a.body.Results) != 1 {
				t.Errorf("the answer does not describe one queued file: %s", a.raw)
			}
			if strings.Contains(a.raw, "old.mkv") {
				t.Errorf("the answer names the replaced file: %s", a.raw)
			}
		})
	}
}

// A Rename queues the file under its NEW path. The previous path is read by nothing.
func TestWebhook_RenameQueuesTheNewPathOnly(t *testing.T) {
	t.Run("sonarr", func(t *testing.T) {
		h := newWebhookHarness(t, hookTok)
		// The previous paths exist too, so only the rule under test keeps them out.
		h.file(t, "tv/Synthetic Series/Season 01/synthetic.s01e01.mkv")
		h.file(t, "tv/Synthetic Series/Season 01/synthetic.s01e02.mkv")
		a := h.post(t, "sonarr", hookFixture(t, "sonarr-rename.json"))
		if a.code != http.StatusAccepted || a.body.EventType != "Rename" || a.body.Accepted != 2 {
			t.Fatalf("status, event_type, accepted = %d, %q, %d, want 202, Rename, 2: %s", a.code, a.body.EventType, a.body.Accepted, a.raw)
		}
		h.assertQueued(t, h.want(t, hookEp1), h.want(t, hookEp2))
	})
	t.Run("radarr", func(t *testing.T) {
		h := newWebhookHarness(t, hookTok)
		h.file(t, "movies/Synthetic Film (2002)/synthetic.film.2002.mkv")
		a := h.post(t, "radarr", hookFixture(t, "radarr-rename.json"))
		if a.code != http.StatusAccepted || a.body.EventType != "Rename" || a.body.Accepted != 1 {
			t.Fatalf("status, event_type, accepted = %d, %q, %d, want 202, Rename, 1: %s", a.code, a.body.EventType, a.body.Accepted, a.raw)
		}
		h.assertQueued(t, h.want(t, hookFilm))
	})
}

// An arr's Webhook connection sends POST or PUT. Both reach the same handler.
func TestWebhook_PutIsAcceptedAsPostIs(t *testing.T) {
	for _, tc := range []struct{ app, fixture, file string }{
		{"sonarr", "sonarr-download-episodefile.json", hookEp1},
		{"radarr", "radarr-download-moviefile.json", hookFilm},
	} {
		h := newWebhookHarness(t, hookTok)
		a := h.send(t, http.MethodPut, tc.app, hookFixture(t, tc.fixture), bearer(hookTok))
		if a.code != http.StatusAccepted {
			t.Fatalf("PUT %s: status = %d, want 202: %s", tc.app, a.code, a.raw)
		}
		h.assertQueued(t, h.want(t, tc.file))
		// And no other method is routed.
		for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPatch} {
			if got := h.send(t, method, tc.app, nil, bearer(hookTok)); got.code != http.StatusMethodNotAllowed {
				t.Errorf("%s /api/webhook/%s = %d, want 405", method, tc.app, got.code)
			}
		}
	}
}

// ---- the credential ----------------------------------------------------------

func TestWebhook_IsAuthenticated(t *testing.T) {
	sonarr := hookFixture(t, "sonarr-download-episodefile.json")
	radarr := hookFixture(t, "radarr-download-moviefile.json")
	payloads := map[string][]byte{"sonarr": sonarr, "radarr": radarr}

	t.Run("no webhook_token configured: 403 and nothing queued", func(t *testing.T) {
		h := newWebhookHarness(t, "")
		for app, payload := range payloads {
			for name, auth := range map[string]func(*http.Request){
				"no credential":                 nil,
				"the control token":             bearer(hookControlTok),
				"the read token":                bearer(hookReadTok),
				"an empty bearer":               bearer(""),
				"an empty basic password":       basic("holdfast", ""),
				"the control token as password": basic("holdfast", hookControlTok),
			} {
				for _, method := range []string{http.MethodPost, http.MethodPut} {
					a := h.send(t, method, app, payload, auth)
					if a.code != http.StatusForbidden {
						t.Errorf("%s %s with %s = %d, want 403: %s", method, app, name, a.code, a.raw)
					}
					if !strings.Contains(a.raw, "webhook_token") {
						t.Errorf("the 403 does not name webhook_token: %s", a.raw)
					}
				}
			}
		}
		h.assertNothingWasTaken(t)
		h.assertQueued(t)
	})

	t.Run("a missing or wrong credential: 401 and nothing queued", func(t *testing.T) {
		h := newWebhookHarness(t, hookTok)
		for app, payload := range payloads {
			for name, auth := range map[string]func(*http.Request){
				"no credential":                       nil,
				"a wrong bearer":                      bearer("not-the-token"),
				"an empty bearer":                     bearer(""),
				"the token with a suffix":             bearer(hookTok + "x"),
				"a prefix of the token":               bearer(hookTok[:len(hookTok)-1]),
				"a wrong basic password":              basic("sonarr", "not-the-token"),
				"an empty basic password":             basic("sonarr", ""),
				"the token as the basic USERNAME":     basic(hookTok, "not-the-token"),
				"the token as username, no password":  basic(hookTok, ""),
				"the control token as a bearer":       bearer(hookControlTok),
				"the control token as basic password": basic("holdfast", hookControlTok),
				"the read token as a bearer":          bearer(hookReadTok),
				"the read token as basic password":    basic("holdfast", hookReadTok),
				"the token under another scheme": func(r *http.Request) {
					r.Header.Set("Authorization", "Token "+hookTok)
				},
				"the token in X-Api-Key": func(r *http.Request) { r.Header.Set("X-Api-Key", hookTok) },
				"the token in the query string": func(r *http.Request) {
					r.URL.RawQuery = "token=" + hookTok + "&apikey=" + hookTok + "&access_token=" + hookTok
				},
			} {
				a := h.send(t, http.MethodPost, app, payload, auth)
				if a.code != http.StatusUnauthorized {
					t.Errorf("%s with %s = %d, want 401: %s", app, name, a.code, a.raw)
				}
				if strings.TrimSpace(a.raw) != "unauthorized" {
					t.Errorf("the 401 body is %q, want the one word every gate answers with", a.raw)
				}
				challenges := a.header.Values("WWW-Authenticate")
				if fmt.Sprint(challenges) != fmt.Sprint([]string{`Bearer realm="holdfast"`, `Basic realm="holdfast"`}) {
					t.Errorf("the 401 challenges are %q, want Bearer then Basic", challenges)
				}
			}
		}
		h.assertNothingWasTaken(t)
		h.assertQueued(t)
		if strings.Contains(h.logs.String(), "webhook") {
			t.Errorf("a refused credential reached the handler's log:\n%s", h.logs.String())
		}
	})

	t.Run("the token as a bearer is accepted", func(t *testing.T) {
		h := newWebhookHarness(t, hookTok)
		if a := h.send(t, http.MethodPost, "sonarr", sonarr, bearer(hookTok)); a.code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", a.code, a.raw)
		}
		// The scheme is matched without regard to case, as the other gates match it.
		if a := h.send(t, http.MethodPost, "radarr", radarr, func(r *http.Request) {
			r.Header.Set("Authorization", "bearer "+hookTok)
		}); a.code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", a.code, a.raw)
		}
		h.assertQueued(t, h.want(t, hookEp1), h.want(t, hookFilm))
	})

	t.Run("the token as the basic password is accepted, with any username", func(t *testing.T) {
		h := newWebhookHarness(t, hookTok)
		if a := h.send(t, http.MethodPost, "sonarr", sonarr, basic("sonarr", hookTok)); a.code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", a.code, a.raw)
		}
		if a := h.send(t, http.MethodPost, "radarr", radarr, basic("", hookTok)); a.code != http.StatusAccepted {
			t.Fatalf("an empty username: status = %d, want 202: %s", a.code, a.raw)
		}
		h.assertQueued(t, h.want(t, hookEp1), h.want(t, hookFilm))
	})

	t.Run("the webhook token opens no control or read endpoint", func(t *testing.T) {
		h := newWebhookHarness(t, hookTok)
		scanBody := `{"paths": ["` + filepath.Join(h.root, hookEp1) + `"]}`
		for _, ep := range []struct{ method, path, body string }{
			{http.MethodPost, "/api/scan", scanBody},
			{http.MethodPost, "/api/pause", ""},
			{http.MethodPost, "/api/resume", ""},
			{http.MethodPost, "/api/rescan", ""},
			{http.MethodGet, "/api/search?q=Synthetic", ""},
			{http.MethodGet, "/api/exclusions/", ""},
			{http.MethodPost, "/api/exclusions/", `{"path": "` + filepath.Join(h.root, hookEp1) + `"}`},
			{http.MethodDelete, "/api/exclusions/", `{"path": "` + filepath.Join(h.root, hookEp1) + `"}`},
			{http.MethodGet, "/api/summary", ""},
			{http.MethodGet, "/api/queue", ""},
			{http.MethodGet, "/api/history", ""},
			{http.MethodGet, "/api/health", ""},
			{http.MethodGet, "/api/events", ""},
		} {
			for name, auth := range map[string]func(*http.Request){
				"bearer": bearer(hookTok), "basic password": basic("sonarr", hookTok),
			} {
				req := httptest.NewRequest(ep.method, ep.path, strings.NewReader(ep.body))
				req.Header.Set("Content-Type", "application/json")
				auth(req)
				resp := h.serve(req)
				if resp.StatusCode != http.StatusUnauthorized {
					t.Errorf("%s %s with the WEBHOOK token as %s = %d, want 401", ep.method, ep.path, name, resp.StatusCode)
				}
			}
		}
		if h.ctrl.Paused() {
			t.Error("the webhook token paused holdfast")
		}
		h.assertNothingWasTaken(t)
		h.assertQueued(t)

		// Anti-vacuity: the same requests with the credential each endpoint takes are
		// answered, so the 401s above are about the credential and not the route.
		req := httptest.NewRequest(http.MethodPost, "/api/pause", nil)
		bearer(hookControlTok)(req)
		resp := h.serve(req)
		if resp.StatusCode != http.StatusOK || !h.ctrl.Paused() {
			t.Fatalf("POST /api/pause with the control token = %d (paused=%v), want 200 and paused", resp.StatusCode, h.ctrl.Paused())
		}
	})

	t.Run("no answer and no log record carries the credential", func(t *testing.T) {
		h := newWebhookHarness(t, hookTok)
		var seen strings.Builder
		for _, a := range []hookAnswer{
			h.send(t, http.MethodPost, "sonarr", sonarr, bearer(hookTok)),
			h.send(t, http.MethodPost, "radarr", radarr, basic("radarr", hookTok)),
			h.send(t, http.MethodPost, "sonarr", sonarr, bearer("wrong")),
			h.send(t, http.MethodPost, "sonarr", hookFixture(t, "sonarr-test.json"), bearer(hookTok)),
			h.send(t, http.MethodPost, "sonarr", []byte("{"), bearer(hookTok)),
		} {
			seen.WriteString(a.raw)
			fmt.Fprint(&seen, a.header)
		}
		seen.WriteString(h.logs.String())
		if strings.Contains(seen.String(), hookTok) {
			t.Errorf("the webhook credential appears in an answer or a log record:\n%s", seen.String())
		}
	})
}

// ---- the events that queue nothing ---------------------------------------------

func TestWebhook_TestEventAnswers200AndQueuesNothing(t *testing.T) {
	for _, app := range []string{"sonarr", "radarr"} {
		t.Run(app, func(t *testing.T) {
			h := newWebhookHarness(t, hookTok)
			a := h.post(t, app, hookFixture(t, app+"-test.json"))
			h.assertQueuedNothing(t, a, http.StatusOK, "test-event", 0)
			if a.body.App != app || a.body.EventType != "Test" || a.body.Retryable || len(a.body.Results) != 0 {
				t.Errorf("the answer to a Test is not the documented one: %s", a.raw)
			}
			if !strings.Contains(h.logs.String(),
				`msg="webhook queued nothing" app=`+app+` event_type=Test rule=test-event status=200 rejected=0`) {
				t.Errorf("the skip record is not the documented one:\n%s", h.logs.String())
			}

			// A Test proves the connection, so it is answered the same while paused.
			h.ctrl.Pause()
			a = h.post(t, app, hookFixture(t, app+"-test.json"))
			h.assertQueuedNothing(t, a, http.StatusOK, "test-event", 1)
		})
	}
}

func TestWebhook_UnknownEventOrMissingPathQueuesNothing(t *testing.T) {
	h := newWebhookHarness(t, hookTok)
	good := "/" + hookEp1
	for _, tc := range []struct {
		name, app, payload string
		rule, eventType    string
		results            int
	}{
		{"an event the intake does not act on (Grab)", "sonarr", string(hookFixture(t, "sonarr-grab.json")),
			"event-not-consumed", "Grab", 0},
		{"a declared event with a file in it (EpisodeFileDelete)", "sonarr",
			`{"eventType":"EpisodeFileDelete","episodeFile":{"path":"` + good + `"}}`, "event-not-consumed", "EpisodeFileDelete", 0},
		{"a declared Radarr event (MovieFileDelete)", "radarr",
			`{"eventType":"MovieFileDelete","movieFile":{"path":"/` + hookFilm + `"}}`, "event-not-consumed", "MovieFileDelete", 0},
		{"an event type neither arr declares", "sonarr",
			`{"eventType":"Mystery","episodeFile":{"path":"` + good + `"}}`, "event-not-consumed", "unrecognised", 0},
		{"the event type in another case", "sonarr",
			`{"eventType":"download","episodeFile":{"path":"` + good + `"}}`, "event-not-consumed", "unrecognised", 0},
		{"no event type at all", "sonarr",
			`{"episodeFile":{"path":"` + good + `"}}`, "event-not-consumed", "unrecognised", 0},
		{"a null event type", "sonarr",
			`{"eventType":null,"episodeFile":{"path":"` + good + `"}}`, "event-not-consumed", "unrecognised", 0},
		{"an empty object", "radarr", `{}`, "event-not-consumed", "unrecognised", 0},
		{"a Download naming no file", "sonarr", `{"eventType":"Download","series":{"path":"/tv/Synthetic Series"}}`,
			"no-file-path", "Download", 0},
		{"a Download whose file is null", "sonarr", `{"eventType":"Download","episodeFile":null,"episodeFiles":null}`,
			"no-file-path", "Download", 0},
		{"a Download with an empty file list", "sonarr", `{"eventType":"Download","episodeFiles":[]}`,
			"no-file-path", "Download", 0},
		{"a Radarr Download naming no file", "radarr", `{"eventType":"Download","movie":{"folderPath":"/movies/Synthetic Film (2002)"}}`,
			"no-file-path", "Download", 0},
		{"a Radarr Download with a file LIST, a shape Radarr does not send", "radarr",
			`{"eventType":"Download","movieFiles":[{"path":"/` + hookFilm + `"}]}`, "no-file-path", "Download", 0},
		{"a Rename naming no file", "sonarr", `{"eventType":"Rename"}`, "no-file-path", "Rename", 0},
		{"a Rename carrying a Download's key", "sonarr",
			`{"eventType":"Rename","episodeFile":{"path":"` + good + `"}}`, "no-file-path", "Rename", 0},
		{"a Download carrying a Rename's key", "sonarr",
			`{"eventType":"Download","renamedEpisodeFiles":[{"path":"` + good + `"}]}`, "no-file-path", "Download", 0},
		{"a file with no path", "sonarr", `{"eventType":"Download","episodeFile":{"relativePath":"Season 01/x.mkv"}}`,
			"nothing-accepted", "Download", 1},
		{"a file with a null path", "sonarr", `{"eventType":"Download","episodeFile":{"path":null}}`,
			"nothing-accepted", "Download", 1},
		{"a file with an empty path", "radarr", `{"eventType":"Download","movieFile":{"path":""}}`,
			"nothing-accepted", "Download", 1},
		{"a relative path", "sonarr", `{"eventType":"Download","episodeFile":{"path":"` + hookEp1 + `"}}`,
			"nothing-accepted", "Download", 1},
		{"a Windows path", "sonarr", `{"eventType":"Download","episodeFile":{"path":"C:\\tv\\Synthetic Series\\x.mkv"}}`,
			"nothing-accepted", "Download", 1},
		{"a file that does not exist", "sonarr", `{"eventType":"Download","episodeFile":{"path":"/tv/Synthetic Series/absent.mkv"}}`,
			"nothing-accepted", "Download", 1},
		{"a file that is not a video", "sonarr", `{"eventType":"Rename","renamedEpisodeFiles":[{"path":"/tv/notes.txt"}]}`,
			"nothing-accepted", "Rename", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := h.skipRecords()
			a := h.post(t, tc.app, []byte(tc.payload))
			h.assertQueuedNothing(t, a, http.StatusOK, tc.rule, before)
			if a.body.EventType != tc.eventType {
				t.Errorf("event_type = %q, want %q", a.body.EventType, tc.eventType)
			}
			if a.body.Retryable {
				t.Errorf("the answer is marked retryable: %s", a.raw)
			}
			if len(a.body.Results) != tc.results || a.body.Rejected != tc.results {
				t.Errorf("results, rejected = %d, %d, want %d each: %s", len(a.body.Results), a.body.Rejected, tc.results, a.raw)
			}
			for _, res := range a.body.Results {
				if res.Rule == "" || res.Detail == "" {
					t.Errorf("a refused file carries no rule or no detail: %+v", res)
				}
			}
			if strings.Contains(h.logs.String(), "Mystery") {
				t.Errorf("an undeclared event type was echoed into the log:\n%s", h.logs.String())
			}
		})
	}

	// The per-file rule is the one that fits: the intake's own for a file with no path, and
	// the engine's for a path it was handed.
	h.file(t, "tv/notes.txt")
	for payload, want := range map[string]string{
		`{"eventType":"Download","episodeFile":{"relativePath":"x.mkv"}}`:                     "no-file-path",
		`{"eventType":"Download","episodeFile":{"path":""}}`:                                  "no-file-path",
		`{"eventType":"Download","episodeFile":{"path":"` + hookEp1 + `"}}`:                   engine.RuleNotAbsolute,
		`{"eventType":"Download","episodeFile":{"path":"/tv/Synthetic Series/absent.mkv"}}`:   engine.RuleNotARegularFile,
		`{"eventType":"Rename","renamedEpisodeFiles":[{"path":"/tv/notes.txt"}]}`:             engine.RuleNotAVideoFile,
		`{"eventType":"Download","episodeFile":{"path":"/tv/Synthetic Series/a\tb.mkv"}}`:     engine.RuleUnsupportedCharacters,
		`{"eventType":"Download","episodeFile":{"path":"/tv/Synthetic Series/Season 01"}}`:    engine.RuleNotARegularFile,
		`{"eventType":"Download","episodeFile":{"path":"/tv/Synthetic Series/Season 01/"}}`:   engine.RuleNotARegularFile,
		`{"eventType":"Download","episodeFile":{"path":"/tv/Synthetic Series/../notes.txt"}}`: engine.RuleNotAVideoFile,
	} {
		a := h.post(t, "sonarr", []byte(payload))
		if a.code != http.StatusOK || len(a.body.Results) != 1 || a.body.Results[0].Rule != want {
			t.Errorf("%s: status %d, results %+v, want 200 and one result under %s", payload, a.code, a.body.Results, want)
		}
	}
	// A relative path is handed to the engine as it was sent, never mapped or resolved.
	a := h.post(t, "sonarr", []byte(`{"eventType":"Download","episodeFile":{"path":"`+hookEp1+`"}}`))
	if len(a.body.Results) != 1 || a.body.Results[0].Mapped != hookEp1 || a.body.Results[0].Path != hookEp1 {
		t.Errorf("a relative path was rewritten before it was judged: %+v", a.body.Results)
	}
	h.assertNothingWasTaken(t)
	h.assertQueued(t)
}

// A body that is not the JSON an arr sends is the caller's error, answered as POST /api/scan
// answers one: 400 for a malformed body, 413 for one over a bound.
func TestWebhook_MalformedOrOversizedBodyIsRefused(t *testing.T) {
	h := newWebhookHarness(t, hookTok)
	good := "/" + hookEp1
	for name, payload := range map[string]string{
		"not JSON":                     `not json`,
		"an empty body":                ``,
		"truncated JSON":               `{"eventType":"Download","episodeFile":{"path":"` + good,
		"an array":                     `[{"eventType":"Download"}]`,
		"a string":                     `"Download"`,
		"a numeric event type":         `{"eventType":7,"episodeFile":{"path":"` + good + `"}}`,
		"a file that is a string":      `{"eventType":"Download","episodeFile":"` + good + `"}`,
		"a file that is an array":      `{"eventType":"Download","episodeFile":[{"path":"` + good + `"}]}`,
		"a numeric path":               `{"eventType":"Download","episodeFile":{"path":7}}`,
		"a file list that is one":      `{"eventType":"Download","episodeFiles":{"path":"` + good + `"}}`,
		"a file list of strings":       `{"eventType":"Download","episodeFiles":["` + good + `"]}`,
		"a rename list that is one":    `{"eventType":"Rename","renamedEpisodeFiles":{"path":"` + good + `"}}`,
		"a good file beside a bad one": `{"eventType":"Download","episodeFile":{"path":"` + good + `"},"episodeFiles":[{"path":7}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			before := h.skipRecords()
			a := h.post(t, "sonarr", []byte(payload))
			h.assertQueuedNothing(t, a, http.StatusBadRequest, "malformed-body", before)
			if a.body.App != "sonarr" || a.body.EventType != "" || a.body.Retryable || len(a.body.Results) != 0 {
				t.Errorf("the refusal is not the documented one: %s", a.raw)
			}
		})
	}

	t.Run("a body over the bound", func(t *testing.T) {
		before := h.skipRecords()
		big := `{"eventType":"Download","episodeFile":{"path":"` + good + `"},"pad":"` + strings.Repeat("x", MaxScanBodyBytes) + `"}`
		a := h.post(t, "sonarr", []byte(big))
		h.assertQueuedNothing(t, a, http.StatusRequestEntityTooLarge, "body-too-large", before)
		if !strings.Contains(a.body.Reason, fmt.Sprint(MaxScanBodyBytes)) {
			t.Errorf("the refusal does not name the bound: %s", a.raw)
		}
	})

	t.Run("more files than one request may name", func(t *testing.T) {
		entries := func(n int) string {
			es := make([]string, n)
			for i := range es {
				es[i] = `{"path":"` + good + `"}`
			}
			return `{"eventType":"Download","episodeFiles":[` + strings.Join(es, ",") + `]}`
		}
		before := h.skipRecords()
		a := h.post(t, "sonarr", []byte(entries(MaxScanPaths+1)))
		h.assertQueuedNothing(t, a, http.StatusRequestEntityTooLarge, "too-many-paths", before)
		if !strings.Contains(a.body.Reason, fmt.Sprint(MaxScanPaths)) || !strings.Contains(a.body.Reason, fmt.Sprint(MaxScanPaths+1)) {
			t.Errorf("the refusal does not name the bound and the count: %s", a.raw)
		}
		if a.body.EventType != "Download" || len(a.body.Results) != 0 {
			t.Errorf("the refusal is not the documented one: %s", a.raw)
		}
		// Exactly the maximum is taken: the one file, named 256 times, is queued once.
		a = h.post(t, "sonarr", []byte(entries(MaxScanPaths)))
		if a.code != http.StatusAccepted || a.body.Accepted != 1 || a.body.Rejected != MaxScanPaths-1 {
			t.Fatalf("status, accepted, rejected = %d, %d, %d, want 202, 1, %d", a.code, a.body.Accepted, a.body.Rejected, MaxScanPaths-1)
		}
		h.assertQueued(t, h.want(t, hookEp1))
	})
}

// ---- the path map and the roots ------------------------------------------------

func TestWebhook_PathIsMappedBackThroughThePathMap(t *testing.T) {
	sonarr := hookFixture(t, "sonarr-download-episodefile.json")
	radarr := hookFixture(t, "radarr-download-moviefile.json")

	t.Run("each arr's own map translates its paths", func(t *testing.T) {
		h := newWebhookHarness(t, hookTok)
		// Each arr's map sends the OTHER arr's prefix somewhere that does not exist, so a
		// handler reading the wrong map queues nothing.
		h.srv.cfg.SonarrPathMap = append(h.srv.cfg.SonarrPathMap, config.PathMapEntry{From: filepath.Join(h.root, "nowhere"), To: "/movies"})
		h.srv.cfg.RadarrPathMap = append(h.srv.cfg.RadarrPathMap, config.PathMapEntry{From: filepath.Join(h.root, "nowhere"), To: "/tv"})
		a := h.post(t, "sonarr", sonarr)
		if a.code != http.StatusAccepted || a.body.Results[0].Mapped != filepath.Join(h.root, hookEp1) {
			t.Fatalf("sonarr: status %d, results %+v", a.code, a.body.Results)
		}
		a = h.post(t, "radarr", radarr)
		if a.code != http.StatusAccepted || a.body.Results[0].Mapped != filepath.Join(h.root, hookFilm) {
			t.Fatalf("radarr: status %d, results %+v", a.code, a.body.Results)
		}
		h.assertQueued(t, h.want(t, hookEp1), h.want(t, hookFilm))
		// The mapped paths, and only at debug, are in the log; no title is.
		logs := h.logs.String()
		if !strings.Contains(logs, `level=DEBUG msg="webhook paths" app=sonarr event_type=Download`) ||
			!strings.Contains(logs, filepath.Join(h.root, hookEp1)) {
			t.Errorf("the debug record of the mapped paths is missing:\n%s", logs)
		}
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, "level=INFO") && strings.Contains(line, h.root) {
				t.Errorf("a path was logged at info: %s", line)
			}
		}
	})

	t.Run("the longest matching prefix wins", func(t *testing.T) {
		h := newWebhookHarness(t, hookTok)
		h.file(t, "anime/Season 01/Synthetic Series - S01E01.mkv")
		h.srv.cfg.SonarrPathMap = config.PathMap{
			{From: filepath.Join(h.root, "tv"), To: "/tv"},
			{From: filepath.Join(h.root, "anime"), To: "/tv/Synthetic Series"},
		}
		a := h.post(t, "sonarr", sonarr)
		if a.code != http.StatusAccepted {
			t.Fatalf("status = %d: %s", a.code, a.raw)
		}
		h.assertQueued(t, h.want(t, "anime/Season 01/Synthetic Series - S01E01.mkv"))
	})

	t.Run("with no map the arr's path is judged as it stands", func(t *testing.T) {
		h := newWebhookHarness(t, hookTok)
		h.srv.cfg.SonarrPathMap, h.srv.cfg.RadarrPathMap = nil, nil
		a := h.post(t, "sonarr", sonarr)
		h.assertQueuedNothing(t, a, http.StatusOK, "nothing-accepted", 0)
		if len(a.body.Results) != 1 || a.body.Results[0].Mapped != "/"+hookEp1 {
			t.Fatalf("results = %+v, want the one path unmapped", a.body.Results)
		}
		if !strings.Contains(a.body.Reason, "sonarr_path_map") {
			t.Errorf("the reason does not point at sonarr_path_map: %s", a.body.Reason)
		}
		a = h.post(t, "radarr", radarr)
		if a.code != http.StatusOK || !strings.Contains(a.body.Reason, "radarr_path_map") {
			t.Errorf("radarr: status %d, reason %q, want 200 pointing at radarr_path_map", a.code, a.body.Reason)
		}

		// And a path that is ALREADY holdfast's view needs no map at all.
		same := `{"eventType":"Download","episodeFile":{"path":"` + filepath.Join(h.root, hookEp1) + `"}}`
		if a := h.post(t, "sonarr", []byte(same)); a.code != http.StatusAccepted {
			t.Fatalf("an unmapped path under a root: status %d: %s", a.code, a.raw)
		}
		h.assertQueued(t, h.want(t, hookEp1))
	})

	t.Run("the map direction is the reverse of the rescan client's", func(t *testing.T) {
		h := newWebhookHarness(t, hookTok)
		// A path written in HOLDFAST's view of a mapped prefix is not the arr's view, and
		// the forward direction is never applied to it: from /tv to root/tv, never back.
		m := h.srv.cfg.SonarrPathMap
		if got := mapWebhookPath(m, "/"+hookEp1); got != filepath.Join(h.root, hookEp1) {
			t.Errorf("mapWebhookPath = %q, want the path under the root", got)
		}
		if got := mapWebhookPath(m, filepath.Join(h.root, hookEp1)); got != filepath.Join(h.root, hookEp1) {
			t.Errorf("a holdfast-view path was mapped forward to %q", got)
		}
		if got := mapWebhookPath(m, "/tv/a/../b.mkv"); got != filepath.Join(h.root, "tv/b.mkv") {
			t.Errorf("mapWebhookPath = %q, want the cleaned path under the root", got)
		}
		for _, sent := range []string{"", "tv/x.mkv", "./tv/x.mkv", `C:\tv\x.mkv`, " /tv/x.mkv"} {
			if got := mapWebhookPath(m, sent); got != sent {
				t.Errorf("mapWebhookPath(%q) = %q, want it untouched", sent, got)
			}
		}
	})
}

func TestWebhook_PathOutsideEveryRootIsRefused(t *testing.T) {
	h := newWebhookHarness(t, hookTok)
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "Synthetic Series - S01E09.mkv")
	if err := os.WriteFile(outsideFile, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A link INSIDE the root whose target is outside it.
	link := filepath.Join(h.root, "tv", "link.mkv")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Fatal(err)
	}
	// A map entry that sends an arr prefix outside every root.
	h.srv.cfg.SonarrPathMap = append(h.srv.cfg.SonarrPathMap, config.PathMapEntry{From: outside, To: "/elsewhere"})

	download := func(p string) []byte {
		b, err := json.Marshal(map[string]any{"eventType": "Download", "episodeFile": map[string]string{"path": p}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for name, sent := range map[string]string{
		"an absolute path outside the roots":     outsideFile,
		"a mapped path that lands outside":       "/elsewhere/Synthetic Series - S01E09.mkv",
		"a path that climbs out of a mapped dir": "/tv/.." + outsideFile,
		"a link inside the root pointing out":    "/tv/link.mkv",
	} {
		t.Run(name, func(t *testing.T) {
			before := h.skipRecords()
			a := h.post(t, "sonarr", download(sent))
			h.assertQueuedNothing(t, a, http.StatusOK, "nothing-accepted", before)
			if len(a.body.Results) != 1 {
				t.Fatalf("%d results, want 1: %s", len(a.body.Results), a.raw)
			}
			res := a.body.Results[0]
			if res.Rule != engine.RuleOutsideRoots {
				t.Errorf("rule = %q, want %q: %+v", res.Rule, engine.RuleOutsideRoots, res)
			}
			if res.Path != sent || res.Retryable || a.body.Rejected != 1 {
				t.Errorf("the refusal is not the documented one: %s", a.raw)
			}
		})
	}

	// One event naming a file inside and a file outside: the inside one is queued, the
	// outside one is refused in the per-path report, and the answer is the accepting one.
	mixed := `{"eventType":"Download","episodeFiles":[{"path":"` + outsideFile + `"},{"path":"/` + hookEp2 + `"},{"path":"/tv/link.mkv"}]}`
	a := h.post(t, "sonarr", []byte(mixed))
	if a.code != http.StatusAccepted || a.body.Accepted != 1 || a.body.Rejected != 2 || a.body.Rule != "" {
		t.Fatalf("status, accepted, rejected, rule = %d, %d, %d, %q, want 202, 1, 2 and no rule: %s",
			a.code, a.body.Accepted, a.body.Rejected, a.body.Rule, a.raw)
	}
	h.assertQueued(t, h.want(t, hookEp2))
	if len(a.body.Results) != 3 || a.body.Results[0].Rule != engine.RuleOutsideRoots || !a.body.Results[1].Accepted ||
		a.body.Results[2].Rule != engine.RuleOutsideRoots {
		t.Errorf("the per-path report is out of order or wrong: %+v", a.body.Results)
	}
	if !strings.Contains(h.logs.String(), `msg="webhook accepted" app=sonarr event_type=Download accepted=1 rejected=2`) {
		t.Errorf("the accept record does not carry the counts:\n%s", h.logs.String())
	}
}

func TestWebhook_WrongAppShapeQueuesNothing(t *testing.T) {
	h := newWebhookHarness(t, hookTok)
	// Both maps translate both prefixes, so the only thing keeping a file out is the shape.
	both := config.PathMap{
		{From: filepath.Join(h.root, "tv"), To: "/tv"},
		{From: filepath.Join(h.root, "movies"), To: "/movies"},
	}
	h.srv.cfg.SonarrPathMap, h.srv.cfg.RadarrPathMap = both, both

	for _, tc := range []struct{ name, app, payload, eventType, carries string }{
		{"a Sonarr per-file Download at /radarr", "radarr", string(hookFixture(t, "sonarr-download-episodefile.json")), "Download", "series"},
		{"a Sonarr import-complete Download at /radarr", "radarr", string(hookFixture(t, "sonarr-download-importcomplete.json")), "Download", "series"},
		{"a Sonarr Rename at /radarr", "radarr", string(hookFixture(t, "sonarr-rename.json")), "Rename", "series"},
		{"a Sonarr Grab at /radarr", "radarr", string(hookFixture(t, "sonarr-grab.json")), "Grab", "series"},
		{"a Radarr Download at /sonarr", "sonarr", string(hookFixture(t, "radarr-download-moviefile.json")), "Download", "movie"},
		{"a Radarr Rename at /sonarr", "sonarr", string(hookFixture(t, "radarr-rename.json")), "Rename", "movie"},
		// A payload carrying BOTH shapes is ambiguous, and an ambiguous payload queues nothing
		// even though the endpoint's own key names a file that would pass every rule.
		{"both shapes at /radarr", "radarr",
			`{"eventType":"Download","movieFile":{"path":"/` + hookFilm + `"},"episodeFile":{"path":"/` + hookEp1 + `"}}`, "Download", "episodeFile"},
		{"both shapes at /sonarr", "sonarr",
			`{"eventType":"Download","episodeFile":{"path":"/` + hookEp1 + `"},"movieFile":{"path":"/` + hookFilm + `"}}`, "Download", "movieFile"},
		{"each foreign key alone: episodes", "radarr", `{"eventType":"Download","movieFile":{"path":"/` + hookFilm + `"},"episodes":[]}`, "Download", "episodes"},
		{"each foreign key alone: episodeFiles", "radarr", `{"eventType":"Download","movieFile":{"path":"/` + hookFilm + `"},"episodeFiles":[]}`, "Download", "episodeFiles"},
		{"each foreign key alone: renamedEpisodeFiles", "radarr", `{"eventType":"Rename","renamedMovieFiles":[{"path":"/` + hookFilm + `"}],"renamedEpisodeFiles":[]}`, "Rename", "renamedEpisodeFiles"},
		{"each foreign key alone: remoteMovie", "sonarr", `{"eventType":"Download","episodeFile":{"path":"/` + hookEp1 + `"},"remoteMovie":{}}`, "Download", "remoteMovie"},
		{"each foreign key alone: renamedMovieFiles", "sonarr", `{"eventType":"Rename","renamedEpisodeFiles":[{"path":"/` + hookEp1 + `"}],"renamedMovieFiles":[]}`, "Rename", "renamedMovieFiles"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := h.skipRecords()
			a := h.post(t, tc.app, []byte(tc.payload))
			h.assertQueuedNothing(t, a, http.StatusOK, "wrong-app-shape", before)
			if a.body.App != tc.app || a.body.EventType != tc.eventType || a.body.Retryable || len(a.body.Results) != 0 {
				t.Errorf("the answer is not the documented one: %s", a.raw)
			}
			if !strings.Contains(a.body.Reason, `"`+tc.carries+`"`) || !strings.Contains(a.body.Reason, tc.app+" payloads") {
				t.Errorf("the reason does not name the key %q and the endpoint's arr: %s", tc.carries, a.body.Reason)
			}
		})
	}

	// A foreign key that is null is a key the arr did not send: the payload is the
	// endpoint's own shape and is queued.
	a := h.post(t, "radarr", []byte(`{"eventType":"Download","movieFile":{"path":"/`+hookFilm+`"},"series":null}`))
	if a.code != http.StatusAccepted {
		t.Fatalf("a null foreign key refused the payload: %d %s", a.code, a.raw)
	}
	h.assertQueued(t, h.want(t, hookFilm))

	// A Test is the arr's "does this connection work" button. Pointed at the wrong
	// endpoint it does not, and the one status that makes the button say so is a non-2xx.
	t.Run("a Test from the other arr fails the Test", func(t *testing.T) {
		h := newWebhookHarness(t, hookTok)
		for app, fixture := range map[string]string{"radarr": "sonarr-test.json", "sonarr": "radarr-test.json"} {
			before := h.skipRecords()
			a := h.post(t, app, hookFixture(t, fixture))
			h.assertQueuedNothing(t, a, http.StatusBadRequest, "wrong-app-shape", before)
			if a.body.EventType != "Test" {
				t.Errorf("event_type = %q, want Test", a.body.EventType)
			}
		}
	})
}

// ---- the states POST /api/scan refuses in ---------------------------------------

// Paused feeds no new file to the workers, exactly as POST /api/scan's pause does. The
// answer is a 2xx: a pause is the operator's standing decision, and an arr that got a
// non-2xx for every import during one would stop sending.
func TestWebhook_PausedQueuesNothingAndAnswers200(t *testing.T) {
	h := newWebhookHarness(t, hookTok)
	h.ctrl.Pause()
	for app, fixture := range map[string]string{
		"sonarr": "sonarr-download-importcomplete.json", "radarr": "radarr-download-moviefile.json",
	} {
		before := h.skipRecords()
		a := h.post(t, app, hookFixture(t, fixture))
		h.assertQueuedNothing(t, a, http.StatusOK, "paused", before)
		if !a.body.Retryable || len(a.body.Results) != 0 || a.body.EventType != "Download" {
			t.Errorf("the paused answer is not the documented one: %s", a.raw)
		}
	}
	h.ctrl.Resume()
	if a := h.post(t, "radarr", hookFixture(t, "radarr-download-moviefile.json")); a.code != http.StatusAccepted {
		t.Fatalf("after resume: status %d: %s", a.code, a.raw)
	}
	h.assertQueued(t, h.want(t, hookFilm))
}

// A full queue is the one refusal that is holdfast's own failure to take a file, and it is
// answered as POST /api/scan answers it: 503, retryable, naming which paths were not taken.
func TestWebhook_QueueFullAnswers503AndNamesThePathsNotTaken(t *testing.T) {
	h := newWebhookHarness(t, hookTok)
	small := h.eng.NewSubmissions(1, 1)
	h.rec = &recordingSubs{Submissions: small}
	h.srv.SetSubmissions(h.rec)

	a := h.post(t, "sonarr", hookFixture(t, "sonarr-download-importcomplete.json"))
	if a.code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", a.code, a.raw)
	}
	if a.body.Rule != "submission-queue-full" || !a.body.Retryable || a.body.Accepted != 1 || a.body.Rejected != 2 {
		t.Errorf("rule, retryable, accepted, rejected = %q, %v, %d, %d, want submission-queue-full, true, 1, 2",
			a.body.Rule, a.body.Retryable, a.body.Accepted, a.body.Rejected)
	}
	if !strings.Contains(a.body.Reason, "capacity 1") {
		t.Errorf("the reason does not name the capacity: %s", a.body.Reason)
	}
	if got := h.rec.queued(); len(got) != 1 || got[0] != h.want(t, hookEp1) {
		t.Errorf("the queue was handed %q, want only the first file", got)
	}
	if small.Pending() != 1 {
		t.Errorf("%d waiting, want 1", small.Pending())
	}
	if len(a.body.Results) != 3 || !a.body.Results[0].Accepted {
		t.Fatalf("results = %+v", a.body.Results)
	}
	for _, res := range a.body.Results[1:] {
		if res.Accepted || res.Rule != "submission-queue-full" || !res.Retryable || res.Resolved != "" {
			t.Errorf("a path the queue could not take is not reported as one: %+v", res)
		}
	}
	// One file WAS queued, so this is not the "queued nothing" record.
	logs := h.logs.String()
	if !strings.Contains(logs, `level=WARN msg="webhook not fully queued" app=sonarr event_type=Download rule=submission-queue-full accepted=1 rejected=2`) {
		t.Errorf("the partial-queue record is missing:\n%s", logs)
	}
	if h.skipRecords() != 0 {
		t.Errorf("a partly queued event logged a skip record:\n%s", logs)
	}
}

func TestWebhook_NotWiredAnswers503(t *testing.T) {
	h := newWebhookHarness(t, hookTok)
	h.srv.subs = nil
	a := h.post(t, "sonarr", hookFixture(t, "sonarr-download-episodefile.json"))
	if a.code != http.StatusServiceUnavailable || a.body.Rule != "targeted-scanning-not-wired" || a.body.Reason == "" {
		t.Fatalf("status, rule = %d, %q, want 503, targeted-scanning-not-wired: %s", a.code, a.body.Rule, a.raw)
	}
	if a.body.App != "sonarr" || a.body.Retryable || a.body.Results == nil || len(a.body.Results) != 0 {
		t.Errorf("the refusal is not the documented one: %s", a.raw)
	}
	if h.skipRecords() != 1 {
		t.Errorf("want one skip record:\n%s", h.logs.String())
	}
	if n := h.scanHarness.subs.Pending(); n != 0 {
		t.Errorf("%d path(s) reached a queue the server was not wired to", n)
	}
}

// Sonarr sends BOTH Download shapes for one import when both triggers are ticked. A file
// named twice in one event is queued once.
func TestWebhook_FileNamedTwiceInOneEventIsQueuedOnce(t *testing.T) {
	h := newWebhookHarness(t, hookTok)
	payload := `{"eventType":"Download","episodeFile":{"path":"/` + hookEp1 + `"},` +
		`"episodeFiles":[{"path":"/` + hookEp1 + `"},{"path":"/` + hookEp2 + `"},{}]}`
	a := h.post(t, "sonarr", []byte(payload))
	if a.code != http.StatusAccepted || a.body.Accepted != 2 || a.body.Rejected != 2 {
		t.Fatalf("status, accepted, rejected = %d, %d, %d, want 202, 2, 2: %s", a.code, a.body.Accepted, a.body.Rejected, a.raw)
	}
	h.assertQueued(t, h.want(t, hookEp1), h.want(t, hookEp2))
	// The report is in payload order: the single file, then the list, with the entry that
	// has no path accounted for where it stood.
	var rules []string
	for _, res := range a.body.Results {
		rules = append(rules, res.Rule)
	}
	if fmt.Sprint(rules) != fmt.Sprint([]string{"", "duplicate-in-request", "", "no-file-path"}) {
		t.Errorf("the per-file rules are %q", rules)
	}
	last := a.body.Results[3]
	if last.Path != "" || last.Mapped != "" || last.Accepted || last.Detail == "" {
		t.Errorf("the entry with no path is reported as %+v", last)
	}
	if a.body.Results[2].Path != "/"+hookEp2 || a.body.Results[2].Resolved != h.want(t, hookEp2) {
		t.Errorf("a result after the duplicate is misaligned: %+v", a.body.Results[2])
	}
}

// ---- the surface -----------------------------------------------------------------

// Every status the intake answers with is declared in the surface document, for both arrs
// and both methods, and each live answer validates against the shape declared for it.
func TestWebhook_AnswersMatchTheSurfaceDocument(t *testing.T) {
	h := newWebhookHarness(t, hookTok)
	doc, err := h.srv.Surface()
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	declared := map[string][]int{}
	for _, ep := range doc.Endpoints {
		if strings.HasPrefix(ep.Path, WebhookPathPrefix) {
			var codes []int
			for _, r := range ep.Responses {
				codes = append(codes, r.Status)
			}
			declared[ep.Method+" "+ep.Path] = codes
		}
	}
	wantCodes := fmt.Sprint([]int{200, 202, 400, 401, 403, 405, 413, 503})
	for _, key := range []string{
		"POST /api/webhook/sonarr", "PUT /api/webhook/sonarr", "POST /api/webhook/radarr", "PUT /api/webhook/radarr",
	} {
		if got := fmt.Sprint(declared[key]); got != wantCodes {
			t.Errorf("%s declares %s, want %s", key, got, wantCodes)
		}
	}
	if len(declared) != 4 {
		t.Errorf("the document declares %d webhook endpoints, want 4: %v", len(declared), declared)
	}

	check := func(method, app string, a hookAnswer, want int) {
		t.Helper()
		if a.code != want {
			t.Fatalf("%s %s: status %d, want %d: %s", method, app, a.code, want, a.raw)
		}
		bad, err := doc.ValidateResponse(method, WebhookPathPrefix+app, a.code, []byte(a.raw))
		if err != nil {
			t.Fatalf("%s %s %d: %v", method, app, a.code, err)
		}
		if len(bad) != 0 {
			t.Errorf("%s %s %d does not match the document: %v", method, app, a.code, bad)
		}
	}
	check(http.MethodPost, "sonarr", h.post(t, "sonarr", hookFixture(t, "sonarr-download-episodefile.json")), 202)
	check(http.MethodPut, "radarr", h.send(t, http.MethodPut, "radarr", hookFixture(t, "radarr-download-moviefile.json"), bearer(hookTok)), 202)
	check(http.MethodPost, "sonarr", h.post(t, "sonarr", hookFixture(t, "sonarr-test.json")), 200)
	check(http.MethodPost, "radarr", h.post(t, "radarr", []byte(`{"eventType":"Download","movieFile":{}}`)), 200)
	check(http.MethodPost, "radarr", h.post(t, "radarr", []byte(`{`)), 400)
	check(http.MethodPut, "sonarr", h.send(t, http.MethodPut, "sonarr",
		[]byte(`{"pad":"`+strings.Repeat("x", MaxScanBodyBytes)+`"}`), bearer(hookTok)), 413)
	h.srv.subs = nil
	check(http.MethodPost, "sonarr", h.post(t, "sonarr", hookFixture(t, "sonarr-download-episodefile.json")), 503)
}
