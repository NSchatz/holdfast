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
// got. It is live state about a process, never a ledger fact — nothing here is persisted,
// and after a restart an in-flight job simply has no progress reported yet.
//
// PositionSec is the encoder's position in the SOURCE timeline, in seconds.
// DurationSec is the length that position is measured against, or nil when the container
// reports no duration. nil is UNKNOWN and must be rendered as such: a zero here would
// read as a zero-length source, i.e. an encode that is complete the instant it starts.
type Progress struct {
	PositionSec float64
	DurationSec *float64
}

// ProgressSink receives live Progress reports from a running encode.
//
// It MUST be non-blocking, for the same reason engine.Observer must be and one more
// besides: it is called from the goroutine draining the encoder's progress pipe, so a
// sink that blocked would stop that drain, fill the pipe, and block ffmpeg itself on a
// write. Losing an update is losing granularity; blocking here would be losing the
// encode's throughput. A dropped update is always the right trade.
type ProgressSink func(Progress)

// ProgressEncoder is an Encoder that can additionally report how far through the source
// it has got while it runs. It is an OPTIONAL capability, discovered with a type
// assertion (the http.Flusher idiom) rather than folded into Encoder: an Encoder that
// cannot report progress — every test fake, and any future non-ffmpeg encoder — is
// simply run through Encode and reports nothing, which is the documented "no progress
// reported" state, not a failure.
//
// The contract is otherwise EXACTLY Encoder's: the returned error, and therefore the
// subprocess exit status and captured error text behind it, must be what Encode would
// have returned for the same call. Progress collection is additive or it is a defect.
type ProgressEncoder interface {
	EncodeWithProgress(ctx context.Context, in, out string, props *probe.VideoProps, sink ProgressSink) error
}

// maxProgressLine bounds how much of one line this parser will hold in memory. A
// documented progress line is tens of bytes, so it only ever bites on output no encoder
// should be producing — and when it does, the excess is READ AND DISCARDED rather than
// left sitting in the pipe. Bounding memory must never be paid for with a stalled
// encoder; see scanProgressStream.
const maxProgressLine = 64 << 10

// scanProgressStream parses ffmpeg's -progress output and calls emit once per completed
// report. It never returns an error: an unreadable, truncated, malformed or entirely
// absent stream is "no progress reported", which the reporting surface shows as unknown.
//
// IT READS TO EOF UNCONDITIONALLY, AND THAT IS A SAFETY PROPERTY RATHER THAN TIDINESS.
// The write end of this pipe is held by the RUNNING encoder, and the parent's read end
// stays open until after cmd.Wait() returns — so a reader that stops early does not
// merely lose figures. The kernel pipe buffer fills, the encoder blocks on its next
// write, it never exits, cmd.Wait() never returns, and the worker is wedged with no
// timeout to free it. A reporting path may lose granularity and may NEVER cost an encode
// (AC6), so every exit from the loop below still drains, and the deferred copy makes that
// true of any exit added here later.
//
// That is why this reads through bufio.Reader.ReadSlice rather than a bufio.Scanner: a
// Scanner hands back bufio.ErrTooLong for a token past its buffer and STOPS, which is
// precisely the stall described above. ReadSlice reports a full buffer and lets the
// caller carry on, so an over-long line is discarded — it cannot be a progress report,
// whose key and value are both short — while the drain continues.
//
// THE FORMAT, as ffmpeg documents it (work/specs sources/ffmpeg.org-ffmpeg.html,
// `-progress url`): "Progress information is written periodically and at the end of the
// encoding process. It is made of "key=value" lines. key consists of only alphanumeric
// characters. The last key of a sequence of progress information is always "progress"
// with the value "continue" or "end"." The period is `-stats_period`, default 0.5s.
//
// THE KEYS ARE NOT DOCUMENTED, so they were MEASURED, not assumed: one short real encode
// was run with -progress against the ffmpeg this repository PINS and gates on
// (N-125875-g5d4d3bdc61, the BtbN build scripts/install-ffmpeg.sh installs), and again
// against a second, unrelated build (8.0.1) to check the key set is not one build's
// quirk. Both emit exactly this, verbatim, once per report:
//
//	frame=27
//	fps=0.00
//	stream_0_0_q=26.1
//	bitrate=   9.4kbits/s
//	total_size=2942
//	out_time_us=2500000
//	out_time_ms=2500000
//	out_time=00:00:02.500000
//	dup_frames=0
//	drop_frames=0
//	speed=   5x
//	progress=continue
//
// Two things in that sample decide this parser:
//
//  1. `out_time_us` is MICROSECONDS (2500000 at the 2.5s mark), and it is the key read
//     here. `out_time` (HH:MM:SS.ffffff) is read only as a fallback, for a build that
//     stops emitting `out_time_us`.
//  2. `out_time_ms` is MIS-NAMED: the sample above shows it carrying 2500000 at 2.5s —
//     the SAME microseconds as out_time_us, not milliseconds. Reading it as its name
//     suggests would put every encode at 0.1% of a two-hour film for its whole run. It
//     is deliberately IGNORED, and that is the single most load-bearing line in this
//     file: a unit misread here is a wrong number on the operator's page, which is this
//     repository's cardinal sin.
//
// A report is emitted only at its `progress=` terminator, which is precisely what the
// documentation guarantees to be last — so a half-written final block (a killed encoder)
// is never published as a real position, and a report whose keys are all unrecognised
// publishes nothing at all.
func scanProgressStream(r io.Reader, emit func(positionSec float64)) {
	// The obligation to the process on the other end of this pipe outlives the parse:
	// whatever the loop below does or stops doing, the stream is consumed to EOF, so the
	// encoder can always finish its write and exit. Cheap on the ordinary path (the loop
	// already ended at EOF, so this copies nothing) and the difference between a lost
	// figure and a wedged worker on every other path.
	defer func() { _, _ = io.Copy(io.Discard, r) }()

	br := bufio.NewReaderSize(r, maxProgressLine)
	haveTime := false
	discarding := false
	var positionSec float64
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// A line longer than the buffer. It cannot be a progress report, so throw the
			// fragment away and KEEP READING — the one thing that must not happen here is
			// that the reader stops while the encoder still holds the write end.
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
			// Microseconds (measured, see above). ParseInt, so a NaN/Inf can never
			// reach the wire and blow up the snapshot's JSON encoding.
			if us, perr := strconv.ParseInt(strings.TrimSpace(value), 10, 64); perr == nil && us >= 0 {
				positionSec, haveTime = float64(us)/1e6, true
			}
		case key == "out_time":
			// HH:MM:SS.ffffff — the fallback, used only when out_time_us was absent
			// from this report.
			if !haveTime {
				if sec, ok := parseClock(strings.TrimSpace(value)); ok {
					positionSec, haveTime = sec, true
				}
			}
		case key == "progress":
			// The documented terminator: "continue" or "end". Publish whatever this
			// report established, then start a fresh one.
			if haveTime {
				emit(positionSec)
			}
			haveTime, positionSec = false, 0
		}
		if err != nil {
			// io.EOF (the encoder closed its end) or an unrecoverable read error. Either
			// way there is nothing left to parse, and the deferred drain costs nothing.
			return
		}
	}
}

// parseClock parses ffmpeg's HH:MM:SS.ffffff progress timestamp into seconds. It refuses
// anything else — including ffmpeg's "N/A" and its negative pre-roll timestamps — so an
// unparseable value is "no position reported" rather than a guess.
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
