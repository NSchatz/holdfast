package mediaclient

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/store"
)

// The two transitions after which the file at the path IS the replacement.
var replacedStatuses = []store.Status{store.Done, store.AppliedDespiteError}

// commandOf decodes a command body, failing on one that is not the documented object.
func commandOf(t *testing.T, r request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatalf("the command body %q is not a JSON object: %v", r.Body, err)
	}
	return body
}

// TestHook_AC2_RadarrRescansTheMovieThatOwnsTheDirectory is S0179 AC-2: after `done` or
// `applied-despite-error`, the Radarr movie whose path is the swapped file's mapped
// directory or its nearest ancestor - on whole components, so `/movies/Film` never owns
// `/movies/Film 2` - gets exactly one RescanMovie carrying its movieId, with the key in the
// X-Api-Key header and in no URL.
func TestHook_AC2_RadarrRescansTheMovieThatOwnsTheDirectory(t *testing.T) {
	cases := []struct {
		name, file string
		wantID     float64
	}{
		{"the movie's own directory", "/mnt/media/movies/Film/film.mkv", 11},
		{"a sibling whose name starts the same", "/mnt/media/movies/Film 2/film.mkv", 12},
		{"a subdirectory: the nearest ancestor", "/mnt/media/movies/Film 2/extras/clip.mkv", 12},
		{"only the far ancestor owns it", "/mnt/media/movies/Other/film.mkv", 13},
	}
	for _, status := range replacedStatuses {
		for _, tc := range cases {
			t.Run(string(status)+"/"+tc.name, func(t *testing.T) {
				radarr := newFake(t, arrList("/api/v3/movie", movieList))
				logs, log := newRecorder()
				run(t, []Target{radarrAt(radarr)}, log, engine.Event{Path: tc.file, Status: status})

				posts := radarr.posts()
				if len(posts) != 1 {
					t.Fatalf("radarr received %d command(s), want exactly 1: %+v", len(posts), posts)
				}
				if posts[0].Method != http.MethodPost || posts[0].Path != "/api/v3/command" {
					t.Errorf("the command went to %s %s, want POST /api/v3/command", posts[0].Method, posts[0].Path)
				}
				body := commandOf(t, posts[0])
				if body["name"] != "RescanMovie" || body["movieId"] != tc.wantID || len(body) != 2 {
					t.Errorf("the command body is %v, want {name: RescanMovie, movieId: %v}", body, tc.wantID)
				}
				if got := posts[0].Header.Get("Content-Type"); got != "application/json" {
					t.Errorf("the command's Content-Type is %q", got)
				}
				if got := len(radarr.where(http.MethodGet, "/api/v3/movie")); got != 1 {
					t.Errorf("radarr's movie list was read %d time(s), want 1", got)
				}
				for _, r := range radarr.seen() {
					if r.Header.Get("X-Api-Key") != radarrSecret {
						t.Errorf("%s %s did not carry the key in X-Api-Key", r.Method, r.Path)
					}
					if r.RawQuery != "" {
						t.Errorf("%s %s carries a query (%q): the key must never travel in a URL", r.Method, r.Path, r.RawQuery)
					}
				}
				if infos := logs.at(slog.LevelInfo); len(infos) != 1 || infos[0].Attrs["target"] != "radarr" ||
					infos[0].Attrs["file"] != tc.file || !strings.Contains(infos[0].Msg, "requested") {
					t.Errorf("want one info record saying the rescan was requested, got %+v", logs.all())
				}
				if len(logs.at(slog.LevelWarn))+len(logs.at(slog.LevelError)) != 0 {
					t.Errorf("a successful rescan logged above info: %+v", logs.all())
				}
			})
		}
	}
}

