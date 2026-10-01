package health

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/schedule"
	"github.com/NSchatz/holdfast/internal/store"
)

// ---- fakes -------------------------------------------------------------------

// clock is a fake clock the sweeper's Now and Wait read, so nothing here sleeps.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// Wait advances the fake clock by d instead of sleeping.
func (c *clock) Wait(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	c.Advance(d)
	return true
}

// decodes is the DECODE SEAM: every decode the sweep asks for is counted here, so "this
// file was not decoded again" is an observation and not an inference.
type decodes struct {
	mu      sync.Mutex
	paths   []string
	corrupt map[string]string // path -> reason
	each    func(path string) // runs inside each decode, e.g. to advance the clock
	err     error             // returned instead of a verdict
}

func (d *decodes) Decode(ctx context.Context, path string) (bool, string, error) {
	d.mu.Lock()
	d.paths = append(d.paths, path)
	each, err := d.each, d.err
	reason, bad := d.corrupt[path]
	d.mu.Unlock()
	if each != nil {
		each(path)
	}
	if err != nil {
		return false, "", err
	}
	if ctx.Err() != nil {
		return false, "", ctx.Err()
	}
	return !bad, reason, nil
}

func (d *decodes) got() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.paths...)
}

// reporter records what the sweep told its reporters.
type reporter struct {
	mu       sync.Mutex
	checked  map[store.HealthResult]int
	finished []store.HealthSweep
	problems [][]store.HealthCheck
}

func (r *reporter) HealthFileChecked(res store.HealthResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.checked == nil {
		r.checked = map[store.HealthResult]int{}
	}
	r.checked[res]++
}

func (r *reporter) HealthSweepFinished(sw store.HealthSweep, p []store.HealthCheck) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = append(r.finished, sw)
	r.problems = append(r.problems, p)
}

// library writes n small regular files and returns their paths in walk order.
func library(t *testing.T, n int) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	var paths []string
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("clip%02d.mkv", i))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("synthetic %d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	return dir, paths
}

// walkSources is a Sources over every regular-or-not entry of dir, in name order.
func walkSources(dir string) func(stop func() bool, offer func(string) bool) {
	return func(stop func() bool, offer func(string) bool) {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if stop() || !offer(filepath.Join(dir, e.Name())) {
				return
			}
		}
	}
}

func openLedger(t *testing.T, path string) *store.SQLite {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return st
}

// sweeper builds a Sweeper over the fakes, at a fixed point in time.
func sweeper(st store.HealthLedger, dir string, dec Decoder, clk *clock) *Sweeper {
	s := New(24*time.Hour, 1, st, walkSources(dir), dec, nil)
	s.Now, s.Wait = clk.Now, clk.Wait
	return s
}

// ---- scheduled -----------------------------------------------------------------

// TestHealthSweep_IsScheduledFromTheLedger: with nothing recorded the first sweep is due at
// once; after it finishes, nothing is decoded until the interval has passed SINCE IT
// FINISHED; a new process on the same ledger (a restart) does not reset that; and at the
// interval the next sweep starts as a new sweep.
func TestHealthSweep_IsScheduledFromTheLedger(t *testing.T) {
	ctx := context.Background()
	dir, paths := library(t, 3)
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	st := openLedger(t, dbPath)
	clk := &clock{t: time.Date(2026, 10, 1, 2, 0, 0, 0, time.Local)}
	dec := &decodes{each: func(string) { clk.Advance(time.Minute) }}

	s := sweeper(st, dir, dec, clk)
	ran, err := s.RunDue(ctx)
	if err != nil || !ran {
		t.Fatalf("first RunDue: ran=%v err=%v, want the first sweep to be due at once", ran, err)
	}
	first, ok, err := st.LastFinishedHealthSweep(ctx)
	if err != nil || !ok {
		t.Fatalf("no finished sweep after the first run: ok=%v err=%v", ok, err)
	}
	t.Logf("sweep %d finished at %s, %d file(s) decoded", first.ID, first.FinishedAt.Format(time.Kitchen), len(dec.got()))
	if len(dec.got()) != len(paths) || first.Counts.OK != int64(len(paths)) {
		t.Fatalf("first sweep decoded %d and counted %+v, want %d ok", len(dec.got()), first.Counts, len(paths))
	}

	// A restart: a new store handle and a new sweeper, one second short of the interval.
	_ = st.Close()
	st = openLedger(t, dbPath)
	defer func() { _ = st.Close() }()
	s = sweeper(st, dir, dec, clk)
	clk.t = first.FinishedAt.Add(24*time.Hour - time.Second)
	ran, err = s.RunDue(ctx)
	if err != nil || ran {
		t.Fatalf("RunDue one second before the interval: ran=%v err=%v, want not due", ran, err)
	}
	t.Logf("at finished+24h-1s (after a restart): not due, %d decode(s) in total", len(dec.got()))
	if len(dec.got()) != len(paths) {
		t.Fatalf("a sweep that was not due decoded %d file(s)", len(dec.got())-len(paths))
	}

	clk.t = first.FinishedAt.Add(24 * time.Hour)
	ran, err = s.RunDue(ctx)
	if err != nil || !ran {
		t.Fatalf("RunDue at the interval: ran=%v err=%v, want due", ran, err)
	}
	second, _, _ := st.LastFinishedHealthSweep(ctx)
	t.Logf("at finished+24h: sweep %d ran, %d decode(s) in total", second.ID, len(dec.got()))
	if second.ID == first.ID || len(dec.got()) != 2*len(paths) {
		t.Fatalf("the due sweep is %d (first %d) with %d decodes, want a new sweep decoding every file again",
			second.ID, first.ID, len(dec.got()))
	}
}

