package main

// These tests drive the live check against httptest fakes and nothing else: no goal, test or
// gate contacts a real Plex, Sonarr or Radarr (brief T49). Every fake answers the documented
// shape with a CANARY planted in each field that describes a real installation - titles,
// paths, host names, addresses, machine identifiers, user and device names - and the tests
// assert that no canary, the fake's own host and port, or the resolved credential reaches the
// report or anything the tool prints.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/corpus"
	"github.com/NSchatz/holdfast/internal/mediaclient"
	"github.com/NSchatz/holdfast/internal/secret"
)

// canary marks every planted identity. No report and no output line may carry it.
const canary = "CANARY"

const (
	plexToken = "RESOLVED-PLEX-TOKEN-zq81-MUST-NEVER-BE-WRITTEN"
	arrKey    = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"
)

type seen struct {
	Method, Path, Query, Body string
	Header                    http.Header
}

// service is one fake: it records every request and answers from routes, keyed by
// "METHOD path". An unknown route is a 404 carrying a canary.
type service struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []seen
}

type answer struct {
	status int
	body   string
	hang   bool
}

func newService(t *testing.T, routes map[string]answer) *service {
	t.Helper()
	s := &service{}
	release := make(chan struct{})
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.requests = append(s.requests, seen{r.Method, r.URL.Path, r.URL.RawQuery, string(body), r.Header.Clone()})
		s.mu.Unlock()
		a, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			a = answer{status: http.StatusNotFound, body: canary + "-NOT-FOUND at http://" + r.Host}
		}
		if a.hang {
			<-release
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Plex-Protocol", canary+"-HEADER")
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(func() {
		close(release)
		s.srv.Close()
	})
	return s
}

func (s *service) all() []seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seen(nil), s.requests...)
}