// TestHook_AC3_SonarrRescansTheSeriesThatOwnsTheSeasonDirectory is S0179 AC-3: a file in
// `Show/Season 1/` is owned by the series whose path is `Show`, which gets exactly one
// RescanSeries carrying its seriesId, with the key in X-Api-Key.
func TestHook_AC3_SonarrRescansTheSeriesThatOwnsTheSeasonDirectory(t *testing.T) {
	cases := []struct {
		name, file string
		wantID     float64
	}{
		{"a season directory", "/mnt/media/tv/Show/Season 1/e01.mkv", 21},
		{"the series directory itself", "/mnt/media/tv/Show/e01.mkv", 21},
		{"a sibling series, listed with a trailing slash", "/mnt/media/tv/Show 2/Season 1/e01.mkv", 22},
	}
	for _, status := range replacedStatuses {
		for _, tc := range cases {
			t.Run(string(status)+"/"+tc.name, func(t *testing.T) {
				sonarr := newFake(t, arrList("/api/v3/series", seriesList))
				_, log := newRecorder()
				run(t, []Target{sonarrAt(sonarr)}, log, engine.Event{Path: tc.file, Status: status})

				posts := sonarr.posts()
				if len(posts) != 1 || posts[0].Path != "/api/v3/command" {
					t.Fatalf("sonarr received %+v, want exactly one command", posts)
				}
				body := commandOf(t, posts[0])
				if body["name"] != "RescanSeries" || body["seriesId"] != tc.wantID || len(body) != 2 {
					t.Errorf("the command body is %v, want {name: RescanSeries, seriesId: %v}", body, tc.wantID)
				}
				for _, r := range sonarr.seen() {
					if r.Header.Get("X-Api-Key") != sonarrSecret || r.RawQuery != "" {
						t.Errorf("%s %s: key header %q, query %q", r.Method, r.Path, r.Header.Get("X-Api-Key"), r.RawQuery)
					}
				}
			})
		}
	}
}

// TestHook_AC4_PlexGetsAPartialScanOfTheDirectoryAndNeverAWholeSection is S0179 AC-4: the
// section whose location owns the mapped directory - the longest location, on whole
// components - is refreshed with `path` set to the swapped file's mapped parent directory.
// No request is a refresh without a path, and none names every section.
func TestHook_AC4_PlexGetsAPartialScanOfTheDirectoryAndNeverAWholeSection(t *testing.T) {
	cases := []struct {
		name, file, wantSection, wantDir string
	}{
		{"a movie", "/mnt/media/movies/Film/film.mkv", "1", "/data/movies/Film"},
		{"an episode", "/mnt/media/tv/Show/Season 1/e01.mkv", "2", "/data/tv/Show/Season 1"},
		{"a section's second location, with a space", "/mnt/media/more tv/Show & Co/e01.mkv", "2", "/data/more tv/Show & Co"},
		{"only the widest section owns it", "/mnt/media/other/clip.mkv", "7", "/data/other"},
	}
	for _, status := range replacedStatuses {
		for _, tc := range cases {
			t.Run(string(status)+"/"+tc.name, func(t *testing.T) {
				plex := newFake(t, plexSectionsHandler(sectionList))
				_, log := newRecorder()
				run(t, []Target{plexAt(plex)}, log, engine.Event{Path: tc.file, Status: status})

				posts := plex.posts()
				if len(posts) != 1 {
					t.Fatalf("plex received %d refresh(es), want exactly 1: %+v", len(posts), posts)
				}
				if posts[0].Method != http.MethodPost || posts[0].Path != "/library/sections/"+tc.wantSection+"/refresh" {
					t.Errorf("the refresh went to %s %s, want POST /library/sections/%s/refresh",
						posts[0].Method, posts[0].Path, tc.wantSection)
				}
				q, err := url.ParseQuery(posts[0].RawQuery)
				if err != nil {
					t.Fatalf("the refresh query %q does not parse: %v", posts[0].RawQuery, err)
				}
				if got := q["path"]; len(got) != 1 || got[0] != tc.wantDir || len(q) != 1 {
					t.Errorf("the refresh query is %v, want only path=%q", q, tc.wantDir)
				}
				for _, r := range plex.seen() {
					if r.Header.Get("X-Plex-Token") != plexSecret {
						t.Errorf("%s %s did not carry the token in X-Plex-Token", r.Method, r.Path)
					}
					if r.Header.Get("X-Plex-Client-Identifier") == "" || r.Header.Get("Accept") != "application/json" {
						t.Errorf("%s %s: client identifier %q, Accept %q", r.Method, r.Path,
							r.Header.Get("X-Plex-Client-Identifier"), r.Header.Get("Accept"))
					}
					if strings.Contains(r.RawQuery, "X-Plex-Token") || strings.Contains(r.RawQuery, plexSecret) {
						t.Errorf("%s %s carries the token in its URL: %q", r.Method, r.Path, r.RawQuery)
					}
					if strings.Contains(r.Path, "/all/refresh") || r.Path == "/library/sections/refresh" {
						t.Errorf("an all-sections refresh was requested: %s %s", r.Method, r.Path)
					}
					if strings.HasSuffix(r.Path, "/refresh") && url.Values(mustQuery(t, r.RawQuery)).Get("path") == "" {
						t.Errorf("a whole-section refresh was requested: %s %s?%s", r.Method, r.Path, r.RawQuery)
					}
				}
				if got := len(plex.where(http.MethodGet, "/library/sections/all")); got != 1 {
					t.Errorf("the section list was read %d time(s), want 1", got)
				}
			})
		}
	}
}

