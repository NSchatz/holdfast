package playback

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// fakeSource is a history that answers what it is given and records each since it was asked.
// Its read's Newest is newest where set, and otherwise the newest start among its rows.
type fakeSource struct {
	rows              []Observed
	complete          bool
	newest            int64
	resume            Resume
	named, unresolved int
	err               error
	asked             []int64
	from              []Resume
}

func (f *fakeSource) Name() string { return "tautulli" }
func (f *fakeSource) Sessions(_ context.Context, since time.Time, from Resume) ([]Observed, Read, error) {
	f.asked = append(f.asked, since.Unix())
	f.from = append(f.from, from)
	if f.err != nil {
		return nil, Read{}, f.err
	}
	read := Read{Complete: f.complete, Newest: f.newest, Resume: f.resume, Named: f.named, Unresolved: f.unresolved}
	for _, r := range f.rows {
		if f.newest == 0 {
			read.Newest = max(read.Newest, r.Started)
		}
	}
	return f.rows, read, nil
}

// memLedger is the ledger a poll reads and writes, in memory, with a failure seam per read. A
// swap reads as current while it is not superseded and not named in gone.
type memLedger struct {
	swaps          []store.SwappedFile
	gone           map[fileKey]bool
	sessions       []store.PlaybackSession
	poll           *store.PlaybackPoll
	retained       map[string]store.Retained
	recordedSwaps  int
	recordSwapsErr error
	swapsErr       error
	pollErr        error
	evErr          error
	recordErr      error
	setPollErr     error
	retErr         error
}

func (m *memLedger) RecordSwaps(context.Context, int64) (int, int, error) {
	m.recordedSwaps++
	return 0, 0, m.recordSwapsErr
}
func (m *memLedger) PlaybackSwaps(context.Context) ([]store.SwappedFile, error) {
	out := make([]store.SwappedFile, len(m.swaps))
	for i, w := range m.swaps {
		w.Current = w.SupersededAt == 0 && !m.gone[fileKey{w.Path, w.Fingerprint}]
		out[i] = w
	}
	return out, m.swapsErr
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
	return Observed{ID: id, File: file, Started: started, OffsetMS: ms(off), OffsetDerived: true, DurationMS: ms(film),
		PlayedSeconds: ms(600), Client: "client-" + id, Platform: "Chrome", Player: "Plex Web", Decision: "copy"}
}

