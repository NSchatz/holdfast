package playback

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// fakeSource is a history that answers what it is given and records each since it was asked.
type fakeSource struct {
	rows     []Observed
	complete bool
	err      error
	asked    []int64
}

func (f *fakeSource) Name() string { return "tautulli" }
func (f *fakeSource) Sessions(_ context.Context, since time.Time) ([]Observed, bool, error) {
	f.asked = append(f.asked, since.Unix())
	if f.err != nil {
		return nil, false, f.err
	}
	return f.rows, f.complete, nil
}

// memLedger is the ledger a poll reads and writes, in memory, with a failure seam per read.
type memLedger struct {
	files      []store.SwappedFile
	sessions   []store.PlaybackSession
	poll       *store.PlaybackPoll
	retained   map[string]store.Retained
	filesErr   error
	pollErr    error
	evErr      error
	recordErr  error
	setPollErr error
	retErr     error
}

func (m *memLedger) SwappedFiles(context.Context) ([]store.SwappedFile, error) {
	return m.files, m.filesErr
}
func (m *memLedger) PlaybackEvidence(context.Context) ([]store.PlaybackSession, error) {
	return m.sessions, m.evErr
}
func (m *memLedger) PlaybackPollOf(_ context.Context, source string) (store.PlaybackPoll, bool, error) {
	if m.pollErr != nil || m.poll == nil {
		return store.PlaybackPoll{}, false, m.pollErr
	}
	return *m.poll, true, nil
}
func (m *memLedger) GetRetained(_ context.Context, p string) (store.Retained, bool, error) {
	r, ok := m.retained[p]
	return r, ok, m.retErr
}
func (m *memLedger) RecordPlaybackSessions(_ context.Context, ss []store.PlaybackSession) (int, error) {
	if m.recordErr != nil {
		return 0, m.recordErr
	}
	n := 0
outer:
	for _, s := range ss {
		for _, have := range m.sessions {
			if have.Source == s.Source && have.SessionID == s.SessionID && have.Path == s.Path {
				continue outer
			}
		}
		m.sessions = append(m.sessions, s)
		n++
	}
	return n, nil
}
func (m *memLedger) SetPlaybackPoll(_ context.Context, p store.PlaybackPoll) error {
	if m.setPollErr != nil {
		return m.setPollErr
	}
	m.poll = &p
	return nil
}

// newTestPoller is a poller over a fake source and an in-memory ledger, at a fixed clock, with
// its log captured.
func newTestPoller(src *fakeSource, led *memLedger, paths config.PathMap, now int64) (*Poller, *bytes.Buffer) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := NewPoller(src, led, led, paths, rules, time.Minute, log)
	p.now = func() time.Time { return time.Unix(now, 0) }
	return p, &buf
}

func obs(id, file string, started, off int64) Observed {
	return Observed{ID: id, File: file, Started: started, OffsetMS: ms(off), DurationMS: ms(film),
		Platform: "Chrome", Player: "Plex Web", Decision: "copy"}
}

// TestPoller_ReadsFromTheEarliestSwapThenFromTheCursor: the first poll reads from the earliest
// swap; one that read to the end leaves the next to read from the overlap before it began and
// marks the history covered to that moment; the next reads from there.
func TestPoller_ReadsFromTheEarliestSwapThenFromTheCursor(t *testing.T) {
	led := &memLedger{files: []store.SwappedFile{{Path: "/lib/b.mkv", Fingerprint: "2:2", SwappedAt: 5000}, {Path: "/lib/a.mkv", Fingerprint: "1:1", SwappedAt: 3000}}}
	src := &fakeSource{complete: true}
	now := int64(1_000_000)
	p, _ := newTestPoller(src, led, nil, now)

	res := p.PollOnce(context.Background())
	if !res.Asked || !res.Complete || res.Failure != "" || len(src.asked) != 1 || src.asked[0] != 3000 {
		t.Fatalf("first poll %+v, asked from %v; want from 3000", res, src.asked)
	}
	want := store.PlaybackPoll{Source: "tautulli", Cursor: now - int64(HistoryOverlap.Seconds()),
		CoveredThrough: now, LastAttemptAt: now}
	if led.poll == nil || *led.poll != want {
		t.Fatalf("state %+v, want %+v", led.poll, want)
	}
	if HistoryOverlap != 48*time.Hour {
		t.Errorf("overlap %v", HistoryOverlap)
	}
	p.PollOnce(context.Background())
	if src.asked[1] != want.Cursor {
		t.Errorf("second poll read from %d, want the cursor %d", src.asked[1], want.Cursor)
	}
	// A cursor earlier than every swap reads from the earliest swap instead.
	led.poll.Cursor = 10
	p.PollOnce(context.Background())
	if src.asked[2] != 3000 {
		t.Errorf("a cursor before every swap: read from %d, want 3000", src.asked[2])
	}
}