// TestHook_AC4_ADirectoryThatIsTheSectionLocationItselfIsNeverRefreshed: a file that sits
// directly in a section's location (a flat library) has that location as its directory, and a
// refresh restricted to it would scan the whole location. Nothing is sent to Plex, and one
// info record says why - a reason distinct from "nothing owns this directory".
func TestHook_AC4_ADirectoryThatIsTheSectionLocationItselfIsNeverRefreshed(t *testing.T) {
	for _, file := range []string{"/mnt/media/movies/film.mkv", "/mnt/media/more tv/e01.mkv"} {
		t.Run(file, func(t *testing.T) {
			plex := newFake(t, plexSectionsHandler(sectionList))
			logs, log := newRecorder()
			run(t, []Target{plexAt(plex)}, log, swapped(file))

			if posts := plex.posts(); len(posts) != 0 {
				t.Fatalf("plex was sent %+v for a file in a section location's root", posts)
			}
			recs := logs.all()
			if len(recs) != 1 || recs[0].Level != slog.LevelInfo {
				t.Fatalf("want exactly one info record, got %+v", recs)
			}
			if !strings.Contains(recs[0].Msg, "section location itself") || !strings.Contains(recs[0].Msg, "whole location") {
				t.Errorf("the record does not give the whole-location reason: %s", recs[0].Msg)
			}
			if strings.Contains(recs[0].Msg, "nothing that owns") {
				t.Errorf("the record gives the no-owner reason: %s", recs[0].Msg)
			}
			if recs[0].Attrs["target"] != "plex" || recs[0].Attrs["file"] != file ||
				recs[0].Attrs["directory"] != plexPaths.Map(file[:strings.LastIndex(file, "/")]) {
				t.Errorf("the record's fields are %+v", recs[0].Attrs)
			}
		})
	}
	// One level down, the same library is refreshed as before.
	plex := newFake(t, plexSectionsHandler(sectionList))
	_, log := newRecorder()
	run(t, []Target{plexAt(plex)}, log, swapped("/mnt/media/movies/Film/film.mkv"))
	if len(plex.posts()) != 1 {
		t.Errorf("a file one directory below the location was not refreshed: %+v", plex.seen())
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("query %q: %v", raw, err)
	}
	return q
}

// TestHook_AC5_NothingIsSentForAnyOtherEvent is S0179 AC-5: a skip, a failure, a parked job,
// a dry run's decision, every non-terminal transition, and a progress report - even one that
// carries a `done` status - send no request to any target and log nothing.
func TestHook_AC5_NothingIsSentForAnyOtherEvent(t *testing.T) {
	radarr := newFake(t, arrList("/api/v3/movie", movieList))
	sonarr := newFake(t, arrList("/api/v3/series", seriesList))
	plex := newFake(t, plexSectionsHandler(sectionList))
	logs, log := newRecorder()

	const file = "/mnt/media/movies/Film/film.mkv"
	events := []engine.Event{
		{Path: file, Status: store.Skipped, Outcome: &store.Outcome{}},
		{Path: file, Status: store.Failed, Outcome: &store.Outcome{}},
		{Path: file, Status: store.Indeterminate, Outcome: &store.Outcome{}},
		{Path: file, Status: store.WouldTranscode, Outcome: &store.Outcome{}},
		{Path: file, Status: store.Probing},
		{Path: file, Status: store.Encoding},
		{Path: file, Status: store.Verifying},
		{Path: file, Status: store.Encoding, Progress: &engine.Progress{}},
		{Path: file, Status: store.Done, Progress: &engine.Progress{}},
		{Path: file, Status: store.AppliedDespiteError, Progress: &engine.Progress{}},
	}
	run(t, []Target{radarrAt(radarr), sonarrAt(sonarr), plexAt(plex)}, log, events...)

	for name, f := range map[string]*fake{"radarr": radarr, "sonarr": sonarr, "plex": plex} {
		if got := f.seen(); len(got) != 0 {
			t.Errorf("%s received %d request(s) for events that replaced nothing: %+v", name, len(got), got)
		}
	}
	if got := logs.all(); len(got) != 0 {
		t.Errorf("events that replaced nothing were logged: %+v", got)
	}
}

