package mediaclient

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/store"
)

// The three resolved credentials every case configures. None may appear in any record.
const (
	radarrSecret = "RESOLVED-RADARR-KEY-MUST-NEVER-BE-LOGGED"
	sonarrSecret = "RESOLVED-SONARR-KEY-MUST-NEVER-BE-LOGGED"
	plexSecret   = "RESOLVED-PLEX-TOKEN-MUST-NEVER-BE-LOGGED"
)

// request is one request a fake received.
type request struct {
	Method, Path, RawQuery, Body string
	Header                       http.Header

	release <-chan struct{}
}

// hang blocks until the fake is being closed: a target that accepted and never answers.
func (r request) hang() { <-r.release }

// fake is an httptest server that records every request and answers from its handler.
type fake struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []request
	handler  func(w http.ResponseWriter, r request)
}

func newFake(t *testing.T, handler func(w http.ResponseWriter, r request)) *fake {
	t.Helper()
	release := make(chan struct{})
	f := &fake{t: t, handler: handler}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := request{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Body: string(body), Header: r.Header.Clone(), release: release}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		h := f.handler
		f.mu.Unlock()
		h(w, req)
	}))
	t.Cleanup(func() {
		close(release) // let every hung handler go, or Close would wait on it for ever
		f.srv.Close()
	})
	return f
}

// seen is every request so far.
func (f *fake) seen() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.requests...)
}

// where returns the requests with that method and path.
func (f *fake) where(method, path string) []request {
	var out []request
	for _, r := range f.seen() {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// posts returns every request that is not a GET: every request that could change something.
func (f *fake) posts() []request {
	var out []request
	for _, r := range f.seen() {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

// arrList answers the list endpoint with body and accepts every command.
func arrList(listPath, body string) func(w http.ResponseWriter, r request) {
	return func(w http.ResponseWriter, r request) {
		switch {
		case r.Method == http.MethodGet && r.Path == listPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		case r.Method == http.MethodPost && r.Path == "/api/v3/command":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id": 1, "name": "accepted"}`)
		default:
			http.NotFound(w, nil)
		}
	}
}

// A synthetic library as each service sees it. The shapes are the documented ones: an array
// of resources with `id` and `path`, and Plex's `MediaContainer.Directory[]`.
const (
	movieList = `[
	  {"id": 11, "title": "Synthetic Film", "path": "/movies/Film", "monitored": true},
	  {"id": 12, "title": "Synthetic Film 2", "path": "/movies/Film 2"},
	  {"id": 13, "title": "A Collection", "path": "/movies"}
	]`
	seriesList = `[
	  {"id": 21, "title": "Synthetic Show", "path": "/tv/Show", "seasons": [{"seasonNumber": 1}]},
	  {"id": 22, "title": "Synthetic Show 2", "path": "/tv/Show 2/"}
	]`
	sectionList = `{"MediaContainer": {"size": 3, "title1": "Plex Library", "Directory": [
	  {"key": "1", "type": "movie", "title": "Movies", "Location": [{"id": 1, "path": "/data/movies"}]},
	  {"key": "2", "type": "show", "title": "Shows", "Location": [{"id": 2, "path": "/data/tv"}, {"id": 3, "path": "/data/more tv"}]},
	  {"key": "7", "type": "movie", "title": "Everything", "Location": [{"id": 4, "path": "/data"}]}
	]}}`
)

// plexSections answers the section list with body and accepts every refresh.
func plexSectionsHandler(body string) func(w http.ResponseWriter, r request) {
	return func(w http.ResponseWriter, r request) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/library/sections/all":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/refresh"):
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, nil)
		}
	}
}

// The path maps every case uses: holdfast sees the library under /mnt/media.
var (
	radarrPaths = config.PathMap{{From: "/mnt/media/movies", To: "/movies"}}
	sonarrPaths = config.PathMap{{From: "/mnt/media/tv", To: "/tv"}}
	plexPaths   = config.PathMap{{From: "/mnt/media", To: "/data"}}
)

func radarrAt(f *fake) *Arr { return NewRadarr(f.srv.URL, secret.NewValue(radarrSecret), radarrPaths) }
func sonarrAt(f *fake) *Arr { return NewSonarr(f.srv.URL, secret.NewValue(sonarrSecret), sonarrPaths) }
func plexAt(f *fake) *Plex  { return NewPlex(f.srv.URL, secret.NewValue(plexSecret), plexPaths) }

// record is one log record, flattened.
type record struct {
	Level slog.Level
	Msg   string
	Attrs map[string]string
}

// text is everything the record says, for a search.
func (r record) text() string {
	var b strings.Builder
	b.WriteString(r.Msg)
	for k, v := range r.Attrs {
		b.WriteString(" " + k + "=" + v)
	}
	return b.String()
}

// recorder is a slog.Handler that keeps every record at every level.
type recorder struct {
	mu      sync.Mutex
	records []record
}

func (h *recorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *recorder) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *recorder) WithGroup(string) slog.Handler            { return h }
func (h *recorder) Handle(_ context.Context, r slog.Record) error {
	rec := record{Level: r.Level, Msg: r.Message, Attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *recorder) all() []record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]record(nil), h.records...)
}

// at returns the records at that level.
func (h *recorder) at(level slog.Level) []record {
	var out []record
	for _, r := range h.all() {
		if r.Level == level {
			out = append(out, r)
		}
	}
	return out
}

func newRecorder() (*recorder, *slog.Logger) {
	h := &recorder{}
	return h, slog.New(h)
}

// assertNoCredential fails if any record, at any level, carries a resolved credential.
func assertNoCredential(t *testing.T, h *recorder) {
	t.Helper()
	for _, r := range h.all() {
		for _, secret := range []string{radarrSecret, sonarrSecret, plexSecret} {
			if strings.Contains(r.text(), secret) {
				t.Errorf("a %s record carries a resolved credential: %s", r.Level, r.text())
			}
		}
	}
}

// run drives a hook over events the way the engine does and waits for every request to
// finish: it starts the hook, observes each event, and drains with a bound far longer than
// the work.
func run(t *testing.T, targets []Target, log *slog.Logger, events ...engine.Event) {
	t.Helper()
	h := NewHook(targets, log)
	h.Start()
	for _, ev := range events {
		h.Observe(ev)
	}
	if left := h.Drain(30 * time.Second); left != 0 {
		t.Fatalf("the drain left %d swap(s) undelivered", left)
	}
}

func swapped(path string) engine.Event {
	return engine.Event{Path: path, Status: store.Done, Outcome: &store.Outcome{}}
}

// shortTimeout shortens the per-request timeout for one test.
func shortTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := requestTimeout
	requestTimeout = d
	t.Cleanup(func() { requestTimeout = prev })
}
