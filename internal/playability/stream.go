package playability

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Rational is a time base: one tick is Num/Den seconds.
type Rational struct{ Num, Den int64 }

func (r Rational) valid() bool { return r.Num > 0 && r.Den > 0 }

// Packet is one packet of a stream, in decode order.
type Packet struct {
	// PTS is the presentation time in the stream's time base, meaningful only where HasPTS.
	PTS    int64
	HasPTS bool
	// Key is the container's keyframe flag: a picture a decoder may start at.
	Key bool
}

// Stream is a file's first video stream as the checks read it.
type Stream struct {
	TimeBase Rational
	// Packets are every packet of the stream, in decode order.
	Packets []Packet
}

// ReadStream lists file's first video stream (not an attached picture) packet by packet:
// each one's presentation time and keyframe flag, and the stream's time base. It reads the
// whole file and decodes nothing.
func (c Checker) ReadStream(ctx context.Context, file string) (*Stream, error) {
	cmd, out, errs := command(ctx, c.FFprobe, "-v", "error", "-select_streams", "V:0",
		"-show_entries", "stream=time_base:packet=pts,flags", "-of", "compact=p=1:nk=0", "file:"+file)
	// A clean listing is the answer whatever ffprobe printed beside it: it decodes nothing here,
	// so only a failed exit's error line is a verdict about the file.
	runErr := cmd.Run()
	stderr := ""
	if runErr != nil {
		stderr = errs.String()
	}
	reason, unrun := verdict(ctx, "the packet listing", runErr, stderr)
	if unrun != nil {
		return nil, unrun
	}
	if reason != "" {
		return nil, &Failure{Check: "stream", Reason: "ffprobe could not list the video stream's packets: " + reason}
	}
	s, err := parseStream(out.String())
	if err != nil {
		return nil, &Unrun{What: "the packet listing", Err: err}
	}
	if len(s.Packets) == 0 {
		return nil, &Failure{Check: "stream", Reason: "the file carries no video packet a player could start at"}
	}
	return s, nil
}

// parseStream reads ffprobe's compact listing: one "stream|..." line and a "packet|..." line
// per packet, each a list of key=value fields. Fields other than the ones asked for (side
// data ffprobe prints beside them) are ignored; a field asked for that does not read is an
// error, never a default.
func parseStream(out string) (*Stream, error) {
	s := &Stream{}
	streams := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		section, rest, _ := strings.Cut(line, "|")
		fields := map[string]string{}
		for _, f := range strings.Split(rest, "|") {
			if k, v, ok := strings.Cut(f, "="); ok {
				fields[k] = v
			}
		}
		switch section {
		case "packet":
			p, err := parsePacket(fields)
			if err != nil {
				return nil, fmt.Errorf("packet %d: %w", len(s.Packets)+1, err)
			}
			s.Packets = append(s.Packets, p)
		case "stream":
			streams++
			tb, ok := parseRational(fields["time_base"])
			if !ok {
				return nil, fmt.Errorf("time base %q does not read", fields["time_base"])
			}
			s.TimeBase = tb
		}
	}
	if streams != 1 {
		return nil, fmt.Errorf("%d video streams listed, want 1", streams)
	}
	return s, nil
}

func parsePacket(f map[string]string) (Packet, error) {
	flags, ok := f["flags"]
	if !ok || flags == "" {
		return Packet{}, fmt.Errorf("no flags")
	}
	p := Packet{Key: flags[0] == 'K'}
	switch v := f["pts"]; v {
	case "N/A":
	default:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Packet{}, fmt.Errorf("pts %q does not read", v)
		}
		p.PTS, p.HasPTS = n, true
	}
	return p, nil
}

func parseRational(v string) (Rational, bool) {
	a, b, ok := strings.Cut(v, "/")
	if !ok {
		return Rational{}, false
	}
	num, err1 := strconv.ParseInt(a, 10, 64)
	den, err2 := strconv.ParseInt(b, 10, 64)
	if r := (Rational{Num: num, Den: den}); err1 == nil && err2 == nil && r.valid() {
		return r, true
	}
	return Rational{}, false
}

// Keyframes are the decode-order indices of the packets a cold start can be aimed at: every
// one the container flags as a keyframe. A cold start is aimed by packet order, never by time,
// so a keyframe whose container records no presentation time (AVI, ASF) is one too.
func (s *Stream) Keyframes() []int {
	var keys []int
	for i, p := range s.Packets {
		if p.Key {
			keys = append(keys, i)
		}
	}
	return keys
}

// Leads reports whether the keyframe at decode index k is followed, before the next keyframe
// in decode order, by a picture presented before it: a leading picture. In HEVC that is a
// RASL or RADL picture of a CRA or an IDR; in an open-GOP H.264 stream, a B picture after a
// recovery-point I picture. Decoded linearly it is decoded with every reference it has; a
// decoder starting at the keyframe drops it (RASL) or decodes it (RADL) without the pictures
// before the keyframe.
//
// A keyframe whose container records no presentation time cannot be told to lead anything, and
// reads as leading nothing: it is still a keyframe, and the spread sample still reaches it.
func (s *Stream) Leads(k int) bool {
	if !s.Packets[k].HasPTS {
		return false
	}
	at := s.Packets[k].PTS
	for i := k + 1; i < len(s.Packets) && !s.Packets[i].Key; i++ {
		if p := s.Packets[i]; p.HasPTS && p.PTS < at {
			return true
		}
	}
	return false
}

// ColdStartTargets are the keyframes n cold starts are aimed at, in decode order: n spread
// across every keyframe, and up to LeadingPerColdStart*n spread across the keyframes followed
// by leading pictures. Every keyframe is a target where there are no more than the sample.
func (s *Stream) ColdStartTargets(n int) []int {
	keys := s.Keyframes()
	var leading []int
	for _, k := range keys {
		if s.Leads(k) {
			leading = append(leading, k)
		}
	}
	seen := map[int]bool{}
	var out []int
	for _, k := range append(spread(keys, n), spread(leading, LeadingPerColdStart*n)...) {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Ints(out)
	return out
}

// spread picks n of xs evenly, the middle of each of n equal strata, so the sample reaches
// across the whole list and does not lean on its first or last member; all of xs where it
// has no more than n.
func spread(xs []int, n int) []int {
	if n <= 0 {
		return nil
	}
	if len(xs) <= n {
		return append([]int(nil), xs...)
	}
	out := make([]int, n)
	for i := range out {
		out[i] = xs[(2*i+1)*len(xs)/(2*n)]
	}
	return out
}

// keyframeAt names the keyframe at decode index k for a message: by its presentation time, or
// by its place in decode order where the container records none.
func (s *Stream) keyframeAt(k int) string {
	if p := s.Packets[k]; p.HasPTS {
		return "the keyframe at " + seconds(s.seconds(p.PTS))
	}
	return fmt.Sprintf("the keyframe that is picture %d in decode order", k+1)
}

// seconds is a time in the stream's time base in seconds, for messages.
func (s *Stream) seconds(pts int64) float64 {
	return float64(pts) * float64(s.TimeBase.Num) / float64(s.TimeBase.Den)
}