// ---- the run window ------------------------------------------------------------

// windowMayRun is the production rule the daemon wires (schedule.Window.Contains) read
// against the fake clock.
func windowMayRun(t *testing.T, spec string, clk *clock) func(context.Context) (bool, string) {
	t.Helper()
	w, err := schedule.ParseWindow(spec)
	if err != nil {
		t.Fatal(err)
	}
	return func(context.Context) (bool, string) {
		if w.Contains(clk.Now()) {
			return true, ""
		}
		return false, "outside run window " + w.String()
	}
}

// TestHealthSweep_HonoursTheRunWindow: outside the window no decode starts and no sweep is
// opened; inside it the sweep proceeds; and when the window closes MID-SWEEP the decode in
// flight finishes, no new one starts until the window opens again, and the sweep then
// continues from where it was and finishes.
func TestHealthSweep_HonoursTheRunWindow(t *testing.T) {
	ctx := context.Background()
	dir, paths := library(t, 4)
	st := openLedger(t, filepath.Join(t.TempDir(), "jobs.db"))
	defer func() { _ = st.Close() }()
	clk := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.Local)}

	type decodeAt struct {
		path string
		at   time.Time
	}
	var mu sync.Mutex
	var log []decodeAt
	dec := &decodes{each: func(p string) {
		mu.Lock()
		log = append(log, decodeAt{p, clk.Now()})
		mu.Unlock()
		clk.Advance(50 * time.Minute) // one decode takes 50 minutes
	}}
	s := sweeper(st, dir, dec, clk)
	s.Poll = 10 * time.Minute
	s.MayRun = windowMayRun(t, "01:00-03:00", clk)

	// 12:00 is outside 01:00-03:00. Run the loop's first question by hand, bounded: a
	// context that ends while the sweeper is waiting for the window.
	waitCtx, cancel := context.WithCancel(ctx)
	waits := 0
	s.Wait = func(c context.Context, d time.Duration) bool {
		waits++
		if waits == 3 {
			if live := s.Live(); live.State != StateWaiting || !strings.Contains(live.Why, "outside run window") {
				t.Errorf("while outside the window the live state is %+v, want waiting with the window named", live)
			}
			cancel()
			return false
		}
		return clk.Wait(c, d)
	}
	if _, err := s.RunDue(waitCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunDue outside the window returned %v, want it to wait until cancelled", err)
	}
	if n := len(dec.got()); n != 0 {
		t.Fatalf("%d decode(s) started outside the run window", n)
	}
	if _, ok, _ := st.LatestHealthSweep(ctx); ok {
		t.Fatal("a sweep was opened outside the run window; none should be until a decode may start")
	}
	t.Logf("outside the window (12:00-12:20): %d wait(s), 0 decodes, no sweep opened", waits)

	// Into the window: 01:00. Two decodes fit (01:00, 01:50); the third would start at
	// 02:40, which is inside, and finishes at 03:30 - outside. The fourth must then wait
	// for the next 01:00.
	clk.t = time.Date(2026, 10, 2, 1, 0, 0, 0, time.Local)
	s.Wait = clk.Wait
	ran, err := s.RunDue(ctx)
	if err != nil || !ran {
		t.Fatalf("RunDue in the window: ran=%v err=%v", ran, err)
	}
	for _, d := range log {
		t.Logf("decoded %s at %s", filepath.Base(d.path), d.at.Format("Jan 2 15:04"))
		if h := d.at.Hour(); h < 1 || h >= 3 {
			t.Errorf("%s was decoded at %s, outside the run window", filepath.Base(d.path), d.at.Format("15:04"))
		}
	}
	if len(log) != len(paths) {
		t.Fatalf("%d decode(s), want %d", len(log), len(paths))
	}
	if log[3].at.Day() != 3 || log[2].at.Day() != 2 {
		t.Errorf("the window closed mid-sweep after the third decode, so the fourth belongs to the next "+
			"window: got the third on day %d and the fourth on day %d", log[2].at.Day(), log[3].at.Day())
	}
	sw, ok, _ := st.LastFinishedHealthSweep(ctx)
	if !ok || sw.Counts.OK != int64(len(paths)) {
		t.Fatalf("the sweep that crossed the window did not finish whole: ok=%v %+v", ok, sw)
	}
}