func (s *service) writes() []seen {
	var out []seen
	for _, r := range s.all() {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

// The documented shapes, with a canary in every field that names something real.
const (
	plexIdentityBody = `{"MediaContainer":{"size":1,"claimed":true,"machineIdentifier":"CANARY0123456789abcdef","version":"1.40.2.8395-c67dce28e"}}`
	plexSectionsBody = `{"MediaContainer":{"size":3,"title1":"CANARY Library","Directory":[
	  {"key":"1","type":"movie","title":"CANARY Movies","uuid":"CANARY-uuid-1","agent":"tv.plex.agents.movie",
	   "Location":[{"id":1,"path":"/CANARY-plex/movies"}]},
	  {"key":"2","type":"show","title":"CANARY Shows","uuid":"CANARY-uuid-2",
	   "Location":[{"id":2,"path":"/CANARY-plex/tv"},{"id":3,"path":"/CANARY-plex/more tv"}]},
	  {"key":"3","type":"CANARY-type","title":"CANARY Odd","Location":[{"id":4}]}
	]}}`
	plexSessionsBody = `{"MediaContainer":{"size":2,"Metadata":[
	  {"title":"CANARY Film","grandparentTitle":"CANARY Show","librarySectionTitle":"CANARY Movies",
	   "User":{"id":"1","title":"CANARY-user","thumb":"https://CANARY.example/avatar"},
	   "Player":{"title":"CANARY-device","address":"10.11.12.13","machineIdentifier":"CANARY-player","remotePublicAddress":"198.51.100.7"},
	   "Session":{"id":"CANARY-session","location":"lan"},
	   "Media":[{"id":1,"Part":[{"id":1,"file":"/CANARY-plex/movies/CANARY Film/CANARY Film.mkv"}]}]},
	  {"title":"CANARY Live","User":{"title":"CANARY-user-2"},
	   "Media":[{"id":2,"Part":[{"id":2,"key":"/CANARY/part"},{"id":3,"file":"/CANARY-plex/tv/CANARY.mkv"}]}]},
	  {"title":"CANARY Bare","User":{"title":"CANARY-user-3"}}
	]}}`
	sonarrStatusBody = `{"appName":"Sonarr","instanceName":"CANARY-instance","version":"4.0.20.3014","osName":"CANARY-os",
	  "startupPath":"/CANARY/opt/Sonarr","appData":"/CANARY/config","urlBase":"/CANARY-base","branch":"main"}`
	radarrStatusBody = `{"appName":"Radarr","instanceName":"CANARY-instance","version":"6.4.4.10685","osName":"CANARY-os",
	  "startupPath":"/CANARY/opt/Radarr","appData":"/CANARY/config"}`
	seriesBody = `[
	  {"id":21,"title":"CANARY Show","sortTitle":"canary show","path":"/CANARY-arr/tv/CANARY Show","tvdbId":123456},
	  {"id":22,"title":"CANARY Show 2","path":"/CANARY-arr/tv/CANARY Show 2/"},
	  {"id":23,"title":"CANARY Elsewhere","path":"/CANARY-other/CANARY Elsewhere"}
	]`
	movieBody = `[
	  {"id":11,"title":"CANARY Film","path":"/CANARY-arr/movies/CANARY Film","tmdbId":654321},
	  {"id":12,"title":"CANARY Film 2","path":"/CANARY-arr/movies/CANARY Film 2"}
	]`
	sonarrCommandBody = `{"id":901,"name":"RescanSeries","commandName":"Rescan Series","message":"CANARY message",
	  "body":{"seriesId":21,"sendUpdatesToClient":true,"clientUserAgent":"CANARY-agent"},"status":"queued","trigger":"manual"}`
	radarrCommandBody = `{"id":902,"name":"RescanMovie","commandName":"Rescan Movie","message":"CANARY message","status":"started"}`
)

func plexRoutes() map[string]answer {
	return map[string]answer{
		"GET /identity":                      {status: 200, body: plexIdentityBody},
		"GET /library/sections/all":          {status: 200, body: plexSectionsBody},
		"GET /status/sessions":               {status: 200, body: plexSessionsBody},
		"POST /library/sections/1/refresh":   {status: 200},
		"POST /library/sections/2/refresh":   {status: 200},
		"POST /library/sections/all/refresh": {status: 200},
	}
}

func sonarrRoutes() map[string]answer {
	return map[string]answer{
		"GET /api/v3/system/status": {status: 200, body: sonarrStatusBody},
		"GET /api/v3/series":        {status: 200, body: seriesBody},
		"POST /api/v3/command":      {status: 201, body: sonarrCommandBody},
	}
}

func radarrRoutes() map[string]answer {
	return map[string]answer{
		"GET /api/v3/system/status": {status: 200, body: radarrStatusBody},
		"GET /api/v3/movie":         {status: 200, body: movieBody},
		"POST /api/v3/command":      {status: 201, body: radarrCommandBody},
	}
}

// lab is one configured run: a configuration naming the fake, its credential in a file, and
// where the report goes.
type lab struct {
	t          *testing.T
	service    string
	address    string
	credential string
	cfg, out   string
}

// newLab writes a configuration whose library roots, path map and credential file are all
// canaries, pointing service at address.
func newLab(t *testing.T, service, address, credential string) *lab {
	t.Helper()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "credential")
	if err := os.WriteFile(keyFile, []byte(credential+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	credKey := map[string]string{servicePlex: "plex_token", serviceSonarr: "sonarr_api_key", serviceRadarr: "radarr_api_key"}[service]
	yaml := "library_roots:\n" +
		"  - /mnt/CANARY-holdfast/movies\n" +
		"  - /mnt/CANARY-holdfast/tv\n" +
		"  - /mnt/CANARY-unmapped/music\n" +
		service + "_url: " + address + "\n" +
		credKey + ": file:" + keyFile + "\n" +
		service + "_path_map:\n" +
		"  - from: /mnt/CANARY-holdfast\n" +
		"    to: " + map[string]string{servicePlex: "/CANARY-plex", serviceSonarr: "/CANARY-arr", serviceRadarr: "/CANARY-arr"}[service] + "\n"
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return &lab{t: t, service: service, address: address, credential: credential, cfg: cfg,
		out: filepath.Join(dir, service+"-report.json")}
}

// run runs the tool and returns its exit code and everything it printed.
func (l *lab) run(extra ...string) (code int, output string) {
	l.t.Helper()
	var stdout, stderr bytes.Buffer
	args := append([]string{"--service", l.service, "--config", l.cfg, "--out", l.out}, extra...)
	code = run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

// report reads the written report back, as bytes and decoded.
func (l *lab) report() ([]byte, Report) {
	l.t.Helper()
	b, err := os.ReadFile(l.out)
	if err != nil {
		l.t.Fatalf("no report was written: %v", err)
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		l.t.Fatalf("the report is not JSON: %v\n%s", err, b)
	}
	return b, r
}

// assertNoIdentity fails if text carries a canary, the credential, or any part of the
// fake's address.
func (l *lab) assertNoIdentity(what, text string) {
	l.t.Helper()
	u, err := url.Parse(l.address)
	if err != nil {
		l.t.Fatal(err)
	}
	low := strings.ToLower(text)
	for name, needle := range map[string]string{
		"a canary":                canary,
		"the resolved credential": l.credential,
		"the configured address":  l.address,
		"the host and port":       u.Host,
		"the host":                u.Hostname(),
		"the port":                u.Port(),
		"a path separator run":    "/mnt/",
		"the config directory":    filepath.Dir(l.cfg),
	} {
		if needle != "" && strings.Contains(low, strings.ToLower(needle)) {
			l.t.Errorf("%s carries %s (%q):\n%s", what, name, needle, text)
		}
	}
}

// writtenClean asserts a report was written, passes Verify, and that neither it nor the
// output carries an identity. It returns the decoded report.
func (l *lab) writtenClean(code int, output string) Report {
	l.t.Helper()
	if code != exitWritten {
		l.t.Fatalf("exit %d, want %d (a report is written even when a request failed):\n%s", code, exitWritten, output)
	}
	b, r := l.report()
	l.assertNoIdentity("the report", string(b))
	l.assertNoIdentity("the tool's output", output)
	if err := Verify(b); err != nil {
		l.t.Errorf("the written report does not pass Verify: %v\n%s", err, b)
	}
	if !strings.HasSuffix(output, "clientreport: wrote the report\n") {
		l.t.Errorf("the output does not end by saying a report was written:\n%s", output)
	}
	return r
}

func fixClock(t *testing.T) {
	t.Helper()
	prev := now
	now = func() time.Time { return time.Date(2026, 10, 2, 23, 30, 0, 0, time.FixedZone("west", -5*3600)) }
	t.Cleanup(func() { now = prev })
}

func boolPtr(t *testing.T, name string, p *bool, want bool) {
	t.Helper()
	if p == nil || *p != want {
		t.Errorf("%s = %v, want %t", name, p, want)
	}
}

func TestClientReport_PlexReportCarriesNoIdentity(t *testing.T) {
	fixClock(t)
	plex := newService(t, plexRoutes())
	l := newLab(t, servicePlex, plex.srv.URL, plexToken)
	code, output := l.run()
	r := l.writtenClean(code, output)

	if r.Schema != Schema || r.Service != servicePlex || r.Arr != nil || r.Plex == nil {
		t.Fatalf("report header: %+v", r)
	}
	if r.Date != "2026-10-03" {
		t.Errorf("date = %q, want the UTC date 2026-10-03", r.Date)
	}
	if r.Holdfast.Version != "0.0.0-dev" || r.Holdfast.Commit != "unknown" {
		t.Errorf("holdfast build = %+v, want the unstamped build's 0.0.0-dev and unknown", r.Holdfast)
	}
	if want := (ConfigFacts{URLScheme: "http", URLHasBasePath: false, PathMapEntries: 1, LibraryRoots: 3}); r.Config != want {
		t.Errorf("config facts = %+v, want %+v", r.Config, want)
	}
	boolPtr(t, "credential_accepted", r.CredentialAccepted, true)

	okReq := func(label string) Request { return Request{Request: label, OK: true, Status: 200} }
	if want := (PlexIdentity{Request: okReq("GET /identity"), Version: "1.40.2.8395-c67dce28e"}); r.Plex.Identity != want {
		t.Errorf("identity = %+v, want %+v", r.Plex.Identity, want)
	}
	sec := r.Plex.Sections
	if sec.Request != okReq("GET /library/sections/all") || sec.Count != 3 || len(sec.Sections) != 3 {
		t.Fatalf("sections = %+v", sec)
	}
	wantSections := []PlexSection{
		{Type: "movie", Locations: 1, EveryLocationCarriesAPath: true, KeyIsANumber: true},
		{Type: "show", Locations: 2, EveryLocationCarriesAPath: true, KeyIsANumber: true},
		{Type: "other", Locations: 1, EveryLocationCarriesAPath: false, KeyIsANumber: true},
	}
	for i, want := range wantSections {
		if sec.Sections[i] != want {
			t.Errorf("section %d = %+v, want %+v", i, sec.Sections[i], want)
		}
	}
	if sec.EveryLocationCarriesAPath || !sec.EverySectionKeyIsANumber {
		t.Errorf("every_location_carries_a_path = %t (want false: one location has none), every_section_key_is_a_number = %t (want true)",
			sec.EveryLocationCarriesAPath, sec.EverySectionKeyIsANumber)
	}
	// Two of the three roots map onto a section location exactly; the third is unmapped.
	if sec.LibraryRootsInsideALocation != 2 || sec.LibraryRootsContainingOne != 2 || !sec.ALibraryRootMapsIntoASection {
		t.Errorf("roots: inside %d containing %d maps %t, want 2, 2, true",
			sec.LibraryRootsInsideALocation, sec.LibraryRootsContainingOne, sec.ALibraryRootMapsIntoASection)
	}
	ses := r.Plex.Sessions
	wantSes := PlexSessions{Request: okReq("GET /status/sessions"), Count: 3, SessionsWithMedia: 2, SessionsWithPart: 2,
		SessionsEveryPartHasFile: 1, Parts: 3, PartsWithFile: 2}
	every := ses.EverySessionCarriesPartFile
	ses.EverySessionCarriesPartFile = nil
	if ses != wantSes {
		t.Errorf("sessions = %+v, want %+v", ses, wantSes)
	}
	boolPtr(t, "every_session_carries_part_file", every, false)
	if r.Plex.Refresh != nil {
		t.Errorf("a read-only run recorded a refresh: %+v", r.Plex.Refresh)
	}

	// The requests went through the shipped client: the token in its header, never in a URL.
	reqs := plex.all()
	if len(reqs) != 3 {
		t.Fatalf("the fake saw %d requests, want 3: %+v", len(reqs), reqs)
	}
	for _, q := range reqs {
		if q.Header.Get("X-Plex-Token") != plexToken || q.Query != "" || q.Method != http.MethodGet {
			t.Errorf("request %s %s?%s: token header %q", q.Method, q.Path, q.Query, q.Header.Get("X-Plex-Token"))
		}
	}
	for _, line := range []string{
		"clientreport: plex: GET /identity: ok (status 200)\n",
		"clientreport: plex: GET /library/sections/all: ok (status 200)\n",
		"clientreport: plex: GET /status/sessions: ok (status 200)\n",
	} {
		if !strings.Contains(output, line) {
			t.Errorf("the output lacks %q:\n%s", line, output)
		}
	}
	if strings.Contains(output, "refused") {
		t.Errorf("an accepted credential was reported as refused:\n%s", output)
	}
}

// With nothing playing the sessions answer settles nothing, and the report says so with a
// null rather than a true.
func TestClientReport_PlexWithNoSessionRecordsNothingObserved(t *testing.T) {
	routes := map[string]answer{
		"GET /CANARY-base/identity":             {status: 200, body: plexIdentityBody},
		"GET /CANARY-base/status/sessions":      {status: 200, body: `{"MediaContainer":{"size":0}}`},
		"GET /CANARY-base/library/sections/all": {status: 200, body: `{"MediaContainer":{"size":0}}`},
	}
	plex := newService(t, routes)
	l := newLab(t, servicePlex, plex.srv.URL+"/CANARY-base/", plexToken)
	code, output := l.run()
	r := l.writtenClean(code, output)
	if r.Plex.Sessions.EverySessionCarriesPartFile != nil || r.Plex.Sessions.Count != 0 {
		t.Errorf("sessions = %+v, want a null every_session_carries_part_file and a zero count", r.Plex.Sessions)
	}
	sec := r.Plex.Sections
	if sec.Count != 0 || sec.Sections == nil || len(sec.Sections) != 0 || sec.ALibraryRootMapsIntoASection ||
		!sec.EveryLocationCarriesAPath || !sec.EverySectionKeyIsANumber {
		t.Errorf("sections = %+v, want an empty list, no root mapped, and both every_ facts vacuously true", sec)
	}
	if !r.Config.URLHasBasePath {
		t.Errorf("url_has_base_path = false for an address with a base path")
	}
	for _, q := range plex.all() {
		if !strings.HasPrefix(q.Path, "/CANARY-base/") {
			t.Errorf("request %s did not go under the configured base path", q.Path)
		}
	}
}

func TestClientReport_SonarrReportCarriesNoIdentity(t *testing.T) {
	sonarr := newService(t, sonarrRoutes())
	l := newLab(t, serviceSonarr, sonarr.srv.URL, arrKey)
	code, output := l.run()
	r := l.writtenClean(code, output)

	if r.Service != serviceSonarr || r.Plex != nil || r.Arr == nil {
		t.Fatalf("report header: %+v", r)
	}
	boolPtr(t, "credential_accepted", r.CredentialAccepted, true)
	if want := (ArrStatus{Request: Request{Request: "GET /api/v3/system/status", OK: true, Status: 200}, Version: "4.0.20.3014", App: "sonarr"}); r.Arr.Status != want {
		t.Errorf("status = %+v, want %+v", r.Arr.Status, want)
	}
	// Three series, two of them under the one root the path map carries to /CANARY-arr/tv.
	want := ArrLibrary{Request: Request{Request: "GET /api/v3/series", OK: true, Status: 200}, Count: 3,
		ItemsWithIntegerID: 3, ItemsWithStringPath: 3, ItemsUsable: 3, ItemsTheClientReads: 3,
		EveryItemHasIntegerIDAndPath: true, LibraryRootsContainingAnItem: 1, LibraryRootsInsideAnItem: 0,
		ALibraryRootMapsOntoTheirPath: true}
	if r.Arr.Library != want {
		t.Errorf("library = %+v, want %+v", r.Arr.Library, want)
	}
	if r.Arr.Rescan != nil {
		t.Errorf("a read-only run recorded a rescan: %+v", r.Arr.Rescan)
	}
	reqs := sonarr.all()
	if len(reqs) != 2 {
		t.Fatalf("the fake saw %d requests, want 2: %+v", len(reqs), reqs)
	}
	for _, q := range reqs {
		if q.Header.Get("X-Api-Key") != arrKey || q.Query != "" || q.Method != http.MethodGet {
			t.Errorf("request %s %s?%s: key header %q", q.Method, q.Path, q.Query, q.Header.Get("X-Api-Key"))
		}
	}
	if !strings.Contains(output, "clientreport: sonarr: GET /api/v3/series: ok (status 200)\n") {
		t.Errorf("the output lacks the series line:\n%s", output)
	}
}

func TestClientReport_RadarrReportCarriesNoIdentity(t *testing.T) {
	routes := radarrRoutes()
	// A list whose items do not all carry what the client needs: a string id, a missing
	// path, a null id, a relative path and a zero id.
	routes["GET /api/v3/movie"] = answer{status: 200, body: `[
	  {"id":11,"title":"CANARY Film","path":"/CANARY-arr/movies/CANARY Film"},
	  {"id":"CANARY-12","title":"CANARY Film 2","path":"/CANARY-arr/movies/CANARY Film 2"},
	  {"id":13,"title":"CANARY Film 3"},
	  {"id":null,"title":"CANARY Film 4","path":"/CANARY-arr/movies/CANARY Film 4"},
	  {"id":15,"title":"CANARY Film 5","path":"CANARY-relative/Film 5"},
	  {"id":0,"title":"CANARY Film 6","path":"/CANARY-arr/movies/CANARY Film 6"},
	  {"id":17,"title":"CANARY Film 7","path":17}
	]`}
	radarr := newService(t, routes)
	l := newLab(t, serviceRadarr, radarr.srv.URL, arrKey)
	code, output := l.run()
	r := l.writtenClean(code, output)

	if r.Service != serviceRadarr || r.Arr == nil {
		t.Fatalf("report header: %+v", r)
	}
	if want := (ArrStatus{Request: Request{Request: "GET /api/v3/system/status", OK: true, Status: 200}, Version: "6.4.4.10685", App: "radarr"}); r.Arr.Status != want {
		t.Errorf("status = %+v, want %+v", r.Arr.Status, want)
	}
	want := ArrLibrary{Request: Request{Request: "GET /api/v3/movie", OK: true, Status: 200}, Count: 7,
		ItemsWithIntegerID: 5, ItemsWithStringPath: 5, ItemsUsable: 1, ItemsTheClientReads: 5,
		EveryItemHasIntegerIDAndPath: false, LibraryRootsContainingAnItem: 1, LibraryRootsInsideAnItem: 0,
		ALibraryRootMapsOntoTheirPath: true}
	if r.Arr.Library != want {
		t.Errorf("library = %+v, want %+v", r.Arr.Library, want)
	}
	if len(radarr.all()) != 2 || len(radarr.writes()) != 0 {
		t.Errorf("the fake saw %+v, want two GETs", radarr.all())
	}
}

// Every way a service can fail is recorded as a class and a status, the report is still
// written, and neither it nor the output carries an identity - including the error body the
// service sent and the address a Go request error would quote.
func TestClientReport_FailureModesAreRecordedAsClassesWithoutIdentity(t *testing.T) {
	prev := requestBound
	requestBound = 300 * time.Millisecond
	t.Cleanup(func() { requestBound = prev })

	errBody := `{"message":"CANARY failure at /CANARY/path","error":"CANARY"}`
	modes := []struct {
		name     string
		answer   answer
		closed   bool
		class    string
		status   int
		accepted *bool
	}{
		{name: "refused", closed: true, class: mediaclient.ClassUnreachable},
		{name: "401", answer: answer{status: 401, body: errBody}, class: mediaclient.ClassUnauthorized, status: 401, accepted: new(bool)},
		{name: "403", answer: answer{status: 403, body: errBody}, class: mediaclient.ClassUnauthorized, status: 403, accepted: new(bool)},
		{name: "500", answer: answer{status: 500, body: errBody}, class: mediaclient.ClassStatus, status: 500},
		{name: "redirect", answer: answer{status: 302, body: errBody}, class: mediaclient.ClassStatus, status: 302},
		{name: "unparseable", answer: answer{status: 200, body: `<html>CANARY login page</html>`}, class: mediaclient.ClassUnparseable, status: 200},
		{name: "timeout", answer: answer{hang: true}, class: mediaclient.ClassTimeout},
	}
	services := []struct {
		kind, credential string
		paths            []string
	}{
		{servicePlex, plexToken, []string{"GET /identity", "GET /library/sections/all", "GET /status/sessions"}},
		{serviceSonarr, arrKey, []string{"GET /api/v3/system/status", "GET /api/v3/series"}},
		{serviceRadarr, arrKey, []string{"GET /api/v3/system/status", "GET /api/v3/movie"}},
	}
	for _, svc := range services {
		for _, mode := range modes {
			t.Run(svc.kind+"/"+mode.name, func(t *testing.T) {
				routes := map[string]answer{}
				for _, p := range svc.paths {
					routes[p] = mode.answer
				}
				fake := newService(t, routes)
				address := fake.srv.URL
				if mode.closed {
					fake.srv.Close()
				}
				l := newLab(t, svc.kind, address, svc.credential)
				code, output := l.run()
				r := l.writtenClean(code, output)

				var got []Request
				if svc.kind == servicePlex {
					got = []Request{r.Plex.Identity.Request, r.Plex.Sections.Request, r.Plex.Sessions.Request}
					if r.Plex.Identity.Version != "" || r.Plex.Sections.Count != 0 || r.Plex.Sessions.Count != 0 ||
						r.Plex.Sections.EveryLocationCarriesAPath || r.Plex.Sections.EverySectionKeyIsANumber ||
						r.Plex.Sessions.EverySessionCarriesPartFile != nil {
						t.Errorf("a failed check recorded facts: %+v", r.Plex)
					}
				} else {
					got = []Request{r.Arr.Status.Request, r.Arr.Library.Request}
					if r.Arr.Status.Version != "" || r.Arr.Status.App != "" || r.Arr.Library.Count != 0 ||
						r.Arr.Library.EveryItemHasIntegerIDAndPath || r.Arr.Library.ALibraryRootMapsOntoTheirPath {
						t.Errorf("a failed check recorded facts: %+v", r.Arr)
					}
				}
				for i, q := range got {
					want := Request{Request: svc.paths[i], OK: false, Status: mode.status, FailureClass: mode.class}
					if q != want {
						t.Errorf("request %d = %+v, want %+v", i, q, want)
					}
					line := "clientreport: " + svc.kind + ": " + svc.paths[i] + ": " + mode.class + " (status "
					if !strings.Contains(output, line) {
						t.Errorf("the output lacks %q:\n%s", line, output)
					}
				}
				switch {
				case mode.accepted == nil && r.CredentialAccepted != nil:
					t.Errorf("credential_accepted = %t, want null: the mode decides nothing about it", *r.CredentialAccepted)
				case mode.accepted != nil:
					boolPtr(t, "credential_accepted", r.CredentialAccepted, false)
					hint := "check the api key"
					if svc.kind == servicePlex {
						hint = "the admin scope"
					}
					if !strings.Contains(output, "the credential was refused") || !strings.Contains(output, hint) {
						t.Errorf("the output does not say the credential was refused with the hint %q:\n%s", hint, output)
					}
				}
			})
		}
	}
}

// Plex answers /identity without a token, so a refused token shows as one success beside two
// refusals, and the credential is still reported as refused.
func TestClientReport_APlexTokenWithoutTheAdminScopeIsRecordedAsRefused(t *testing.T) {
	routes := plexRoutes()
	routes["GET /library/sections/all"] = answer{status: 401, body: "CANARY"}
	routes["GET /status/sessions"] = answer{status: 403, body: "CANARY"}
	plex := newService(t, routes)
	l := newLab(t, servicePlex, plex.srv.URL, plexToken)
	code, output := l.run()
	r := l.writtenClean(code, output)
	boolPtr(t, "credential_accepted", r.CredentialAccepted, false)
	if !r.Plex.Identity.OK || r.Plex.Sections.Status != 401 || r.Plex.Sessions.Status != 403 {
		t.Errorf("plex = %+v", r.Plex)
	}
}

func TestClientReport_WriteChecksOnlyRunWhenAsked(t *testing.T) {
	t.Run("no flag, no write", func(t *testing.T) {
		for kind, routes := range map[string]map[string]answer{servicePlex: plexRoutes(), serviceSonarr: sonarrRoutes(), serviceRadarr: radarrRoutes()} {
			fake := newService(t, routes)
			cred := arrKey
			if kind == servicePlex {
				cred = plexToken
			}
			l := newLab(t, kind, fake.srv.URL, cred)
			code, output := l.run()
			l.writtenClean(code, output)
			if w := fake.writes(); len(w) != 0 {
				t.Errorf("%s: a read-only run sent %+v", kind, w)
			}
			b, _ := l.report()
			for _, key := range []string{`"refresh"`, `"rescan"`} {
				if bytes.Contains(b, []byte(key)) {
					t.Errorf("%s: a read-only report carries %s", kind, key)
				}
			}
		}
	})

	t.Run("plex refresh of one directory", func(t *testing.T) {
		plex := newService(t, plexRoutes())
		l := newLab(t, servicePlex, plex.srv.URL, plexToken)
		code, output := l.run("--refresh-dir", "/mnt/CANARY-holdfast/tv/CANARY Show/Season 01")
		r := l.writtenClean(code, output)
		w := plex.writes()
		if len(w) != 1 || w[0].Method != http.MethodPost || w[0].Path != "/library/sections/2/refresh" ||
			w[0].Query != "path="+url.QueryEscape("/CANARY-plex/tv/CANARY Show/Season 01") || w[0].Header.Get("X-Plex-Token") != plexToken {
			t.Fatalf("the write requests were %+v, want one partial refresh of section 2 restricted to the mapped directory", w)
		}
		want := WriteReport{
			Lookup:     Request{Request: "GET /library/sections/all", OK: true, Status: 200},
			OwnerFound: true, Sent: true, Accepted: true,
			Request: &Request{Request: "POST /library/sections/{section}/refresh?path={directory}", OK: true, Status: 200},
		}
		assertWrite(t, r.Plex.Refresh, want)
		if !strings.Contains(output, "clientreport: plex: POST /library/sections/{section}/refresh?path={directory}: ok (status 200)\n") {
			t.Errorf("the output lacks the refresh line:\n%s", output)
		}
	})

	t.Run("plex refresh with no owning section sends nothing", func(t *testing.T) {
		plex := newService(t, plexRoutes())
		l := newLab(t, servicePlex, plex.srv.URL, plexToken)
		code, output := l.run("--refresh-dir", "/mnt/CANARY-unmapped/music/CANARY Album")
		r := l.writtenClean(code, output)
		if w := plex.writes(); len(w) != 0 {
			t.Fatalf("a directory no section owns still sent %+v", w)
		}
		assertWrite(t, r.Plex.Refresh, WriteReport{Lookup: Request{Request: "GET /library/sections/all", OK: true, Status: 200}})
		if !strings.Contains(output, "clientreport: plex: write check: nothing was sent (owner found: false)\n") {
			t.Errorf("the output does not say nothing was sent:\n%s", output)
		}
	})

	t.Run("plex refresh refused by the server", func(t *testing.T) {
		routes := plexRoutes()
		routes["POST /library/sections/1/refresh"] = answer{status: 403, body: "CANARY"}
		plex := newService(t, routes)
		l := newLab(t, servicePlex, plex.srv.URL, plexToken)
		code, output := l.run("--refresh-dir=/mnt/CANARY-holdfast/movies/CANARY Film")
		r := l.writtenClean(code, output)
		assertWrite(t, r.Plex.Refresh, WriteReport{
			Lookup:     Request{Request: "GET /library/sections/all", OK: true, Status: 200},
			OwnerFound: true, Sent: true, Accepted: false,
			Request: &Request{Request: "POST /library/sections/{section}/refresh?path={directory}", Status: 403, FailureClass: "unauthorized"},
		})
	})

	t.Run("sonarr rescan of the one series that owns the directory", func(t *testing.T) {
		sonarr := newService(t, sonarrRoutes())
		l := newLab(t, serviceSonarr, sonarr.srv.URL, arrKey)
		code, output := l.run("--rescan-dir", "/mnt/CANARY-holdfast/tv/CANARY Show/Season 01")
		r := l.writtenClean(code, output)
		w := sonarr.writes()
		if len(w) != 1 || w[0].Path != "/api/v3/command" || w[0].Header.Get("X-Api-Key") != arrKey {
			t.Fatalf("the write requests were %+v, want one command", w)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(w[0].Body), &body); err != nil || len(body) != 2 || body["name"] != "RescanSeries" || body["seriesId"] != float64(21) {
			t.Fatalf("the command body was %s, want RescanSeries for series 21 and nothing else", w[0].Body)
		}
		assertWrite(t, r.Arr.Rescan, WriteReport{
			Lookup:     Request{Request: "GET /api/v3/series", OK: true, Status: 200},
			OwnerFound: true, Sent: true, Accepted: true, AnswerParsed: true, CommandName: "RescanSeries", CommandStatus: "queued",
			Request: &Request{Request: "POST /api/v3/command", OK: true, Status: 201},
		})
	})

	t.Run("radarr rescan, and an answer that is not a command", func(t *testing.T) {
		radarr := newService(t, radarrRoutes())
		l := newLab(t, serviceRadarr, radarr.srv.URL, arrKey)
		code, output := l.run("--rescan-dir", "/mnt/CANARY-holdfast/movies/CANARY Film 2")
		r := l.writtenClean(code, output)
		w := radarr.writes()
		var body map[string]any
		if len(w) != 1 || json.Unmarshal([]byte(w[0].Body), &body) != nil || len(body) != 2 || body["name"] != "RescanMovie" || body["movieId"] != float64(12) {
			t.Fatalf("the write requests were %+v, want one RescanMovie for movie 12", w)
		}
		assertWrite(t, r.Arr.Rescan, WriteReport{
			Lookup:     Request{Request: "GET /api/v3/movie", OK: true, Status: 200},
			OwnerFound: true, Sent: true, Accepted: true, AnswerParsed: true, CommandName: "RescanMovie", CommandStatus: "started",
			Request: &Request{Request: "POST /api/v3/command", OK: true, Status: 201},
		})

		routes := radarrRoutes()
		routes["POST /api/v3/command"] = answer{status: 200, body: `CANARY not json`}
		radarr = newService(t, routes)
		l = newLab(t, serviceRadarr, radarr.srv.URL, arrKey)
		code, output = l.run("--rescan-dir", "/mnt/CANARY-holdfast/movies/CANARY Film 2")
		r = l.writtenClean(code, output)
		assertWrite(t, r.Arr.Rescan, WriteReport{
			Lookup:     Request{Request: "GET /api/v3/movie", OK: true, Status: 200},
			OwnerFound: true, Sent: true, Accepted: true,
			Request: &Request{Request: "POST /api/v3/command", OK: true, Status: 200},
		})

		routes["POST /api/v3/command"] = answer{status: 201, body: `{"name":"CANARY-name","status":"CANARY-status"}`}
		radarr = newService(t, routes)
		l = newLab(t, serviceRadarr, radarr.srv.URL, arrKey)
		code, output = l.run("--rescan-dir", "/mnt/CANARY-holdfast/movies/CANARY Film 2")
		r = l.writtenClean(code, output)
		if r.Arr.Rescan.CommandName != "other" || r.Arr.Rescan.CommandStatus != "other" || !r.Arr.Rescan.AnswerParsed {
			t.Errorf("a command answered with unknown words was recorded as %+v, want `other` twice", r.Arr.Rescan)
		}
	})

	t.Run("an arr rescan with no owner, and one whose lookup fails, send nothing", func(t *testing.T) {
		sonarr := newService(t, sonarrRoutes())
		l := newLab(t, serviceSonarr, sonarr.srv.URL, arrKey)
		code, output := l.run("--rescan-dir", "/mnt/CANARY-holdfast/tv/CANARY Unknown Show")
		r := l.writtenClean(code, output)
		if w := sonarr.writes(); len(w) != 0 {
			t.Fatalf("a directory no series owns still sent %+v", w)
		}
		assertWrite(t, r.Arr.Rescan, WriteReport{Lookup: Request{Request: "GET /api/v3/series", OK: true, Status: 200}})

		routes := sonarrRoutes()
		routes["GET /api/v3/series"] = answer{status: 500, body: "CANARY"}
		sonarr = newService(t, routes)
		l = newLab(t, serviceSonarr, sonarr.srv.URL, arrKey)
		code, output = l.run("--rescan-dir", "/mnt/CANARY-holdfast/tv/CANARY Show")
		r = l.writtenClean(code, output)
		if w := sonarr.writes(); len(w) != 0 {
			t.Fatalf("a failed lookup still sent %+v", w)
		}
		assertWrite(t, r.Arr.Rescan, WriteReport{Lookup: Request{Request: "GET /api/v3/series", Status: 500, FailureClass: "http-status"}})
	})

	t.Run("a write flag that is not the service's is refused before anything is sent", func(t *testing.T) {
		cases := []struct {
			kind string
			args []string
			want string
		}{
			{servicePlex, []string{"--rescan-dir", "/mnt/CANARY-holdfast/tv"}, "plex takes --refresh-dir"},
			{serviceSonarr, []string{"--refresh-dir", "/mnt/CANARY-holdfast/tv"}, "sonarr and radarr take --rescan-dir"},
			{serviceRadarr, []string{"--refresh-dir", "/mnt/CANARY-holdfast/tv"}, "sonarr and radarr take --rescan-dir"},
			{servicePlex, []string{"--refresh-dir", "/mnt/CANARY-holdfast/tv", "--rescan-dir", "/mnt/CANARY-holdfast/tv"}, "cannot both be given"},
			{servicePlex, []string{"--refresh-dir", "CANARY-relative/tv"}, "must be an absolute path"},
			{serviceSonarr, []string{"--rescan-dir", "CANARY-relative/tv"}, "must be an absolute path"},
		}
		for _, c := range cases {
			fake := newService(t, map[string]answer{})
			l := newLab(t, c.kind, fake.srv.URL, arrKey)
			code, output := l.run(c.args...)
			if code != exitUsage || !strings.Contains(output, c.want) {
				t.Errorf("%s %v: exit %d, output %q; want exit %d naming %q", c.kind, c.args, code, output, exitUsage, c.want)
			}
			if len(fake.all()) != 0 {
				t.Errorf("%s %v: a refused invocation still sent %+v", c.kind, c.args, fake.all())
			}
			if _, err := os.Stat(l.out); !os.IsNotExist(err) {
				t.Errorf("%s %v: a refused invocation wrote a report", c.kind, c.args)
			}
			l.assertNoIdentity("the refusal", output)
		}
	})
}

func assertWrite(t *testing.T, got *WriteReport, want WriteReport) {
	t.Helper()
	if got == nil {
		t.Fatalf("no write check was recorded, want %+v", want)
	}
	if (got.Request == nil) != (want.Request == nil) || (got.Request != nil && *got.Request != *want.Request) {
		t.Errorf("write request = %+v, want %+v", got.Request, want.Request)
	}
	g, w := *got, want
	g.Request, w.Request = nil, nil
	if g != w {
		t.Errorf("write check = %+v, want %+v", g, w)
	}
}

func TestClientReport_RefusesToWriteAReportCarryingTheCredentialOrHost(t *testing.T) {
	t.Run("a credential that is in the report's bytes", func(t *testing.T) {
		// The credential is a word every report carries, so the encoded report contains it.
		for _, credential := range []string{"sections", "Every_Location"} {
			plex := newService(t, plexRoutes())
			l := newLab(t, servicePlex, plex.srv.URL, credential)
			code, output := l.run()
			if code != exitRefused || !strings.Contains(output, "refusing to write the report: it would carry the resolved credential") {
				t.Errorf("credential %q: exit %d, output %q; want a refusal naming the credential", credential, code, output)
			}
			if _, err := os.Stat(l.out); !os.IsNotExist(err) {
				t.Errorf("credential %q: a report was written", credential)
			}
			if strings.Contains(output, "wrote") {
				t.Errorf("credential %q: the output claims a report was written:\n%s", credential, output)
			}
		}
	})

	t.Run("a host that is in the report's bytes", func(t *testing.T) {
		// The fake listens on 127.0.0.1, and answers that very string as its version: a
		// version-shaped value that IS the configured host.
		routes := sonarrRoutes()
		routes["GET /api/v3/system/status"] = answer{status: 200, body: `{"appName":"Sonarr","version":"127.0.0.1"}`}
		sonarr := newService(t, routes)
		l := newLab(t, serviceSonarr, sonarr.srv.URL, arrKey)
		code, output := l.run()
		if code != exitRefused || !strings.Contains(output, "refusing to write the report: it would carry the configured host") {
			t.Errorf("exit %d, output %q; want a refusal naming the host", code, output)
		}
		if _, err := os.Stat(l.out); !os.IsNotExist(err) {
			t.Errorf("a report was written")
		}
	})

	t.Run("the scan itself", func(t *testing.T) {
		plex := newService(t, plexRoutes())
		l := newLab(t, servicePlex, plex.srv.URL, plexToken)
		if code, output := l.run(); code != exitWritten {
			t.Fatalf("exit %d: %s", code, output)
		}
		clean, _ := l.report()
		with := func(s string) []byte {
			return bytes.Replace(clean, []byte(`"1.40.2.8395-c67dce28e"`), []byte(`"`+s+`"`), 1)
		}
		cases := []struct {
			name       string
			report     []byte
			credential string
			address    string
			want       string // "" passes
		}{
			{"a clean report", clean, plexToken, "http://media.example.test:32400", ""},
			{"a host named like the service passes", clean, plexToken, "http://plex:32400", ""},
			{"a host named like a word of the report passes", clean, plexToken, "http://movie", ""},
			{"the credential", with(plexToken), plexToken, "http://plex:32400", "the resolved credential"},
			{"the credential in another case", with(strings.ToLower(plexToken)), plexToken, "http://plex:32400", "the resolved credential"},
			{"the credential with spaces around it", with(plexToken), "  " + plexToken + "\n", "http://plex:32400", "the resolved credential"},
			{"the address", with("see http://plex:32400/web"), plexToken, "http://plex:32400", "the configured address"},
			{"the host and port", with("plex:32400"), plexToken, "https://plex:32400", "the configured host and port"},
			{"a dotted host", with("MEDIA.example.test"), plexToken, "http://media.example.test:32400", "the configured host"},
			{"an address host", with("192.0.2.10"), plexToken, "http://192.0.2.10", "the configured host"},
			{"an IPv6 host", with("at 2001:db8::1 today"), plexToken, "http://[2001:db8::1]:32400", "the configured host"},
			{"a one-label host in a value that is not this build's word", with("nasbox-9"), plexToken, "http://NASbox:32400", "the configured host name"},
			{"a one-label host in the build version", clean, plexToken, "http://dev:32400", "the configured host name"},
			{"an address that cannot be read", clean, plexToken, "http://[::1", "could not be read"},
		}
		for _, c := range cases {
			err := identityScan(c.report, secret.NewValue(c.credential), c.address)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("%s: refused with %v, want a pass", c.name, err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("%s: got %v, want a refusal naming %q", c.name, err, c.want)
			case err != nil && (strings.Contains(err.Error(), plexToken) || strings.Contains(err.Error(), "32400")):
				t.Errorf("%s: the refusal quotes what it refuses: %v", c.name, err)
			}
		}
		if err := identityScan([]byte("not json"), secret.NewValue(plexToken), "http://plex:32400"); err == nil {
			t.Errorf("a report that is not JSON passed the scan for a one-label host")
		}
	})
}

// Verify is what holds a COMMITTED report to the closed struct. Each tampering below is a way
// an identity could be put into a report by hand, and each must be refused.
func TestClientReport_VerifyRefusesAReportThatIsNotTheClosedStruct(t *testing.T) {
	reports := map[string][]byte{}
	for kind, routes := range map[string]map[string]answer{servicePlex: plexRoutes(), serviceSonarr: sonarrRoutes()} {
		fake := newService(t, routes)
		cred, flag, dir := arrKey, "--rescan-dir", "/mnt/CANARY-holdfast/tv/CANARY Show"
		if kind == servicePlex {
			cred, flag, dir = plexToken, "--refresh-dir", "/mnt/CANARY-holdfast/movies/CANARY Film"
		}
		l := newLab(t, kind, fake.srv.URL, cred)
		if code, output := l.run(flag, dir); code != exitWritten {
			t.Fatalf("exit %d: %s", code, output)
		}
		reports[kind], _ = l.report()
		if err := Verify(reports[kind]); err != nil {
			t.Fatalf("the %s report does not pass: %v", kind, err)
		}
	}
	plex, sonarr := reports[servicePlex], reports[serviceSonarr]
	sub := func(b []byte, old, new string) []byte {
		if !bytes.Contains(b, []byte(old)) {
			t.Fatalf("the report does not carry %s, so the tampering would test nothing:\n%s", old, b)
		}
		return bytes.Replace(b, []byte(old), []byte(new), 1)
	}
	cases := []struct {
		name   string
		report []byte
		want   string
	}{
		{"an unknown field", sub(plex, `"schema": 1,`, `"schema": 1, "host": "nas.example.test",`), "a field is unknown"},
		{"a wrong type", sub(plex, `"count": 3`, `"count": "three"`), "a field is unknown or has the wrong type"},
		{"a second document", append(append([]byte{}, plex...), plex...), "more than one JSON document"},
		{"reformatted bytes", bytes.ReplaceAll(plex, []byte("  "), []byte("\t")), "not what this build writes"},
		{"no trailing newline", bytes.TrimSuffix(plex, []byte("\n")), "not what this build writes"},
		{"another schema", sub(plex, `"schema": 1`, `"schema": 2`), "its schema is not 1"},
		{"a date that is not one", sub(plex, `"date": "20`, `"date": "30-`), "its date"},
		{"a date with a time", sub(plex, `"date": "`, `"date": "2026-10-02T00:00:00Z `), "its date"},
		{"a holdfast version that is a host", sub(plex, `"version": "0.0.0-dev"`, `"version": "nas.example.test"`), "its holdfast version"},
		{"a commit that is a name", sub(plex, `"commit": "unknown"`, `"commit": "someones-laptop"`), "its holdfast commit"},
		{"a url scheme that is an address", sub(plex, `"url_scheme": "http"`, `"url_scheme": "nas"`), "its url scheme"},
		{"a section type that is a title", sub(plex, `"type": "movie"`, `"type": "Home Videos"`), "a section type"},
		{"a missing section list", sub(plex, `"sections": [`, `"sections": null, "sections_": [`), "a field is unknown"},
		{"a service that disagrees with its check", sub(plex, `"service": "plex"`, `"service": "sonarr"`), "do not agree"},
		{"a service that is not one", sub(sonarr, `"service": "sonarr"`, `"service": "jellyfin"`), "do not agree"},
		{"an application name that is an instance name", sub(sonarr, `"app": "sonarr"`, `"app": "Living Room"`), "its application name"},
		{"a command name that is not one", sub(sonarr, `"command_name": "RescanSeries"`, `"command_name": "DeleteSeries"`), "a command name or status"},
		{"a command status that is not one", sub(sonarr, `"command_status": "queued"`, `"command_status": "see nas"`), "a command name or status"},
		{"a request that carries a path", sub(sonarr, `"request": "GET /api/v3/series"`, `"request": "GET /tv/Some Show"`), "a request is not one this build sends"},
		{"a lookup that carries a path", sub(sonarr, "\"lookup\": {\n        \"request\": \"GET /api/v3/series\"", "\"lookup\": {\n        \"request\": \"GET /tv/Some Show\""), "a request is not one this build sends"},
		{"a write request that carries a path", sub(sonarr, `"request": "POST /api/v3/command"`, `"request": "POST /tv/Some Show"`), "a request is not one this build sends"},
		{"a failure class that is an error text", sub(sonarr, "\"ok\": true,\n      \"status\": 200,\n      \"failure_class\": \"\"", "\"ok\": false,\n      \"status\": 200,\n      \"failure_class\": \"dial tcp 192.0.2.10:8989\""), "a failure class"},
		{"an ok that disagrees with its class", sub(sonarr, `"ok": true`, `"ok": false`), "does not agree with itself"},
		{"a status that is a port", sub(sonarr, `"status": 200`, `"status": 8989`), "does not agree with itself"},
		{"a status below the range", sub(sonarr, `"status": 200`, `"status": 99`), "does not agree with itself"},
		{"a version that is a host", sub(sonarr, `"version": "4.0.20.3014"`, `"version": "nas.example.test"`), "a version is not shaped like one"},
		{"a plex version that is an identifier", sub(plex, `"version": "1.40.2.8395-c67dce28e"`, `"version": "0123456789abcdef0123456789abcdef"`), "a version is not shaped like one"},
	}
	for _, c := range cases {
		err := Verify(c.report)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want a refusal naming %q", c.name, err, c.want)
		}
	}

	// The status bounds themselves are inside the range.
	for _, status := range []string{"100", "599", "0"} {
		if err := Verify(sub(sonarr, `"status": 201`, `"status": `+status)); err != nil {
			t.Errorf("a status of %s was refused: %v", status, err)
		}
	}
	for _, status := range []string{"600", "-1"} {
		if err := Verify(sub(sonarr, `"status": 201`, `"status": `+status)); err == nil {
			t.Errorf("a status of %s passed", status)
		}
	}
}

// Every report committed under testdata/client-reports/ is held to the closed struct by the
// gate: `make check` runs this test, so a report the owner commits later cannot carry an
// identity field, a title or a path without turning the gate red.
func TestClientReport_EveryCommittedReportPassesTheIdentityCheck(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := corpus.RepoRoot(wd)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "testdata", "client-reports")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("testdata/client-reports must exist (it holds the owner's reports and its README): %v", err)
	}
	readme := false
	for _, e := range entries {
		name := e.Name()
		if name == "README.md" {
			readme = true
			continue
		}
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			t.Errorf("testdata/client-reports/%s is neither the README nor a report: nothing else belongs here", name)
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if err := Verify(b); err != nil {
			t.Errorf("testdata/client-reports/%s does not pass the identity check: %v", name, err)
			continue
		}
		var r Report
		if err := json.Unmarshal(b, &r); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if want := r.Service + "-" + r.Date; !strings.HasPrefix(name, want) {
			t.Errorf("testdata/client-reports/%s is not named for its content: want %s[-<suffix>].json", name, want)
		}
	}
	if !readme {
		t.Errorf("testdata/client-reports/README.md is missing")
	}
}

func TestClientReport_VerifyFlagChecksAnExistingReport(t *testing.T) {
	sonarr := newService(t, sonarrRoutes())
	l := newLab(t, serviceSonarr, sonarr.srv.URL, arrKey)
	if code, output := l.run(); code != exitWritten {
		t.Fatalf("exit %d: %s", code, output)
	}
	verify := func(args ...string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), args, &stdout, &stderr)
		return code, stdout.String() + stderr.String()
	}
	if code, output := verify("--verify", l.out); code != exitWritten || !strings.Contains(output, "carries only what a report may carry") {
		t.Errorf("a clean report: exit %d, output %q", code, output)
	}
	b, _ := l.report()
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, bytes.Replace(b, []byte(`"app": "sonarr"`), []byte(`"app": "CANARY-instance"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output := verify("--verify", bad)
	if code != exitRefused || !strings.Contains(output, "the report does not pass: its application name") || strings.Contains(output, canary) {
		t.Errorf("a tampered report: exit %d, output %q", code, output)
	}
	if code, output := verify("--verify", filepath.Join(t.TempDir(), "absent.json")); code != exitRefused || !strings.Contains(output, "could not be read") {
		t.Errorf("an absent report: exit %d, output %q", code, output)
	}
	for _, extra := range [][]string{{"--service", "plex"}, {"--config", l.cfg}, {"--out", bad}, {"--refresh-dir", "/x"}, {"--rescan-dir", "/x"}} {
		if code, output := verify(append([]string{"--verify", l.out}, extra...)...); code != exitUsage || !strings.Contains(output, "--verify takes no other flag") {
			t.Errorf("--verify with %v: exit %d, output %q", extra, code, output)
		}
	}
}

func TestClientReport_RefusesWithoutWritingAReport(t *testing.T) {
	sonarr := newService(t, sonarrRoutes())
	call := func(args ...string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), args, &stdout, &stderr)
		return code, stdout.String() + stderr.String()
	}

	t.Run("usage", func(t *testing.T) {
		l := newLab(t, serviceSonarr, sonarr.srv.URL, arrKey)
		cases := []struct {
			args []string
			want string
		}{
			{[]string{"--service", "jellyfin", "--config", l.cfg, "--out", l.out}, "--service must be plex, sonarr or radarr"},
			{[]string{"--config", l.cfg, "--out", l.out}, "--service must be plex, sonarr or radarr"},
			{[]string{"--service", "sonarr", "--out", l.out}, "--config and --out are both required"},
			{[]string{"--service", "sonarr", "--config", l.cfg}, "--config and --out are both required"},
			{[]string{"--service", "sonarr", "--config", l.cfg, "--out", l.out, "stray"}, "unexpected argument"},
			{[]string{"--no-such-flag"}, "flag provided but not defined"},
		}
		before := len(sonarr.all())
		for _, c := range cases {
			code, output := call(c.args...)
			if code != exitUsage || !strings.Contains(output, c.want) {
				t.Errorf("%v: exit %d, output %q; want exit %d naming %q", c.args, code, output, exitUsage, c.want)
			}
		}
		if code, output := call("--help"); code != exitWritten || !strings.Contains(output, "--refresh-dir DIR") {
			t.Errorf("--help: exit %d, output %q", code, output)
		}
		if len(sonarr.all()) != before {
			t.Errorf("a usage error sent a request")
		}
		if _, err := os.Stat(l.out); !os.IsNotExist(err) {
			t.Errorf("a usage error wrote a report")
		}
	})

	t.Run("an existing report is never overwritten", func(t *testing.T) {
		l := newLab(t, serviceSonarr, sonarr.srv.URL, arrKey)
		if err := os.WriteFile(l.out, []byte("an earlier record\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, output := l.run()
		if code != exitRefused || !strings.Contains(output, "refusing to overwrite an existing report") {
			t.Errorf("exit %d, output %q", code, output)
		}
		if b, _ := os.ReadFile(l.out); string(b) != "an earlier record\n" {
			t.Errorf("the existing report was changed to %q", b)
		}
		l.assertNoIdentity("the refusal", output)
	})

	t.Run("a directory that does not exist", func(t *testing.T) {
		l := newLab(t, serviceSonarr, sonarr.srv.URL, arrKey)
		l.out = filepath.Join(filepath.Dir(l.out), "absent", "report.json")
		code, output := l.run()
		if code != exitRefused || !strings.Contains(output, "the report could not be written") {
			t.Errorf("exit %d, output %q", code, output)
		}
	})

	t.Run("a target that is not configured", func(t *testing.T) {
		l := newLab(t, serviceSonarr, sonarr.srv.URL, arrKey)
		before := len(sonarr.all())
		l.service = serviceRadarr
		code, output := l.run()
		if code != exitRefused || !strings.Contains(output, "the radarr target is not configured: set radarr_url and radarr_api_key") {
			t.Errorf("exit %d, output %q", code, output)
		}
		if len(sonarr.all()) != before {
			t.Errorf("an unconfigured target sent a request")
		}
	})

	t.Run("a literal credential, an unresolvable one and an empty one", func(t *testing.T) {
		l := newLab(t, serviceSonarr, sonarr.srv.URL, arrKey)
		before := len(sonarr.all())
		yaml, err := os.ReadFile(l.cfg)
		if err != nil {
			t.Fatal(err)
		}
		keyFile := filepath.Join(filepath.Dir(l.cfg), "credential")

		literal := bytes.Replace(yaml, []byte("file:"+keyFile), []byte(arrKey), 1)
		if err := os.WriteFile(l.cfg, literal, 0o600); err != nil {
			t.Fatal(err)
		}
		code, output := l.run()
		if code != exitRefused || !strings.Contains(output, "invalid config") || strings.Contains(output, arrKey) {
			t.Errorf("a literal credential: exit %d, output %q; want holdfast's refusal, without the value", code, output)
		}

		if err := os.WriteFile(l.cfg, yaml, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyFile, []byte(" \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, output = l.run()
		if code != exitRefused || !strings.Contains(output, "sonarr_api_key") {
			t.Errorf("an empty credential: exit %d, output %q", code, output)
		}

		if err := os.Remove(keyFile); err != nil {
			t.Fatal(err)
		}
		code, output = l.run()
		if code != exitRefused || !strings.Contains(output, "sonarr_api_key") {
			t.Errorf("an unresolvable credential: exit %d, output %q", code, output)
		}
		if len(sonarr.all()) != before {
			t.Errorf("a refused credential still sent a request")
		}
		if _, err := os.Stat(l.out); !os.IsNotExist(err) {
			t.Errorf("a refused credential wrote a report")
		}
	})
}

// --- scripts/client-report.sh ------------------------------------------------------------------

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := corpus.RepoRoot(wd)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// buildTool builds the Go half once for the script tests.
func buildTool(t *testing.T, root string) string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go is not on PATH: the script's tests build the tool, and a test that skipped here would be a false green")
	}
	bin := filepath.Join(t.TempDir(), "holdfast-client-report")
	cmd := exec.Command(gobin, "build", "-o", bin, "./scripts/clientreport")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./scripts/clientreport: %v\n%s", err, out)
	}
	return bin
}

// script runs scripts/client-report.sh and returns its exit code and everything it printed.
func script(t *testing.T, root string, pathFirst string, args ...string) (int, string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("bash is not on PATH: client-report.sh needs it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, append([]string{filepath.Join(root, "scripts", "client-report.sh")}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.Env = os.Environ()
	if pathFirst != "" {
		cmd.Env = append(cmd.Env, "PATH="+pathFirst+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &exit):
		return exit.ExitCode(), string(out)
	}
	t.Fatalf("client-report.sh did not run: %v\n%s", err, out)
	return 0, ""
}

// The script, in host mode with a built binary, writes the same redacted report, never
// overwrites one, sends a write only when asked, and re-checks a report with --verify.
func TestClientReport_ScriptWritesARedactedReportAndNeverOverwritesOne(t *testing.T) {
	root := repoRoot(t)
	bin := buildTool(t, root)
	sonarr := newService(t, sonarrRoutes())
	l := newLab(t, serviceSonarr, sonarr.srv.URL, arrKey)

	code, output := script(t, root, "", "--service", "sonarr", "--config", l.cfg, "--out", l.out, "--bin", bin)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, output)
	}
	b, r := l.report()
	l.assertNoIdentity("the report", string(b))
	// The script names the report's own path, which the owner typed; nothing else of the lab.
	l.assertNoIdentity("the script's output", strings.ReplaceAll(output, l.out, "<out>"))
	if err := Verify(b); err != nil {
		t.Errorf("the report does not pass Verify: %v", err)
	}
	if r.Service != serviceSonarr || r.Arr == nil || r.Arr.Library.Count != 3 || r.Arr.Rescan != nil {
		t.Errorf("report = %+v", r)
	}
	if len(sonarr.writes()) != 0 {
		t.Errorf("a read-only run sent %+v", sonarr.writes())
	}
	if !strings.Contains(output, "client-report: wrote "+l.out) {
		t.Errorf("the output does not name the report:\n%s", output)
	}

	code, output = script(t, root, "", "--verify", l.out, "--bin", bin)
	if code != 0 || !strings.Contains(output, "carries only what a report may carry") {
		t.Errorf("--verify of a clean report: exit %d\n%s", code, output)
	}

	before := len(sonarr.all())
	code, output = script(t, root, "", "--service", "sonarr", "--config", l.cfg, "--out", l.out, "--bin", bin)
	if code == 0 || !strings.Contains(output, "refusing to overwrite the existing report") {
		t.Errorf("a second run over the same report: exit %d\n%s", code, output)
	}
	if again, _ := os.ReadFile(l.out); !bytes.Equal(again, b) {
		t.Errorf("the existing report was changed")
	}
	if len(sonarr.all()) != before {
		t.Errorf("a refused run still sent a request")
	}

	// A write check is sent only when its flag is typed, and the script says so.
	l.out = filepath.Join(filepath.Dir(l.out), "with-rescan.json")
	code, output = script(t, root, "", "--service=sonarr", "--config="+l.cfg, "--out="+l.out, "--bin="+bin,
		"--rescan-dir=/mnt/CANARY-holdfast/tv/CANARY Show")
	if code != 0 || !strings.Contains(output, "a WRITE check was asked for") {
		t.Fatalf("exit %d:\n%s", code, output)
	}
	if w := sonarr.writes(); len(w) != 1 || !strings.Contains(w[0].Body, `"seriesId":21`) {
		t.Errorf("the writes were %+v, want one RescanSeries for series 21", w)
	}
	b, r = l.report()
	l.assertNoIdentity("the report", string(b))
	if r.Arr.Rescan == nil || !r.Arr.Rescan.Accepted {
		t.Errorf("rescan = %+v", r.Arr.Rescan)
	}

	// A failed check leaves no report and a non-zero exit.
	tampered := filepath.Join(t.TempDir(), "tampered.json")
	if err := os.WriteFile(tampered, bytes.Replace(b, []byte(`"app": "sonarr"`), []byte(`"app": "Living Room"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output = script(t, root, "", "--verify", tampered, "--bin", bin); code == 0 || !strings.Contains(output, "its application name") {
		t.Errorf("--verify of a tampered report: exit %d\n%s", code, output)
	}
	l.out = filepath.Join(filepath.Dir(l.out), "wrong-flag.json")
	code, output = script(t, root, "", "--service", "sonarr", "--config", l.cfg, "--out", l.out, "--bin", bin, "--refresh-dir", "/mnt/x")
	if code == 0 || !strings.Contains(output, "no report was written") {
		t.Errorf("a write flag of the wrong service: exit %d\n%s", code, output)
	}
	if _, err := os.Stat(l.out); !os.IsNotExist(err) {
		t.Errorf("a refused run wrote a report")
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--service", "jellyfin", "--config", l.cfg}, "--service must be plex, sonarr or radarr"},
		{[]string{"--service", "plex"}, "--config is required"},
		{[]string{"--service", "plex", "--config", filepath.Join(t.TempDir(), "absent.yaml")}, "no configuration at"},
		{[]string{"--service", "plex", "--config", l.cfg, "--bin", bin, "--image", "holdfast:synthetic"}, "two different modes"},
		{[]string{"--service", "plex", "--config", l.cfg, "--bin", bin, "--docker-arg=--network=host"}, "--docker-arg needs --image"},
		{[]string{"--service", "plex", "--config", l.cfg, "--bin", filepath.Join(t.TempDir(), "absent")}, "is not an executable file"},
		{[]string{"--verify", l.cfg, "--service", "plex", "--bin", bin}, "--verify takes no other flag"},
		{[]string{"--no-such-flag"}, "unknown argument"},
	} {
		if code, output := script(t, root, "", c.args...); code == 0 || !strings.Contains(output, c.want) {
			t.Errorf("%v: exit %d, want a refusal naming %q:\n%s", c.args, code, c.want, output)
		}
	}
}

// Image mode cannot be run here (the gate has no Docker daemon), so what is proven is the
// command line it builds, through a stand-in `docker` that records its arguments: the check
// runs as the calling user, the configuration is mounted read-only, the image's own binary
// is the entrypoint, the owner's --docker-arg values are passed through, and no credential
// is an argument.
func TestClientReport_ScriptImageModeRunsTheImagesOwnBinaryAsTheCaller(t *testing.T) {
	root := repoRoot(t)
	l := newLab(t, servicePlex, "http://plex.synthetic.test:32400", plexToken)
	tools := t.TempDir()
	argv := filepath.Join(tools, "argv")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argv + "'\n" +
		"for a in \"$@\"; do case \"$a\" in /client-report/out/*) : > '" + filepath.Dir(l.out) + "'/\"${a##*/}\";; esac; done\n"
	if err := os.WriteFile(filepath.Join(tools, "docker"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	code, output := script(t, root, tools, "--service", "plex", "--config", l.cfg, "--out", l.out, "--image", "holdfast:synthetic",
		"--docker-arg=-v", "--docker-arg=/srv/synthetic/secrets:/run/secrets:ro", "--refresh-dir", "/mnt/synthetic/movies/Film")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, output)
	}
	b, err := os.ReadFile(argv)
	if err != nil {
		t.Fatalf("the stand-in docker was never run: %v\n%s", err, output)
	}
	got := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	want := []string{"run", "--rm", "-u", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		"-v", l.cfg + ":/client-report/config.yaml:ro", "-v", filepath.Dir(l.out) + ":/client-report/out",
		"-v", "/srv/synthetic/secrets:/run/secrets:ro",
		"--entrypoint", "/usr/local/bin/holdfast-client-report", "holdfast:synthetic",
		"--service", "plex", "--refresh-dir", "/mnt/synthetic/movies/Film",
		"--config", "/client-report/config.yaml", "--out", "/client-report/out/" + filepath.Base(l.out)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("docker was run with\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(string(b), plexToken) {
		t.Errorf("the credential is a docker argument")
	}

	// The binary the script names is the one the Dockerfile puts into the image, and the
	// smoke gate runs.
	for _, name := range []string{"Dockerfile", filepath.Join("scripts", "smoke-image.sh")} {
		text, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(text), "/usr/local/bin/holdfast-client-report") {
			t.Errorf("%s does not name /usr/local/bin/holdfast-client-report, the binary client-report.sh --image runs", name)
		}
	}
	dockerfile, _ := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if !strings.Contains(string(dockerfile), "-o /out/holdfast-client-report ./scripts/clientreport") {
		t.Errorf("the Dockerfile does not build ./scripts/clientreport into the image")
	}
}