// failureModes are the four ways S0179 AC-6 says a target can fail, each as a way to build
// that target's address and the class its record must name.
type failureMode struct {
	name  string
	class string
	// serve builds the failing endpoint and returns its address.
	serve func(t *testing.T) string
}

func failureModes() []failureMode {
	return []failureMode{
		{"the connection is refused", ClassUnreachable, func(t *testing.T) string {
			srv := httptest.NewServer(http.NotFoundHandler())
			addr := srv.URL
			srv.Close() // nothing listens there any more
			return addr
		}},
		{"a 401 answer", ClassUnauthorized + " 401", func(t *testing.T) string {
			return newFake(t, func(w http.ResponseWriter, _ request) { w.WriteHeader(http.StatusUnauthorized) }).srv.URL
		}},
		{"a 403 answer", ClassUnauthorized + " 403", func(t *testing.T) string {
			return newFake(t, func(w http.ResponseWriter, _ request) { w.WriteHeader(http.StatusForbidden) }).srv.URL
		}},
		{"a 500 answer", ClassStatus + " 500", func(t *testing.T) string {
			return newFake(t, func(w http.ResponseWriter, _ request) { w.WriteHeader(http.StatusInternalServerError) }).srv.URL
		}},
		{"a redirect is not followed", ClassStatus + " 302", func(t *testing.T) string {
			return newFake(t, func(w http.ResponseWriter, _ request) {
				w.Header().Set("Location", "http://elsewhere.invalid/")
				w.WriteHeader(http.StatusFound)
			}).srv.URL
		}},
		{"a body that does not parse", ClassUnparseable, func(t *testing.T) string {
			return newFake(t, func(w http.ResponseWriter, _ request) { _, _ = io.WriteString(w, "<html>not json</html>") }).srv.URL
		}},
		{"no answer within the timeout", ClassTimeout, func(t *testing.T) string {
			shortTimeout(t, 150*time.Millisecond)
			return newFake(t, func(_ http.ResponseWriter, r request) { r.hang() }).srv.URL
		}},
	}
}

// targetAt builds each target against an address, with its own credential.
var targetAt = map[string]func(addr string) Target{
	"radarr": func(addr string) Target { return NewRadarr(addr, secret.NewValue(radarrSecret), radarrPaths) },
	"sonarr": func(addr string) Target { return NewSonarr(addr, secret.NewValue(sonarrSecret), sonarrPaths) },
	"plex":   func(addr string) Target { return NewPlex(addr, secret.NewValue(plexSecret), plexPaths) },
}

