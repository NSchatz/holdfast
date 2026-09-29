package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// `holdfast run`'s progress narration (S0173): while a pass runs, one line on stderr for each
// encode in flight, every runProgressEvery, saying how far through its source that encode has
// got, how fast it is going now and how long it has left at that speed.
//
// THE FIGURES ARE FFMPEG'S OWN. Attaching the reporter as the engine's Observer is what turns
// on the progress collection the daemon already relies on - engine.encode passes
// `-progress pipe:3` only when an Observer is set - so a position here is one the encoder
// reported on that channel. Nothing probes the working file to produce one: the length of a
// partial output is not the encoder's position, and reading it would cost a subprocess a line.
//
// SPEED IS THE RECENT RATE, not ffmpeg's cumulative `speed=` average, which decays slowly after
// a stall. Telling a stalled encode from a slow one is what this exists for, because stopping
// the wrong job mid-flight is how a working file is left beside a source: a line whose position
// has not moved since the previous one says `speed=0.00x`, and a slow encode never prints it.
//
// THE LINES ARE NARRATION, NOT LOG RECORDS. They go to the stderr writer `run` was handed,
// whatever log_level says, exactly as `plan`'s do: the operator asked for them, and a statement
// filtered away at a legal level has not been made.
//
// NOTHING HERE MAY SLOW AN ENCODE. The Observer runs inline on a worker and on the goroutine
// draining ffmpeg's progress pipe, and a drain that stops wedges ffmpeg. So the Observer only
// folds an event into a table under a lock nothing holds across I/O; every line is formatted
// and written on the reporter's own goroutine, which a failing or blocked stderr can hold up
// without that reaching a worker or the pipe.

// runProgressEvery is how often `run` narrates each encode in flight on stderr. A test may
// shorten it as it shortens planProgressEvery, or stand in a clock and keep the shipped one.
var runProgressEvery = 30 * time.Second

// progressClock is the time the reporter reads and the ticks it narrates on. It is an
// interface so a test can stand in a clock it advances by hand, which grades the thirty-second
// cadence and a speed measured over it without waiting thirty seconds for either.
type progressClock interface {
	Now() time.Time
	Ticker(every time.Duration) (ticks <-chan time.Time, stop func())
}

// systemClock is the clock production reads.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) Ticker(every time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(every)
	return t.C, t.Stop
}

// runProgressClock is the clock `run`'s reporter reads. Production never replaces it.
var runProgressClock progressClock = systemClock{}

// runProgress is one run's reporter: the encodes in flight, and what each has reported.
type runProgress struct {
	clock progressClock
	w     io.Writer

	mu       sync.Mutex
	inFlight map[string]*encodeProgress // by source path
}

// encodeProgress is what one encode in flight has reported, and where the span the next line
// measures its speed over began.
type encodeProgress struct {
	// position is the last source position the encoder reported, in seconds. reported says
	// whether there has been one at all, because a position never reported is UNKNOWN, and a
	// zero standing in for it would read as an encode that has not moved.
	position float64
	reported bool
	// duration is the source length the engine measured that position against, nil where the
	// probe established none.
	duration *float64
	// spanStart and spanFrom are the wall time and source position the next line's speed is
	// measured from: the previous line that carried a position, or else the encode's entry into
	// `encoding`, at the start of its source.
	spanStart time.Time
	spanFrom  float64
}

// startRunProgress attaches a reporter to eng and narrates on w every runProgressEvery until ctx
// is done or the function it returns is called. That function waits for the reporter's
// goroutine, so nothing is written once it has returned, and it is idempotent. A zero interval
// turns the reporter off and leaves eng without an Observer, as `plan`'s zero interval does.
func startRunProgress(ctx context.Context, eng *engine.Engine, w io.Writer) (stop func()) {
	if runProgressEvery <= 0 {
		return func() {}
	}
	r := &runProgress{clock: runProgressClock, w: w, inFlight: map[string]*encodeProgress{}}
	eng.Observer = r.observe
	ticks, stopTicks := r.clock.Ticker(runProgressEvery)
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		defer stopTicks()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				// An interrupt cancels every encode in flight, and a cancelled encode leaves
				// `encoding` without a transition the engine reports, so the reporter stops here
				// rather than describe encodes that are no longer running.
				return
			case <-ticks:
				r.report(ctx)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done); <-stopped }) }
}

