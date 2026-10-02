package mediaclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/secret"
)

// TestArr_NoCommandIsEverSentWithoutAnOwnerID proves the rule the whole Arr client is built
// around: a RescanSeries or RescanMovie with no id rescans the whole library, so none can be
// sent. An owner whose id is absent, zero, negative, null or not a number is not an owner;
// the one function that builds a command body refuses an id that is not positive; and every
// command either application was sent, across every shape of list, names a positive id.
func TestArr_NoCommandIsEverSentWithoutAnOwnerID(t *testing.T) {
	for _, kind := range []arrKind{radarrKind, sonarrKind} {
		for _, id := range []int{0, -1, -2147483648} {
			if body, err := rescanCommand(kind, id); !errors.Is(err, errNoOwnerID) || body != nil {
				t.Errorf("%s: rescanCommand(%d) = %q, %v; want a refusal and no body", kind.name, id, body, err)
			}
		}
		body, err := rescanCommand(kind, 1)
		if err != nil {
			t.Fatalf("%s: rescanCommand(1): %v", kind.name, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["name"] != kind.command || decoded[kind.idField] != float64(1) || len(decoded) != 2 {
			t.Errorf("%s: the command body is %s", kind.name, body)
		}
	}

	lists := map[string]string{
		"an owner with no id":            `[{"path": "/movies/Film"}]`,
		"an owner whose id is zero":      `[{"id": 0, "path": "/movies/Film"}]`,
		"an owner whose id is null":      `[{"id": null, "path": "/movies/Film"}]`,
		"an owner whose id is negative":  `[{"id": -4, "path": "/movies/Film"}]`,
		"two owners at one path":         `[{"id": 1, "path": "/movies/Film"}, {"id": 2, "path": "/movies/Film/"}]`,
		"an owner with no path":          `[{"id": 9}]`,
		"an owner with a relative path":  `[{"id": 9, "path": "movies/Film"}]`,
		"a valid owner beside a bad one": `[{"path": "/movies/Film"}, {"id": 7, "path": "/movies"}]`,
	}
	for name, list := range lists {
		t.Run(name, func(t *testing.T) {
			for _, build := range []func(*fake) *Arr{
				func(f *fake) *Arr { return NewRadarr(f.srv.URL, testKey, nil) },
				func(f *fake) *Arr { return NewSonarr(f.srv.URL, testKey, nil) },
			} {
				f := newFake(t, func(w http.ResponseWriter, r request) {
					if r.Method == http.MethodGet {
						_, _ = io.WriteString(w, list)
						return
					}
					w.WriteHeader(http.StatusCreated)
				})
				arr := build(f)
				res := arr.Rescan(context.Background(), "/movies/Film")
				for _, post := range f.posts() {
					var body map[string]any
					if err := json.Unmarshal([]byte(post.Body), &body); err != nil {
						t.Fatalf("a command with an unreadable body was sent: %q", post.Body)
					}
					id, ok := body[arr.kind.idField].(float64)
					if !ok || id <= 0 {
						t.Errorf("%s was sent a command with no positive %s: %s", arr.Name(), arr.kind.idField, post.Body)
					}
				}
				wantSent := name == "a valid owner beside a bad one"
				if res.Sent != wantSent || res.NoOwner == wantSent || (len(f.posts()) == 1) != wantSent {
					t.Errorf("%s: sent=%v noOwner=%v posts=%d, want sent=%v", arr.Name(), res.Sent, res.NoOwner, len(f.posts()), wantSent)
				}
			}
		})
	}
}

// TestArr_AListThatIsNotTheDocumentedArrayIsUnparseable: the list endpoint answers an array
// of objects; anything else is a failure and sends nothing.
func TestArr_AListThatIsNotTheDocumentedArrayIsUnparseable(t *testing.T) {
	for name, body := range map[string]string{
		"an object":                `{"records": []}`,
		"nothing":                  ``,
		"a truncated array":        `[{"id": 1, "path": "/movies/Film"}`,
		"an array of the wrong id": `[{"id": "one", "path": "/movies/Film"}]`,
		"a string":                 `"ok"`,
		"an array cut mid-element": `[{"id": 1, "path": "/mov`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r request) { _, _ = io.WriteString(w, body) })
			res := NewRadarr(f.srv.URL, testKey, nil).Rescan(context.Background(), "/movies/Film")
			if res.Failure == nil || res.Failure.String() != ClassUnparseable || res.Sent || res.NoOwner {
				t.Errorf("result = %+v (failure %v), want an unparseable-response failure", res, res.Failure)
			}
			if len(f.posts()) != 0 {
				t.Errorf("a command was sent after an unreadable list: %+v", f.posts())
			}
		})
	}
}

