package main

// The media-server clients, end to end (S0179 and decision T28): a REAL swap through the real
// `run` command over the smallest fixture this suite has, against httptest fakes of Radarr,
// Sonarr and Plex. Nothing here contacts a real service.
//
// What the fakes prove is the shape of each request and what holdfast does around it. What
// the real services then do is S0179 AC-18, which only the owner's live check can grade.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/mediaclient"
	"github.com/NSchatz/holdfast/internal/store"
)

// The resolved credentials the fixtures configure. None may reach any stream or record.
const (
	radarrKeySentinel = "RESOLVED-RADARR-KEY-MUST-NEVER-BE-EMITTED"
	sonarrKeySentinel = "RESOLVED-SONARR-KEY-MUST-NEVER-BE-EMITTED"
	plexTokenSentinel = "RESOLVED-PLEX-TOKEN-MUST-NEVER-BE-EMITTED"
)

func assertNoMediaCredential(t *testing.T, where, text string) {
	t.Helper()
	for key, value := range map[string]string{
		"radarr_api_key": radarrKeySentinel, "sonarr_api_key": sonarrKeySentinel, "plex_token": plexTokenSentinel,
	} {
		if strings.Contains(text, value) {
			t.Errorf("%s LEAKED the resolved value of %s:\n%s", where, key, text)
		}
	}
}

// mediaRequest is one request a fake service received.
type mediaRequest struct {
	Method, Path, RawQuery, Body string
	Header                       http.Header
}

// mediaFake is one fake service: it records every request and answers from its handler.
type mediaFake struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []mediaRequest
}

func newMediaFake(t *testing.T, handler func(w http.ResponseWriter, r mediaRequest, hang <-chan struct{})) *mediaFake {
	t.Helper()
	f := &mediaFake{}
	release := make(chan struct{})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := mediaRequest{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Body: string(body), Header: r.Header.Clone()}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.mu.Unlock()
		handler(w, req, release)
	}))
	t.Cleanup(func() {
		close(release)
		f.srv.Close()
	})
	return f
}

func (f *mediaFake) seen() []mediaRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mediaRequest(nil), f.requests...)
}