// TestHook_AC7_AC14_AFailedRequestIsOneWarnRecordAndNoCredential is S0179 AC-7 and the
// failure half of AC-14: for every target and every failure mode there is exactly one warn
// record for that target and file - naming the target, the swapped path, what was attempted,
// the failure class, that the swap stands and that the request is not retried - no
// error-level record, and no record at any level that carries a resolved credential.
func TestHook_AC7_AC14_AFailedRequestIsOneWarnRecordAndNoCredential(t *testing.T) {
	const file = "/mnt/media/tv/Show/Season 1/e01.mkv"
	for _, name := range []string{"radarr", "sonarr", "plex"} {
		for _, mode := range failureModes() {
			t.Run(name+"/"+mode.name, func(t *testing.T) {
				logs, log := newRecorder()
				run(t, []Target{targetAt[name](mode.serve(t))}, log, swapped(file))

				warns := logs.at(slog.LevelWarn)
				if len(warns) != 1 {
					t.Fatalf("want exactly one warn record, got %d: %+v", len(warns), logs.all())
				}
				w := warns[0]
				if w.Attrs["target"] != name {
					t.Errorf("the record names target %q, want %q", w.Attrs["target"], name)
				}
				if w.Attrs["file"] != file {
					t.Errorf("the record names file %q, want the swapped path", w.Attrs["file"])
				}
				if w.Attrs["failure"] != mode.class {
					t.Errorf("the record's failure class is %q, want %q", w.Attrs["failure"], mode.class)
				}
				if !strings.HasPrefix(w.Attrs["attempted"], "GET ") {
					t.Errorf("the record does not say what was attempted: %q", w.Attrs["attempted"])
				}
				if w.Attrs["directory"] == "" {
					t.Errorf("the record does not name the directory asked about")
				}
				for _, want := range []string{"the swap stands", "not retried", "scheduled rescan remains the fallback"} {
					if !strings.Contains(w.Msg, want) {
						t.Errorf("the record does not say %q: %s", want, w.Msg)
					}
				}
				if got := logs.at(slog.LevelError); len(got) != 0 {
					t.Errorf("a failed rescan logged at error: %+v", got)
				}
				if len(logs.all()) != 1 {
					t.Errorf("a failed rescan logged more than its one record: %+v", logs.all())
				}
				assertNoCredential(t, logs)
				if strings.Contains(w.text(), "http://") {
					t.Errorf("the record carries a request URL: %s", w.text())
				}
			})
		}
	}
}