// TestNearestOwner: the longest owning path wins, on whole components.
func TestNearestOwner(t *testing.T) {
	id := func(n int) *int { return &n }
	owners := []arrOwner{
		{ID: id(1), Path: "/tv"},
		{ID: id(2), Path: "/tv/Show"},
		{ID: id(3), Path: "/tv/Show/Season 1"},
		{ID: id(4), Path: "/tv/Show 2"},
	}
	for dir, want := range map[string]int{
		"/tv/Show/Season 1":        3,
		"/tv/Show/Season 1/extras": 3,
		"/tv/Show/Season 2":        2,
		"/tv/Show":                 2,
		"/tv/Show 2":               4,
		"/tv/Show 22":              1,
		"/tv":                      1,
	} {
		if got, ok := nearestOwner(owners, dir); !ok || got != want {
			t.Errorf("nearestOwner(%q) = %d, %v; want %d", dir, got, ok, want)
		}
	}
	if got, ok := nearestOwner(owners, "/movies/Film"); ok {
		t.Errorf("nearestOwner outside every path = %d, true", got)
	}
	if _, ok := nearestOwner(nil, "/tv"); ok {
		t.Error("nearestOwner over no owners found one")
	}
	// The same item listed twice is not a tie; two items at one path are.
	same := []arrOwner{{ID: id(5), Path: "/tv/Show"}, {ID: id(5), Path: "/tv/Show/"}}
	if got, ok := nearestOwner(same, "/tv/Show"); !ok || got != 5 {
		t.Errorf("one item listed twice = %d, %v; want 5", got, ok)
	}
	tie := []arrOwner{{ID: id(5), Path: "/tv/Show"}, {ID: id(6), Path: "/tv/Show"}, {ID: id(7), Path: "/tv"}}
	if _, ok := nearestOwner(tie, "/tv/Show"); ok {
		t.Error("two items at the winning path produced an owner")
	}
	// A tie at a path that then loses to a longer one is no tie.
	if got, ok := nearestOwner(append(tie, arrOwner{ID: id(8), Path: "/tv/Show/S1"}), "/tv/Show/S1"); !ok || got != 8 {
		t.Errorf("a longer owner past a tie = %d, %v; want 8", got, ok)
	}
}

// TestPlex_RefreshIsAlwaysOneSectionAndOneDirectory: the one function that builds a refresh
// request cannot build a whole-section or an all-sections one.
func TestPlex_RefreshIsAlwaysOneSectionAndOneDirectory(t *testing.T) {
	bad := []struct{ key, dir string }{
		{"all", "/data/movies"},
		{"", "/data/movies"},
		{"1", ""},
		{"1", "relative/dir"},
		{"0", "/data/movies"},
		{"01", "/data/movies"},
		{"-1", "/data/movies"},
		{"1/../all", "/data/movies"},
		{"1?x=", "/data/movies"},
		{"1234567890", "/data/movies"},
		{"1a", "/data/movies"},
		{"a1", "/data/movies"},
	}
	for _, tc := range bad {
		if got, err := refreshTarget(tc.key, tc.dir); !errors.Is(err, errUnspecificRefresh) || got != "" {
			t.Errorf("refreshTarget(%q, %q) = %q, %v; want a refusal", tc.key, tc.dir, got, err)
		}
	}
	got, err := refreshTarget("12", "/data/tv/Show & Co/Season 1")
	if err != nil || got != "/library/sections/12/refresh?path=%2Fdata%2Ftv%2FShow+%26+Co%2FSeason+1" {
		t.Errorf("refreshTarget = %q, %v", got, err)
	}
	if got, err := refreshTarget("123456789", "/"); err != nil || got != "/library/sections/123456789/refresh?path=%2F" {
		t.Errorf("refreshTarget at the widest accepted key = %q, %v", got, err)
	}
}