// observe is the reporter's engine.Observer. It mirrors the daemon's in-flight rule
// (server.Hub.Observe) with a table of its own: a job's transition INTO `encoding` opens a fresh
// entry, a progress report updates the entry its job opened, and any other transition closes it.
//
// The entry is opened fresh every time, so nothing an earlier encode reported - of this file or
// of any other - is carried onto a later one. A report for a path with no open entry is dropped:
// the engine joins the progress drain before the job moves on, so none can arrive late today,
// and one that did must not put a finished file back on stderr.
func (r *runProgress) observe(ev engine.Event) {
	switch {
	case ev.Progress != nil:
		r.mu.Lock()
		if e, ok := r.inFlight[ev.Path]; ok {
			e.position, e.reported = ev.Progress.PositionSec, true
			e.duration = nil
			if d := ev.Progress.DurationSec; d != nil {
				v := *d
				e.duration = &v
			}
		}
		r.mu.Unlock()
	case ev.Status == store.Encoding:
		e := &encodeProgress{spanStart: r.clock.Now()}
		r.mu.Lock()
		r.inFlight[ev.Path] = e
		r.mu.Unlock()
	default:
		r.mu.Lock()
		delete(r.inFlight, ev.Path)
		r.mu.Unlock()
	}
}

// report writes this tick's lines. They are composed under the lock and written after it is
// released, so a stderr that fails or blocks holds up this goroutine and nothing else; a failed
// write is dropped, because a narration line that could not be written is not a failed run.
func (r *runProgress) report(ctx context.Context) {
	for _, line := range r.lines(r.clock.Now()) {
		if ctx.Err() != nil {
			return
		}
		_, _ = fmt.Fprintln(r.w, line)
	}
}

// lines composes one line per encode in flight at now, in path order, and moves each encode's
// measuring span on to now.
func (r *runProgress) lines(now time.Time) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	paths := make([]string, 0, len(r.inFlight))
	for p := range r.inFlight {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		position, speed, eta := r.inFlight[p].figures(now)
		out = append(out, fmt.Sprintf("holdfast run: still encoding file=%s position=%s speed=%s eta=%s",
			progressPath(p), position, speed, eta))
	}
	return out
}

// unknownFigure is what a line says for a figure it cannot measure. It is never a zero, a NaN
// or an infinity standing in for one.
const unknownFigure = "unknown"

// figures is what one line says about this encode at now: its position, its speed over the
// span since the previous line that carried a position (or since it entered `encoding`), and
// the time left at that speed.
func (e *encodeProgress) figures(now time.Time) (position, speed, eta string) {
	if !e.reported {
		// Nothing reported yet. The span stays where it began, so the first line that does
		// carry a position measures its speed from the start of the encode.
		return unknownFigure, unknownFigure, unknownFigure
	}
	position = unknownFigure
	if at, ok := clockText(e.position); ok {
		position = at
	}
	wall := now.Sub(e.spanStart).Seconds()
	advanced := e.position - e.spanFrom
	e.spanStart, e.spanFrom = now, e.position
	if !(wall > 0) || advanced < 0 {
		// No wall time to divide by, or a position that went BACKWARDS, which no encoder moving
		// through its source reports: no rate can be stated honestly over this span.
		return position, unknownFigure, unknownFigure
	}
	rate := advanced / wall
	return position, speedText(rate), etaText(rate, e.duration, e.position)
}

// etaText is the time left at rate: (duration - position) / rate. It is unknown for a stalled
// encode, whose rate is zero, for a source whose duration the probe could not establish or
// reported as zero or less, and for a position already past the reported duration, where that
// duration no longer bounds what is left.
func etaText(rate float64, duration *float64, position float64) string {
	if !(rate > 0) || duration == nil || !(*duration > 0) {
		return unknownFigure
	}
	left := *duration - position
	if left < 0 {
		return unknownFigure
	}
	if t, ok := clockText(left / rate); ok {
		return t
	}
	return unknownFigure
}

// speedText renders a rate with two decimal places, and with as many more as a rate above zero
// needs to read as above zero: `0.00x` is the stalled encode's figure, and a slow encode that
// printed it would be the confusion this reporter exists to prevent.
func speedText(rate float64) string {
	places := 2
	if rate > 0 && rate < 0.01 {
		places = int(-math.Floor(math.Log10(rate))) + 1
	}
	return strconv.FormatFloat(rate, 'f', places, 64) + "x"
}

// maxClockSeconds bounds what clockText converts to whole seconds: far beyond any source or ETA
// a person reads, and far inside what an int64 holds.
const maxClockSeconds = 1 << 50

// clockText renders seconds as HH:MM:SS, truncated to the whole second, with the hours
// uncapped. ok is false for anything that is not a finite, non-negative number of seconds.
func clockText(sec float64) (string, bool) {
	if !(sec >= 0) || sec > maxClockSeconds {
		return "", false
	}
	s := int64(sec)
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, s/60%60, s%60), true
}

// progressPath renders a source path the way the run's log records render a value: bare where
// it is one unbroken token, quoted otherwise, so a name holding a space, a quote, an equals sign
// or a line break can neither split its line nor forge the next one.
func progressPath(p string) string {
	if p == "" || strings.IndexFunc(p, func(r rune) bool {
		return r == ' ' || r == '"' || r == '=' || r == utf8.RuneError || !unicode.IsPrint(r)
	}) >= 0 {
		return strconv.Quote(p)
	}
	return p
}