// TestHook_AC7_AFailedCommandNamesTheCommandItAttempted: when the lookup succeeds and the
// command or the refresh itself fails, the one warn record names THAT request.
func TestHook_AC7_AFailedCommandNamesTheCommandItAttempted(t *testing.T) {
	failPost := func(get func(w http.ResponseWriter, r request)) func(w http.ResponseWriter, r request) {
		return func(w http.ResponseWriter, r request) {
			if r.Method == http.MethodGet {
				get(w, r)
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}
	cases := []struct {
		name, file, want string
		target           func(t *testing.T) Target
	}{
		{"radarr", "/mnt/media/movies/Film/film.mkv", "POST /api/v3/command RescanMovie for movie 11", func(t *testing.T) Target {
			return radarrAt(newFake(t, failPost(arrList("/api/v3/movie", movieList))))
		}},
		{"sonarr", "/mnt/media/tv/Show/Season 1/e01.mkv", "POST /api/v3/command RescanSeries for series 21", func(t *testing.T) Target {
			return sonarrAt(newFake(t, failPost(arrList("/api/v3/series", seriesList))))
		}},
		{"plex", "/mnt/media/tv/Show/Season 1/e01.mkv", "POST /library/sections/2/refresh restricted to the directory", func(t *testing.T) Target {
			return plexAt(newFake(t, failPost(plexSectionsHandler(sectionList))))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs, log := newRecorder()
			run(t, []Target{tc.target(t)}, log, swapped(tc.file))
			warns := logs.at(slog.LevelWarn)
			if len(warns) != 1 || len(logs.all()) != 1 {
				t.Fatalf("want exactly one record, a warn; got %+v", logs.all())
			}
			if warns[0].Attrs["attempted"] != tc.want || warns[0].Attrs["failure"] != ClassStatus+" 503" {
				t.Errorf("attempted=%q failure=%q, want %q and http-status 503",
					warns[0].Attrs["attempted"], warns[0].Attrs["failure"], tc.want)
			}
		})
	}
}

// TestHook_AC8_NoOwnerSendsNothingAndIsOneInfoRecord is S0179 AC-8: a directory no movie,
// series or section owns sends no command or scan and is one info record naming the target
// and the mapped directory.
func TestHook_AC8_NoOwnerSendsNothingAndIsOneInfoRecord(t *testing.T) {
	const file = "/mnt/media/unlisted/Thing/file.mkv"
	cases := []struct {
		name, wantDir string
		build         func(t *testing.T) (*fake, Target)
	}{
		{"radarr", "/mnt/media/unlisted/Thing", func(t *testing.T) (*fake, Target) {
			f := newFake(t, arrList("/api/v3/movie", `[{"id": 11, "path": "/movies/Film"}]`))
			return f, radarrAt(f)
		}},
		{"sonarr", "/mnt/media/unlisted/Thing", func(t *testing.T) (*fake, Target) {
			f := newFake(t, arrList("/api/v3/series", `[]`))
			return f, sonarrAt(f)
		}},
		{"plex", "/data/unlisted/Thing", func(t *testing.T) (*fake, Target) {
			f := newFake(t, plexSectionsHandler(`{"MediaContainer": {"size": 1, "Directory": [
			  {"key": "1", "Location": [{"id": 1, "path": "/data/movies"}]}]}}`))
			return f, plexAt(f)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, target := tc.build(t)
			logs, log := newRecorder()
			run(t, []Target{target}, log, swapped(file))

			if posts := f.posts(); len(posts) != 0 {
				t.Errorf("%s was sent %+v for a directory nothing owns", tc.name, posts)
			}
			recs := logs.all()
			if len(recs) != 1 || recs[0].Level != slog.LevelInfo {
				t.Fatalf("want exactly one info record, got %+v", recs)
			}
			if recs[0].Attrs["target"] != tc.name || recs[0].Attrs["directory"] != tc.wantDir {
				t.Errorf("the record names target %q and directory %q, want %q and %q",
					recs[0].Attrs["target"], recs[0].Attrs["directory"], tc.name, tc.wantDir)
			}
			if !strings.Contains(recs[0].Msg, "not sent") {
				t.Errorf("the record does not say nothing was sent: %s", recs[0].Msg)
			}
		})
	}
}

// TestHook_AC9_OneFailingTargetDoesNotStopTheOthers is S0179 AC-9: with each target failing
// in turn, both of the others still receive their request for that same swap.
func TestHook_AC9_OneFailingTargetDoesNotStopTheOthers(t *testing.T) {
	const file = "/mnt/media/tv/Show/Season 1/e01.mkv"
	broken := func(w http.ResponseWriter, _ request) { w.WriteHeader(http.StatusInternalServerError) }
	for _, failing := range []string{"radarr", "sonarr", "plex"} {
		t.Run(failing+" fails", func(t *testing.T) {
			handlers := map[string]func(http.ResponseWriter, request){
				"radarr": arrList("/api/v3/movie", `[{"id": 31, "path": "/mnt/media/tv/Show"}]`),
				"sonarr": arrList("/api/v3/series", seriesList),
				"plex":   plexSectionsHandler(sectionList),
			}
			handlers[failing] = broken
			fakes := map[string]*fake{}
			var targets []Target
			for _, name := range []string{"radarr", "sonarr", "plex"} {
				fakes[name] = newFake(t, handlers[name])
				targets = append(targets, targetAt[name](fakes[name].srv.URL))
			}
			logs, log := newRecorder()
			run(t, targets, log, swapped(file))

			for name, f := range fakes {
				if name == failing {
					if len(f.seen()) != 1 {
						t.Errorf("the failing target %s was asked %d time(s), want one attempt and no retry", name, len(f.seen()))
					}
					continue
				}
				if len(f.posts()) != 1 {
					t.Errorf("%s did not receive its request after %s failed: %+v", name, failing, f.seen())
				}
			}
			warns := logs.at(slog.LevelWarn)
			if len(warns) != 1 || warns[0].Attrs["target"] != failing {
				t.Errorf("want one warn record, for %s; got %+v", failing, warns)
			}
		})
	}
}

// TestHook_AC10_AHangingTargetNeverHoldsTheEngineAndAFullQueueDrops is S0179 AC-10: with a
// target that accepts the connection and never answers, Observe returns without waiting on
// the network for every swap offered; once the bounded queue is full the next swap is
// dropped with one warn record naming its path; and nothing is retried.
func TestHook_AC10_AHangingTargetNeverHoldsTheEngineAndAFullQueueDrops(t *testing.T) {
	entered := make(chan struct{}, 1)
	hung := newFake(t, func(_ http.ResponseWriter, r request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		r.hang()
	})

	logs, log := newRecorder()
	h := NewHook([]Target{radarrAt(hung)}, log)
	h.Start()

	// The first swap is taken off the queue and its request hangs.
	h.Observe(swapped("/mnt/media/movies/Film/first.mkv"))
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the hook never sent its first request")
	}

	// The queue then takes exactly QueueSize more, and every Observe returns at once.
	start := time.Now()
	for i := 0; i < QueueSize; i++ {
		h.Observe(swapped("/mnt/media/movies/Film/queued.mkv"))
	}
	if got := logs.all(); len(got) != 0 {
		t.Fatalf("a swap was dropped before the queue was full: %+v", got)
	}
	const dropped = "/mnt/media/movies/Film/one-too-many.mkv"
	h.Observe(swapped(dropped))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Observe took %s across %d swaps while a target hung: it waited on the network", elapsed, QueueSize+1)
	}

	warns := logs.at(slog.LevelWarn)
	if len(warns) != 1 || len(logs.all()) != 1 {
		t.Fatalf("want exactly one record, a warn for the dropped swap; got %+v", logs.all())
	}
	if warns[0].Attrs["file"] != dropped || !strings.Contains(warns[0].Msg, "queue is full") {
		t.Errorf("the drop record does not name the dropped path and the full queue: %+v", warns[0])
	}

	// AC-11 at this level: a drain whose bound passes reports how many swaps were not
	// delivered, in one warn record, and returns. The count is the one in flight plus the
	// QueueSize queued; the dropped one was never accepted.
	began := time.Now()
	left := h.Drain(200 * time.Millisecond)
	if waited := time.Since(began); waited > 5*time.Second {
		t.Errorf("the drain took %s against a 200ms bound", waited)
	}
	if left != QueueSize+1 {
		t.Errorf("the drain reports %d undelivered, want %d", left, QueueSize+1)
	}
	var drainWarn *record
	for _, r := range logs.at(slog.LevelWarn) {
		if strings.Contains(r.Msg, "not delivered") {
			r := r
			drainWarn = &r
		}
	}
	if drainWarn == nil || drainWarn.Attrs["not_delivered"] != "65" || drainWarn.Attrs["drain_bound"] != "200ms" {
		t.Fatalf("want one warn record with the count not delivered and the bound; got %+v", logs.all())
	}
	if got := len(logs.at(slog.LevelWarn)); got != 2 {
		t.Errorf("want two warn records in all (the drop and the drain), got %d: %+v", got, logs.all())
	}
	if got := len(hung.seen()); got != 1 {
		t.Errorf("the hung target received %d request(s), want the one in flight", got)
	}

	// After a drain the hook observes nothing, and a second drain is a no-op.
	h.Observe(swapped("/mnt/media/movies/Film/late.mkv"))
	if again := h.Drain(time.Second); again != 0 {
		t.Errorf("a second drain reports %d", again)
	}
	if got := len(logs.all()); got != 2 {
		t.Errorf("the hook logged after its drain: %+v", logs.all())
	}
}

