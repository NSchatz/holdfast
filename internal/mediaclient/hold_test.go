package mediaclient

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/secret"
)

var testKey = secret.NewValue("a-test-credential")

// sessionsBody is `GET /status/sessions` as the reference documents it: MediaContainer,
// Metadata[], each with Media[].Part[].file beside the User, Player and Session objects.
// Every path is synthetic.
func sessionsBody(files ...string) string {
	var items []string
	for _, f := range files {
		items = append(items, `{"type": "episode", "title": "Synthetic",
		  "Media": [{"id": 1, "Part": [{"id": 1, "file": "`+f+`", "decision": "directplay"}]}],
		  "User": {"id": "1", "title": "viewer"},
		  "Player": {"state": "playing", "local": true},
		  "Session": {"id": "abc", "location": "lan"}}`)
	}
	if len(items) == 0 {
		return `{"MediaContainer": {"size": 0}}`
	}
	return `{"MediaContainer": {"size": ` + string(rune('0'+len(items))) + `, "Metadata": [` + strings.Join(items, ",") + `]}}`
}

// sessionsFake answers /status/sessions from body() and counts the requests.
func sessionsFake(t *testing.T, answer func(w http.ResponseWriter, r request)) *fake {
	t.Helper()
	return newFake(t, func(w http.ResponseWriter, r request) {
		if r.Method != http.MethodGet || r.Path != "/status/sessions" {
			http.NotFound(w, nil)
			return
		}
		answer(w, r)
	})
}

// TestPlayHold_AFileBeingPlayedIsHeldByItsHoldfastPath: the file a session names is mapped
// from Plex's view back to holdfast's with the path map reversed, and only that file is held.
func TestPlayHold_AFileBeingPlayedIsHeldByItsHoldfastPath(t *testing.T) {
	plex := sessionsFake(t, func(w http.ResponseWriter, _ request) {
		_, _ = io.WriteString(w, sessionsBody("/data/tv/Show/Season 1/e01.mkv", "/data/movies/Film/./film.mkv"))
	})
	logs, log := newRecorder()
	hold := NewPlayHold(plexAt(plex), time.Hour, log)

	for path, want := range map[string]bool{
		"/mnt/media/tv/Show/Season 1/e01.mkv":             true,
		"/mnt/media/tv/Show/Season 1/../Season 1/e01.mkv": true, // cleaned before matching
		"/mnt/media/movies/Film/film.mkv":                 true,
		"/mnt/media/tv/Show/Season 1/e02.mkv":             false,
		"/data/tv/Show/Season 1/e01.mkv":                  false, // Plex's own spelling is not holdfast's
		"/mnt/media/tv/Show/Season 1":                     false,
	} {
		held, why := hold.Held(context.Background(), path)
		if held != want {
			t.Errorf("Held(%q) = %v, want %v", path, held, want)
		}
		if held && why != HoldReason {
			t.Errorf("Held(%q) gives the reason %q", path, why)
		}
		if !held && why != "" {
			t.Errorf("a file that is not held carries the reason %q", why)
		}
	}
	// One answer served every question inside the cache window.
	reqs := plex.seen()
	if len(reqs) != 1 {
		t.Fatalf("Plex was asked %d time(s) inside one cache window, want 1", len(reqs))
	}
	if reqs[0].Header.Get("X-Plex-Token") != plexSecret || reqs[0].Header.Get("Accept") != "application/json" ||
		reqs[0].Header.Get("X-Plex-Client-Identifier") == "" || reqs[0].RawQuery != "" {
		t.Errorf("the sessions request arrived as %+v: the token belongs in its header and nowhere else", reqs[0])
	}
	if got := logs.all(); len(got) != 0 {
		t.Errorf("an answered hold logged: %+v", got)
	}
}

