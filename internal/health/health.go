// Package health is the library health sweep (docs/design/health-sweep.md#health-sweep):
// on a schedule, under `holdfast serve`, it fully decodes every source the enumeration
// offers and RECORDS what it found. It is report-only by construction. Nothing here
// writes, renames, moves, deletes, truncates or touches a library file, and nothing it
// records is read by the encode pipeline: the sweep reports and an operator decides.
//
// The sweep is resumable. Its progress is the ledger's (store.HealthLedger), so a restart
// mid-sweep resumes the same sweep and decodes only the files it has not yet checked, or
// whose size or modification time moved since it checked them.
package health

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

// Decoder fully decodes one file. ok reports a clean decode to the end; when it is false,
// reason says what the decoder reported. A non-nil err means NO VERDICT was reached - the
// decoder could not be run, or ctx ended - and nothing is recorded for the file.
type Decoder interface {
	Decode(ctx context.Context, path string) (ok bool, reason string, err error)
}

// Reporter hears what a sweep found. Every method must be cheap and non-blocking: it runs
// on the sweep's own goroutines.
type Reporter interface {
	// HealthFileChecked is told of every result recorded.
	HealthFileChecked(result store.HealthResult)
	// HealthSweepFinished is told once per sweep that ran to the end, with the sweep's
	// counts and the files it found corrupt or unreadable.
	HealthSweepFinished(sweep store.HealthSweep, problems []store.HealthCheck)
}

// State is what the sweep is doing right now, as the read surface reports it.
type State string

const (
	// StateOff is a daemon whose configuration schedules no sweep.
	StateOff State = "off"
	// StateIdle is a sweep scheduled and not due.
	StateIdle State = "idle"
	// StateRunning is a sweep under way.
	StateRunning State = "running"
	// StateWaiting is a sweep that is due or under way and may not start a decode now: the
	// daemon is paused or the run window, the load cap or the streaming pause says no.
	StateWaiting State = "waiting"
)

// Live is the sweeper's in-memory state, which the ledger does not hold.
type Live struct {
	State State
	// Why is the scheduler's reason while State is StateWaiting.
	Why string
}

// maxReportedProblems bounds the problem list a finished sweep hands its reporters, so one
// notification cannot grow with the library.
const maxReportedProblems = 20

// DefaultPoll is how often an idle sweeper asks whether a sweep is due, and how often a
// waiting one asks the scheduler again. A minute is ASSUMED to be fine-grained enough for
// an interval measured in hours and for a run window measured in minutes.
const DefaultPoll = time.Minute

// Sweeper runs the sweep. The zero value is not usable; set every field New sets.
type Sweeper struct {
	// Interval is the time from the end of one sweep to the start of the next.
	Interval time.Duration
	// Workers is how many decodes run at once. One is the default and the modest choice.
	Workers int
	// Ledger is where the sweep's progress and results are kept.
	Ledger store.HealthLedger
	// Sources hands every source the enumeration offers to offer, stopping when stop
	// returns true or offer returns false. It is the engine's own enumeration in the
	// daemon, so the sweep reads the set a scan would offer: the path filters, the
	// retention area and the work-in-progress names are kept out exactly as they are there.
	Sources func(stop func() bool, offer func(path string) bool)
	// Decoder is the full decode.
	Decoder Decoder
	// MayRun answers whether a NEW decode may start now, and why not. nil always may.
	MayRun func(ctx context.Context) (bool, string)
	// Reporters hear what was found; nil entries are not allowed.
	Reporters []Reporter
	Log       *slog.Logger

	// Seams. Production uses the wall clock and the filesystem.
	Now  func() time.Time
	Wait func(ctx context.Context, d time.Duration) bool
	Poll time.Duration
	stat func(string) (fs.FileInfo, error)
	open func(string) error

	mu   sync.Mutex
	live Live
}

// New builds a Sweeper with the production seams.
func New(interval time.Duration, workers int, ledger store.HealthLedger,
	sources func(stop func() bool, offer func(path string) bool), dec Decoder, log *slog.Logger) *Sweeper {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Sweeper{
		Interval: interval, Workers: workers, Ledger: ledger, Sources: sources, Decoder: dec,
		Log: log.With("component", "health-sweep"), Now: time.Now, Wait: sleep, Poll: DefaultPoll,
		stat: os.Stat, open: openForRead, live: Live{State: StateIdle},
	}
}

// sleep waits d or until ctx ends, reporting whether the wait ran its course.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// openForRead proves this process may READ the file, and closes it at once. It opens for
// reading only; nothing is written.
func openForRead(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return f.Close()
}

// Live is the sweeper's current state.
func (s *Sweeper) Live() Live {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}

func (s *Sweeper) setLive(l Live) {
	s.mu.Lock()
	s.live = l
	s.mu.Unlock()
}