// ---- resumable ---------------------------------------------------------------

// TestHealthSweep_ResumesAfterARestartWithoutDecodingCheckedFilesAgain: a sweep is cut
// short (the daemon stops during its third decode), the store is closed and reopened, and
// a new sweeper resumes THE SAME sweep: the two files it checked are not decoded again,
// except one whose fingerprint moved while the daemon was down, and the sweep finishes.
func TestHealthSweep_ResumesAfterARestartWithoutDecodingCheckedFilesAgain(t *testing.T) {
	ctx := context.Background()
	dir, paths := library(t, 5)
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	st := openLedger(t, dbPath)
	clk := &clock{t: time.Date(2026, 10, 1, 2, 0, 0, 0, time.Local)}

	runCtx, stop := context.WithCancel(ctx)
	first := &decodes{}
	first.each = func(string) {
		if len(first.got()) == 3 {
			stop() // the daemon is stopped while the third decode is running
		}
	}
	s := sweeper(st, dir, first, clk)
	if _, err := s.RunDue(runCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("the interrupted sweep returned %v, want context.Canceled", err)
	}
	cut, ok, _ := st.LatestHealthSweep(ctx)
	counts, _ := st.HealthCountsOf(ctx, cut.ID)
	t.Logf("before the restart: sweep %d, finished=%v, %d decode(s) started, %d recorded",
		cut.ID, cut.Finished(), len(first.got()), counts.Checked())
	if !ok || cut.Finished() || counts.Checked() != 2 {
		t.Fatalf("the interrupted sweep is ok=%v finished=%v with %d checks, want unfinished with 2 "+
			"(the decode cut short records nothing)", ok, cut.Finished(), counts.Checked())
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// While the daemon is down, the second checked file is rewritten.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(paths[1], later, later); err != nil {
		t.Fatal(err)
	}

	st = openLedger(t, dbPath)
	defer func() { _ = st.Close() }()
	second := &decodes{}
	s = sweeper(st, dir, second, clk)
	ran, err := s.RunDue(ctx)
	if err != nil || !ran {
		t.Fatalf("resume: ran=%v err=%v", ran, err)
	}
	var names []string
	for _, p := range second.got() {
		names = append(names, filepath.Base(p))
	}
	t.Logf("after the restart: decoded %v", names)
	want := []string{filepath.Base(paths[1]), filepath.Base(paths[2]), filepath.Base(paths[3]), filepath.Base(paths[4])}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("after the restart the sweep decoded %v, want %v: clip00 was checked and is unchanged, "+
			"clip01 changed while the daemon was down", names, want)
	}
	done, ok, _ := st.LastFinishedHealthSweep(ctx)
	t.Logf("sweep %d finished with %d checked", done.ID, done.Counts.Checked())
	if !ok || done.ID != cut.ID || done.Counts.Checked() != int64(len(paths)) {
		t.Fatalf("the resumed sweep is %+v, want sweep %d finished with %d checks", done, cut.ID, len(paths))
	}
}

// ---- classification, reasons and reporting -------------------------------------