// TestPlex_SectionLookup covers the shapes the section list can take.
func TestPlex_SectionLookup(t *testing.T) {
	cases := []struct {
		name, body, dir, want string
		noOwner, unparseable  bool
	}{
		{name: "a key that is a bare number",
			body: `{"MediaContainer": {"Directory": [{"key": 3, "Location": [{"path": "/data/movies"}]}]}}`,
			dir:  "/data/movies/Film", want: "/library/sections/3/refresh"},
		{name: "a key that is not a section number owns nothing",
			body: `{"MediaContainer": {"Directory": [{"key": "all", "Location": [{"path": "/data/movies"}]}]}}`,
			dir:  "/data/movies/Film", noOwner: true},
		{name: "a key of another type owns nothing",
			body: `{"MediaContainer": {"Directory": [{"key": true, "Location": [{"path": "/data/movies"}]}, {"Location": [{"path": "/data"}]}]}}`,
			dir:  "/data/movies/Film", noOwner: true},
		{name: "a relative or empty location owns nothing",
			body: `{"MediaContainer": {"Directory": [{"key": "1", "Location": [{"path": "data/movies"}, {"path": ""}, {}]}]}}`,
			dir:  "/data/movies/Film", noOwner: true},
		{name: "a location that is a string prefix and not a parent",
			body: `{"MediaContainer": {"Directory": [{"key": "1", "Location": [{"path": "/data/mov"}]}]}}`,
			dir:  "/data/movies/Film", noOwner: true},
		{name: "the longest location wins, whatever the order",
			body: `{"MediaContainer": {"Directory": [{"key": "5", "Location": [{"path": "/data/movies/Film"}]}, {"key": "1", "Location": [{"path": "/data"}]}, {"key": "2", "Location": [{"path": "/data/movies/"}]}]}}`,
			dir:  "/data/movies/Film/extras", want: "/library/sections/5/refresh"},
		{name: "two sections on one location are ambiguous",
			body: `{"MediaContainer": {"Directory": [{"key": "1", "Location": [{"path": "/data/movies"}]}, {"key": "2", "Location": [{"path": "/data/movies"}]}]}}`,
			dir:  "/data/movies/Film", noOwner: true},
		{name: "one section listing a location twice is not",
			body: `{"MediaContainer": {"Directory": [{"key": "1", "Location": [{"path": "/data/movies"}, {"path": "/data/movies/"}]}]}}`,
			dir:  "/data/movies/Film", want: "/library/sections/1/refresh"},
		{name: "no sections at all",
			body: `{"MediaContainer": {"size": 0}}`, dir: "/data/movies/Film", noOwner: true},
		{name: "JSON that is not a Plex answer", body: `{}`, dir: "/data/movies/Film", unparseable: true},
		{name: "XML", body: `<MediaContainer size="0"></MediaContainer>`, dir: "/data/movies/Film", unparseable: true},
		{name: "two documents", body: `{"MediaContainer": {}} {"MediaContainer": {}}`, dir: "/data/movies/Film", unparseable: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, plexSectionsHandler(tc.body))
			res := NewPlex(f.srv.URL, testKey, nil).Rescan(context.Background(), tc.dir)
			switch {
			case tc.unparseable:
				if res.Failure == nil || res.Failure.String() != ClassUnparseable || len(f.posts()) != 0 {
					t.Errorf("result %+v (failure %v), posts %d; want unparseable and nothing sent", res, res.Failure, len(f.posts()))
				}
			case tc.noOwner:
				if !res.NoOwner || res.Sent || res.Failure != nil || len(f.posts()) != 0 {
					t.Errorf("result %+v, posts %+v; want no owner and nothing sent", res, f.posts())
				}
			default:
				posts := f.posts()
				if !res.Sent || len(posts) != 1 || posts[0].Path != tc.want {
					t.Errorf("result %+v, posts %+v; want one refresh at %s", res, posts, tc.want)
				}
			}
			if res.Dir != tc.dir {
				t.Errorf("the result names directory %q, want %q", res.Dir, tc.dir)
			}
		})
	}
}

// TestIsSectionNumber pins the only keys that may be put into a refresh URL.
func TestIsSectionNumber(t *testing.T) {
	for s, want := range map[string]bool{
		"1": true, "9": true, "10": true, "123456789": true,
		"": false, "0": false, "09": false, "1234567890": false, "all": false, "1.5": false,
		"+1": false, "-1": false, " 1": false, "1 ": false, "/": false, ":": false,
	} {
		if got := isSectionNumber(s); got != want {
			t.Errorf("isSectionNumber(%q) = %v, want %v", s, got, want)
		}
	}
}

// TestFailure_StringNamesTheClassAndTheStatus: a record's failure is a class, with the
// status where the target answered one.
func TestFailure_StringNamesTheClassAndTheStatus(t *testing.T) {
	if got := (&failure{class: ClassTimeout}).String(); got != "timeout" {
		t.Errorf("String() = %q", got)
	}
	if got := (&failure{class: ClassStatus, status: 502}).String(); got != "http-status 502" {
		t.Errorf("String() = %q", got)
	}
}

