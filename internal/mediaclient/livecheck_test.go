package mediaclient

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
)

// routes answers each "METHOD path" with a status and a body; anything else is a 404.
func routes(m map[string]struct {
	status int
	body   string
}) func(w http.ResponseWriter, r request) {
	return func(w http.ResponseWriter, r request) {
		a, ok := m[r.Method+" "+r.Path]
		if !ok {
			http.NotFound(w, nil)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	}
}

type route = struct {
	status int
	body   string
}

func TestLiveCheck_AVersionIsCopiedOnlyWhenItIsShapedLikeOne(t *testing.T) {
	for in, want := range map[string]string{
		"":                               "",
		"   ":                            "",
		"1.40.2.8395-c67dce28e":          "1.40.2.8395-c67dce28e",
		" 4.0.20.3014\n":                 "4.0.20.3014",
		"6.4.4.10685":                    "6.4.4.10685",
		"1.2":                            "1.2",
		"1.2.3.4.5":                      "1.2.3.4.5",
		"5.0.0+build7":                   "5.0.0+build7",
		"1":                              UnrecognizedVersion,
		"1.2.3.4.5.6":                    UnrecognizedVersion,
		"v1.2.3":                         UnrecognizedVersion,
		"nas.example.test":               UnrecognizedVersion,
		"192.0.2.10:8989":                UnrecognizedVersion,
		"1.2.3-nas.example.test":         UnrecognizedVersion,
		"1.2.3 on nas":                   UnrecognizedVersion,
		"1.2.3-0123456789abcdef0":        UnrecognizedVersion,
		"0123456789abcdef0123456789abcd": UnrecognizedVersion,
		"1.2.3/opt/app":                  UnrecognizedVersion,
		"123456.1":                       UnrecognizedVersion,
		"1.1234567":                      UnrecognizedVersion,
		"x1.2.3":                         UnrecognizedVersion,
		"1.2.3-":                         UnrecognizedVersion,
	} {
		if got := cleanVersion(in); got != want {
			t.Errorf("cleanVersion(%q) = %q, want %q", in, got, want)
		}
		if got := IsVersion(in); got != (want == in && in != "") {
			t.Errorf("IsVersion(%q) = %t", in, got)
		}
	}
}

func TestLiveCheck_AStringOutsideItsVocabularyIsDropped(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""}, {"  ", ""}, {"movie", "movie"}, {" Movie ", "movie"}, {"SHOW", "show"},
		{"Home Videos", OtherValue}, {"movies", OtherValue},
	} {
		if got := oneOf(c.in, sectionTypes...); got != c.want {
			t.Errorf("oneOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A vocabulary word is answered in the vocabulary's own spelling.
	if got := oneOf("rescanseries", commandNames...); got != "RescanSeries" {
		t.Errorf("oneOf(rescanseries) = %q, want RescanSeries", got)
	}
	got := Vocabulary()
	sort.Strings(got)
	want := []string{"RescanMovie", "RescanSeries", "aborted", "artist", "cancelled", "completed", "failed", "http-status",
		"movie", "orphaned", "other", "photo", "queued", "radarr", "refused-unspecific-request", "show", "sonarr",
		"started", "timeout", "unauthorized", "unparseable-response", "unreachable", "unrecognized"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Vocabulary() = %v\nwant %v", got, want)
	}
}

func TestLiveCheck_RootsAreRelatedToTargetPathsOnWholeComponents(t *testing.T) {
	paths := config.PathMap{{From: "/mnt/media", To: "/data"}}
	for _, c := range []struct {
		name               string
		roots, targets     []string
		inside, containing int
	}{
		{"equal", []string{"/mnt/media/movies"}, []string{"/data/movies"}, 1, 1},
		{"equal after cleaning", []string{"/mnt/media/movies/"}, []string{"/data/movies/"}, 1, 1},
		{"root inside a target path", []string{"/mnt/media/movies/kids"}, []string{"/data/movies"}, 1, 0},
		{"root containing a target path", []string{"/mnt/media"}, []string{"/data/movies"}, 0, 1},
		{"a sibling with a shared prefix is neither", []string{"/mnt/media/movies"}, []string{"/data/movies 2"}, 0, 0},
		{"unmapped root", []string{"/srv/other"}, []string{"/data/movies"}, 0, 0},
		{"a root is counted once", []string{"/mnt/media"}, []string{"/data/a", "/data/b", "/data"}, 1, 1},
		{"each root is counted", []string{"/mnt/media/a", "/mnt/media/b", "/x"}, []string{"/data/a", "/data/b/c"}, 1, 2},
		{"relative and empty paths take no part", []string{"", "mnt/media", "/mnt/media"}, []string{"", "data", "O:\\media"}, 0, 0},
		{"no targets", []string{"/mnt/media"}, nil, 0, 0},
	} {
		inside, containing := relate(paths, c.roots, c.targets)
		if inside != c.inside || containing != c.containing {
			t.Errorf("%s: inside %d containing %d, want %d and %d", c.name, inside, containing, c.inside, c.containing)
		}
	}
}

func TestLiveCheck_PlexIdentityCarriesTheVersionAndNothingElse(t *testing.T) {
	f := newFake(t, routes(map[string]route{
		"GET /identity": {200, `{"MediaContainer":{"size":1,"claimed":true,"machineIdentifier":"0123456789abcdef","version":"1.40.2.8395-c67dce28e"}}`},
	}))
	got := plexAt(f).CheckIdentity(context.Background())
	if want := (PlexIdentity{Probe: Probe{Status: 200}, Version: "1.40.2.8395-c67dce28e"}); got != want {
		t.Errorf("identity = %+v, want %+v", got, want)
	}
	if !got.OK() {
		t.Errorf("a 200 is not OK")
	}

	for name, c := range map[string]struct {
		route route
		want  PlexIdentity
	}{
		"a version that is a host name": {route{200, `{"MediaContainer":{"version":"nas.example.test"}}`},
			PlexIdentity{Probe: Probe{Status: 200}, Version: UnrecognizedVersion}},
		"no version":      {route{200, `{"MediaContainer":{}}`}, PlexIdentity{Probe: Probe{Status: 200}}},
		"no container":    {route{200, `{"version":"1.2.3"}`}, PlexIdentity{Probe: Probe{Status: 200, FailureClass: ClassUnparseable}}},
		"not json":        {route{200, `<html>`}, PlexIdentity{Probe: Probe{Status: 200, FailureClass: ClassUnparseable}}},
		"two documents":   {route{200, `{"MediaContainer":{"version":"1.2.3"}} {}`}, PlexIdentity{Probe: Probe{Status: 200, FailureClass: ClassUnparseable}}},
		"unauthorized":    {route{401, `{"MediaContainer":{"version":"1.2.3"}}`}, PlexIdentity{Probe: Probe{Status: 401, FailureClass: ClassUnauthorized}}},
		"a server error":  {route{503, `{"MediaContainer":{"version":"1.2.3"}}`}, PlexIdentity{Probe: Probe{Status: 503, FailureClass: ClassStatus}}},
		"another success": {route{204, ``}, PlexIdentity{Probe: Probe{Status: 204, FailureClass: ClassUnparseable}}},
	} {
		f := newFake(t, routes(map[string]route{"GET /identity": c.route}))
		if got := plexAt(f).CheckIdentity(context.Background()); got != c.want {
			t.Errorf("%s: identity = %+v, want %+v", name, got, c.want)
		}
	}
}

func TestLiveCheck_PlexSectionsAreCountedAndRelatedToTheRoots(t *testing.T) {
	f := newFake(t, routes(map[string]route{"GET /library/sections/all": {200, `{"MediaContainer":{"Directory":[
	  {"key":"1","type":"movie","title":"Movies","Location":[{"id":1,"path":"/data/movies"}]},
	  {"key":2,"type":"Show","title":"Shows","Location":[{"id":2,"path":"/data/tv"},{"id":3,"path":"/data/more tv"}]},
	  {"key":"all","type":"Home Videos","title":"Odd","Location":[{"id":4,"path":"/data/odd"},{"id":5}]},
	  {"key":"04","title":"Bare"}
	]}}`}}))
	roots := []string{"/mnt/media/movies", "/mnt/media/tv/kids", "/mnt/media", "/srv/elsewhere"}
	got := plexAt(f).CheckSections(context.Background(), roots)
	want := PlexSections{
		Probe: Probe{Status: 200},
		Sections: []PlexSection{
			{Type: "movie", Locations: 1, EveryLocationHasPath: true, KeyIsSectionNumber: true},
			{Type: "show", Locations: 2, EveryLocationHasPath: true, KeyIsSectionNumber: true},
			{Type: OtherValue, Locations: 2, EveryLocationHasPath: false, KeyIsSectionNumber: false},
			{Type: "", Locations: 0, EveryLocationHasPath: true, KeyIsSectionNumber: false},
		},
		Roots: 4, RootsInsideALocation: 2, RootsContainingALocation: 2,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sections = %+v\nwant %+v", got, want)
	}
	reqs := f.seen()
	if len(reqs) != 1 || reqs[0].Method != "GET" || reqs[0].Header.Get("X-Plex-Token") != plexSecret || reqs[0].RawQuery != "" {
		t.Errorf("the request was %+v, want one GET carrying the token in its header", reqs)
	}

	for name, c := range map[string]struct {
		route route
		want  Probe
	}{
		"unauthorized": {route{403, `{}`}, Probe{Status: 403, FailureClass: ClassUnauthorized}},
		"no container": {route{200, `{}`}, Probe{Status: 200, FailureClass: ClassUnparseable}},
		"not json":     {route{200, `nope`}, Probe{Status: 200, FailureClass: ClassUnparseable}},
		"server error": {route{500, `{"MediaContainer":{"Directory":[{"key":"1"}]}}`}, Probe{Status: 500, FailureClass: ClassStatus}},
	} {
		f := newFake(t, routes(map[string]route{"GET /library/sections/all": c.route}))
		got := plexAt(f).CheckSections(context.Background(), roots)
		if want := (PlexSections{Probe: c.want, Roots: 4}); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: sections = %+v, want %+v", name, got, want)
		}
	}

	empty := newFake(t, routes(map[string]route{"GET /library/sections/all": {200, `{"MediaContainer":{}}`}}))
	if got := plexAt(empty).CheckSections(context.Background(), nil); !reflect.DeepEqual(got, PlexSections{Probe: Probe{Status: 200}}) {
		t.Errorf("an empty server = %+v", got)
	}
}

func TestLiveCheck_PlexSessionsAreCountedByWhatTheyCarry(t *testing.T) {
	f := newFake(t, routes(map[string]route{"GET /status/sessions": {200, `{"MediaContainer":{"Metadata":[
	  {"title":"a","Media":[{"Part":[{"file":"/data/movies/a.mkv"}]},{"Part":[{"file":"/data/movies/a2.mkv"}]}]},
	  {"title":"b","Media":[{"Part":[{"file":"/data/tv/b.mkv"},{"key":"/x"}]}]},
	  {"title":"c","Media":[{"Part":[{"file":"relative/c.mkv"}]}]},
	  {"title":"d","Media":[{}]},
	  {"title":"e"}
	]}}`}}))
	got := plexAt(f).CheckSessions(context.Background())
	want := PlexSessionsShape{Probe: Probe{Status: 200}, Sessions: 5, SessionsWithMedia: 4, SessionsWithPart: 3,
		SessionsEveryPartHasFile: 1, Parts: 5, PartsWithFile: 3}
	if got != want {
		t.Errorf("sessions = %+v\nwant %+v", got, want)
	}

	idle := newFake(t, routes(map[string]route{"GET /status/sessions": {200, `{"MediaContainer":{"size":0}}`}}))
	if got := plexAt(idle).CheckSessions(context.Background()); got != (PlexSessionsShape{Probe: Probe{Status: 200}}) {
		t.Errorf("an idle server = %+v", got)
	}
	for name, c := range map[string]struct {
		route route
		want  Probe
	}{
		"unauthorized": {route{401, `{}`}, Probe{Status: 401, FailureClass: ClassUnauthorized}},
		"no container": {route{200, `[]`}, Probe{Status: 200, FailureClass: ClassUnparseable}},
		"null":         {route{200, `{"MediaContainer":null}`}, Probe{Status: 200, FailureClass: ClassUnparseable}},
		"server error": {route{502, `{"MediaContainer":{"Metadata":[{}]}}`}, Probe{Status: 502, FailureClass: ClassStatus}},
	} {
		f := newFake(t, routes(map[string]route{"GET /status/sessions": c.route}))
		if got := plexAt(f).CheckSessions(context.Background()); got != (PlexSessionsShape{Probe: c.want}) {
			t.Errorf("%s: sessions = %+v, want only the probe %+v", name, got, c.want)
		}
	}
}

func TestLiveCheck_ATargetThatNeverAnswersIsATimeoutAndOneThatIsGoneIsUnreachable(t *testing.T) {
	shortTimeout(t, 100*time.Millisecond)
	hung := newFake(t, func(_ http.ResponseWriter, r request) { r.hang() })
	plex := plexAt(hung)
	if got := plex.CheckIdentity(context.Background()); got != (PlexIdentity{Probe: Probe{FailureClass: ClassTimeout}}) {
		t.Errorf("identity = %+v, want a timeout with no status", got)
	}
	if got := plex.CheckSessions(context.Background()); got != (PlexSessionsShape{Probe: Probe{FailureClass: ClassTimeout}}) {
		t.Errorf("sessions = %+v, want a timeout", got)
	}
	if got := sonarrAt(hung).CheckStatus(context.Background()); got != (ArrStatus{Probe: Probe{FailureClass: ClassTimeout}}) {
		t.Errorf("status = %+v, want a timeout", got)
	}
	if got := sonarrAt(hung).CheckLibrary(context.Background(), []string{"/mnt/media/tv"}); got != (ArrLibrary{Probe: Probe{FailureClass: ClassTimeout}, Roots: 1}) {
		t.Errorf("library = %+v, want a timeout", got)
	}
	w := sonarrAt(hung).CheckRescan(context.Background(), "/mnt/media/tv/Show")
	if w != (WriteCheck{Lookup: Probe{FailureClass: ClassTimeout}}) {
		t.Errorf("rescan = %+v, want a lookup timeout and nothing sent", w)
	}

	gone := newFake(t, func(http.ResponseWriter, request) {})
	gone.srv.Close()
	if got := radarrAt(gone).CheckStatus(context.Background()); got != (ArrStatus{Probe: Probe{FailureClass: ClassUnreachable}}) {
		t.Errorf("status = %+v, want unreachable", got)
	}
	if got := plexAt(gone).CheckRefresh(context.Background(), "/mnt/media/movies/Film"); got != (WriteCheck{Lookup: Probe{FailureClass: ClassUnreachable}}) {
		t.Errorf("refresh = %+v, want an unreachable lookup", got)
	}
}

func TestLiveCheck_PlexRefreshIsTheOnePartialRefreshRescanSends(t *testing.T) {
	f := newFake(t, plexSectionsHandler(sectionList))
	got := plexAt(f).CheckRefresh(context.Background(), "/mnt/media/tv/Show/Season 01")
	want := WriteCheck{Lookup: Probe{Status: 200}, OwnerFound: true, Requested: true, Request: Probe{Status: 200}, Accepted: true}
	if got != want {
		t.Errorf("refresh = %+v, want %+v", got, want)
	}
	posts := f.posts()
	if len(posts) != 1 || posts[0].Path != "/library/sections/2/refresh" || posts[0].RawQuery != "path=%2Fdata%2Ftv%2FShow%2FSeason+01" {
		t.Fatalf("the writes were %+v, want one partial refresh of section 2", posts)
	}

	// No section owns the directory: nothing is sent.
	f = newFake(t, plexSectionsHandler(sectionList))
	if got := plexAt(f).CheckRefresh(context.Background(), "/srv/elsewhere/x"); got != (WriteCheck{Lookup: Probe{Status: 200}}) {
		t.Errorf("no owner: %+v", got)
	}
	if len(f.posts()) != 0 {
		t.Errorf("a directory no section owns still sent %+v", f.posts())
	}

	// The server refuses the refresh: the lookup succeeded, the request did not.
	f = newFake(t, func(w http.ResponseWriter, r request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		plexSectionsHandler(sectionList)(w, r)
	})
	got = plexAt(f).CheckRefresh(context.Background(), "/mnt/media/movies/Film")
	want = WriteCheck{Lookup: Probe{Status: 200}, OwnerFound: true, Requested: true, Request: Probe{Status: 403, FailureClass: ClassUnauthorized}}
	if got != want {
		t.Errorf("a refused refresh = %+v, want %+v", got, want)
	}

	// The lookup itself is refused.
	f = newFake(t, func(w http.ResponseWriter, _ request) { w.WriteHeader(http.StatusUnauthorized) })
	if got := plexAt(f).CheckRefresh(context.Background(), "/mnt/media/movies/Film"); got != (WriteCheck{Lookup: Probe{Status: 401, FailureClass: ClassUnauthorized}}) {
		t.Errorf("a refused lookup = %+v", got)
	}

	// Rescan still answers what it answered before.
	f = newFake(t, plexSectionsHandler(sectionList))
	if res := plexAt(f).Rescan(context.Background(), "/mnt/media/movies/Film"); !res.Sent || res.Failure != nil || res.NoOwner {
		t.Errorf("Rescan = %+v", res)
	}
}

func TestLiveCheck_ArrStatusCarriesTheVersionAndWhichApplicationAnswered(t *testing.T) {
	f := newFake(t, routes(map[string]route{"GET /api/v3/system/status": {200,
		`{"appName":"Sonarr","instanceName":"Living Room","version":"4.0.20.3014","startupPath":"/opt/app","urlBase":"/x"}`}}))
	got := sonarrAt(f).CheckStatus(context.Background())
	if want := (ArrStatus{Probe: Probe{Status: 200}, Version: "4.0.20.3014", App: "sonarr"}); got != want {
		t.Errorf("status = %+v, want %+v", got, want)
	}
	if reqs := f.seen(); len(reqs) != 1 || reqs[0].Header.Get("X-Api-Key") != sonarrSecret || reqs[0].RawQuery != "" {
		t.Errorf("the request was %+v, want one GET carrying the key in its header", reqs)
	}
	for name, c := range map[string]struct {
		route route
		want  ArrStatus
	}{
		"radarr":              {route{200, `{"appName":"Radarr","version":"6.4.4.10685"}`}, ArrStatus{Probe: Probe{Status: 200}, Version: "6.4.4.10685", App: "radarr"}},
		"another application": {route{200, `{"appName":"Living Room","version":"nas.example.test"}`}, ArrStatus{Probe: Probe{Status: 200}, Version: UnrecognizedVersion, App: OtherValue}},
		"nothing":             {route{200, `{}`}, ArrStatus{Probe: Probe{Status: 200}}},
		"not json":            {route{200, `<html>login</html>`}, ArrStatus{Probe: Probe{Status: 200, FailureClass: ClassUnparseable}}},
		"unauthorized":        {route{401, `{"appName":"Sonarr","version":"4.0.20.3014"}`}, ArrStatus{Probe: Probe{Status: 401, FailureClass: ClassUnauthorized}}},
	} {
		f := newFake(t, routes(map[string]route{"GET /api/v3/system/status": c.route}))
		if got := radarrAt(f).CheckStatus(context.Background()); got != c.want {
			t.Errorf("%s: status = %+v, want %+v", name, got, c.want)
		}
	}
}

func TestLiveCheck_ArrLibraryIsCountedByWhatEachItemCarries(t *testing.T) {
	f := newFake(t, routes(map[string]route{"GET /api/v3/movie": {200, `[
	  {"id": 11, "title": "Film", "path": "/movies/Film"},
	  {"id": "12", "title": "Film 2", "path": "/movies/Film 2"},
	  {"id": 13, "title": "Film 3"},
	  {"id": null, "title": "Film 4", "path": "/movies/Film 4"},
	  {"id": 15, "title": "Film 5", "path": "relative/Film 5"},
	  {"id": 0, "title": "Film 6", "path": "/movies/Film 6"},
	  {"id": 17, "title": "Film 7", "path": null},
	  {"id": 18.5, "title": "Film 8", "path": ""},
	  {"id": -3, "title": "Film 9", "path": "/movies/Film 9"},
	  "not an object"
	]`}}))
	got := radarrAt(f).CheckLibrary(context.Background(), []string{"/mnt/media/movies", "/mnt/media/movies/Film/extras", "/srv/elsewhere"})
	want := ArrLibrary{Probe: Probe{Status: 200}, Items: 10,
		ItemsWithIntegerID: 6, ItemsWithStringPath: 7, ItemsUsable: 1, ItemsTheClientReads: 7,
		Roots: 3, RootsContainingAnItem: 1, RootsInsideAnItem: 1}
	if got != want {
		t.Errorf("library = %+v\nwant %+v", got, want)
	}
	if reqs := f.seen(); len(reqs) != 1 || reqs[0].Header.Get("X-Api-Key") != radarrSecret {
		t.Errorf("the request was %+v", reqs)
	}

	series := newFake(t, routes(map[string]route{"GET /api/v3/series": {200, seriesList}}))
	got = sonarrAt(series).CheckLibrary(context.Background(), []string{"/mnt/media/tv"})
	want = ArrLibrary{Probe: Probe{Status: 200}, Items: 2, ItemsWithIntegerID: 2, ItemsWithStringPath: 2, ItemsUsable: 2,
		ItemsTheClientReads: 2, Roots: 1, RootsContainingAnItem: 1}
	if got != want {
		t.Errorf("series = %+v\nwant %+v", got, want)
	}

	for name, c := range map[string]struct {
		route route
		want  Probe
	}{
		"an object, not a list": {route{200, `{"id":1,"path":"/movies/Film"}`}, Probe{Status: 200, FailureClass: ClassUnparseable}},
		"a list cut short":      {route{200, `[{"id":1,"path":"/movies/Film"},`}, Probe{Status: 200, FailureClass: ClassUnparseable}},
		"a list never closed":   {route{200, `[{"id":1,"path":"/movies/Film"}`}, Probe{Status: 200, FailureClass: ClassUnparseable}},
		"empty body":            {route{200, ``}, Probe{Status: 200, FailureClass: ClassUnparseable}},
		"unauthorized":          {route{401, `[{"id":1,"path":"/movies/Film"}]`}, Probe{Status: 401, FailureClass: ClassUnauthorized}},
	} {
		f := newFake(t, routes(map[string]route{"GET /api/v3/movie": c.route}))
		got := radarrAt(f).CheckLibrary(context.Background(), []string{"/mnt/media/movies"})
		if want := (ArrLibrary{Probe: c.want, Roots: 1}); got != want {
			t.Errorf("%s: library = %+v, want only the probe %+v", name, got, c.want)
		}
	}

	empty := newFake(t, routes(map[string]route{"GET /api/v3/movie": {200, `[]`}}))
	if got := radarrAt(empty).CheckLibrary(context.Background(), nil); got != (ArrLibrary{Probe: Probe{Status: 200}}) {
		t.Errorf("an empty library = %+v", got)
	}
}

func TestLiveCheck_ArrRescanIsTheOneCommandRescanSendsAndRecordsItsAnswer(t *testing.T) {
	command := func(status int, body string) func(w http.ResponseWriter, r request) {
		return func(w http.ResponseWriter, r request) {
			if r.Method == http.MethodPost && r.Path == "/api/v3/command" {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
				return
			}
			arrList("/api/v3/series", seriesList)(w, r)
		}
	}
	f := newFake(t, command(201, `{"id":5,"name":"RescanSeries","commandName":"Rescan Series","status":"queued","body":{"seriesId":21}}`))
	got := sonarrAt(f).CheckRescan(context.Background(), "/mnt/media/tv/Show/Season 01")
	want := WriteCheck{Lookup: Probe{Status: 200}, OwnerFound: true, Requested: true, Request: Probe{Status: 201}, Accepted: true,
		CommandName: "RescanSeries", CommandStatus: "queued", AnswerParsed: true}
	if got != want {
		t.Errorf("rescan = %+v\nwant %+v", got, want)
	}
	posts := f.posts()
	if len(posts) != 1 || !strings.Contains(posts[0].Body, `"seriesId":21`) || !strings.Contains(posts[0].Body, `"name":"RescanSeries"`) {
		t.Fatalf("the writes were %+v, want one RescanSeries for series 21", posts)
	}

	for name, c := range map[string]struct {
		status int
		body   string
		want   WriteCheck
	}{
		"an answer with unknown words": {200, `{"name":"Living Room","status":"see nas.example.test"}`,
			WriteCheck{Lookup: Probe{Status: 200}, OwnerFound: true, Requested: true, Request: Probe{Status: 200}, Accepted: true,
				CommandName: OtherValue, CommandStatus: OtherValue, AnswerParsed: true}},
		"an answer with neither field": {200, `{}`,
			WriteCheck{Lookup: Probe{Status: 200}, OwnerFound: true, Requested: true, Request: Probe{Status: 200}, Accepted: true, AnswerParsed: true}},
		"an answer that is not json": {202, `accepted`,
			WriteCheck{Lookup: Probe{Status: 200}, OwnerFound: true, Requested: true, Request: Probe{Status: 202}, Accepted: true}},
		"a refused command": {401, `{"name":"RescanSeries","status":"queued"}`,
			WriteCheck{Lookup: Probe{Status: 200}, OwnerFound: true, Requested: true, Request: Probe{Status: 401, FailureClass: ClassUnauthorized}}},
		"a failed command": {500, `{"name":"RescanSeries","status":"failed"}`,
			WriteCheck{Lookup: Probe{Status: 200}, OwnerFound: true, Requested: true, Request: Probe{Status: 500, FailureClass: ClassStatus}}},
	} {
		f := newFake(t, command(c.status, c.body))
		if got := sonarrAt(f).CheckRescan(context.Background(), "/mnt/media/tv/Show"); got != c.want {
			t.Errorf("%s: rescan = %+v\nwant %+v", name, got, c.want)
		}
	}

	// No series owns the directory: nothing is sent.
	f = newFake(t, command(201, `{}`))
	if got := sonarrAt(f).CheckRescan(context.Background(), "/mnt/media/tv/Unknown"); got != (WriteCheck{Lookup: Probe{Status: 200}}) {
		t.Errorf("no owner: %+v", got)
	}
	if len(f.posts()) != 0 {
		t.Errorf("a directory no series owns still sent %+v", f.posts())
	}

	// The lookup fails: nothing is sent, and the failure is the lookup's.
	f = newFake(t, func(w http.ResponseWriter, _ request) { w.WriteHeader(http.StatusInternalServerError) })
	if got := sonarrAt(f).CheckRescan(context.Background(), "/mnt/media/tv/Show"); got != (WriteCheck{Lookup: Probe{Status: 500, FailureClass: ClassStatus}}) {
		t.Errorf("a failed lookup: %+v", got)
	}

	// Rescan still answers what it answered before, and reads no answer body.
	f = newFake(t, command(201, `not json`))
	if res := sonarrAt(f).Rescan(context.Background(), "/mnt/media/tv/Show"); !res.Sent || res.Failure != nil || res.NoOwner {
		t.Errorf("Rescan = %+v", res)
	}
}

func TestLiveCheck_AProbeTakesTheFailuresStatusWhereItHasOne(t *testing.T) {
	for _, c := range []struct {
		status int
		f      *failure
		want   Probe
	}{
		{200, nil, Probe{Status: 200}},
		{0, &failure{class: ClassTimeout}, Probe{FailureClass: ClassTimeout}},
		{200, &failure{class: ClassUnparseable}, Probe{Status: 200, FailureClass: ClassUnparseable}},
		{0, &failure{class: ClassStatus, status: 500}, Probe{Status: 500, FailureClass: ClassStatus}},
	} {
		if got := probeOf(c.status, c.f); got != c.want {
			t.Errorf("probeOf(%d, %+v) = %+v, want %+v", c.status, c.f, got, c.want)
		}
	}
	if !(Probe{Status: 200}).OK() || (Probe{Status: 200, FailureClass: ClassUnparseable}).OK() {
		t.Errorf("OK does not follow the failure class")
	}
}