// TestPoller_AReadStoppedAtItsBoundContinuesForward: an incomplete read moves the cursor to its
// newest session, with no overlap, and does not claim the history covered.
func TestPoller_AReadStoppedAtItsBoundContinuesForward(t *testing.T) {
	led := &memLedger{files: []store.SwappedFile{{Path: "/lib/a.mkv", Fingerprint: "1:1", SwappedAt: 3000}},
		poll: &store.PlaybackPoll{Source: "tautulli", Cursor: 4000, CoveredThrough: 4500, LastAttemptAt: 1}}
	src := &fakeSource{rows: []Observed{obs("1", "/lib/a.mkv", 4100, 5), obs("2", "/lib/a.mkv", 4200, 5)}}
	p, buf := newTestPoller(src, led, nil, 9000)
	res := p.PollOnce(context.Background())
	if res.Complete || res.Read != 2 || res.Recorded != 2 {
		t.Fatalf("result %+v", res)
	}
	if *led.poll != (store.PlaybackPoll{Source: "tautulli", Cursor: 4200, CoveredThrough: 4500, LastAttemptAt: 9000}) {
		t.Errorf("state %+v", led.poll)
	}
	if !strings.Contains(buf.String(), "the next poll continues from here") {
		t.Errorf("an incomplete read was not said:\n%s", buf)
	}
	// An incomplete read that returned nothing leaves the cursor where it was.
	src.rows = nil
	p.PollOnce(context.Background())
	if led.poll.Cursor != 4200 {
		t.Errorf("an empty incomplete read moved the cursor to %d", led.poll.Cursor)
	}
}

// TestPoller_NothingSwappedAsksNothing: with no done row there is nothing to attribute a session
// to, so the source is not asked and no state is written.
func TestPoller_NothingSwappedAsksNothing(t *testing.T) {
	led := &memLedger{}
	src := &fakeSource{complete: true}
	p, _ := newTestPoller(src, led, nil, 100)
	if res := p.PollOnce(context.Background()); res.Asked || len(src.asked) != 0 || led.poll != nil {
		t.Errorf("result %+v, asked %v, state %+v", res, src.asked, led.poll)
	}
}