// TestHook_AC11_ADrainWithNothingPendingSaysNothing: a drain that finishes inside its bound
// returns zero and writes no record.
func TestHook_AC11_ADrainWithNothingPendingSaysNothing(t *testing.T) {
	radarr := newFake(t, arrList("/api/v3/movie", movieList))
	logs, log := newRecorder()
	h := NewHook([]Target{radarrAt(radarr)}, log)
	h.Start()
	h.Observe(swapped("/mnt/media/movies/Film/film.mkv"))
	if left := h.Drain(30 * time.Second); left != 0 {
		t.Fatalf("the drain left %d undelivered", left)
	}
	if len(radarr.posts()) != 1 {
		t.Errorf("the pending request was not delivered by the drain: %+v", radarr.seen())
	}
	if got := logs.at(slog.LevelWarn); len(got) != 0 {
		t.Errorf("a clean drain warned: %+v", got)
	}
}

// TestHook_AC14_NoCredentialInAnyRecordOnSuccess is the success half of S0179 AC-14, at every
// level: all three targets configured with known credentials, a swap announced to each, and
// no record carries one.
func TestHook_AC14_NoCredentialInAnyRecordOnSuccess(t *testing.T) {
	radarr := newFake(t, arrList("/api/v3/movie", `[{"id": 31, "path": "/mnt/media/tv/Show"}]`))
	sonarr := newFake(t, arrList("/api/v3/series", seriesList))
	plex := newFake(t, plexSectionsHandler(sectionList))
	logs, log := newRecorder()
	run(t, []Target{radarrAt(radarr), sonarrAt(sonarr), plexAt(plex)}, log,
		swapped("/mnt/media/tv/Show/Season 1/e01.mkv"))

	if got := len(logs.all()); got != 3 {
		t.Fatalf("want one record per target, got %d: %+v", got, logs.all())
	}
	assertNoCredential(t, logs)
	for _, f := range []*fake{radarr, sonarr, plex} {
		if len(f.posts()) != 1 {
			t.Errorf("a target did not receive its request, so the case proves nothing: %+v", f.seen())
		}
	}
}