// TestPlayHold_SessionsThatNameNoFileHoldNothing: a session with no part, no file, an empty
// file or a relative one names no path, so it holds nothing - and is not a failure.
func TestPlayHold_SessionsThatNameNoFileHoldNothing(t *testing.T) {
	body := `{"MediaContainer": {"size": 4, "Metadata": [
	  {"type": "clip", "Session": {"id": "a"}},
	  {"Media": [{"id": 1}]},
	  {"Media": [{"Part": [{"id": 2}, {"file": ""}, {"file": "relative/file.mkv"}]}]},
	  {"Media": [{"Part": [{"file": "/data/movies/Film/film.mkv"}]}, {"Part": [{"file": "/data/movies/Film/film-4k.mkv"}]}]}
	]}}`
	plex := sessionsFake(t, func(w http.ResponseWriter, _ request) { _, _ = io.WriteString(w, body) })
	logs, log := newRecorder()
	files, class, ok := plexAt(plex).Playing(context.Background())
	if !ok || class != "" {
		t.Fatalf("Playing() = ok %v, class %q", ok, class)
	}
	if len(files) != 2 || !files["/mnt/media/movies/Film/film.mkv"] || !files["/mnt/media/movies/Film/film-4k.mkv"] {
		t.Errorf("Playing() = %v, want the two absolute files, in holdfast's view", files)
	}
	hold := NewPlayHold(plexAt(plex), 0, log)
	if held, _ := hold.Held(context.Background(), "relative/file.mkv"); held {
		t.Error("a relative session file held a path")
	}
	if got := logs.all(); len(got) != 0 {
		t.Errorf("sessions with no file were logged: %+v", got)
	}
}

// TestPlayHold_TheAnswerIsCachedForTheTTLAndNoLonger: inside the window one request serves
// every caller; at the window's end Plex is asked again and the new answer is the one used.
func TestPlayHold_TheAnswerIsCachedForTheTTLAndNoLonger(t *testing.T) {
	var playing atomic.Bool
	playing.Store(true)
	const file = "/mnt/media/movies/Film/film.mkv"
	plex := sessionsFake(t, func(w http.ResponseWriter, _ request) {
		if playing.Load() {
			_, _ = io.WriteString(w, sessionsBody("/data/movies/Film/film.mkv"))
			return
		}
		_, _ = io.WriteString(w, sessionsBody())
	})
	_, log := newRecorder()
	hold := NewPlayHold(plexAt(plex), 0, log)
	if hold.ttl != HoldCacheTTL {
		t.Fatalf("a zero TTL resolved to %s, want the default %s", hold.ttl, HoldCacheTTL)
	}
	if neg := NewPlayHold(plexAt(plex), -time.Second, log); neg.ttl != HoldCacheTTL {
		t.Errorf("a negative TTL resolved to %s", neg.ttl)
	}
	now := time.Unix(1_000_000, 0)
	hold.now = func() time.Time { return now }

	ask := func(want bool, wantRequests int, when string) {
		t.Helper()
		if held, _ := hold.Held(context.Background(), file); held != want {
			t.Errorf("%s: Held = %v, want %v", when, held, want)
		}
		if got := len(plex.seen()); got != wantRequests {
			t.Errorf("%s: Plex has been asked %d time(s), want %d", when, got, wantRequests)
		}
	}
	ask(true, 1, "the first question")
	playing.Store(false)
	now = now.Add(HoldCacheTTL - time.Millisecond)
	ask(true, 1, "just inside the window") // the cached answer, and no request
	now = now.Add(time.Millisecond)
	ask(false, 2, "at the window's end") // asked again, and the playback has stopped
	ask(false, 2, "inside the new window")
	playing.Store(true)
	now = now.Add(HoldCacheTTL)
	ask(true, 3, "a window later")
}