// TestPoller_AFailedReadRecordsTheAttemptAndChangesNoEvidence: the attempt and its class are
// recorded and the cursor and the coverage the previous poll left are kept; the outage is said
// once at warn, again only at debug, and its end once.
func TestPoller_AFailedReadRecordsTheAttemptAndChangesNoEvidence(t *testing.T) {
	led := &memLedger{files: []store.SwappedFile{{Path: "/lib/a.mkv", Fingerprint: "1:1", SwappedAt: 3000}},
		poll: &store.PlaybackPoll{Source: "tautulli", Cursor: 4000, CoveredThrough: 4500, LastAttemptAt: 1}}
	src := &fakeSource{err: &SourceError{Class: ClassResult, Err: errors.New("tautulli: get_history answered result \"error\"")},
		rows: []Observed{obs("1", "/lib/a.mkv", 4100, 5)}}
	p, buf := newTestPoller(src, led, nil, 9000)

	res := p.PollOnce(context.Background())
	if res.Failure != ClassResult || !res.Asked || res.Recorded != 0 {
		t.Fatalf("result %+v", res)
	}
	if *led.poll != (store.PlaybackPoll{Source: "tautulli", Cursor: 4000, CoveredThrough: 4500, LastAttemptAt: 9000,
		LastFailure: ClassResult}) {
		t.Errorf("state %+v", led.poll)
	}
	if len(led.sessions) != 0 {
		t.Errorf("a failed read recorded %d sessions", len(led.sessions))
	}
	p.PollOnce(context.Background())
	log := buf.String()
	if strings.Count(log, "level=WARN") != 1 || !strings.Contains(log, "every file keeps the verdict it had") ||
		!strings.Contains(log, "failure=error-result") || !strings.Contains(log, "the history still cannot be read") {
		t.Errorf("outage records:\n%s", log)
	}
	src.err = nil
	src.complete = true
	buf.Reset()
	res = p.PollOnce(context.Background())
	if res.Failure != "" || led.poll.LastFailure != "" || res.Recorded != 1 {
		t.Errorf("recovery: %+v, state %+v", res, led.poll)
	}
	if !strings.Contains(buf.String(), "the history answers again") || !strings.Contains(buf.String(), "sessions=1") {
		t.Errorf("recovery records:\n%s", buf)
	}
	buf.Reset()
	p.PollOnce(context.Background())
	if strings.Contains(buf.String(), "answers again") || strings.Contains(buf.String(), "recorded sessions") ||
		strings.Contains(buf.String(), "reads the same history again") || strings.Contains(buf.String(), "could not record") {
		t.Errorf("a quiet poll said something:\n%s", buf)
	}
	if strings.Contains(log, "could not record the failed poll") {
		t.Errorf("a failed poll whose state was recorded said it was not:\n%s", log)
	}
	// A failed read whose attempt cannot be recorded either says so.
	led.setPollErr = errors.New("read-only file system")
	src.err = errors.New("down")
	buf.Reset()
	p.PollOnce(context.Background())
	if !strings.Contains(buf.String(), "could not record the failed poll in the ledger") {
		t.Errorf("an unrecorded failed poll was not said:\n%s", buf)
	}
}

// TestPoller_LedgerFailuresAreFailuresOfTheirOwn: an unreadable ledger asks nothing, and one that
// cannot take the sessions keeps the previous state as a failure.
func TestPoller_LedgerFailuresAreFailuresOfTheirOwn(t *testing.T) {
	boom := errors.New("disk I/O error")
	files := []store.SwappedFile{{Path: "/lib/a.mkv", Fingerprint: "1:1", SwappedAt: 3000}}
	for _, tc := range []struct {
		name  string
		led   *memLedger
		asked bool
	}{
		{"swapped files", &memLedger{files: files, filesErr: boom}, false},
		{"poll state", &memLedger{files: files, pollErr: boom}, false},
		{"recording", &memLedger{files: files, recordErr: boom}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{complete: true, rows: []Observed{obs("1", "/lib/a.mkv", 4000, 5)}}
			p, buf := newTestPoller(src, tc.led, nil, 9000)
			res := p.PollOnce(context.Background())
			if res.Failure != ClassLedger || res.Asked != tc.asked || (len(src.asked) > 0) != tc.asked {
				t.Errorf("result %+v, asked %v", res, src.asked)
			}
			if tc.asked && (tc.led.poll == nil || tc.led.poll.LastFailure != ClassLedger || tc.led.poll.CoveredThrough != 0) {
				t.Errorf("state %+v", tc.led.poll)
			}
			if !strings.Contains(buf.String(), "level=WARN") {
				t.Errorf("not said:\n%s", buf)
			}
		})
	}
	// A poll whose state cannot be written still recorded its sessions, and says so.
	led := &memLedger{files: files, setPollErr: boom}
	p, buf := newTestPoller(&fakeSource{complete: true, rows: []Observed{obs("1", "/lib/a.mkv", 4000, 5)}}, led, nil, 9000)
	if res := p.PollOnce(context.Background()); res.Recorded != 1 || res.Failure != "" {
		t.Errorf("result %+v", res)
	}
	if !strings.Contains(buf.String(), "the next poll reads the same history again") {
		t.Errorf("not said:\n%s", buf)
	}
}