// TestNewHook_NoTargetsIsNoHook: with nothing configured there is no hook at all.
func TestNewHook_NoTargetsIsNoHook(t *testing.T) {
	if h := NewHook(nil, nil); h != nil {
		t.Errorf("NewHook with no targets = %v, want nil", h)
	}
	if h := NewHook([]Target{radarrAt(newFake(t, arrList("/api/v3/movie", "[]")))}, nil); h == nil || h.log == nil {
		t.Error("NewHook with a target and no logger did not fall back to the default logger")
	}
}

// TestTargetsFor_BuildsOnlyTheEnabledTargetsInOrder: a target is built when its address and
// credential are both set, with its own credential and its own path map.
func TestTargetsFor_BuildsOnlyTheEnabledTargetsInOrder(t *testing.T) {
	resolved := func(key string) secret.Value { return secret.NewValue("value-of-" + key) }
	if got := TargetsFor(&config.Config{}, resolved); len(got) != 0 {
		t.Fatalf("an empty configuration built %d target(s)", len(got))
	}

	radarr := newFake(t, arrList("/api/v3/movie", `[{"id": 5, "path": "/r/Film"}]`))
	sonarr := newFake(t, arrList("/api/v3/series", `[{"id": 6, "path": "/s/Film"}]`))
	plex := newFake(t, plexSectionsHandler(`{"MediaContainer": {"Directory": [{"key": "4", "Location": [{"path": "/p"}]}]}}`))
	cfg := &config.Config{
		RadarrURL: radarr.srv.URL + "/", RadarrAPIKey: "file:/x", RadarrPathMap: config.PathMap{{From: "/lib", To: "/r"}},
		SonarrURL: sonarr.srv.URL, SonarrAPIKey: "file:/x", SonarrPathMap: config.PathMap{{From: "/lib", To: "/s"}},
		PlexURL: plex.srv.URL, PlexToken: "file:/x", PlexPathMap: config.PathMap{{From: "/lib", To: "/p"}},
	}
	targets := TargetsFor(cfg, resolved)
	var names []string
	for _, target := range targets {
		names = append(names, target.Name())
	}
	if strings.Join(names, ",") != "radarr,sonarr,plex" {
		t.Fatalf("built %v, want radarr, sonarr, plex", names)
	}
	_, log := newRecorder()
	run(t, targets, log, swapped("/lib/Film/film.mkv"))
	for name, tc := range map[string]struct {
		f           *fake
		header, key string
	}{
		"radarr": {radarr, "X-Api-Key", "radarr_api_key"},
		"sonarr": {sonarr, "X-Api-Key", "sonarr_api_key"},
		"plex":   {plex, "X-Plex-Token", "plex_token"},
	} {
		posts := tc.f.posts()
		if len(posts) != 1 {
			t.Errorf("%s: %d request(s) sent, want 1 (its own path map decides the owner)", name, len(posts))
			continue
		}
		if got := posts[0].Header.Get(tc.header); got != "value-of-"+tc.key {
			t.Errorf("%s was sent %q in %s, want the value resolved for %s", name, got, tc.header, tc.key)
		}
	}

	// One target on, two off.
	only := TargetsFor(&config.Config{SonarrURL: sonarr.srv.URL, SonarrAPIKey: "file:/x", RadarrURL: radarr.srv.URL}, resolved)
	if len(only) != 1 || only[0].Name() != "sonarr" {
		t.Errorf("with only sonarr fully configured, built %d target(s)", len(only))
	}
}