// TestPlayHold_FailsOpenAndWarnsOncePerOutage is the fail-open rule (ledger D3): when Plex
// cannot be asked - refused, 401, 500, an answer that does not parse, no answer in time - no
// file is held, and the outage is ONE warn record however many files and polls it spans.
// When Plex answers again the hold is in force again, and a second outage warns again.
func TestPlayHold_FailsOpenAndWarnsOncePerOutage(t *testing.T) {
	const file = "/mnt/media/movies/Film/film.mkv"
	modes := map[string]struct {
		class  string
		answer func(t *testing.T) func(w http.ResponseWriter, r request)
	}{
		"a 401 answer": {"unauthorized 401", func(*testing.T) func(http.ResponseWriter, request) {
			return func(w http.ResponseWriter, _ request) { w.WriteHeader(http.StatusUnauthorized) }
		}},
		"a 500 answer": {"http-status 500", func(*testing.T) func(http.ResponseWriter, request) {
			return func(w http.ResponseWriter, _ request) { w.WriteHeader(http.StatusInternalServerError) }
		}},
		"XML": {ClassUnparseable, func(*testing.T) func(http.ResponseWriter, request) {
			return func(w http.ResponseWriter, _ request) { _, _ = io.WriteString(w, `<MediaContainer size="0"/>`) }
		}},
		"JSON that is not a Plex answer": {ClassUnparseable, func(*testing.T) func(http.ResponseWriter, request) {
			return func(w http.ResponseWriter, _ request) { _, _ = io.WriteString(w, `{"error": "nope"}`) }
		}},
		"no answer in time": {ClassTimeout, func(t *testing.T) func(http.ResponseWriter, request) {
			shortTimeout(t, 100*time.Millisecond)
			return func(_ http.ResponseWriter, r request) { r.hang() }
		}},
	}
	for name, mode := range modes {
		t.Run(name, func(t *testing.T) {
			var broken atomic.Bool
			broken.Store(true)
			fail := mode.answer(t)
			plex := sessionsFake(t, func(w http.ResponseWriter, r request) {
				if broken.Load() {
					fail(w, r)
					return
				}
				_, _ = io.WriteString(w, sessionsBody("/data/movies/Film/film.mkv"))
			})
			logs, log := newRecorder()
			hold := NewPlayHold(plexAt(plex), time.Nanosecond, log)
			hold.failTTL = time.Nanosecond // every question asks Plex, failing or not

			for i := 0; i < 4; i++ {
				for _, f := range []string{file, "/mnt/media/tv/Show/e01.mkv"} {
					if held, why := hold.Held(context.Background(), f); held || why != "" {
						t.Fatalf("with Plex failing, %s was held (%q): the hold must fail open", f, why)
					}
				}
			}
			if got := len(plex.seen()); got != 8 {
				t.Errorf("Plex was asked %d time(s), want 8: the case must span several polls", got)
			}
			warns := logs.at(slog.LevelWarn)
			if len(warns) != 1 || len(logs.all()) != 1 {
				t.Fatalf("want exactly one record, a warn, for the whole outage; got %+v", logs.all())
			}
			w := warns[0]
			if w.Attrs["target"] != "plex" || w.Attrs["failure"] != mode.class || w.Attrs["attempted"] != "GET /status/sessions" {
				t.Errorf("the record's fields are %+v, want target plex and failure %q", w.Attrs, mode.class)
			}
			if !strings.Contains(w.Msg, "NO file is held") || !strings.Contains(w.Msg, "fails open") {
				t.Errorf("the record does not say the hold fails open: %s", w.Msg)
			}
			assertNoCredential(t, logs)

			// Plex answers again: the hold is back, said once at info.
			broken.Store(false)
			if held, _ := hold.Held(context.Background(), file); !held {
				t.Error("with Plex answering again, the file being played is not held")
			}
			_, _ = hold.Held(context.Background(), file)
			if infos := logs.at(slog.LevelInfo); len(infos) != 1 || !strings.Contains(infos[0].Msg, "in force again") {
				t.Errorf("want one info record saying the hold is in force again, got %+v", logs.all())
			}
			// And a second outage is a second warn.
			broken.Store(true)
			if held, _ := hold.Held(context.Background(), file); held {
				t.Error("a file stayed held on a stale answer after Plex stopped answering")
			}
			_, _ = hold.Held(context.Background(), file)
			if got := len(logs.at(slog.LevelWarn)); got != 2 {
				t.Errorf("a second outage produced %d warn record(s) in all, want 2", got)
			}
			if got := len(logs.at(slog.LevelError)); got != 0 {
				t.Errorf("the hold logged at error: %+v", logs.at(slog.LevelError))
			}
		})
	}

	t.Run("the connection is refused", func(t *testing.T) {
		dead := newFake(t, func(http.ResponseWriter, request) {})
		addr := dead.srv.URL
		dead.srv.Close()
		logs, log := newRecorder()
		hold := NewPlayHold(NewPlex(addr, secret.NewValue(plexSecret), plexPaths), time.Nanosecond, log)
		hold.failTTL = time.Nanosecond
		for i := 0; i < 3; i++ {
			if held, _ := hold.Held(context.Background(), file); held {
				t.Fatal("an unreachable Plex held a file")
			}
		}
		warns := logs.at(slog.LevelWarn)
		if len(warns) != 1 || len(logs.all()) != 1 || warns[0].Attrs["failure"] != ClassUnreachable {
			t.Fatalf("want one warn record with failure %q, got %+v", ClassUnreachable, logs.all())
		}
		assertNoCredential(t, logs)
		if strings.Contains(warns[0].text(), addr) {
			t.Errorf("the record carries the request address: %s", warns[0].text())
		}
	})
}