// TestPoller_ASessionIsAttributedToTheSwapWhoseBytesItPlayed: through the Plex path map in
// reverse; to the newest swap at or before it started; never to a swap after it (it played the
// source), to a file the map does not cover, or to a file with no name or a relative one.
func TestPoller_ASessionIsAttributedToTheSwapWhoseBytesItPlayed(t *testing.T) {
	led := &memLedger{files: []store.SwappedFile{
		{Path: "/media/movies/Film/Film.mkv", Fingerprint: "2:2", SwappedAt: 5000},
		{Path: "/media/movies/Film/Film.mkv", Fingerprint: "1:1", SwappedAt: 3000},
		{Path: "/media/tv/Show/e1.mkv", Fingerprint: "3:3", SwappedAt: 3000},
	}}
	src := &fakeSource{complete: true, rows: []Observed{
		obs("before-any-swap", "/data/movies/Film/Film.mkv", 2999, 1),
		obs("first-swap", "/data/movies/Film/Film.mkv", 3000, 1),
		obs("still-first", "/data/movies/Film/Film.mkv", 4999, 1),
		obs("second-swap", "/data/movies/Film/Film.mkv", 5000, 1),
		obs("unclean", "/data/movies/Film/../Film/Film.mkv", 6000, 1),
		obs("uncovered", "/elsewhere/Film.mkv", 6000, 1),
		obs("not-swapped", "/data/movies/Other/Other.mkv", 6000, 1),
		obs("no-file", "", 6000, 1),
		obs("relative", "data/movies/Film/Film.mkv", 6000, 1),
		obs("tv", "/data/tv/Show/e1.mkv", 6000, 1),
	}}
	paths := config.PathMap{{From: "/media/movies", To: "/data/movies"}, {From: "/media/tv", To: "/data/tv"}}
	p, _ := newTestPoller(src, led, paths, 9000)
	res := p.PollOnce(context.Background())
	if res.Read != 10 || res.Recorded != 5 {
		t.Fatalf("result %+v", res)
	}
	got := map[string]store.PlaybackSession{}
	for _, s := range led.sessions {
		got[s.SessionID] = s
	}
	for id, fp := range map[string]string{"first-swap": "1:1", "still-first": "1:1", "second-swap": "2:2", "unclean": "2:2"} {
		if s, ok := got[id]; !ok || s.Fingerprint != fp || s.Path != "/media/movies/Film/Film.mkv" {
			t.Errorf("%s: %+v, want fingerprint %s", id, s, fp)
		}
	}
	if s := got["tv"]; s.Path != "/media/tv/Show/e1.mkv" || s.Fingerprint != "3:3" || s.Source != "tautulli" ||
		s.StartedAt != 6000 || *s.OffsetMS != 1 || *s.DurationMS != film || s.Platform != "Chrome" ||
		s.Player != "Plex Web" || s.Decision != "copy" || s.RecordedAt != 9000 {
		t.Errorf("tv: %+v", s)
	}
	// With no map, the media server's path is taken as holdfast's own.
	led2 := &memLedger{files: []store.SwappedFile{{Path: "/data/movies/Film/Film.mkv", Fingerprint: "1:1", SwappedAt: 3000}}}
	src2 := &fakeSource{complete: true, rows: []Observed{obs("x", "/data/movies/Film/Film.mkv", 4000, 1)}}
	p2, _ := newTestPoller(src2, led2, nil, 9000)
	if res := p2.PollOnce(context.Background()); res.Recorded != 1 {
		t.Errorf("no map: %+v", res)
	}
}