// TestHealthSweep_ClassifiesEveryFileAndReportsOnce: a corrupt file is recorded corrupt
// with the decoder's reason; a file that cannot be opened and a non-regular file are
// unreadable WITHOUT a decode; a file gone since the listing is not recorded; the reporters
// hear every result and one finished sweep with its problems.
func TestHealthSweep_ClassifiesEveryFileAndReportsOnce(t *testing.T) {
	ctx := context.Background()
	dir, paths := library(t, 4)
	if err := os.Mkdir(filepath.Join(dir, "zdir.mkv"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := openLedger(t, filepath.Join(t.TempDir(), "jobs.db"))
	defer func() { _ = st.Close() }()
	clk := &clock{t: time.Date(2026, 10, 1, 2, 0, 0, 0, time.Local)}
	dec := &decodes{corrupt: map[string]string{paths[1]: "Invalid NAL unit size"}}
	rep := &reporter{}
	s := sweeper(st, dir, dec, clk)
	s.Reporters = []Reporter{rep}
	s.open = func(p string) error {
		if p == paths[2] {
			return fs.ErrPermission
		}
		return nil
	}
	gone := paths[3]
	statReal := s.stat
	s.stat = func(p string) (fs.FileInfo, error) {
		if p == gone {
			return nil, fs.ErrNotExist
		}
		return statReal(p)
	}
	if ran, err := s.RunDue(ctx); err != nil || !ran {
		t.Fatalf("RunDue: ran=%v err=%v", ran, err)
	}
	if got := dec.got(); len(got) != 2 {
		t.Errorf("decoded %v, want only the two files that could be opened", got)
	}
	sw, _, _ := st.LastFinishedHealthSweep(ctx)
	want := store.HealthCounts{OK: 1, Corrupt: 1, Unreadable: 2}
	if sw.Counts != want {
		t.Errorf("counts %+v, want %+v", sw.Counts, want)
	}
	problems, _ := st.HealthProblems(ctx, sw.ID, 0)
	byPath := map[string]store.HealthCheck{}
	for _, p := range problems {
		byPath[filepath.Base(p.Path)] = p
	}
	if c := byPath["clip01.mkv"]; c.Result != store.HealthCorrupt || c.Reason != "Invalid NAL unit size" {
		t.Errorf("clip01: %+v, want corrupt with the decoder's reason", c)
	}
	if c := byPath["clip02.mkv"]; c.Result != store.HealthUnreadable || !strings.Contains(c.Reason, "could not open it for reading") {
		t.Errorf("clip02: %+v, want unreadable naming the open", c)
	}
	if c := byPath["zdir.mkv"]; c.Result != store.HealthUnreadable || !strings.Contains(c.Reason, "not a regular file") {
		t.Errorf("zdir.mkv: %+v, want unreadable as not a regular file", c)
	}
	if _, ok := byPath["clip03.mkv"]; ok {
		t.Error("a file gone since the listing was recorded")
	}
	if rep.checked[store.HealthOK] != 1 || rep.checked[store.HealthCorrupt] != 1 || rep.checked[store.HealthUnreadable] != 2 {
		t.Errorf("reporters heard %v", rep.checked)
	}
	if len(rep.finished) != 1 || rep.finished[0].ID != sw.ID || rep.finished[0].Counts != want || len(rep.problems[0]) != 3 {
		t.Errorf("reporters heard %d finished sweep(s) %+v with %d problems", len(rep.finished), rep.finished, len(rep.problems))
	}
}

// TestHealthSweep_AFileThatKeepsChangingIsNeverReportedOK: the fingerprint is taken again
// after the decode; a file that moved is decoded once more, and one that moves under both
// attempts is unreadable, never ok.
func TestHealthSweep_AFileThatKeepsChangingIsNeverReportedOK(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		changes int
		want    store.HealthResult
		decodes int
	}{
		{"changed once, then settled", 1, store.HealthOK, 2},
		{"changed under both attempts", 2, store.HealthUnreadable, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, paths := library(t, 1)
			st := openLedger(t, filepath.Join(t.TempDir(), "jobs.db"))
			defer func() { _ = st.Close() }()
			clk := &clock{t: time.Date(2026, 10, 1, 2, 0, 0, 0, time.Local)}
			n := 0
			dec := &decodes{each: func(p string) {
				if n < tc.changes {
					n++
					if err := os.WriteFile(p, []byte(strings.Repeat("x", 100+n)), 0o644); err != nil {
						t.Error(err)
					}
				}
			}}
			s := sweeper(st, dir, dec, clk)
			if _, err := s.RunDue(ctx); err != nil {
				t.Fatal(err)
			}
			sw, _, _ := st.LastFinishedHealthSweep(ctx)
			c, ok, _ := st.HealthCheckOf(ctx, sw.ID, paths[0])
			fi, _ := os.Stat(paths[0])
			if !ok || c.Result != tc.want || len(dec.got()) != tc.decodes || c.Size != fi.Size() {
				t.Errorf("recorded %+v after %d decodes, want %s after %d with the final size %d",
					c, len(dec.got()), tc.want, tc.decodes, fi.Size())
			}
		})
	}
}