// Run asks whether a sweep is due every Poll, and runs one whenever it is, until ctx ends.
func (s *Sweeper) Run(ctx context.Context) {
	s.Log.Info("health sweep scheduled: every source is fully decoded and the result recorded; no file is ever changed",
		"interval", s.Interval.String(), "workers", s.Workers)
	for {
		if _, err := s.RunDue(ctx); err != nil && ctx.Err() == nil {
			s.Log.Warn("the health sweep stopped before it finished; it resumes from where it got to on the next attempt",
				"err", err, "next", "asked again in "+s.Poll.String())
		}
		if !s.Wait(ctx, s.Poll) {
			return
		}
	}
}

// RunDue starts a sweep, or resumes the one a restart interrupted, when one is due, and
// runs it until it finishes or ctx ends. ran reports whether a sweep was due.
func (s *Sweeper) RunDue(ctx context.Context) (ran bool, err error) {
	sw, due, err := s.due(ctx)
	if err != nil || !due {
		return false, err
	}
	err = s.run(ctx, sw)
	s.setLive(Live{State: StateIdle})
	return true, err
}

// due decides from the LEDGER, never from memory, so a restart neither resets the
// interval nor forgets a sweep it interrupted. An unfinished sweep is always due: it is
// resumed. A finished one makes the next due Interval after it finished. With no sweep
// recorded at all, the first one is due now.
func (s *Sweeper) due(ctx context.Context) (store.HealthSweep, bool, error) {
	latest, ok, err := s.Ledger.LatestHealthSweep(ctx)
	if err != nil {
		return store.HealthSweep{}, false, err
	}
	if !ok {
		return store.HealthSweep{}, true, nil
	}
	if !latest.Finished() {
		return latest, true, nil
	}
	return store.HealthSweep{}, !s.Now().Before(latest.FinishedAt.Add(s.Interval)), nil
}

// NextDue is when the next sweep is due after a sweep that finished at finished.
func (s *Sweeper) NextDue(finished time.Time) time.Time { return finished.Add(s.Interval) }

// permitted waits until a new decode may start, reporting false only when ctx ended
// first. While it waits the live state says why, and the reason is logged once per wait.
func (s *Sweeper) permitted(ctx context.Context) bool {
	if s.MayRun == nil {
		return ctx.Err() == nil
	}
	logged := false
	for {
		if ctx.Err() != nil {
			return false
		}
		ok, why := s.MayRun(ctx)
		if ok {
			if logged {
				s.Log.Info("health sweep continuing: new decodes may start again")
			}
			s.setLive(Live{State: StateRunning})
			return true
		}
		s.setLive(Live{State: StateWaiting, Why: why})
		if !logged {
			s.Log.Info("health sweep waiting: no new decode starts until this clears; a decode already running finishes",
				"why", why)
			logged = true
		}
		if !s.Wait(ctx, s.Poll) {
			return false
		}
	}
}

// run starts (sw.ID == 0) or resumes a sweep and drives it to the end of the enumeration.
// A sweep is FINISHED only when every offered source was checked: a cancelled one, or one
// whose decoder or ledger failed, stays unfinished and is resumed next time.
func (s *Sweeper) run(ctx context.Context, sw store.HealthSweep) error {
	if sw.ID == 0 {
		// A new sweep is not even opened until a decode could start, so a sweep that
		// falls due outside the run window leaves no row until the window opens.
		if !s.permitted(ctx) {
			return ctx.Err()
		}
		started, err := s.Ledger.StartHealthSweep(ctx, s.Now())
		if err != nil {
			return err
		}
		sw = started
		s.Log.Info("health sweep started", "sweep", sw.ID)
	} else {
		s.Log.Info("health sweep resumed: files it already checked are not decoded again unless they changed",
			"sweep", sw.ID, "started_at", sw.StartedAt.UTC().Format(time.RFC3339))
	}
	s.setLive(Live{State: StateRunning})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		failMu sync.Mutex
		failed error
	)
	fail := func(err error) {
		failMu.Lock()
		if failed == nil {
			failed = err
		}
		failMu.Unlock()
		cancel()
	}

	paths := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < max(s.Workers, 1); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range paths {
				if runCtx.Err() != nil {
					continue
				}
				if err := s.check(runCtx, sw.ID, p); err != nil {
					fail(err)
				}
			}
		}()
	}
	s.Sources(func() bool { return runCtx.Err() != nil }, func(p string) bool {
		select {
		case paths <- p:
			return true
		case <-runCtx.Done():
			return false
		}
	})
	close(paths)
	wg.Wait()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if failed != nil {
		return failed
	}
	return s.finish(ctx, sw)
}

