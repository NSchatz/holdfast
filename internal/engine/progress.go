package engine

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/NSchatz/holdfast/internal/probe"
)

// Progress is one live report from a RUNNING encoder: how far through the source it has
// got. It is live state about a process, never a ledger fact, so nothing here is persisted.
//
// PositionSec is the position in the SOURCE timeline, in seconds. DurationSec is the
// length it is measured against, or nil when the container reports none. nil is UNKNOWN
// and must be rendered as such: a zero would read as an encode complete the instant it
// starts.
type Progress struct {
	PositionSec float64
	DurationSec *float64
}

// ProgressSink receives live Progress reports from a running encode. It MUST be
// non-blocking: it is called from the goroutine draining the encoder's progress pipe, so a
// sink that blocked would stop that drain, fill the pipe and block ffmpeg itself on a
// write. A dropped update costs granularity; blocking costs the encode's throughput.
type ProgressSink func(Progress)

// ProgressEncoder is an Encoder that can also report how far through the source it has
// got. The capability is OPTIONAL and discovered with a type assertion (the http.Flusher
// idiom): an Encoder that cannot report progress is run through Encode and reports
// nothing, which is the documented "no progress reported" state, not a failure.
//
// The contract is otherwise EXACTLY Encoder's: the returned error, and the subprocess exit
// status and captured error text behind it, must be what Encode would have returned for
// the same call. Progress collection is additive or it is a defect.
type ProgressEncoder interface {
	EncodeWithProgress(ctx context.Context, in, out string, props *probe.VideoProps, sink ProgressSink) error
}

// maxProgressLine bounds how much of one line this parser holds in memory. A progress line
// is tens of bytes, so it bites only on output no encoder should produce, and the excess is
// READ AND DISCARDED rather than left in the pipe: bounding memory must never be paid for
// with a stalled encoder.
const maxProgressLine = 64 << 10

// scanProgressStream parses ffmpeg's -progress output and calls emit once per completed
// report. It never returns an error: an unreadable, truncated, malformed or entirely
// absent stream is "no progress reported", which the reporting surface shows as unknown.
//
// IT READS TO EOF UNCONDITIONALLY, AND THAT IS A SAFETY PROPERTY RATHER THAN TIDINESS.
// The write end of this pipe is held by the RUNNING encoder and the parent's read end
// stays open until after cmd.Wait() returns, so a reader that stops early fills the kernel
// pipe buffer, blocks the encoder on its next write, and wedges the worker with no timeout
// to free it. A reporting path may lose granularity and may NEVER cost an encode, so every
// exit from the loop below still drains and the deferred copy makes that true of any exit
// added later. It is also why this uses bufio.Reader.ReadSlice rather than a bufio.Scanner:
// a Scanner hands back bufio.ErrTooLong for an over-long token and STOPS, which is that
// exact stall, while ReadSlice reports a full buffer and lets the caller carry on.
//
// THE FORMAT, as ffmpeg documents it for `-progress url`: "key=value" lines written
// periodically (`-stats_period`, default 0.5s), and "the last key of a sequence of progress
// information is always progress with the value continue or end".
//
// THE KEYS ARE NOT DOCUMENTED, so they were MEASURED rather than assumed: a short real
// encode against the ffmpeg this repository pins and gates on, and again against a second
// unrelated build to check the key set is not one build's quirk. Both emit, once per report
// and among others:
//
//	out_time_us=2500000
//	out_time_ms=2500000
//	out_time=00:00:02.500000
//	progress=continue
//
// Two things there decide this parser. `out_time_us` is MICROSECONDS (2500000 at the 2.5s
// mark) and is the key read here, with `out_time` (HH:MM:SS.ffffff) as a fallback for a
// build that stops emitting it. `out_time_ms` is MIS-NAMED: the same microseconds, not
// milliseconds, so reading it as its name suggests would put every encode at 0.1% of a
// two-hour film for its whole run. It is deliberately IGNORED, and that is the most
// load-bearing line here: a unit misread is a wrong number on the operator's page.
//
// A report is emitted only at its `progress=` terminator, which the documentation
// guarantees to be last, so a half-written final block from a killed encoder is never
// published as a real position.
func scanProgressStream(r io.Reader, emit func(positionSec float64)) {
	// Whatever the loop below does or stops doing, the stream is consumed to EOF so the
	// encoder can always finish its write and exit.
	defer func() { _, _ = io.Copy(io.Discard, r) }()

	br := bufio.NewReaderSize(r, maxProgressLine)
	haveTime := false
	discarding := false
	var positionSec float64
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// Longer than the buffer, so not a progress report: drop the fragment and KEEP
			// READING. The reader must never stop while the encoder holds the write end.
			discarding = true
			continue
		}
		if discarding {
			// The tail of a line already abandoned above; drop it with the rest of it.
			discarding, line = false, nil
		}
		key, value, ok := strings.Cut(strings.TrimSpace(string(line)), "=")
		switch {
		case !ok:
			// Not a key=value line at all; nothing to fold in.
		case key == "out_time_us":
			// ParseInt, so a NaN or Inf can never reach the wire and blow up the
			// snapshot's JSON encoding.
			if us, perr := strconv.ParseInt(strings.TrimSpace(value), 10, 64); perr == nil && us >= 0 {
				positionSec, haveTime = float64(us)/1e6, true
			}
		case key == "out_time":
			// The fallback, used only when out_time_us was absent from this report.
			if !haveTime {
				if sec, ok := parseClock(strings.TrimSpace(value)); ok {
					positionSec, haveTime = sec, true
				}
			}
		case key == "progress":
			// The documented terminator. Publish what this report established, then
			// start a fresh one.
			if haveTime {
				emit(positionSec)
			}
			haveTime, positionSec = false, 0
		}
		if err != nil {
			// EOF or an unrecoverable read error: nothing left to parse either way.
			return
		}
	}
}

// parseClock parses ffmpeg's HH:MM:SS.ffffff progress timestamp into seconds. It refuses
// anything else, including "N/A" and negative pre-roll timestamps, so an unparseable value
// is "no position reported" rather than a guess.
func parseClock(v string) (float64, bool) {
	parts := strings.Split(v, ":")
	if len(parts) != 3 {
		return 0, false
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 {
		return 0, false
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, false
	}
	s, err := strconv.ParseFloat(parts[2], 64)
	if err != nil || s < 0 || s >= 60 {
		return 0, false
	}
	return float64(h)*3600 + float64(m)*60 + s, true
}