// TestPoller_ASuspectedStallIsSaidOnceWithWhatWouldWalkItBack: at warn, with the restore command
// while a live retention of this very swap holds the original, and without it otherwise; once
// per process, and again only after the file stopped being one.
func TestPoller_ASuspectedStallIsSaidOnceWithWhatWouldWalkItBack(t *testing.T) {
	stalled := func(path, fp string) []store.PlaybackSession {
		a, b := at("s1", 2_000_000, film), at("s2", 2_040_000, film)
		a.Path, a.Fingerprint, b.Path, b.Fingerprint = path, fp, path, fp
		b.Platform, b.Player = "macOS", "Plex for Mac"
		return []store.PlaybackSession{a, b}
	}
	led := &memLedger{
		files: []store.SwappedFile{{Path: "/lib/kept.mkv", Fingerprint: "1:1", SwappedAt: 10}, {Path: "/lib/gone.mkv", Fingerprint: "2:2", SwappedAt: 10}},
		retained: map[string]store.Retained{
			"/lib/kept.mkv": {SourcePath: "/lib/kept.mkv", SwappedPath: "/lib/kept.mkv", SwappedFingerprint: "1:1"},
		},
	}
	led.sessions = append(stalled("/lib/kept.mkv", "1:1"), stalled("/lib/gone.mkv", "2:2")...)
	p, buf := newTestPoller(&fakeSource{complete: true}, led, nil, 9000)
	res := p.PollOnce(context.Background())
	if len(res.Suspects) != 2 || res.Suspects[0].Path != "/lib/gone.mkv" || res.Suspects[1].Restore != "holdfast restore /lib/kept.mkv" ||
		res.Suspects[0].Restore != "" {
		t.Fatalf("suspects %+v", res.Suspects)
	}
	log := buf.String()
	if strings.Count(log, "looks like it stalls") != 2 ||
		!strings.Contains(log, "`holdfast restore /lib/kept.mkv` would put it back") ||
		!strings.Contains(log, "no longer retained, so replacing it means acquiring it again") ||
		!strings.Contains(log, "stall_at=0:33:20") || !strings.Contains(log, "sessions_ending_there=2") ||
		!strings.Contains(log, `clients="Chrome/Plex Web, macOS/Plex for Mac"`) {
		t.Errorf("suspect records:\n%s", log)
	}
	buf.Reset()
	p.PollOnce(context.Background())
	if strings.Contains(buf.String(), "looks like it stalls") {
		t.Errorf("a suspect was said twice:\n%s", buf)
	}
	// The kept file is played through by... nothing; it stops being a suspect only when its
	// sessions stop forming a stall. Drop its sessions, poll, and bring them back.
	led.sessions = stalled("/lib/gone.mkv", "2:2")
	p.PollOnce(context.Background())
	led.sessions = append(led.sessions, stalled("/lib/kept.mkv", "1:1")...)
	buf.Reset()
	p.PollOnce(context.Background())
	if strings.Count(buf.String(), "looks like it stalls") != 1 || !strings.Contains(buf.String(), "file=/lib/kept.mkv") {
		t.Errorf("a suspect that came back was not said again, once:\n%s", buf)
	}
	// An unreadable evidence read reports nothing and says so.
	led.evErr = errors.New("locked")
	buf.Reset()
	if res := p.PollOnce(context.Background()); res.Suspects != nil || !strings.Contains(buf.String(), "no suspected stall is reported") {
		t.Errorf("an unreadable evidence read: %+v\n%s", res, buf)
	}
}

// TestPoller_ClientsAreNamedOnceEach: a client named twice is listed once, in order, and one
// that named nothing is listed as such; an offset is h:mm:ss past an hour too.
func TestPoller_ClientsAreNamedOnceEach(t *testing.T) {
	a, b, c, d := at("a", 1, film), at("b", 1, film), at("c", 1, film), at("d", 1, film)
	b.Platform, b.Player = "", ""
	c.Platform, c.Player = "Tizen", ""
	if got := clientList([]store.PlaybackSession{a, b, a, c, d}); got != "Chrome/Plex Web, unnamed client, Tizen" {
		t.Errorf("clients %q", got)
	}
	if got := offsetText((1*3600 + 2*60 + 3) * 1000); got != "1:02:03" {
		t.Errorf("offset %q", got)
	}
}