// finish marks the sweep finished and tells the reporters.
func (s *Sweeper) finish(ctx context.Context, sw store.HealthSweep) error {
	if err := s.Ledger.FinishHealthSweep(ctx, sw.ID, s.Now()); err != nil {
		return err
	}
	done, ok, err := s.Ledger.LatestHealthSweep(ctx)
	if err != nil {
		return err
	}
	if !ok || done.ID != sw.ID {
		return fmt.Errorf("health sweep %d was finished and is not the newest sweep", sw.ID)
	}
	problems, err := s.Ledger.HealthProblems(ctx, sw.ID, maxReportedProblems)
	if err != nil {
		return err
	}
	s.Log.Info("health sweep finished", "sweep", done.ID, "checked", done.Counts.Checked(),
		"ok", done.Counts.OK, "corrupt", done.Counts.Corrupt, "unreadable", done.Counts.Unreadable)
	for _, r := range s.Reporters {
		r.HealthSweepFinished(done, problems)
	}
	return nil
}

// fingerprint is the pair a check is keyed against: the size and the modification time.
type fingerprint struct{ size, mtimeNS int64 }

func fingerprintOf(fi fs.FileInfo) fingerprint {
	return fingerprint{size: fi.Size(), mtimeNS: fi.ModTime().UnixNano()}
}

// errVanished is a file that was offered and is not there any more.
var errVanished = errors.New("the file is gone")

// check decides one file and records the result. It returns an error only for a fault
// that should stop the sweep (the decoder could not run, the ledger failed, ctx ended);
// every property of the FILE is a recorded result instead.
func (s *Sweeper) check(ctx context.Context, sweepID int64, path string) error {
	fi, err := s.stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		// Gone between the listing and now - replaced through a swap's rename is not this,
		// that leaves a file at the path. Nothing is there to report on.
		s.Log.Debug("health sweep: an offered file is gone; nothing to check", "file", path)
		return nil
	}
	if err != nil {
		return s.record(ctx, sweepID, path, fingerprint{}, store.HealthUnreadable,
			"could not read its attributes: "+err.Error())
	}
	fp := fingerprintOf(fi)
	if !fi.Mode().IsRegular() {
		// Never opened: opening a FIFO for reading blocks, and a device is not media.
		return s.record(ctx, sweepID, path, fp, store.HealthUnreadable, "not a regular file ("+fi.Mode().Type().String()+")")
	}
	prior, ok, err := s.Ledger.HealthCheckOf(ctx, sweepID, path)
	if err != nil {
		return err
	}
	if ok && prior.Size == fp.size && prior.MtimeNS == fp.mtimeNS {
		return nil // checked already in this sweep, and the bytes are the ones it checked
	}
	if !s.permitted(ctx) {
		return ctx.Err()
	}
	result, reason, fp, err := s.decode(ctx, path, fp)
	if errors.Is(err, errVanished) {
		s.Log.Debug("health sweep: a file went away while it was being checked", "file", path)
		return nil
	}
	if err != nil {
		return err
	}
	return s.record(ctx, sweepID, path, fp, result, reason)
}

// decode reads the file through the decoder and makes sure the verdict is about ONE set of
// bytes: the fingerprint is taken again afterwards and, if it moved, the decode is run once
// more. A file that changes under both attempts is reported unreadable, never ok.
func (s *Sweeper) decode(ctx context.Context, path string, fp fingerprint) (store.HealthResult, string, fingerprint, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if err := s.open(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "", "", fp, errVanished
			}
			return store.HealthUnreadable, "could not open it for reading: " + err.Error(), fp, nil
		}
		ok, reason, err := s.Decoder.Decode(ctx, path)
		if err != nil {
			return "", "", fp, err
		}
		after, err := s.stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", fp, errVanished
		}
		if err != nil {
			return store.HealthUnreadable, "could not read its attributes after decoding: " + err.Error(), fp, nil
		}
		if now := fingerprintOf(after); now != fp {
			fp = now
			continue
		}
		if ok {
			return store.HealthOK, "", fp, nil
		}
		return store.HealthCorrupt, reason, fp, nil
	}
	return store.HealthUnreadable, "the file changed while it was being decoded, twice", fp, nil
}

// record writes one result and tells the reporters.
func (s *Sweeper) record(ctx context.Context, sweepID int64, path string, fp fingerprint,
	result store.HealthResult, reason string) error {
	if err := s.Ledger.RecordHealthCheck(ctx, store.HealthCheck{
		SweepID: sweepID, Path: path, Size: fp.size, MtimeNS: fp.mtimeNS,
		CheckedAt: s.Now(), Result: result, Reason: reason,
	}); err != nil {
		return err
	}
	if result != store.HealthOK {
		s.Log.Warn("health sweep: a source did not pass a full decode; it is reported and nothing is done to it",
			"file", path, "result", string(result), "reason", reason)
	}
	for _, r := range s.Reporters {
		r.HealthFileChecked(result)
	}
	return nil
}
