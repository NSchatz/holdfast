package probe

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// Timestamps is the presentation timing of a file's first video stream (`v:0`, the stream
// every other probe and gate reads): one timestamp per packet, in file order, in ticks of the
// stream's own time base. It is read by demuxing alone, never by decoding.
type Timestamps struct {
	// TimeBaseNum and TimeBaseDen are the stream's time base: one tick is
	// TimeBaseNum/TimeBaseDen seconds. Both are positive.
	TimeBaseNum, TimeBaseDen int64
	// Packets is how many packets the stream carries.
	Packets int
	// Ticks holds each packet's presentation timestamp, in the order the packets are stored:
	// Packets of them, or none where the stream is Untimed.
	Ticks []int64
	// FromDTS is true where no packet of the stream carries a presentation timestamp at all and
	// Ticks are its decode timestamps instead. That is how an AVI is stored: one frame per
	// chunk, timed by its position, and the demuxer reports that time as the dts alone. In
	// such a container the decode timestamps are the slots the frames are presented in, so the
	// set of them is the set of presentation times.
	FromDTS bool
	// Untimed is true where no packet carries a timestamp of either kind: a raw elementary
	// stream (`.h264`, `.hevc`), which stores frames and no times, and which ffmpeg times at
	// the one rate it guesses when it reads it. Ticks is empty.
	Untimed bool
}

// ErrTimestampsUnanswered is what VideoTimestamps wraps when ffprobe produced no listing at
// all: it could not be started, was killed or cancelled, exited non-zero, or its output could
// not be read. It is evidence about the run, not about the file. Every other error
// VideoTimestamps returns is ffprobe's own answer about the file, read and found unusable.
var ErrTimestampsUnanswered = errors.New("ffprobe did not answer")

// VideoTimestamps lists every packet of f's first video stream with its presentation and
// decode timestamps, and the stream's time base, in ONE ffprobe that demuxes the file and
// decodes nothing. The listing is read as it streams, so a film's hundreds of thousands of
// packets are never buffered as text.
//
// It refuses rather than guesses: a listing with no packet, no time base, a line this reader
// does not know, or packets of which only some carry a presentation timestamp is an error,
// never a partial answer.
func (p *Prober) VideoTimestamps(ctx context.Context, f string) (Timestamps, error) {
	cmd := exec.CommandContext(ctx, p.FFprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "packet=pts,dts:stream=time_base", "-of", "compact=p=1:nk=0", "--", f)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return Timestamps{}, fmt.Errorf("%w: %v", ErrTimestampsUnanswered, err)
	}
	if err := cmd.Start(); err != nil {
		return Timestamps{}, fmt.Errorf("%w: %v", ErrTimestampsUnanswered, err)
	}
	ts, parseErr := parseTimestamps(out)
	// The rest of the listing is drained before the wait, so a refusal part-way through
	// never leaves ffprobe blocked on a full pipe.
	_, _ = io.Copy(io.Discard, out)
	waitErr := cmd.Wait()
	if ctx.Err() != nil || waitErr != nil {
		return Timestamps{}, fmt.Errorf("%w: %v", ErrTimestampsUnanswered, errors.Join(ctx.Err(), waitErr))
	}
	return ts, parseErr
}

// parseTimestamps reads VideoTimestamps' listing: one `packet|pts=<n>|dts=<n>` line per
// packet (either value may be N/A) and one `stream|time_base=<num>/<den>` line. A transport
// stream also lists its programs' streams (`program|stream|...`), which are skipped; the
// stream line itself may carry trailing empty subsections, which are ignored.
func parseTimestamps(r io.Reader) (Timestamps, error) {
	var (
		ts           Timestamps
		pts, dts     []int64
		withPTS, all int
		tbSeen       bool
	)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		fields := strings.Split(line, "|")
		switch {
		case line == "" || fields[0] == "program":
			continue
		case fields[0] == "packet":
			p, okP, err := tickField(fields, "pts")
			if err != nil {
				return Timestamps{}, err
			}
			d, okD, err := tickField(fields, "dts")
			if err != nil {
				return Timestamps{}, err
			}
			all++
			if okP {
				withPTS++
				pts = append(pts, p)
			}
			if okD {
				dts = append(dts, d)
			}
		case fields[0] == "stream":
			if tbSeen {
				return Timestamps{}, fmt.Errorf("the listing names more than one video stream")
			}
			num, den, err := timeBaseField(fields)
			if err != nil {
				return Timestamps{}, err
			}
			ts.TimeBaseNum, ts.TimeBaseDen, tbSeen = num, den, true
		default:
			return Timestamps{}, fmt.Errorf("the listing carries a line this reader does not know: %q", line)
		}
	}
	if err := sc.Err(); err != nil {
		return Timestamps{}, fmt.Errorf("%w: %v", ErrTimestampsUnanswered, err)
	}
	switch {
	case !tbSeen:
		return Timestamps{}, fmt.Errorf("the listing names no video stream time base")
	case all == 0:
		return Timestamps{}, fmt.Errorf("the video stream carries no packets")
	case withPTS == all:
		ts.Ticks = pts
	case withPTS > 0:
		return Timestamps{}, fmt.Errorf("%d of the video stream's %d packets carry no presentation timestamp",
			all-withPTS, all)
	case len(dts) == all:
		ts.Ticks, ts.FromDTS = dts, true
	case len(dts) == 0:
		ts.Untimed = true
	default:
		return Timestamps{}, fmt.Errorf("%d of the video stream's %d packets carry neither a presentation "+
			"nor a decode timestamp", all-len(dts), all)
	}
	ts.Packets = all
	return ts, nil
}

// tickField reads the `<key>=<value>` field of a packet line: ok is false for ffprobe's N/A,
// and a field that is missing or not an integer is an error.
func tickField(fields []string, key string) (v int64, ok bool, err error) {
	for _, f := range fields[1:] {
		val, found := strings.CutPrefix(f, key+"=")
		if !found {
			continue
		}
		if val == "N/A" {
			return 0, false, nil
		}
		n, perr := strconv.ParseInt(val, 10, 64)
		if perr != nil {
			return 0, false, fmt.Errorf("a packet's %s is not a timestamp: %q", key, val)
		}
		return n, true, nil
	}
	return 0, false, fmt.Errorf("a packet line carries no %s: %q", key, strings.Join(fields, "|"))
}

// timeBaseField reads the `time_base=<num>/<den>` field of a stream line; both parts must be
// positive integers.
func timeBaseField(fields []string) (num, den int64, err error) {
	for _, f := range fields[1:] {
		val, found := strings.CutPrefix(f, "time_base=")
		if !found {
			continue
		}
		n, d, cut := strings.Cut(val, "/")
		num, errN := strconv.ParseInt(n, 10, 64)
		den, errD := strconv.ParseInt(d, 10, 64)
		if !cut || errN != nil || errD != nil || num <= 0 || den <= 0 {
			return 0, 0, fmt.Errorf("the video stream's time base is not a ratio of positive integers: %q", val)
		}
		return num, den, nil
	}
	return 0, 0, fmt.Errorf("the stream line carries no time base: %q", strings.Join(fields, "|"))
}
