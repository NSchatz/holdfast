package probe

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// CensusProps is everything a library census reads out of one file: the codec it
// carries, the coded height of its first video stream, and its pixel format. Every
// field is what the corresponding single-field Prober method returns, and the whole
// snapshot costs ONE ffprobe process rather than three.
//
// That matters at census scale and nowhere else: the single-field probes exist for a
// pipeline that asks one question, decides, and usually stops, while a census asks
// every question about every file in the library and never stops early. Three
// subprocesses per file would make the cost of counting a library three times the cost
// of describing one.
//
// ABSENCE IS NOT ZERO here, exactly as it is not in VideoProps. An unanswered height is
// 0 and an unanswered codec or pixel format is "", and a reporting surface must render
// each as UNKNOWN: a census that filed an unanswered height under a numeric band, or an
// unanswered pixel format under 8-bit, would be inventing evidence about the operator's
// library in the one report whose whole job is to be evidence.
type CensusProps struct {
	// Answered reports whether ffprobe reached a verdict of its own about this path
	// (see firstLineAnswered). false means it never got to look - a missing or
	// unexecutable binary, a killing signal, or a cancelled context - and the other
	// three fields then say nothing about the file.
	Answered bool
	// Codec is codec_name for the first video stream, "" when there is none.
	Codec string
	// Height is the coded height of the first video stream in pixels, 0 when unknown.
	Height int
	// PixFmt is pix_fmt verbatim, "" when unknown.
	PixFmt string
}

// censusEntries is the single -show_entries argument the census probe issues.
const censusEntries = "stream=codec_name,height,pix_fmt"

// CensusProps takes one snapshot of what f contributes to a census.
func (p *Prober) CensusProps(ctx context.Context, f string) CensusProps {
	fields, answered := p.censusFields(ctx, f)
	out := CensusProps{Answered: answered}
	if !answered {
		return out
	}
	out.Codec = fields["codec_name"]
	out.PixFmt = fields["pix_fmt"]
	if h := fields["height"]; intRe.MatchString(h) {
		n, err := strconv.Atoi(h)
		if err == nil {
			out.Height = n
		}
	}
	return out
}

// censusFields runs the one probe and parses its `key=value` lines, reporting whether
// ffprobe answered at all. The answered rule is firstLineAnswered's, for the reason
// given there: a non-zero exit AFTER reading the path is ffprobe's way of saying "this
// is not media I can decode", which is evidence about the FILE, while a binary that
// could not be started or a cancelled context is evidence about this host.
func (p *Prober) censusFields(ctx context.Context, f string) (map[string]string, bool) {
	m := map[string]string{}
	out, err := exec.CommandContext(ctx, p.FFprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", censusEntries, "-of", "default=nw=1", "--", f).Output()
	if ctx.Err() != nil {
		return m, false
	}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || !ee.ProcessState.Exited() {
			return m, false
		}
		return m, true
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m, true
}
