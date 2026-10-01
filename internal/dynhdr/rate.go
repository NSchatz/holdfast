package dynhdr

import (
	"fmt"
	"strconv"
	"strings"
)

// Rate is a frame rate as ffprobe reports one: a positive rational, never a float, so the
// raw stream a profile 7 conversion writes is retimed at exactly the source's rate.
type Rate struct {
	Num, Den int64
}

// Valid reports whether r is a positive rational.
func (r Rate) Valid() bool { return r.Num > 0 && r.Den > 0 }

// String spells r as ffmpeg reads a -framerate: "24000/1001".
func (r Rate) String() string {
	return strconv.FormatInt(r.Num, 10) + "/" + strconv.FormatInt(r.Den, 10)
}

// ParseRate reads ffprobe's "num/den". "0/0", a non-positive part and anything that is not a
// rational are not a rate.
func ParseRate(s string) (Rate, bool) {
	n, d, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return Rate{}, false
	}
	num, err1 := strconv.ParseInt(n, 10, 64)
	den, err2 := strconv.ParseInt(d, 10, 64)
	r := Rate{Num: num, Den: den}
	if err1 != nil || err2 != nil || !r.Valid() {
		return Rate{}, false
	}
	return r, true
}

// equal compares two rationals by value: 48/2 is 24/1.
func (r Rate) equal(o Rate) bool { return r.Num*o.Den == o.Num*r.Den }

// ConstantRate is the one rate a source's video runs at, where ffprobe establishes that it
// has one. r is the stream's r_frame_rate (the lowest rate every timestamp can be represented
// at) and avg its avg_frame_rate (frames over duration); where the two differ the stream is
// variable-rate, or ffprobe could not tell, and a raw stream retimed at either would play at
// the wrong speed or drift from its audio. (The two fields are defined in
// libavformat/avformat.h at 5d4d3bdc61, AVStream.r_frame_rate and avg_frame_rate,
// https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/libavformat/avformat.h , read 2026-10-01.)
// Reading equal fields as constant is ASSUMED sufficient for a Matroska source that carries a
// default duration, which is where both come from; a source where they differ is refused.
func ConstantRate(r, avg string) (Rate, error) {
	rr, ok1 := ParseRate(r)
	ar, ok2 := ParseRate(avg)
	switch {
	case !ok1 || !ok2:
		return Rate{}, fmt.Errorf("the video's frame rate could not be established (r_frame_rate %q, avg_frame_rate %q)", r, avg)
	case !rr.equal(ar):
		return Rate{}, fmt.Errorf("the video's frame rate is not constant (r_frame_rate %s, avg_frame_rate %s)", rr, ar)
	}
	return rr, nil
}

// StartsAtZero reports whether ffprobe's start_time of the video is zero: a raw stream
// carries no timestamps, so it is read from zero, and a source video starting anywhere else
// would come out shifted against the audio it is muxed beside.
func StartsAtZero(start string) bool {
	f, err := strconv.ParseFloat(strings.TrimSpace(start), 64)
	return err == nil && f == 0
}
