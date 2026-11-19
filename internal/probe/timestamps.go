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

// Timestamps is the packet listing of a file's first moving-picture video stream (`V:0`: the
// first video stream that is not an attached picture, the one the playability gate lists too, so
// a cover picture stored first is never the stream timed): every packet in file order with its
// timestamps and flags, in ticks of the stream's own time base. It is read by demuxing alone,
// never by decoding, and it is what the file stores: what a decoder shows of it is the timing
// gate's question (timing.Check).
type Timestamps struct {
	// TimeBaseNum and TimeBaseDen are the stream's time base: one tick is
	// TimeBaseNum/TimeBaseDen seconds. Both are positive.
	TimeBaseNum, TimeBaseDen int64
	// Packets are the stream's packets in the order the file stores them.
	Packets []Packet
}

// Packet is one packet of the listing.
type Packet struct {
	// PTS and DTS are its presentation and decode timestamps, where HasPTS and HasDTS say the
	// file carries them (ffprobe prints N/A for one it does not). AVI and ASF store no
	// presentation timestamp, a raw elementary stream stores neither, and a Matroska block
	// timed before zero is read back with none.
	PTS, DTS       int64
	HasPTS, HasDTS bool
	// Key is the keyframe flag, and Discard the flag a demuxer sets on a packet the decoder
	// decodes and never shows (an MP4 edit list starting after the keyframe a cut began at).
	Key, Discard bool
}

// ErrTimestampsUnanswered is what VideoTimestamps wraps when ffprobe produced no listing at
// all: it could not be started, was killed or cancelled, exited non-zero, or its output could
// not be read. It is evidence about the run, not about the file. Every other error
// VideoTimestamps returns is ffprobe's own answer about the file, read and found unusable.
var ErrTimestampsUnanswered = errors.New("ffprobe did not answer")

// VideoTimestamps lists every packet of f's first moving-picture video stream with its
// presentation and decode timestamps and its flags, and the stream's time base, in ONE ffprobe
// that demuxes the file and decodes nothing. The listing is read as it streams, so a film's
// hundreds of thousands of packets are never buffered as text.
//
// It refuses rather than guesses: a listing with no packet, no time base, a line this reader
// does not know or a value that does not read is an error, never a partial answer. A packet
// that carries no timestamp is not an error: N/A is what the file stores, and the gate decides
// what it means.
func (p *Prober) VideoTimestamps(ctx context.Context, f string) (Timestamps, error) {
	cmd := exec.CommandContext(ctx, p.FFprobe, "-v", "error", "-select_streams", "V:0",
		"-show_entries", "packet=pts,dts,flags:stream=time_base", "-of", "compact=p=1:nk=0", "--", f)
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

// parseTimestamps reads VideoTimestamps' listing: one `packet|pts=<n>|dts=<n>|flags=<f>` line
// per packet (either timestamp may be N/A; the flags are K for a keyframe and D for a packet to
// discard, `_` where unset) and one `stream|time_base=<num>/<den>` line. A transport stream also
// lists its programs' streams (`program|stream|...`) and an MP4 or MOV whose video is linked to a
// timecode track its stream group's (`stream_group|stream|...`), which are skipped; a line may
// carry trailing empty subsections, which are ignored.
func parseTimestamps(r io.Reader) (Timestamps, error) {
	var (
		ts     Timestamps
		tbSeen bool
	)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		fields := strings.Split(line, "|")
		switch {
		case line == "" || fields[0] == "program" || fields[0] == "stream_group":
			continue
		case fields[0] == "packet":
			pkt, err := packetFields(fields)
			if err != nil {
				return Timestamps{}, err
			}
			ts.Packets = append(ts.Packets, pkt)
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
	if !tbSeen {
		return Timestamps{}, fmt.Errorf("the listing names no video stream time base")
	}
	if len(ts.Packets) == 0 {
		return Timestamps{}, fmt.Errorf("the video stream carries no packets")
	}
	return ts, nil
}

// packetFields reads one packet line.
func packetFields(fields []string) (Packet, error) {
	var (
		pkt Packet
		err error
	)
	if pkt.PTS, pkt.HasPTS, err = tickField(fields, "pts"); err != nil {
		return Packet{}, err
	}
	if pkt.DTS, pkt.HasDTS, err = tickField(fields, "dts"); err != nil {
		return Packet{}, err
	}
	flags, ok := field(fields, "flags")
	if !ok {
		return Packet{}, fmt.Errorf("a packet line carries no flags: %q", strings.Join(fields, "|"))
	}
	pkt.Key, pkt.Discard = strings.Contains(flags, "K"), strings.Contains(flags, "D")
	return pkt, nil
}

// field is the value of the `<key>=<value>` field of a line, and whether it carries one.
func field(fields []string, key string) (string, bool) {
	for _, f := range fields[1:] {
		if val, found := strings.CutPrefix(f, key+"="); found {
			return val, true
		}
	}
	return "", false
}

// tickField reads the `<key>=<value>` timestamp field of a packet line: ok is false for
// ffprobe's N/A, and a field that is missing or not an integer is an error.
func tickField(fields []string, key string) (v int64, ok bool, err error) {
	val, found := field(fields, key)
	if !found {
		return 0, false, fmt.Errorf("a packet line carries no %s: %q", key, strings.Join(fields, "|"))
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

// timeBaseField reads the `time_base=<num>/<den>` field of a stream line; both parts must be
// positive integers.
func timeBaseField(fields []string) (num, den int64, err error) {
	val, found := field(fields, "time_base")
	if !found {
		return 0, 0, fmt.Errorf("the stream line carries no time base: %q", strings.Join(fields, "|"))
	}
	n, d, cut := strings.Cut(val, "/")
	num, errN := strconv.ParseInt(n, 10, 64)
	den, errD := strconv.ParseInt(d, 10, 64)
	if !cut || errN != nil || errD != nil || num <= 0 || den <= 0 {
		return 0, 0, fmt.Errorf("the video stream's time base is not a ratio of positive integers: %q", val)
	}
	return num, den, nil
}
