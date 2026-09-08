// Command fixtureserver stands the dashboard up for the Playwright graders.
//
// It serves the REAL handler - webui.HandlerFor, the same bytes and the same
// Content-Security-Policy `holdfast serve` puts on the wire - and answers the page's own
// API with a scenario chosen by the test. That is the load-bearing property: there is one
// served document and one policy in this repository, and the graders read THAT rather
// than a copy of it assembled in JavaScript. A second reader would agree with the handler
// today and drift from it the first time either moved, which is the failure this
// repository has spent whole phases refusing elsewhere.
//
// A scenario is named in the query of the DOCUMENT request and remembered in a cookie, so
// the page's own subsequent request to /api/events - which carries no query of its own -
// is answered with the matching stream. Playwright gives each browser context its own
// cookie jar, so scenarios stay isolated across parallel workers.
//
// It is a TEST fixture and binds loopback only. It never touches a media file, opens no
// store, and holds nothing to lose.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NSchatz/holdfast/internal/sourceoffer"
	"github.com/NSchatz/holdfast/internal/webui"
)

// scenarioCookie carries the scenario across the page's own requests. The page asks for
// /api/events with no query, so the document request is the only place a test can say
// which stream it wants.
const scenarioCookie = "holdfast_e2e_scenario"

// clientCookie identifies ONE page load, so a scenario that behaves differently on a
// client's first and later connections behaves that way PER CLIENT.
//
// It is not a nicety. The severed stream delivers one snapshot and refuses every
// reconnection, and keying that count by the scenario name alone made it one count for
// every client the server ever had: the first test to ask for a severed stream got its
// snapshot, and every test after it was refused before the page had rendered anything -
// so the case measured a page that never loaded and timed out instead of failing.
const clientCookie = "holdfast_e2e_client"

// A scenario is how the event stream behaves. Everything the dashboard can be shown is one
// of these, and each is a state the page owes an answer to.
const (
	scenarioFull       = "full"       // a whole snapshot, held open: the happy path
	scenarioEmpty      = "empty"      // a valid snapshot with nothing in it
	scenarioLoading    = "loading"    // the stream opens and delivers nothing
	scenarioUnreadable = "unreadable" // a payload that is not JSON at all
	scenarioSevered    = "severed"    // one snapshot, then the stream drops
	scenarioNoStream   = "no-stream"  // the events endpoint answers a server error
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "loopback address to bind")
	dir := flag.String("fixtures", "fixtures", "directory holding the snapshot fixtures")
	portFile := flag.String("port-file", "", "write the bound address here once listening")
	flag.Parse()

	snaps, err := loadFixtures(*dir)
	if err != nil {
		log.Fatalf("fixtureserver: %v", err)
	}

	srv := &fixtureServer{snapshots: snaps, done: make(chan struct{})}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("fixtureserver: binding %s: %v", *addr, err)
	}
	if *portFile != "" {
		if err := os.WriteFile(*portFile, []byte("http://"+ln.Addr().String()), 0o644); err != nil {
			log.Fatalf("fixtureserver: writing %s: %v", *portFile, err)
		}
	}
	fmt.Printf("fixtureserver listening on http://%s\n", ln.Addr().String())
	log.Fatal(http.Serve(ln, srv.routes()))
}

// loadFixtures reads every snapshot fixture once, at startup, and COMPACTS each: an SSE
// `data:` field is one line, and the fixtures are committed readably. A fixture that is
// not valid JSON stops the server here, loudly, rather than surfacing later as a page
// that quietly never rendered - which is a failure mode this repository has already paid
// for once.
func loadFixtures(dir string) (map[string][]byte, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no snapshot fixture found under %s: a grader with no fixture measures nothing", dir)
	}
	out := map[string][]byte{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		var any interface{}
		if err := json.Unmarshal(raw, &any); err != nil {
			return nil, fmt.Errorf("%s is not valid JSON: %w", p, err)
		}
		compact, err := json.Marshal(any)
		if err != nil {
			return nil, fmt.Errorf("compacting %s: %w", p, err)
		}
		out[strings.TrimSuffix(filepath.Base(p), ".json")] = compact
	}
	return out, nil
}

type fixtureServer struct {
	snapshots map[string][]byte
	done      chan struct{}

	// severedSeen counts event-stream connections per scenario cookie value, so the
	// `severed` scenario can drop the stream after delivering exactly one snapshot and
	// then refuse the page's automatic reconnection.
	mu          sync.Mutex
	severedSeen map[string]int

	// control lets a spec drive a REFUSED control action: the page's own click must
	// produce the refusal, never a stub reaching inside the page.
	controlStatus atomic.Int64
}