// TestCaller_StatusBoundaries: 200 and 299 are success, 199 and 300 are not, and 401 and 403
// are named for what they mean.
func TestCaller_StatusBoundaries(t *testing.T) {
	for status, want := range map[int]string{
		200: "", 204: "", 299: "",
		300: "http-status 300", 400: "http-status 400", 404: "http-status 404",
		401: "unauthorized 401", 403: "unauthorized 403", 402: "http-status 402",
	} {
		f := newFake(t, func(w http.ResponseWriter, _ request) { w.WriteHeader(status) })
		got := ""
		if fail := newCaller(f.srv.URL, "", secret.Value{}, nil).do(context.Background(), "GET", "/x", nil, nil); fail != nil {
			got = fail.String()
		}
		if got != want {
			t.Errorf("status %d: failure %q, want %q", status, got, want)
		}
	}
	// A request that cannot be built is refused, not sent.
	if fail := newCaller("http://host.invalid", "", secret.Value{}, nil).do(context.Background(), "BAD METHOD", "/x", nil, nil); fail == nil ||
		fail.String() != ClassRefused {
		t.Errorf("an unbuildable request: %v, want %s", fail, ClassRefused)
	}
	// A body is sent as JSON; a request with none carries no content type.
	f := newFake(t, func(w http.ResponseWriter, _ request) {})
	c := newCaller(f.srv.URL, "X-Credential", secret.NewValue("the-credential"), map[string]string{"X-Test": "v"})
	if printed := fmt.Sprintf("%v %+v %#v", c, *c, c.credential); strings.Contains(printed, "the-credential") {
		t.Errorf("a formatted caller prints its credential: %s", printed)
	}
	_ = c.do(context.Background(), "POST", "/with", strings.NewReader("{}"), nil)
	_ = c.do(context.Background(), "POST", "/without", nil, nil)
	seen := f.seen()
	if seen[0].Header.Get("X-Credential") != "the-credential" || seen[1].Header.Get("X-Credential") != "the-credential" {
		t.Errorf("the credential did not travel in its header: %+v", seen)
	}
	if seen[0].Header.Get("Content-Type") != "application/json" || seen[0].Body != "{}" || seen[0].Header.Get("X-Test") != "v" {
		t.Errorf("the request with a body arrived as %+v", seen[0])
	}
	if seen[1].Header.Get("Content-Type") != "" {
		t.Errorf("the request with no body carries Content-Type %q", seen[1].Header.Get("Content-Type"))
	}
}

// TestCaller_ACancelledCallerIsNotATimeout: a request stopped because its caller's context
// was cancelled is reported as unreachable, and one that ran out of time as a timeout.
func TestCaller_ACancelledCallerIsNotATimeout(t *testing.T) {
	f := newFake(t, func(_ http.ResponseWriter, r request) { r.hang() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if fail := newCaller(f.srv.URL, "", secret.Value{}, nil).do(ctx, "GET", "/x", nil, nil); fail == nil || fail.String() != ClassUnreachable {
		t.Errorf("a cancelled request: %v, want %s", fail, ClassUnreachable)
	}
	shortTimeout(t, 100*time.Millisecond)
	if fail := newCaller(f.srv.URL, "", secret.Value{}, nil).do(context.Background(), "GET", "/x", nil, nil); fail == nil || fail.String() != ClassTimeout {
		t.Errorf("a request with no answer: %v, want %s", fail, ClassTimeout)
	}
}

// TestCaller_ABodyThatStopsMidwayIsATimeoutNotUnparseable: a target that sends its headers
// and then stalls past the timeout is reported as a timeout.
func TestCaller_ABodyThatStopsMidwayIsATimeoutNotUnparseable(t *testing.T) {
	shortTimeout(t, 150*time.Millisecond)
	f := newFake(t, func(w http.ResponseWriter, r request) {
		_, _ = io.WriteString(w, `[{"id": 1, "path": "/movies/Film"},`)
		w.(http.Flusher).Flush()
		r.hang()
	})
	res := NewRadarr(f.srv.URL, testKey, nil).Rescan(context.Background(), "/movies/Film")
	if res.Failure == nil || res.Failure.String() != ClassTimeout {
		t.Errorf("failure = %v, want %s", res.Failure, ClassTimeout)
	}
}