// TestHealthSweep_ADecoderThatCannotRunLeavesTheSweepUnfinishedAndRecordsNothing: no
// verdict is not a verdict. A sweep whose decoder could not run records no file as
// corrupt, stays unfinished, and is resumed by the next attempt.
func TestHealthSweep_ADecoderThatCannotRunLeavesTheSweepUnfinishedAndRecordsNothing(t *testing.T) {
	ctx := context.Background()
	dir, _ := library(t, 3)
	st := openLedger(t, filepath.Join(t.TempDir(), "jobs.db"))
	defer func() { _ = st.Close() }()
	clk := &clock{t: time.Date(2026, 10, 1, 2, 0, 0, 0, time.Local)}
	dec := &decodes{err: ErrDecoderUnavailable}
	rep := &reporter{}
	s := sweeper(st, dir, dec, clk)
	s.Reporters = []Reporter{rep}
	ran, err := s.RunDue(ctx)
	if !ran || !errors.Is(err, ErrDecoderUnavailable) {
		t.Fatalf("ran=%v err=%v, want the decoder's error", ran, err)
	}
	sw, ok, _ := st.LatestHealthSweep(ctx)
	counts, _ := st.HealthCountsOf(ctx, sw.ID)
	if !ok || sw.Finished() || counts.Checked() != 0 || len(rep.finished) != 0 || len(rep.checked) != 0 {
		t.Errorf("sweep %+v with %+v recorded and %v reported, want unfinished with nothing", sw, counts, rep.checked)
	}
	if s.Live().State != StateIdle {
		t.Errorf("live state %+v after the attempt, want idle", s.Live())
	}
	// The next attempt resumes the same sweep.
	dec.err = nil
	if _, err := s.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	done, _, _ := st.LastFinishedHealthSweep(ctx)
	if done.ID != sw.ID || done.Counts.OK != 3 {
		t.Errorf("the next attempt finished %+v, want sweep %d with 3 ok", done, sw.ID)
	}
}

// TestHealthSweep_WorkersDecodeConcurrentlyAndCheckEachFileOnce: with several workers each
// offered file is decoded exactly once.
func TestHealthSweep_WorkersDecodeConcurrentlyAndCheckEachFileOnce(t *testing.T) {
	ctx := context.Background()
	dir, paths := library(t, 9)
	st := openLedger(t, filepath.Join(t.TempDir(), "jobs.db"))
	defer func() { _ = st.Close() }()
	clk := &clock{t: time.Date(2026, 10, 1, 2, 0, 0, 0, time.Local)}
	var inFlight, peak int
	var mu sync.Mutex
	gate := make(chan struct{})
	dec := &decodes{each: func(string) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		if inFlight == 3 {
			close(gate)
		}
		mu.Unlock()
		select {
		case <-gate:
		case <-time.After(2 * time.Second):
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
	}}
	s := sweeper(st, dir, dec, clk)
	s.Workers = 3
	if _, err := s.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	got := dec.got()
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(paths, ",") || peak != 3 {
		t.Errorf("decoded %d file(s) with a peak of %d at once, want each of %d once with 3 at once", len(got), peak, len(paths))
	}
}

// TestRun_AsksAgainEveryPollUntilCancelled drives the loop the daemon runs: it runs a due
// sweep, then waits Poll and asks again, and returns when the context ends.
func TestRun_AsksAgainEveryPollUntilCancelled(t *testing.T) {
	dir, paths := library(t, 2)
	st := openLedger(t, filepath.Join(t.TempDir(), "jobs.db"))
	defer func() { _ = st.Close() }()
	clk := &clock{t: time.Date(2026, 10, 1, 2, 0, 0, 0, time.Local)}
	dec := &decodes{}
	s := sweeper(st, dir, dec, clk)
	s.Interval = 2 * time.Hour
	s.Poll = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	polls := 0
	s.Wait = func(c context.Context, d time.Duration) bool {
		polls++
		if d != time.Hour {
			t.Errorf("waited %v, want the poll interval", d)
		}
		if polls == 4 {
			cancel()
			return false
		}
		return clk.Wait(c, d)
	}
	s.Run(ctx)
	// t=0 sweep 1; +1h not due; +2h sweep 2; +3h not due; cancelled.
	if n := len(dec.got()); n != 2*len(paths) {
		t.Errorf("%d decodes over four polls at an interval of two, want %d (two sweeps)", n, 2*len(paths))
	}
}