func (s *fixtureServer) routes() http.Handler {
	page := webui.HandlerFor(sourceoffer.Current())
	mux := http.NewServeMux()

	// The document. The scenario travels in the query and is remembered in a cookie; the
	// bytes the reader gets are the handler's own, unmodified.
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			if sc := r.URL.Query().Get("scenario"); sc != "" {
				http.SetCookie(w, &http.Cookie{Name: scenarioCookie, Value: sc, Path: "/"})
			}
			// A fresh client id per document load, so per-connection behaviour is per
			// client and never shared between two tests running side by side.
			http.SetCookie(w, &http.Cookie{Name: clientCookie, Value: newClientID(), Path: "/"})
		}
		page.ServeHTTP(w, r)
	}))

	// The read endpoints the page may call. They are answered so the page is never
	// reconnecting while it is being measured, and are never held open.
	for _, ep := range []string{"/api/summary", "/api/queue", "/api/history"} {
		mux.HandleFunc(ep, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{}`))
		})
	}

	// The mutating controls, so a spec can produce a refusal by a real click.
	for _, ep := range []string{"/api/rescan", "/api/pause", "/api/resume"} {
		mux.HandleFunc(ep, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			if code := int(s.controlStatus.Load()); code != 0 {
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"reason":"the control token was refused"}`))
				return
			}
			_, _ = w.Write([]byte(`{"started":true}`))
		})
	}
	// The one endpoint that is the TEST's, not the page's: it arms the refusal above.
	mux.HandleFunc("/e2e/control-status", func(w http.ResponseWriter, r *http.Request) {
		var code int64
		_, _ = fmt.Sscanf(r.URL.Query().Get("code"), "%d", &code)
		s.controlStatus.Store(code)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/api/events", s.events)
	return mux
}

func (s *fixtureServer) events(w http.ResponseWriter, r *http.Request) {
	scenario := scenarioFull
	if c, err := r.Cookie(scenarioCookie); err == nil && c.Value != "" {
		scenario = c.Value
	}

	if scenario == scenarioNoStream {
		http.Error(w, "the snapshot endpoint is unavailable", http.StatusInternalServerError)
		return
	}

	// The severed stream: one snapshot, then gone, and the page's automatic reconnect is
	// refused so the connection state stays down while the rows stay on screen.
	if scenario == scenarioSevered {
		client := scenario
		if c, err := r.Cookie(clientCookie); err == nil && c.Value != "" {
			client = c.Value
		}
		s.mu.Lock()
		if s.severedSeen == nil {
			s.severedSeen = map[string]int{}
		}
		s.severedSeen[client]++
		n := s.severedSeen[client]
		s.mu.Unlock()
		if n > 1 {
			http.Error(w, "the event stream is unavailable", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flush(w)

	// The stream that opens and says nothing: the LOADING state every view owes.
	if scenario == scenarioLoading {
		s.hold(r)
		return
	}

	if scenario == scenarioUnreadable {
		_, _ = fmt.Fprint(w, "event: snapshot\ndata: {not json at all\n\n")
		flush(w)
		s.hold(r)
		return
	}

	body, ok := s.snapshots[scenario]
	if !ok {
		body = s.snapshots[scenarioFull]
	}
	// `now` is re-stamped per connection so every elapsed figure is measured against the
	// moment the page was actually served, not the moment the fixture was committed.
	body = restampNow(body)
	_, _ = fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", body)
	flush(w)

	if scenario == scenarioSevered {
		return // drop it: the rows must stay and every figure must stop advancing
	}
	s.hold(r)
}

// restampNow moves the whole fixture forward to the current second, carrying every
// timestamp in it by the SAME delta.
//
// A committed fixture's clock is fixed, and the page derives every elapsed figure from the
// difference between the snapshot's `now` and each row's own `updated_at`. Rewriting `now`
// alone would leave those differences growing without bound: a fixture written this morning
// would serve rows that had been encoding for hours, and an age of hours is rendered to the
// minute - so a case that needs an age to TICK would watch a figure that cannot move within
// its own lifetime. Shifting every timestamp together keeps the fixture saying exactly what
// it was written to say, at whatever time it is served.
func restampNow(body []byte) []byte {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	old, ok := m["now"].(float64)
	if !ok {
		return body
	}
	delta := float64(time.Now().Unix()) - old
	m["now"] = old + delta
	// The QUEUE's timestamps move with it and the HISTORY's do not, and the difference is
	// what each column renders. A queue row's `updated_at` is only ever read as an AGE -
	// the difference from `now` - so carrying it keeps that age what the fixture wrote and
	// lets it tick in seconds. A history row's is rendered as an ABSOLUTE local time, so
	// carrying that one would make the Updated column read differently on every connection
	// - and two readings of the same page taken a second apart would differ in a column
	// that has nothing to do with what was being compared.
	rows, ok := m["queue"].([]interface{})
	if !ok {
		return marshalOr(m, body)
	}
	for _, r := range rows {
		row, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		// A null updated_at is a fact nobody recorded and stays one; only a real
		// timestamp is carried.
		if at, ok := row["updated_at"].(float64); ok {
			row["updated_at"] = at + delta
		}
	}
	return marshalOr(m, body)
}

// marshalOr re-encodes the rewritten snapshot, falling back to the bytes as read if it
// cannot: a fixture that will not round-trip is served unchanged rather than dropped.
func marshalOr(m map[string]interface{}, fallback []byte) []byte {
	out, err := json.Marshal(m)
	if err != nil {
		return fallback
	}
	return out
}

func (s *fixtureServer) hold(r *http.Request) {
	select {
	case <-s.done:
	case <-r.Context().Done():
	}
}

// newClientID is a value that will not collide between page loads. It identifies a client
// for the length of one load and carries no meaning beyond that.
func newClientID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