// count is how many requests arrived with that method and path.
func (f *mediaFake) count(method, path string) int {
	n := 0
	for _, r := range f.seen() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

// writes is every request that is not a GET.
func (f *mediaFake) writes() []mediaRequest {
	var out []mediaRequest
	for _, r := range f.seen() {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

// The library as each fake service sees it. The fixture's one source sits directly in the
// library root, so its directory is the root, mapped per target.
const (
	radarrDir = "/movies/Synthetic Film"
	sonarrDir = "/tv/Synthetic Show/Season 1"
	plexDir   = "/data/movies/Synthetic Film"
)

// fakeArr answers the list endpoint with list and accepts every command.
func fakeArr(t *testing.T, listPath, list string) *mediaFake {
	t.Helper()
	return newMediaFake(t, func(w http.ResponseWriter, r mediaRequest, _ <-chan struct{}) {
		switch {
		case r.Method == http.MethodGet && r.Path == listPath:
			_, _ = io.WriteString(w, list)
		case r.Method == http.MethodPost && r.Path == "/api/v3/command":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id": 1}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// fakePlex answers the section list, accepts every refresh, and answers /status/sessions
// from playing(): the files being played, in Plex's own view.
func fakePlex(t *testing.T, playing func() []string) *mediaFake {
	t.Helper()
	return newMediaFake(t, func(w http.ResponseWriter, r mediaRequest, _ <-chan struct{}) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/library/sections/all":
			_, _ = io.WriteString(w, `{"MediaContainer": {"size": 2, "Directory": [
			  {"key": "3", "type": "movie", "title": "Movies", "Location": [{"id": 1, "path": "/data/movies"}]},
			  {"key": "4", "type": "show", "title": "Shows", "Location": [{"id": 2, "path": "/data/tv"}]}]}}`)
		case r.Method == http.MethodGet && r.Path == "/status/sessions":
			_, _ = io.WriteString(w, plexSessionsJSON(playing()))
		case r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/refresh"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// plexSessionsJSON is `GET /status/sessions` in the documented shape, for these files.
func plexSessionsJSON(files []string) string {
	type part struct {
		File string `json:"file"`
	}
	type media struct {
		Part []part `json:"Part"`
	}
	type item struct {
		Type    string         `json:"type"`
		Media   []media        `json:"Media"`
		Player  map[string]any `json:"Player"`
		Session map[string]any `json:"Session"`
	}
	var items []item
	for _, f := range files {
		items = append(items, item{Type: "movie", Media: []media{{Part: []part{{File: f}}}},
			Player: map[string]any{"state": "playing"}, Session: map[string]any{"id": "synthetic"}})
	}
	b, _ := json.Marshal(map[string]any{"MediaContainer": map[string]any{"size": len(items), "Metadata": items}})
	return string(b)
}

func nothingPlaying() []string { return nil }

// credentialFile writes one resolved credential to a file and returns its reference.
func credentialFile(t *testing.T, name, value string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(value+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	return "file:" + p
}

// The configuration of each target against an address. lib is filled in by mediaLibrary.
func radarrConfig(t *testing.T, addr string) string {
	return "radarr_url: " + addr + "\nradarr_api_key: " + credentialFile(t, "radarr", radarrKeySentinel) +
		"\nradarr_path_map:\n  - {from: LIB, to: \"" + radarrDir + "\"}\n"
}

func sonarrConfig(t *testing.T, addr string) string {
	return "sonarr_url: " + addr + "\nsonarr_api_key: " + credentialFile(t, "sonarr", sonarrKeySentinel) +
		"\nsonarr_path_map:\n  - {from: LIB, to: \"" + sonarrDir + "\"}\n"
}

func plexConfig(t *testing.T, addr string) string {
	return "plex_url: " + addr + "\nplex_token: " + credentialFile(t, "plex", plexTokenSentinel) +
		"\nplex_path_map:\n  - {from: LIB, to: \"" + plexDir + "\"}\n"
}

// mediaLibrary is the smallest swapping fixture (preflightLibrary, the perceptual gate off:
// it is a second full decode proven by its own suite) with extra appended, every LIB in
// extra replaced by the library root.
func mediaLibrary(t *testing.T, extra string) (cfgPath, lib, state, src string) {
	t.Helper()
	cfgPath, lib, state, src = preflightLibrary(t, "vmaf_enable: false\n")
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, append(body, []byte(strings.ReplaceAll(extra, "LIB", lib))...), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, lib, state, src
}

// runCaptured runs `holdfast run` in-process and returns its exit code with everything it
// wrote: stdout, the stderr it was handed, and the process stderr its logger writes to.
func runCaptured(t *testing.T, cfgPath string) (code int, output string) {
	t.Helper()
	var out, errOut string
	logs := captureStderr(t, func() { code, out, errOut = cli(t, "run", "--config", cfgPath) })
	return code, out + errOut + logs
}

// jobFor returns the ledger's row for path, and whether there is one.
func jobFor(t *testing.T, state, path string) (store.Job, bool) {
	t.Helper()
	st := openStore(t, state)
	defer func() { _ = st.Close() }()
	rows, err := st.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, j := range rows {
		if j.Path == path {
			return j, true
		}
	}
	return store.Job{}, false
}

func requireSwapped(t *testing.T, state, src, before string) {
	t.Helper()
	if got := sha256File(t, src); got == before {
		t.Fatal("the swap did not happen, so the fixture proves nothing")
	}
	if j, ok := jobFor(t, state, src); !ok || j.Status != store.Done {
		t.Fatalf("the job did not reach done: %+v (found %v)", j, ok)
	}
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

// shortMediaIntervals shortens the clients' intervals for one test.
func shortMediaIntervals(t *testing.T, drain, ttl, poll time.Duration) {
	t.Helper()
	d, c, p := mediaDrainBound, mediaHoldCacheTTL, mediaHoldPoll
	mediaDrainBound, mediaHoldCacheTTL, mediaHoldPoll = drain, ttl, poll
	t.Cleanup(func() { mediaDrainBound, mediaHoldCacheTTL, mediaHoldPoll = d, c, p })
}

// TestPostSwapHook_AC1_WithNoTargetConfiguredNothingIsSent is S0179 AC-1: fake Radarr, Sonarr
// and Plex servers listen throughout a `run` that swaps a file under a configuration naming
// none of them; they receive zero requests, the job reaches done, and nothing about the
// clients is logged.
func TestPostSwapHook_AC1_WithNoTargetConfiguredNothingIsSent(t *testing.T) {
	radarr := fakeArr(t, "/api/v3/movie", `[]`)
	sonarr := fakeArr(t, "/api/v3/series", `[]`)
	plex := fakePlex(t, nothingPlaying)

	cfgPath, _, state, src := mediaLibrary(t, "")
	before := sha256File(t, src)
	code, output := runCaptured(t, cfgPath)
	if code != 0 {
		t.Fatalf("run exited %d:\n%s", code, output)
	}
	requireSwapped(t, state, src, before)
	for name, f := range map[string]*mediaFake{"radarr": radarr, "sonarr": sonarr, "plex": plex} {
		if got := f.seen(); len(got) != 0 {
			t.Errorf("%s received %d request(s) from a configuration that does not name it: %+v", name, len(got), got)
		}
	}
	for _, said := range []string{"media-server clients", "post-swap rescan", "play hold"} {
		if strings.Contains(output, said) {
			t.Errorf("a configuration with no target logged %q:\n%s", said, output)
		}
	}
}

// TestMediaClients_AfterASwapPlexGetsAPartialRefreshOfTheSectionPath: a real swap through the
// real `run`, and the fake Plex records exactly one POST /library/sections/<id>/refresh whose
// `path` is the swapped file's parent directory as Plex sees it, on the section whose
// location owns it - and no whole-section or all-sections refresh. The token travels in its
// header and reaches no stream.
func TestMediaClients_AfterASwapPlexGetsAPartialRefreshOfTheSectionPath(t *testing.T) {
	plex := fakePlex(t, nothingPlaying)
	cfgPath, _, state, src := mediaLibrary(t, plexConfig(t, plex.srv.URL))
	before := sha256File(t, src)

	code, output := runCaptured(t, cfgPath)
	if code != 0 {
		t.Fatalf("run exited %d:\n%s", code, output)
	}
	requireSwapped(t, state, src, before)

	writes := plex.writes()
	if len(writes) != 1 {
		t.Fatalf("plex received %d write request(s), want exactly one refresh: %+v", len(writes), writes)
	}
	if writes[0].Method != http.MethodPost || writes[0].Path != "/library/sections/3/refresh" {
		t.Errorf("the refresh went to %s %s, want POST /library/sections/3/refresh", writes[0].Method, writes[0].Path)
	}
	q, err := url.ParseQuery(writes[0].RawQuery)
	if err != nil {
		t.Fatal(err)
	}
	if got := q["path"]; len(got) != 1 || got[0] != plexDir || len(q) != 1 {
		t.Errorf("the refresh query is %v, want only path=%q (the mapped parent directory)", q, plexDir)
	}
	for _, r := range plex.seen() {
		if strings.HasSuffix(r.Path, "/refresh") && (r.RawQuery == "" || strings.Contains(r.Path, "/all/") ||
			r.Path == "/library/sections/refresh") {
			t.Errorf("a whole-section or all-sections refresh was requested: %s %s?%s", r.Method, r.Path, r.RawQuery)
		}
		if r.Header.Get("X-Plex-Token") != plexTokenSentinel {
			t.Errorf("%s %s did not carry the token in X-Plex-Token", r.Method, r.Path)
		}
		if strings.Contains(r.RawQuery, plexTokenSentinel) || strings.Contains(r.RawQuery, "X-Plex-Token") {
			t.Errorf("%s %s carries the token in its URL", r.Method, r.Path)
		}
	}
	if got := plex.count(http.MethodGet, "/library/sections/all"); got != 1 {
		t.Errorf("the section list was read %d time(s), want 1", got)
	}
	if !strings.Contains(output, "post-swap rescan requested") || !strings.Contains(output, "target=plex") {
		t.Errorf("the run does not record that the partial scan was requested:\n%s", output)
	}
	assertNoMediaCredential(t, "the run's output", output)
}

// TestMediaClients_AfterASwapSonarrGetsRescanSeriesAndRadarrGetsRescanMovie: a real swap
// through the real `run`, and the fake Sonarr and the fake Radarr each record exactly one
// command, RescanSeries and RescanMovie, naming the one owner of the file's directory by id.
func TestMediaClients_AfterASwapSonarrGetsRescanSeriesAndRadarrGetsRescanMovie(t *testing.T) {
	radarr := fakeArr(t, "/api/v3/movie", `[
	  {"id": 41, "title": "Synthetic Film", "path": "/movies/Synthetic Film"},
	  {"id": 42, "title": "Synthetic Film 2", "path": "/movies/Synthetic Film 2"},
	  {"id": 43, "title": "Synthetic", "path": "/movies/Synthetic"}]`)
	sonarr := fakeArr(t, "/api/v3/series", `[
	  {"id": 51, "title": "Synthetic Show 2", "path": "/tv/Synthetic Show 2"},
	  {"id": 52, "title": "Synthetic Show", "path": "/tv/Synthetic Show"}]`)
	cfgPath, _, state, src := mediaLibrary(t, radarrConfig(t, radarr.srv.URL)+sonarrConfig(t, sonarr.srv.URL))
	before := sha256File(t, src)

	code, output := runCaptured(t, cfgPath)
	if code != 0 {
		t.Fatalf("run exited %d:\n%s", code, output)
	}
	requireSwapped(t, state, src, before)

	for _, tc := range []struct {
		name          string
		f             *mediaFake
		list, command string
		idField, key  string
		wantID        float64
	}{
		{"radarr", radarr, "/api/v3/movie", "RescanMovie", "movieId", radarrKeySentinel, 41},
		{"sonarr", sonarr, "/api/v3/series", "RescanSeries", "seriesId", sonarrKeySentinel, 52},
	} {
		writes := tc.f.writes()
		if len(writes) != 1 {
			t.Errorf("%s received %d command(s), want exactly 1: %+v", tc.name, len(writes), writes)
			continue
		}
		if writes[0].Method != http.MethodPost || writes[0].Path != "/api/v3/command" {
			t.Errorf("%s: the command went to %s %s", tc.name, writes[0].Method, writes[0].Path)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(writes[0].Body), &body); err != nil {
			t.Fatalf("%s: the command body %q is not JSON: %v", tc.name, writes[0].Body, err)
		}
		if body["name"] != tc.command || body[tc.idField] != tc.wantID || len(body) != 2 {
			t.Errorf("%s: the command body is %v, want {name: %s, %s: %v}", tc.name, body, tc.command, tc.idField, tc.wantID)
		}
		if got := tc.f.count(http.MethodGet, tc.list); got != 1 {
			t.Errorf("%s: the list was read %d time(s), want 1", tc.name, got)
		}
		for _, r := range tc.f.seen() {
			if r.Header.Get("X-Api-Key") != tc.key || r.RawQuery != "" {
				t.Errorf("%s: %s %s carried key header %q and query %q", tc.name, r.Method, r.Path,
					r.Header.Get("X-Api-Key"), r.RawQuery)
			}
		}
	}
	assertNoMediaCredential(t, "the run's output", output)
}

// TestPostSwapHook_AC6_AC11_AFailingTargetChangesNothingAboutTheSwap is S0179 AC-6 and
// AC-11, through a real `run` with the undo window open. Every way a target can fail - a
// refused connection, a 401, a 500, a body that does not parse, and no answer at all - leaves
// the job at the status the engine recorded, the replacement at its path byte-identical to
// what was swapped in, the retained original present and restorable by `holdfast restore`,
// and the exit code the same as the same run with no target configured. With a target that
// never answers, the run keeps trying for the drain bound, says how many requests were not
// delivered in one warn record, and exits with that same code.
func TestPostSwapHook_AC6_AC11_AFailingTargetChangesNothingAboutTheSwap(t *testing.T) {
	// The same run with no target configured: its exit code is the one every case must match.
	baseCfg, _, baseState, baseSrc := mediaLibrary(t, "undo_window_hours: 24\n")
	baseBefore := sha256File(t, baseSrc)
	baseline, baseOut := runCaptured(t, baseCfg)
	if baseline != 0 {
		t.Fatalf("the baseline run exited %d:\n%s", baseline, baseOut)
	}
	requireSwapped(t, baseState, baseSrc, baseBefore)

	refused := func(t *testing.T) string {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
		return srv.URL
	}
	answering := func(status int, body string) func(t *testing.T) string {
		return func(t *testing.T) string {
			return newMediaFake(t, func(w http.ResponseWriter, _ mediaRequest, _ <-chan struct{}) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			}).srv.URL
		}
	}
	// A Plex that answers what is being played and never answers the section list: the
	// connection is accepted and nothing comes back.
	hangingPlex := func(t *testing.T) string {
		return newMediaFake(t, func(w http.ResponseWriter, r mediaRequest, hang <-chan struct{}) {
			if r.Path == "/status/sessions" {
				_, _ = io.WriteString(w, plexSessionsJSON(nil))
				return
			}
			<-hang
		}).srv.URL
	}

	cases := []struct {
		name                 string
		radarr, sonarr, plex func(t *testing.T) string
		wantFailures         map[string]string // target -> the failure its warn record names
		wantUndelivered      bool
	}{
		{"refused, unauthorized and never answering", refused, answering(http.StatusUnauthorized, ""), hangingPlex,
			map[string]string{"radarr": mediaclient.ClassUnreachable, "sonarr": mediaclient.ClassUnauthorized + " 401"}, true},
		{"unparseable, a server error and unparseable",
			answering(http.StatusOK, "<html>a login page</html>"), answering(http.StatusInternalServerError, "boom"),
			answering(http.StatusOK, `{"not": "plex"}`),
			map[string]string{"radarr": mediaclient.ClassUnparseable, "sonarr": mediaclient.ClassStatus + " 500",
				"plex": mediaclient.ClassUnparseable}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shortMediaIntervals(t, 2*time.Second, mediaHoldCacheTTL, mediaHoldPoll)
			cfgPath, _, state, src := mediaLibrary(t, "undo_window_hours: 24\n"+
				radarrConfig(t, tc.radarr(t))+sonarrConfig(t, tc.sonarr(t))+plexConfig(t, tc.plex(t)))
			before := sha256File(t, src)

			began := time.Now()
			code, output := runCaptured(t, cfgPath)
			if code != baseline {
				t.Fatalf("run exited %d with failing targets, want %d as with none configured:\n%s", code, baseline, output)
			}
			if took := time.Since(began); took > 90*time.Second {
				t.Errorf("the run took %s: the drain did not stop at its bound", took)
			}

			// The job kept the status the engine recorded, and the replacement is in place.
			requireSwapped(t, state, src, before)
			replacement := sha256File(t, src)

			// One warn record per failing target, naming it, the file and the class; none at
			// error level about a rescan.
			for target, class := range tc.wantFailures {
				n := 0
				for _, line := range strings.Split(output, "\n") {
					if strings.Contains(line, "post-swap rescan failed") && strings.Contains(line, "target="+target) {
						n++
						if !strings.Contains(line, "level=WARN") || !strings.Contains(line, src) ||
							!strings.Contains(line, `failure=`+quoteIfSpaced(class)) {
							t.Errorf("the %s failure record is not a warn naming the file and %q: %s", target, class, line)
						}
					}
				}
				if n != 1 {
					t.Errorf("want exactly one failure record for %s, got %d:\n%s", target, n, output)
				}
			}
			for _, line := range strings.Split(output, "\n") {
				if strings.Contains(line, "level=ERROR") {
					t.Errorf("a failing target produced an error-level record: %s", line)
				}
			}

			// AC-11: the drain's one record, only where something was left undelivered.
			drainRecords := strings.Count(output, "post-swap rescans not delivered")
			if tc.wantUndelivered {
				if drainRecords != 1 || !strings.Contains(output, "not_delivered=1") || !strings.Contains(output, "drain_bound=2s") {
					t.Errorf("want one warn record with not_delivered=1 and the bound, got %d:\n%s", drainRecords, output)
				}
			} else if drainRecords != 0 {
				t.Errorf("a drain that delivered everything said otherwise:\n%s", output)
			}
			assertNoMediaCredential(t, "the run's output", output)

			// The retained original is present, and `holdfast restore` puts it back.
			st := openStore(t, state)
			r := onlyRetention(t, st)
			_ = st.Close()
			if got := sha256File(t, r.RetainedPath); got != before {
				t.Fatalf("the retained original is not the pre-swap source")
			}
			if got := sha256File(t, src); got != replacement {
				t.Error("the replacement changed after the swap")
			}
			if code, _, errOut := cli(t, "restore", "--config", cfgPath, src); code != 0 {
				t.Fatalf("restore exited %d after a run with failing targets: %s", code, errOut)
			}
			if got := sha256File(t, src); got != before {
				t.Error("restore did not put the original back byte for byte")
			}
		})
	}
}

// quoteIfSpaced renders a value the way the text log handler does.
func quoteIfSpaced(v string) string {
	if strings.ContainsAny(v, " =\"") {
		return fmt.Sprintf("%q", v)
	}
	return v
}

// TestMediaClients_AFileBeingPlayedInPlexIsHeld is the Plex play hold through the real `run`
// (decision T28). At the door: while the fake Plex reports the file as playing, the source
// is untouched - bytes and inode - no swap happened and no ledger row was written; once the
// fake stops reporting it, the same file is processed and swapped. Before the swap: a
// session that starts after the encode began holds the rename for as long as it plays, and
// the rename happens once it stops.
func TestMediaClients_AFileBeingPlayedInPlexIsHeld(t *testing.T) {
	t.Run("at the door: not started, no row, offered again", func(t *testing.T) {
		shortMediaIntervals(t, mediaDrainBound, time.Millisecond, 50*time.Millisecond)
		var mu sync.Mutex
		var playing []string
		plex := fakePlex(t, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return playing
		})
		cfgPath, _, state, src := mediaLibrary(t, plexConfig(t, plex.srv.URL))
		before, beforeInode := sha256File(t, src), inode(t, src)
		// The session names the file as PLEX sees it; the hold maps it back.
		mu.Lock()
		playing = []string{plexDir + "/" + filepath.Base(src)}
		mu.Unlock()

		// A run that holds the file at the door returns as soon as its scan is over. One that
		// started the file instead is waiting in front of the swap for as long as it plays, so
		// the wait here is bounded and the playback is stopped before the case fails.
		type result struct {
			code   int
			output string
		}
		done := make(chan result, 1)
		go func() {
			code, output := runCaptured(t, cfgPath)
			done <- result{code, output}
		}()
		var code int
		var output string
		select {
		case r := <-done:
			code, output = r.code, r.output
		case <-time.After(45 * time.Second):
			mu.Lock()
			playing = nil
			mu.Unlock()
			r := <-done
			t.Fatalf("the run did not return while the file was being played: the file was STARTED rather "+
				"than held at the door.\n%s", r.output)
		}
		if code != 0 {
			t.Fatalf("run exited %d with the file being played:\n%s", code, output)
		}
		if got := sha256File(t, src); got != before {
			t.Fatal("the source's bytes changed while it was being played")
		}
		if got := inode(t, src); got != beforeInode {
			t.Fatal("the source was replaced (its inode moved) while it was being played")
		}
		if j, ok := jobFor(t, state, src); ok {
			t.Fatalf("a ledger row was written for a file that was only held: %+v", j)
		}
		if entries, err := os.ReadDir(filepath.Dir(src)); err != nil || len(entries) != 1 {
			t.Errorf("the library holds %d entries after a held run, want only the source (%v)", len(entries), err)
		}
		if len(plex.writes()) != 0 {
			t.Errorf("a refresh was requested for a file that was never swapped: %+v", plex.writes())
		}
		held := 0
		for _, line := range strings.Split(output, "\n") {
			if strings.Contains(line, "held (not started") {
				held++
				if !strings.Contains(line, "level=INFO") || !strings.Contains(line, src) ||
					!strings.Contains(line, "being played in Plex") {
					t.Errorf("the hold record is not an info naming the path and the reason: %s", line)
				}
			}
		}
		if held != 1 {
			t.Errorf("want exactly one hold record, got %d:\n%s", held, output)
		}

		// The playback stops: the same file is offered again and swapped.
		mu.Lock()
		playing = nil
		mu.Unlock()
		code, output = runCaptured(t, cfgPath)
		if code != 0 {
			t.Fatalf("the run after the playback stopped exited %d:\n%s", code, output)
		}
		requireSwapped(t, state, src, before)
		if got := len(plex.writes()); got != 1 {
			t.Errorf("after the swap plex received %d refresh(es), want 1", got)
		}
	})

	t.Run("before the swap: the rename waits while it plays", func(t *testing.T) {
		shortMediaIntervals(t, mediaDrainBound, time.Millisecond, 50*time.Millisecond)
		// The first question (the door) is answered "nothing playing", so the job starts and
		// encodes. Every later question is the one in front of the swap, and from then on the
		// file is being played until the test says otherwise.
		var mu sync.Mutex
		asked, stopped := 0, false
		var file string
		plex := fakePlex(t, func() []string {
			mu.Lock()
			defer mu.Unlock()
			asked++
			if asked == 1 || stopped {
				return nil
			}
			return []string{file}
		})
		questions := func() int {
			mu.Lock()
			defer mu.Unlock()
			return asked
		}
		cfgPath, _, state, src := mediaLibrary(t, plexConfig(t, plex.srv.URL))
		before, beforeInode := sha256File(t, src), inode(t, src)
		mu.Lock()
		file = plexDir + "/" + filepath.Base(src)
		mu.Unlock()

		type result struct {
			code   int
			output string
		}
		done := make(chan result, 1)
		go func() {
			code, output := runCaptured(t, cfgPath)
			done <- result{code, output}
		}()

		// Wait until the swap has asked several times: the encode is over, every gate has
		// passed, and the rename is being held.
		deadline := time.Now().Add(3 * time.Minute)
		for questions() < 6 {
			select {
			case r := <-done:
				t.Fatalf("the run finished (exit %d) while the file was being played: the swap did not wait.\n%s", r.code, r.output)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("the swap never reached its hold")
			}
			time.Sleep(20 * time.Millisecond)
		}
		// Held: the source is still the source, and nothing was announced.
		if got := sha256File(t, src); got != before {
			t.Fatal("the source's bytes changed while it was being played")
		}
		if got := inode(t, src); got != beforeInode {
			t.Fatal("the rename happened (the source's inode moved) while the file was being played")
		}
		if len(plex.writes()) != 0 {
			t.Errorf("a refresh was requested before the swap: %+v", plex.writes())
		}
		select {
		case r := <-done:
			t.Fatalf("the run finished (exit %d) while the file was being played.\n%s", r.code, r.output)
		case <-time.After(300 * time.Millisecond):
		}

		// The playback stops: the swap proceeds, unchanged.
		mu.Lock()
		stopped = true
		mu.Unlock()
		var r result
		select {
		case r = <-done:
		case <-time.After(3 * time.Minute):
			t.Fatal("the swap did not proceed after the playback stopped")
		}
		if r.code != 0 {
			t.Fatalf("run exited %d:\n%s", r.code, r.output)
		}
		requireSwapped(t, state, src, before)
		if got := inode(t, src); got == beforeInode {
			t.Error("the source's inode did not move: nothing was renamed over it")
		}
		for _, want := range []string{"the swap waits until the file is no longer held", "the swap proceeds: the file is no longer held"} {
			if strings.Count(r.output, want) != 1 {
				t.Errorf("want exactly one %q record:\n%s", want, r.output)
			}
		}
		if got := len(plex.writes()); got != 1 {
			t.Errorf("after the swap plex received %d refresh(es), want 1", got)
		}
	})
}

// TestMediaClients_PlexUnreachableDoesNotHoldAndWarnsOnce is the fail-open rule (ledger D3)
// through the real `run`: with the configured Plex refusing every connection the file is not
// held - it is encoded and swapped - and the play hold says so in exactly one warn record,
// although it asked at the door and again in front of the swap.
func TestMediaClients_PlexUnreachableDoesNotHoldAndWarnsOnce(t *testing.T) {
	shortMediaIntervals(t, mediaDrainBound, time.Nanosecond, 50*time.Millisecond)
	dead := httptest.NewServer(http.NotFoundHandler())
	addr := dead.URL
	dead.Close()

	cfgPath, _, state, src := mediaLibrary(t, plexConfig(t, addr))
	before := sha256File(t, src)
	code, output := runCaptured(t, cfgPath)
	if code != 0 {
		t.Fatalf("run exited %d with Plex unreachable:\n%s", code, output)
	}
	requireSwapped(t, state, src, before)

	warns := 0
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "the Plex play hold could not ask Plex") {
			warns++
			if !strings.Contains(line, "level=WARN") || !strings.Contains(line, "fails open") ||
				!strings.Contains(line, "failure="+mediaclient.ClassUnreachable) {
				t.Errorf("the record is not a warn naming the failure and the fail-open rule: %s", line)
			}
		}
		if strings.Contains(line, `msg="held (`) {
			t.Errorf("a file was held with Plex unreachable: %s", line)
		}
	}
	if warns != 1 {
		t.Errorf("want exactly one play-hold warn record for the outage, got %d:\n%s", warns, output)
	}
	if strings.Contains(output, addr) {
		t.Errorf("the output carries the Plex address of a failed request:\n%s", output)
	}
	assertNoMediaCredential(t, "the run's output", output)
}

// TestPostSwapHook_AC14_NoCredentialInAnyStartupRecord is the start-up half of S0179 AC-14,
// proved at the command level as the spec verdict's binding advisory F4 requires: with all
// three targets configured and their credentials resolved, every record `validate`, `run`
// and `serve` write at DEBUG - start-up included - is captured, and none carries a resolved
// credential. The records still SAY which targets are on.
func TestPostSwapHook_AC14_NoCredentialInAnyStartupRecord(t *testing.T) {
	radarr := fakeArr(t, "/api/v3/movie", `[{"id": 41, "path": "/movies/Synthetic Film"}]`)
	sonarr := fakeArr(t, "/api/v3/series", `[{"id": 52, "path": "/tv/Synthetic Show"}]`)
	plex := fakePlex(t, nothingPlaying)
	cfgPath, _, state, src := mediaLibrary(t, "log_level: debug\nserver_addr: 127.0.0.1:0\n"+
		radarrConfig(t, radarr.srv.URL)+sonarrConfig(t, sonarr.srv.URL)+plexConfig(t, plex.srv.URL))
	before := sha256File(t, src)

	// validate: the configuration is accepted, and nothing it prints is a credential.
	var vout, verr bytes.Buffer
	if code := dispatch([]string{"validate", "--config", cfgPath}, &vout, &verr); code != 0 {
		t.Fatalf("validate exited %d: %s", code, verr.String())
	}
	assertNoMediaCredential(t, "validate", vout.String()+verr.String())

	// run, at debug: start-up, the swap, the three requests and the drain.
	code, output := runCaptured(t, cfgPath)
	if code != 0 {
		t.Fatalf("run exited %d:\n%s", code, output)
	}
	requireSwapped(t, state, src, before)
	assertNoMediaCredential(t, "run at debug", output)
	if !strings.Contains(output, "media-server clients") || !strings.Contains(output, "radarr=true") ||
		!strings.Contains(output, "sonarr=true") || !strings.Contains(output, "plex=true") ||
		!strings.Contains(output, "plex_play_hold=true") {
		t.Errorf("the start-up record does not say which targets are on:\n%s", output)
	}
	for name, f := range map[string]*mediaFake{"radarr": radarr, "sonarr": sonarr, "plex": plex} {
		if len(f.writes()) != 1 {
			t.Errorf("%s did not receive its request, so the run proves nothing about it: %+v", name, f.seen())
		}
	}

	// serve, at debug: every start-up and shutdown record of the daemon.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	logs := &syncWriter{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var serveErr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- runServer(ctx, cfg, log, &serveErr) }()
	deadline := time.Now().Add(60 * time.Second)
	for !strings.Contains(logs.String(), "serve listening") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("serve never started:\n%s\n%s", logs.String(), serveErr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("runServer exited %d: %s", code, serveErr.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatal("runServer did not shut down")
	}
	assertNoMediaCredential(t, "serve at debug", logs.String()+serveErr.String())
	if !strings.Contains(logs.String(), "media-server clients") || !strings.Contains(logs.String(), "plex=true") {
		t.Errorf("serve's start-up does not say which targets are on:\n%s", logs.String())
	}
}

// TestPostSwapHook_AC13_ValidateRunAndServeRefuseAMisconfiguredTargetBeforeAnyWork is S0179
// AC-13 through the real CLI: a half-configured target, an address that is not an absolute
// http or https URL, and a path-map entry with a side that is not absolute each make
// `validate`, `run` and `serve` exit non-zero naming the offending key, before any file is
// probed - the source is untouched and the job store is never opened.
func TestPostSwapHook_AC13_ValidateRunAndServeRefuseAMisconfiguredTargetBeforeAnyWork(t *testing.T) {
	cases := []struct{ name, extra, key string }{
		{"an address without a credential", "plex_url: http://plex.invalid:32400\n", "plex_token"},
		{"a credential without an address", "sonarr_api_key: file:/run/secrets/sonarr\n", "sonarr_url"},
		{"an address that is not a URL", "radarr_url: radarr.invalid:7878\nradarr_api_key: file:/run/secrets/radarr\n", "radarr_url"},
		{"an address that is not http", "plex_url: ftp://plex.invalid\nplex_token: file:/run/secrets/plex\n", "plex_url"},
		{"a relative path-map side", "sonarr_path_map:\n  - {from: media/tv, to: /tv}\n", "sonarr_path_map[0].from"},
		{"a relative path-map target", "plex_path_map:\n  - {from: /media, to: data}\n", "plex_path_map[0].to"},
		{"a misspelled path-map side", "radarr_path_map:\n  - {from: /media, too: /movies}\n", "radarr_path_map[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, lib, state, src := mediaLibrary(t, tc.extra)
			before := sha256File(t, src)
			for _, cmd := range []string{"validate", "run", "serve"} {
				code, out, errOut := cli(t, cmd, "--config", cfgPath)
				if code == 0 {
					t.Errorf("%s ACCEPTED %s", cmd, tc.name)
					continue
				}
				if !strings.Contains(out+errOut, tc.key) {
					t.Errorf("%s did not name %s:\n%s", cmd, tc.key, out+errOut)
				}
			}
			if got := sha256File(t, src); got != before {
				t.Error("the source was modified by a run that refused to start")
			}
			if _, err := os.Stat(filepath.Join(state, "jobs.db")); err == nil {
				t.Error("the job store was opened by a run that refused to start")
			}
			if entries, err := os.ReadDir(lib); err != nil || len(entries) != 1 {
				t.Errorf("the library holds %d entries, want only the untouched source (%v)", len(entries), err)
			}
		})
	}
}

// TestPostSwapHook_AC15_TheDeploymentDocumentStatesTheWarningAndTheFigures is S0179 AC-15:
// docs/post-swap-hook.md carries the TRaSH "x265 (HD)" warning - its -10000 default score,
// the release-title pattern, the {MediaInfo VideoCodec} naming token, what to check, and that
// holdfast changes no arr or Recyclarr setting - and the example configuration points to it.
// The figures the document states are the ones the build uses.
func TestPostSwapHook_AC15_TheDeploymentDocumentStatesTheWarningAndTheFigures(t *testing.T) {
	doc := string(readAll(t, filepath.Join("..", "..", "docs", "post-swap-hook.md")))
	for _, want := range []string{
		`"x265 (HD)"`, "-10000", `[xh][ ._-]?265|\bHEVC(\b|\d)`, "{MediaInfo VideoCodec}",
		"What to check before enabling an arr target",
		"holdfast changes no Radarr, Sonarr or Recyclarr setting",
		"The drain bound is 30 seconds", "10 second timeout per request", "a queue of 64 swaps",
		"asking again every 15 seconds", "reused for 2 seconds", ".holdfast-undo",
		"every 10 minutes", "reused for 60 seconds", "A flat Plex library is not refreshed",
		"a session left paused pins a worker",
		"must be an admin token", "It fails open", "awaiting the owner's ratification",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/post-swap-hook.md does not state %q", want)
		}
	}
	if mediaclient.DrainBound != 30*time.Second || mediaclient.RequestTimeout != 10*time.Second ||
		mediaclient.QueueSize != 64 || mediaclient.HoldCacheTTL != 2*time.Second ||
		mediaclient.HoldFailureTTL != 60*time.Second || engine.PlayHoldReminder != 10*time.Minute ||
		engine.DefaultPlayHoldPoll != 15*time.Second ||
		mediaDrainBound != mediaclient.DrainBound || mediaHoldCacheTTL != mediaclient.HoldCacheTTL {
		t.Errorf("the build's figures (drain %s, timeout %s, queue %d, cache %s) are not the ones the document states",
			mediaclient.DrainBound, mediaclient.RequestTimeout, mediaclient.QueueSize, mediaclient.HoldCacheTTL)
	}
	example := string(readAll(t, filepath.Join("..", "..", "config.example.yaml")))
	for _, want := range []string{"docs/post-swap-hook.md", "radarr_url", "radarr_api_key", "radarr_path_map",
		"sonarr_url", "sonarr_api_key", "sonarr_path_map", "plex_url", "plex_token", "plex_path_map"} {
		if !strings.Contains(example, want) {
			t.Errorf("config.example.yaml does not name %q", want)
		}
	}
}
