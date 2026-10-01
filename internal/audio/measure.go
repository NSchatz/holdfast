package audio

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Measurement is what one full decode of one audio stream established: how long it decodes
// to, and - where asked - its integrated loudness as loudnorm measures it.
type Measurement struct {
	// DurationSec is the decoded length, read from the decode's own progress report.
	DurationSec float64
	// Loudness is the stream's loudnorm measurement, nil where none was asked for.
	Loudness *Stats
}

// DecodeError is a stream that did not decode to the end: ffmpeg ran with -xerror and exited
// non-zero, which is a damaged stream (or a run that could not read it), never a measurement.
type DecodeError struct {
	// Stream is the stream specifier that was decoded.
	Stream string
	// Detail is ffmpeg's own error text, truncated.
	Detail string
	Err    error
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("audio stream %s does not fully decode: %v: %s", e.Stream, e.Err, e.Detail)
}

func (e *DecodeError) Unwrap() error { return e.Err }

// statsDescriptor is where a measurement's loudnorm report goes: the decode's first inherited
// descriptor (its stdout carries the progress report, its stderr the errors).
const statsDescriptor = "/proc/self/fd/3"

// Measure fully decodes the stream of file that spec selects (an ffmpeg stream specifier,
// such as "0:a:1" or "0:3") with -xerror, so a decode error ends the run and fails it, and
// reads the decoded length off the run's progress report. With loudness it also runs a
// measuring loudnorm over the decoded audio and reads its report.
//
// Every way of not establishing a figure is an error: a decode that failed is a *DecodeError,
// and a report that could not be read is a plain error. Neither is ever a zero.
func Measure(ctx context.Context, ffmpeg, file, spec string, loudness bool) (Measurement, error) {
	filter := ""
	if loudness {
		filter = MeasureFilter(statsDescriptor)
	}
	progress, stats, err := run(ctx, ffmpeg, file, spec, filter)
	if err != nil {
		return Measurement{}, err
	}
	dur, ok := lastDuration(progress)
	if !ok {
		return Measurement{}, fmt.Errorf("the decode of audio stream %s reported no length", spec)
	}
	m := Measurement{DurationSec: dur}
	if loudness {
		st, err := ParseStats(stats)
		if err != nil {
			return Measurement{}, fmt.Errorf("the loudness of audio stream %s: %w", spec, err)
		}
		m.Loudness = &st
	}
	return m, nil
}

// MeasureLoudness is the first loudness pass over source stream index of file, through
// prefilter ahead of the measuring loudnorm ("" for none).
func MeasureLoudness(ctx context.Context, ffmpeg, file string, index int, prefilter string) (Stats, error) {
	filter := MeasureFilter(statsDescriptor)
	if prefilter != "" {
		filter = prefilter + "," + filter
	}
	_, stats, err := run(ctx, ffmpeg, file, "0:"+strconv.Itoa(index), filter)
	if err != nil {
		return Stats{}, err
	}
	return ParseStats(stats)
}

// run decodes spec of file to the null muxer, through filter where one is given, and returns
// the progress report and whatever loudnorm wrote to the report descriptor.
func run(ctx context.Context, ffmpeg, file, spec, filter string) (progress, stats []byte, err error) {
	args := []string{"-hide_banner", "-nostdin", "-v", "error", "-xerror", "-i", file, "-map", spec}
	if filter != "" {
		args = append(args, "-filter:a", filter)
	}
	args = append(args, "-f", "null", "-progress", "pipe:1", "-nostats", "-")
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("opening the loudness report channel: %w", err)
	}
	defer func() { _ = pr.Close() }()
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.ExtraFiles = []*os.File{pw}
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return nil, nil, &DecodeError{Stream: spec, Err: err}
	}
	_ = pw.Close()
	got := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(pr)
		got <- b
	}()
	werr := cmd.Wait()
	stats = <-got
	if werr != nil || ctx.Err() != nil {
		if werr == nil {
			werr = ctx.Err()
		}
		return nil, nil, &DecodeError{Stream: spec, Detail: truncate(errb.String(), 300), Err: werr}
	}
	return out.Bytes(), stats, nil
}

// lastDuration is the last out_time_us of an ffmpeg progress report, in seconds.
func lastDuration(progress []byte) (float64, bool) {
	var last string
	sc := bufio.NewScanner(bytes.NewReader(progress))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "out_time_us="); ok {
			last = v
		}
	}
	us, err := strconv.ParseInt(last, 10, 64)
	if err != nil || us < 0 {
		return 0, false
	}
	return float64(us) / 1e6, true
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// IsDecodeError reports whether err is a stream that did not decode.
func IsDecodeError(err error) bool {
	var d *DecodeError
	return errors.As(err, &d)
}