// TestPlayHold_AFailedQuestionIsReusedForAMinute: a failed question is reused for
// HoldFailureTTL, far longer than an answer is, so a Plex that accepts connections and never
// answers costs one request timeout a minute rather than one per question. An answer is
// still reused for HoldCacheTTL only.
func TestPlayHold_AFailedQuestionIsReusedForAMinute(t *testing.T) {
	var broken atomic.Bool
	broken.Store(true)
	plex := sessionsFake(t, func(w http.ResponseWriter, _ request) {
		if broken.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, sessionsBody("/data/movies/Film/film.mkv"))
	})
	_, log := newRecorder()
	hold := NewPlayHold(plexAt(plex), 0, log)
	if HoldFailureTTL != 60*time.Second || hold.failTTL != HoldFailureTTL || HoldFailureTTL <= HoldCacheTTL {
		t.Fatalf("a failed question is reused for %s (default %s), want 60s", hold.failTTL, HoldFailureTTL)
	}
	now := time.Unix(1_000_000, 0)
	hold.now = func() time.Time { return now }
	const file = "/mnt/media/movies/Film/film.mkv"
	ask := func(want bool, wantRequests int, when string) {
		t.Helper()
		if held, _ := hold.Held(context.Background(), file); held != want {
			t.Errorf("%s: Held = %v, want %v", when, held, want)
		}
		if got := len(plex.seen()); got != wantRequests {
			t.Errorf("%s: Plex has been asked %d time(s), want %d", when, got, wantRequests)
		}
	}
	ask(false, 1, "the first question, which fails")
	broken.Store(false)
	now = now.Add(HoldCacheTTL) // past an ANSWER's window, inside a failure's
	ask(false, 1, "past the answer window")
	now = now.Add(HoldFailureTTL - HoldCacheTTL - time.Millisecond)
	ask(false, 1, "just inside the failure window")
	now = now.Add(time.Millisecond)
	ask(true, 2, "at the failure window's end") // asked again, and Plex answers
	now = now.Add(HoldCacheTTL)
	ask(true, 3, "an answer is reused for the short window only")
}

// TestPlayHold_ASessionFileOutsideAConfiguredPathMapIsDropped: with a path map configured, a
// file Plex names that no entry covers is not one of holdfast's paths and holds nothing, even
// when holdfast has a file at that very spelling. With no path map both sides share one view
// and the file is taken as written.
func TestPlayHold_ASessionFileOutsideAConfiguredPathMapIsDropped(t *testing.T) {
	plex := sessionsFake(t, func(w http.ResponseWriter, _ request) {
		_, _ = io.WriteString(w, sessionsBody("/elsewhere/Film/film.mkv", "/data/movies/Film/film.mkv", "/database/x.mkv"))
	})
	mapped, _, ok := plexAt(plex).Playing(context.Background())
	if !ok || len(mapped) != 1 || !mapped["/mnt/media/movies/Film/film.mkv"] {
		t.Errorf("with a path map, Playing() = %v; want only the file the map covers", mapped)
	}
	unmapped, _, ok := NewPlex(plex.srv.URL, testKey, nil).Playing(context.Background())
	if !ok || len(unmapped) != 3 || !unmapped["/elsewhere/Film/film.mkv"] || !unmapped["/data/movies/Film/film.mkv"] {
		t.Errorf("with no path map, Playing() = %v; want all three files as written", unmapped)
	}
}

// TestPlayHold_ACancelledCallerIsNotAnOutage: a question whose own context is cancelled
// records nothing - no warn, no cached answer - and the next caller asks again.
func TestPlayHold_ACancelledCallerIsNotAnOutage(t *testing.T) {
	plex := sessionsFake(t, func(w http.ResponseWriter, _ request) {
		_, _ = io.WriteString(w, sessionsBody("/data/movies/Film/film.mkv"))
	})
	logs, log := newRecorder()
	hold := NewPlayHold(plexAt(plex), time.Hour, log)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if held, _ := hold.Held(ctx, "/mnt/media/movies/Film/film.mkv"); held {
		t.Error("a cancelled question held a file")
	}
	if got := logs.all(); len(got) != 0 {
		t.Errorf("a cancelled question was logged as an outage: %+v", got)
	}
	if held, _ := hold.Held(context.Background(), "/mnt/media/movies/Film/film.mkv"); !held {
		t.Error("the caller after a cancelled one did not ask Plex again")
	}
}

// TestNewPlayHold_DefaultsItsLogger: no logger is the default logger, never a nil one.
func TestNewPlayHold_DefaultsItsLogger(t *testing.T) {
	if h := NewPlayHold(NewPlex("http://plex.invalid", testKey, nil), 0, nil); h.log == nil {
		t.Error("NewPlayHold with no logger left it nil")
	}
}