// TestPoller_ReadsFromTheEarliestSwapThenFromTheCursor: the first poll reads from the earliest
// swap; one that read to the end leaves the next to read from the overlap before it began and
// marks the history covered to that moment; the next reads from there.
func TestPoller_ReadsFromTheEarliestSwapThenFromTheCursor(t *testing.T) {
	led := &memLedger{swaps: []store.SwappedFile{{Path: "/lib/b.mkv", Fingerprint: "2:2", SwappedAt: 5000}, {Path: "/lib/a.mkv", Fingerprint: "1:1", SwappedAt: 3000}}}
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

// TestPoller_AReadStoppedAtItsBoundContinuesForward: an incomplete read moves the cursor to the
// newest row it went through - skipped rows included - with no overlap, and does not claim the
// history covered; one that went nowhere leaves the cursor, and after StuckPolls such polls says
// so at warn, once.
func TestPoller_AReadStoppedAtItsBoundContinuesForward(t *testing.T) {
	led := &memLedger{swaps: []store.SwappedFile{{Path: "/lib/a.mkv", Fingerprint: "1:1", SwappedAt: 3000}},
		poll: &store.PlaybackPoll{Source: "tautulli", Cursor: 4000, CoveredThrough: 4500, LastAttemptAt: 1}}
	// Two kept rows, and music after them up to 4700 that the read skipped.
	src := &fakeSource{rows: []Observed{obs("1", "/lib/a.mkv", 4100, 5), obs("2", "/lib/a.mkv", 4200, 5)}, newest: 4700}
	p, buf := newTestPoller(src, led, nil, 9000)
	res := p.PollOnce(context.Background())
	if res.Complete || res.Read != 2 || res.Recorded != 2 {
		t.Fatalf("result %+v", res)
	}
	if *led.poll != (store.PlaybackPoll{Source: "tautulli", Cursor: 4700, CoveredThrough: 4500, LastAttemptAt: 9000}) {
		t.Errorf("state %+v", led.poll)
	}
	if !strings.Contains(buf.String(), "the next poll continues from here") {
		t.Errorf("an incomplete read was not said:\n%s", buf)
	}
	if StuckPolls != 3 {
		t.Errorf("StuckPolls %d", StuckPolls)
	}
	// Reads that went no further than the cursor leave it, and the third says so, once.
	src.rows, src.newest = nil, 4700
	for i := 1; i <= StuckPolls+1; i++ {
		buf.Reset()
		p.PollOnce(context.Background())
		if led.poll.Cursor != 4700 {
			t.Fatalf("poll %d moved the cursor to %d", i, led.poll.Cursor)
		}
		if said := strings.Contains(buf.String(), "has not moved for several polls"); said != (i == StuckPolls) {
			t.Errorf("poll %d: stuck said %v:\n%s", i, said, buf)
		}
	}
	// Progress clears the count: three more stuck polls are needed before it is said again.
	src.newest = 4800
	p.PollOnce(context.Background())
	src.newest = 4800
	buf.Reset()
	for i := 1; i < StuckPolls; i++ {
		p.PollOnce(context.Background())
	}
	if strings.Contains(buf.String(), "has not moved") {
		t.Errorf("said again before StuckPolls more polls:\n%s", buf)
	}
	// A complete read clears it too.
	src.complete = true
	p.PollOnce(context.Background())
	src.complete, src.newest = false, 1
	buf.Reset()
	for i := 1; i < StuckPolls; i++ {
		p.PollOnce(context.Background())
	}
	if strings.Contains(buf.String(), "has not moved") {
		t.Errorf("said again after a complete read before StuckPolls more polls:\n%s", buf)
	}
}

// TestPoller_NothingSwappedAsksNothing: with no swap recorded there is nothing to attribute a
// session to, so the source is not asked and no state is written; the swaps are still recorded.
func TestPoller_NothingSwappedAsksNothing(t *testing.T) {
	led := &memLedger{}
	src := &fakeSource{complete: true}
	p, _ := newTestPoller(src, led, nil, 100)
	if res := p.PollOnce(context.Background()); res.Asked || len(src.asked) != 0 || led.poll != nil || led.recordedSwaps != 1 {
		t.Errorf("result %+v, asked %v, state %+v, swaps recorded %d times", res, src.asked, led.poll, led.recordedSwaps)
	}
	if err := p.RecordSwaps(context.Background()); err != nil || led.recordedSwaps != 2 {
		t.Errorf("RecordSwaps: %v, %d", err, led.recordedSwaps)
	}
}

// TestPoller_AFailedReadRecordsTheAttemptAndChangesNoEvidence: the attempt and its class are
// recorded and the cursor and the coverage the previous poll left are kept; the outage is said
// once at warn, again only at debug, and its end once.
func TestPoller_AFailedReadRecordsTheAttemptAndChangesNoEvidence(t *testing.T) {
	led := &memLedger{swaps: []store.SwappedFile{{Path: "/lib/a.mkv", Fingerprint: "1:1", SwappedAt: 3000}},
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
		{"recording swaps", &memLedger{swaps: files, recordSwapsErr: boom}, false},
		{"swapped files", &memLedger{swaps: files, swapsErr: boom}, false},
		{"poll state", &memLedger{swaps: files, pollErr: boom}, false},
		{"recording", &memLedger{swaps: files, recordErr: boom}, true},
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
	led := &memLedger{swaps: files, setPollErr: boom}
	p, buf := newTestPoller(&fakeSource{complete: true, rows: []Observed{obs("1", "/lib/a.mkv", 4000, 5)}}, led, nil, 9000)
	if res := p.PollOnce(context.Background()); res.Recorded != 1 || res.Failure != "" {
		t.Errorf("result %+v", res)
	}
	if !strings.Contains(buf.String(), "the next poll reads the same history again") {
		t.Errorf("not said:\n%s", buf)
	}
}

// TestPoller_TheResumePointMovesOnlyOnceTheRowsAreStored: a read whose sessions the ledger could
// not take is read again from where it began, not from where it stopped; a stored one moves on.
func TestPoller_TheResumePointMovesOnlyOnceTheRowsAreStored(t *testing.T) {
	led := &memLedger{swaps: []store.SwappedFile{{Path: "/lib/a.mkv", Fingerprint: "1:1", SwappedAt: 3000}},
		recordErr: errors.New("disk full")}
	src := &fakeSource{rows: []Observed{obs("1", "/lib/a.mkv", 4000, 5)}, resume: Resume{After: "d", Start: 200}}
	p, _ := newTestPoller(src, led, nil, 9000)
	if res := p.PollOnce(context.Background()); res.Failure != ClassLedger {
		t.Fatalf("result %+v", res)
	}
	p.PollOnce(context.Background())
	led.recordErr = nil
	p.PollOnce(context.Background())
	p.PollOnce(context.Background())
	want := []Resume{{}, {}, {}, {After: "d", Start: 200}}
	if fmt.Sprint(src.from) != fmt.Sprint(want) {
		t.Errorf("reads continued from %v, want %v", src.from, want)
	}
}

// TestPoller_SessionsTiedToNoFileAreCountedAndSaid: the count is kept in the poll state; some is
// an info record, all of them a warn record that names the likely cause, none says nothing.
func TestPoller_SessionsTiedToNoFileAreCountedAndSaid(t *testing.T) {
	led := &memLedger{swaps: []store.SwappedFile{{Path: "/lib/a.mkv", Fingerprint: "1:1", SwappedAt: 3000}}}
	src := &fakeSource{complete: true, named: 4, unresolved: 1}
	p, buf := newTestPoller(src, led, nil, 9000)
	res := p.PollOnce(context.Background())
	if res.Named != 4 || res.Unresolved != 1 || led.poll.Unresolved != 1 {
		t.Errorf("result %+v, state %+v", res, led.poll)
	}
	if !strings.Contains(buf.String(), "level=INFO") || !strings.Contains(buf.String(), "sessions_without_a_file=1") ||
		strings.Contains(buf.String(), "cannot reach Plex") {
		t.Errorf("some unresolved:\n%s", buf)
	}
	src.unresolved = 4
	buf.Reset()
	p.PollOnce(context.Background())
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "cannot reach Plex") ||
		led.poll.Unresolved != 4 {
		t.Errorf("all unresolved:\n%s", buf)
	}
	src.named, src.unresolved = 0, 0
	buf.Reset()
	p.PollOnce(context.Background())
	if strings.Contains(buf.String(), "without_a_file") || led.poll.Unresolved != 0 {
		t.Errorf("none unresolved:\n%s", buf)
	}
}

// TestPoller_ASessionIsAttributedToTheSwapWhoseBytesItPlayed: through the Plex path map in
// reverse; to the newest swap at or before it started; never to a swap after it (it played the
// source), to a file the map does not cover, or to a file with no name or a relative one.
func TestPoller_ASessionIsAttributedToTheSwapWhoseBytesItPlayed(t *testing.T) {
	led := &memLedger{swaps: []store.SwappedFile{
		{Path: "/media/movies/Film/Film.mkv", Fingerprint: "2:2", SwappedAt: 5000},
		{Path: "/media/movies/Film/Film.mkv", Fingerprint: "1:1", SwappedAt: 3000},
		{Path: "/media/tv/Show/e1.mkv", Fingerprint: "3:3", SwappedAt: 3000},
		// Replaced by another file at 7000: a session from then on played the other file.
		{Path: "/media/movies/Gone/Gone.mkv", Fingerprint: "4:4", SwappedAt: 3000, SupersededAt: 7000},
	}}
	src := &fakeSource{complete: true, rows: []Observed{
		obs("gone-before", "/data/movies/Gone/Gone.mkv", 6999, 1),
		obs("gone-after", "/data/movies/Gone/Gone.mkv", 7000, 1),
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
	if res.Read != 12 || res.Recorded != 6 {
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
	if _, ok := got["gone-before"]; !ok {
		t.Error("a session before the swap was superseded was not attributed to it")
	}
	if _, ok := got["gone-after"]; ok {
		t.Error("a session from the second the swap was superseded was attributed to it")
	}
	if s := got["tv"]; s.Path != "/media/tv/Show/e1.mkv" || s.Fingerprint != "3:3" || s.Source != "tautulli" ||
		s.StartedAt != 6000 || *s.OffsetMS != 1 || *s.DurationMS != film || s.Platform != "Chrome" ||
		s.Player != "Plex Web" || s.Decision != "copy" || s.RecordedAt != 9000 || s.Client != "client-tv" ||
		!s.OffsetDerived || s.PlayedSeconds == nil || *s.PlayedSeconds != 600 {
		t.Errorf("tv: %+v", s)
	}
	// With no map, the media server's path is taken as holdfast's own.
	led2 := &memLedger{swaps: []store.SwappedFile{{Path: "/data/movies/Film/Film.mkv", Fingerprint: "1:1", SwappedAt: 3000}}}
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
		swaps: []store.SwappedFile{{Path: "/lib/kept.mkv", Fingerprint: "1:1", SwappedAt: 10}, {Path: "/lib/gone.mkv", Fingerprint: "2:2", SwappedAt: 10}},
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
		!strings.Contains(log, "clients_stopped_there=2") || !strings.Contains(log, "retries_that_got_nowhere=0") ||
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

func (s *signalSource) Sessions(ctx context.Context, since time.Time, from Resume) ([]Observed, Read, error) {
	select {
	case s.asked <- struct{}{}:
	default:
	}
	return s.fakeSource.Sessions(ctx, since, from)
}

// TestPoller_RunPollsAtOnceAndStopsWithItsContext. The test reads the ledger and the source only
// after Run has returned, so nothing here races the poll.
func TestPoller_RunPollsAtOnceAndStopsWithItsContext(t *testing.T) {
	led := &memLedger{swaps: []store.SwappedFile{{Path: "/lib/a.mkv", Fingerprint: "1:1", SwappedAt: 3000}}}
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

// e2eFilm is the synthetic swapped film the end-to-end tests below play.
const e2ePath, e2eFP = "/media/movies/Film/Film.mkv", "3200:1"

// e2eSwap opens a ledger with one swapped film and returns it with the swap's second.
func e2eSwap(t *testing.T) (*store.SQLite, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if _, err := st.Claim(ctx, e2ePath, e2eFP, "w1", 3, store.InputsRead(map[string]string{"crf": "22"})); err != nil {
		t.Fatal(err)
	}
	if err := st.Finish(ctx, e2ePath, e2eFP, store.Done, &store.Outcome{
		DecisionInputs: store.InputsRead(map[string]string{"crf": "22"})}, 3); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RecordSwaps(ctx, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	swaps, err := st.PlaybackSwaps(ctx)
	if err != nil || len(swaps) != 1 {
		t.Fatalf("swaps %+v %v", swaps, err)
	}
	return st, swaps[0].SwappedAt
}

// e2eRow is a history row on the film, on its own client.
func e2eRow(id int, started int64, offsetMS int64, playedSec int64, client string) map[string]any {
	r := row(id, started, 77, offsetMS)
	r["play_duration"], r["machine_id"] = playedSec, client
	return r
}

// TestPoller_EndToEndThroughTheStoreAndAFakeTautulli: a real ledger, the real Tautulli client
// against a fake server, and the issue's pattern - a viewer stopping near 33:19 and resuming
// there to no avail, and another client stopping near 34:00 - read back as a suspected stall. A
// changed encoder setting re-opens the done row and the guard re-derives it skipped: the evidence
// stays. Another file arrives at the path: sessions after that are attributed to nothing.
func TestPoller_EndToEndThroughTheStoreAndAFakeTautulli(t *testing.T) {
	st, swapped := e2eSwap(t)
	ctx := context.Background()
	f := &fakeTautulli{items: map[string]any{"77": filmItem(film, "/data/movies/Film/Film.mkv")}}
	f.rows = []map[string]any{
		e2eRow(1, swapped-3600, film, 8700, "a"), // played the source, before the swap
		e2eRow(2, swapped+60, (33*60+19)*1000, 2000, "chrome"),
		e2eRow(3, swapped+86400, (33*60+21)*1000, 90, "chrome"),
		e2eRow(4, swapped+90000, 34*60*1000, 2040, "firefox"),
	}
	src, _ := newFake(t, f)
	clock := swapped + 2*86400
	p := NewPoller(src, st, st, config.PathMap{{From: "/media", To: "/data"}}, rules, time.Minute,
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	p.now = func() time.Time { return time.Unix(clock, 0) }
	res := p.PollOnce(ctx)
	if res.Failure != "" || res.Recorded != 3 || len(res.Suspects) != 1 {
		t.Fatalf("result %+v", res)
	}
	s := res.Suspects[0]
	if s.Path != e2ePath || s.Fingerprint != e2eFP || s.Evidence.Stall.FromMS != (33*60+19)*1000 ||
		s.Evidence.Stall.Clients != 2 || s.Evidence.Stall.Retries != 1 || s.Restore != "" {
		t.Errorf("suspect %+v", s)
	}
	cfg := &config.Config{PlaybackSource: "tautulli"}
	report := func() Report {
		t.Helper()
		rep, err := BuildReport(ctx, st, cfg, time.Unix(clock, 0))
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	if rep := report(); rep.Counts.SuspectStall != 1 || rep.Counts.Swapped != 1 || len(rep.Suspects) != 1 ||
		rep.LastFailure != nil || rep.CoveredThrough == nil || *rep.CoveredThrough != clock {
		t.Errorf("report %+v", rep)
	}

	// The owner changes crf: the next scan re-opens the done row and re-derives it skipped
	// already-at-target-codec, under the same path and fingerprint.
	if ok, err := st.Claim(ctx, e2ePath, e2eFP, "w1", 3, store.InputsRead(map[string]string{"crf": "20"})); err != nil || !ok {
		t.Fatalf("re-open: %v %v", ok, err)
	}
	if err := st.Finish(ctx, e2ePath, e2eFP, store.Skipped, &store.Outcome{Reason: "already-at-target-codec"}, 3); err != nil {
		t.Fatal(err)
	}
	clock += 900
	if res := p.PollOnce(ctx); res.Failure != "" || len(res.Suspects) != 1 {
		t.Errorf("after the re-derived skip: %+v", res)
	}
	if rep := report(); rep.Counts.SuspectStall != 1 || rep.Counts.Swapped != 1 {
		t.Errorf("report after the re-derived skip: %+v", rep.Counts)
	}

	// An arr imports a new release at the path; the scan records the new bytes.
	clock += 900
	if _, err := st.Claim(ctx, e2ePath, "5000:2", "w1", 3, store.InputsRead(nil)); err != nil {
		t.Fatal(err)
	}
	if err := st.Finish(ctx, e2ePath, "5000:2", store.Skipped, &store.Outcome{Reason: "low-bitrate"}, 3); err != nil {
		t.Fatal(err)
	}
	replacedAt := time.Now().Unix()
	f.mu.Lock()
	f.rows = append(f.rows, e2eRow(5, replacedAt+60, 34*60*1000, 2040, "tv"))
	f.mu.Unlock()
	clock = replacedAt + 3600
	res = p.PollOnce(ctx)
	if res.Failure != "" || res.Recorded != 0 || len(res.Suspects) != 0 {
		t.Errorf("after the file was replaced: %+v", res)
	}
	if rep := report(); rep.Counts.Swapped != 0 || len(rep.Suspects) != 0 {
		t.Errorf("report after the file was replaced: %+v", rep)
	}
}

// TestPoller_ABoundOfMusicCannotHoldTheReadInPlace: 300 music rows before a film, read 2 pages of
// 100 a poll. The first poll goes through 200 rows and keeps none; the second continues past
// them and finds the film. The music's metadata is never asked.
func TestPoller_ABoundOfMusicCannotHoldTheReadInPlace(t *testing.T) {
	st, swapped := e2eSwap(t)
	ctx := context.Background()
	f := &fakeTautulli{items: map[string]any{"77": filmItem(film, "/data/movies/Film/Film.mkv")}}
	for i := 0; i < 300; i++ {
		r := row(1000+i, swapped+10+int64(i), 9, 1)
		r["media_type"] = "track"
		f.rows = append(f.rows, r)
	}
	f.rows = append(f.rows, e2eRow(2000, swapped+400, (33*60+19)*1000, 2000, "chrome"))
	src, _ := newFake(t, f)
	src.PageSize, src.MaxPages = 100, 2
	p := NewPoller(src, st, st, config.PathMap{{From: "/media", To: "/data"}}, rules, time.Minute,
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	p.now = func() time.Time { return time.Unix(swapped+1000, 0) }
	first := p.PollOnce(ctx)
	if first.Failure != "" || first.Complete || first.Recorded != 0 {
		t.Fatalf("first poll %+v", first)
	}
	second := p.PollOnce(ctx)
	if second.Failure != "" || !second.Complete || second.Recorded != 1 {
		t.Fatalf("second poll %+v", second)
	}
	if f.meta["9"] != 0 || f.meta["77"] != 1 {
		t.Errorf("metadata asked %v", f.meta)
	}
	if got := f.history[2].Get("start"); got != "200" {
		t.Errorf("the second poll's first page started at row %s, want 200", got)
	}
}

// failingWrites is a ledger whose session write fails the given number of times.
type failingWrites struct {
	*store.SQLite
	fails int
}

func (f *failingWrites) RecordPlaybackSessions(ctx context.Context, ss []store.PlaybackSession) (int, error) {
	if f.fails > 0 {
		f.fails--
		return 0, errors.New("disk full")
	}
	return f.SQLite.RecordPlaybackSessions(ctx, ss)
}

// TestPoller_ALostReadIsReadAgain: 250 sessions, 2 pages of 100 a poll, and the ledger refuses the
// first poll's sessions. The next poll reads those rows again rather than continuing past them,
// and every session is recorded.
func TestPoller_ALostReadIsReadAgain(t *testing.T) {
	st, swapped := e2eSwap(t)
	ctx := context.Background()
	f := &fakeTautulli{items: map[string]any{"77": filmItem(film, "/data/movies/Film/Film.mkv")}}
	for i := 0; i < 250; i++ {
		f.rows = append(f.rows, e2eRow(1+i, swapped+10+int64(i), film, 8700, fmt.Sprint("c", i)))
	}
	src, _ := newFake(t, f)
	src.PageSize, src.MaxPages = 100, 2
	writes := &failingWrites{SQLite: st, fails: 1}
	p := NewPoller(src, st, writes, config.PathMap{{From: "/media", To: "/data"}}, rules, time.Minute,
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	p.now = func() time.Time { return time.Unix(swapped+1000, 0) }
	if res := p.PollOnce(ctx); res.Failure != ClassLedger {
		t.Fatalf("first poll %+v", res)
	}
	recorded := 0
	for i := 0; i < 3; i++ {
		res := p.PollOnce(ctx)
		if res.Failure != "" {
			t.Fatalf("poll %d: %+v", i+2, res)
		}
		recorded += res.Recorded
	}
	if recorded != 250 {
		t.Errorf("recorded %d of 250 sessions", recorded)
	}
}