// signalSource is a fakeSource that says, once, that it has been asked.
type signalSource struct {
	*fakeSource
	asked chan struct{}
}

func (s *signalSource) Sessions(ctx context.Context, since time.Time) ([]Observed, bool, error) {
	select {
	case s.asked <- struct{}{}:
	default:
	}
	return s.fakeSource.Sessions(ctx, since)
}

// TestPoller_RunPollsAtOnceAndStopsWithItsContext. The test reads the ledger and the source only
// after Run has returned, so nothing here races the poll.
func TestPoller_RunPollsAtOnceAndStopsWithItsContext(t *testing.T) {
	led := &memLedger{files: []store.SwappedFile{{Path: "/lib/a.mkv", Fingerprint: "1:1", SwappedAt: 3000}}}
	src := &signalSource{fakeSource: &fakeSource{complete: true}, asked: make(chan struct{}, 1)}
	var buf bytes.Buffer
	p := NewPoller(src, led, led, nil, rules, time.Hour, slog.New(slog.NewTextHandler(&buf, nil)))
	p.now = func() time.Time { return time.Unix(9000, 0) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	select {
	case <-src.asked:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not poll at once")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when its context ended")
	}
	if led.poll == nil || len(src.fakeSource.asked) != 1 {
		t.Errorf("one poll at once and no other: state %+v, asked %v", led.poll, src.fakeSource.asked)
	}
	if !strings.Contains(buf.String(), "poll_interval=1h0m0s") || !strings.Contains(buf.String(), "source=tautulli") {
		t.Errorf("start record:\n%s", buf.String())
	}
}

// TestPoller_EndToEndThroughTheStoreAndAFakeTautulli: a real ledger, the real Tautulli client
// against a fake server, and the issue's pattern - two clients ending near 33:19 and 34:00 of a
// swapped film - read back as a suspected stall.
func TestPoller_EndToEndThroughTheStoreAndAFakeTautulli(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	const path, fp = "/media/movies/Film/Film.mkv", "3200:1"
	if _, err := st.Claim(ctx, path, fp, "w1", 3, store.InputsRead(nil)); err != nil {
		t.Fatal(err)
	}
	if err := st.Finish(ctx, path, fp, store.Done, &store.Outcome{}, 3); err != nil {
		t.Fatal(err)
	}
	files, err := st.SwappedFiles(ctx)
	if err != nil || len(files) != 1 {
		t.Fatalf("swapped files %+v %v", files, err)
	}
	swapped := files[0].SwappedAt

	f := &fakeTautulli{items: map[string]any{"77": filmItem(film, "/data/movies/Film/Film.mkv")}}
	f.rows = []map[string]any{
		row(1, swapped-3600, 77, film), // played the source, before the swap
		row(2, swapped+60, 77, int64((33*60+19)*1000)),
		row(3, swapped+86400, 77, int64(34*60*1000)),
	}
	src, _ := newFake(t, f)
	p := NewPoller(src, st, st, config.PathMap{{From: "/media", To: "/data"}}, rules, time.Minute, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	p.now = func() time.Time { return time.Unix(swapped+2*86400, 0) }
	res := p.PollOnce(ctx)
	if res.Failure != "" || res.Recorded != 2 || len(res.Suspects) != 1 {
		t.Fatalf("result %+v", res)
	}
	s := res.Suspects[0]
	if s.Path != path || s.Fingerprint != fp || s.Evidence.Stall.FromMS != (33*60+19)*1000 || s.Restore != "" {
		t.Errorf("suspect %+v", s)
	}
	cfg := &config.Config{PlaybackSource: "tautulli"}
	rep, err := BuildReport(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Counts.SuspectStall != 1 || rep.Counts.Swapped != 1 || len(rep.Suspects) != 1 ||
		rep.LastFailure != nil || rep.CoveredThrough == nil || *rep.CoveredThrough != swapped+2*86400 {
		t.Errorf("report %+v", rep)
	}
}
