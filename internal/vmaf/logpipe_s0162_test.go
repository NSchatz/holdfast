package vmaf

// S0162: the VMAF log travels through a pipe and is decoded as it streams, so no
// frame-count-sized file lands in the (RAM-backed, in the shipped container) temp directory
// and holdfast never holds the log whole. Every test here names the acceptance criterion it
// grades. The route is recorded in vmaf.go under "Where the log goes".
//
// AC-9's second half (a killed run's leftover on persistent disk) and AC-10 (startup
// validation of a log directory) apply only to a directory route and do not apply here:
// there is no log file and no log directory at all.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/heapmeasure"
)

// pooledVals are the four pooled statistics a synthetic log carries, as the exact decimal
// text written into it, so a test can hold the parsed result bit-identical to that text.
type pooledVals struct {
	HarmonicMean, Min, Cb, Cr string
}

// awkwardPooled has more significant digits than a float64 holds and values that are not
// exactly representable, so "bit-identical" is a claim about the parse, not about round
// numbers that any parse gets right.
var awkwardPooled = pooledVals{
	HarmonicMean: "98.76543210987654321",
	Min:          "97.12345678901234567",
	Cb:           "41.000000000000007",
	Cr:           "40.99999999999999",
}

func (p pooledVals) json() string {
	return `  "pooled_metrics": {
    "psnr_y": {"min": 38.1, "max": 52.2, "mean": 44.4, "harmonic_mean": 44.1},
    "psnr_cb": {"min": ` + p.Cb + `, "max": 55.5, "mean": 47.7, "harmonic_mean": 47.2},
    "psnr_cr": {"min": ` + p.Cr + `, "max": 56.6, "mean": 48.8, "harmonic_mean": 48.3},
    "integer_adm2": {"min": 0.97, "max": 1.0, "mean": 0.99, "harmonic_mean": 0.99},
    "integer_motion2": {"min": 0.0, "max": 9.1, "mean": 3.3, "harmonic_mean": 1.9},
    "vmaf": {"min": ` + p.Min + `, "max": 100.0, "mean": 99.0, "harmonic_mean": ` + p.HarmonicMean + `}
  }`
}

// synthLog generates a libvmaf-shaped JSON log with `frames` realistic per-frame entries
// (vmaf, psnr_y, psnr_cb, psnr_cr and the default model's features), one entry at a time,
// so a log of any size can be fed to a reader without the test ever holding it. The
// per-frame values deliberately DISAGREE with the pooled ones (every frame scores far below
// the pooled min), so a parser that let frames feed the result returns the wrong numbers.
type synthLog struct {
	frames      int
	pooled      pooledVals
	pooledFirst bool

	stage int
	i     int
	buf   bytes.Buffer
	n     int64
}

func newSynthLog(frames int, pooled pooledVals, pooledFirst bool) *synthLog {
	return &synthLog{frames: frames, pooled: pooled, pooledFirst: pooledFirst}
}

func (g *synthLog) Read(p []byte) (int, error) {
	for g.buf.Len() == 0 {
		if !g.fill() {
			return 0, io.EOF
		}
	}
	n, err := g.buf.Read(p)
	g.n += int64(n)
	return n, err
}

func (g *synthLog) fill() bool {
	switch g.stage {
	case 0:
		g.buf.WriteString("{\n  \"version\": \"e9909ad\",\n  \"fps\": 61.27,\n")
		if g.pooledFirst {
			g.buf.WriteString(g.pooled.json() + ",\n")
		}
		g.buf.WriteString("  \"frames\": [")
		g.stage = 1
	case 1:
		if g.i == g.frames {
			g.buf.WriteString("\n  ],\n")
			if !g.pooledFirst {
				g.buf.WriteString(g.pooled.json() + ",\n")
			}
			g.buf.WriteString("  \"aggregate_metrics\": {\n  }\n}\n")
			g.stage = 2
			return true
		}
		sep := ","
		if g.i == 0 {
			sep = ""
		}
		f := float64(g.i%97) / 97
		fmt.Fprintf(&g.buf, `%s
    {
      "frameNum": %d,
      "metrics": {
        "psnr_y": %.6f,
        "psnr_cb": %.6f,
        "psnr_cr": %.6f,
        "integer_adm2": %.6f,
        "integer_adm_scale0": %.6f,
        "integer_adm_scale1": %.16f,
        "integer_adm_scale2": %.16f,
        "integer_adm_scale3": %.16f,
        "integer_motion2": %.6f,
        "integer_motion": %.6f,
        "integer_vif_scale0": %.6f,
        "integer_vif_scale1": %.6f,
        "integer_vif_scale2": %.6f,
        "integer_vif_scale3": %.6f,
        "vmaf": %.6f
      }
    }`, sep, g.i, 20+f, 12+f, 11+f, 0.5+f/4, 0.6+f/4, 0.7+f/4, 0.8+f/4, 0.9+f/4,
			f*5, f*5, 0.4+f/2, 0.5+f/2, 0.6+f/2, 0.7+f/2, 10+f)
		g.i++
	default:
		return false
	}
	return true
}

// writeSynthLog writes a synthetic log to path, streaming, and returns its size.
func writeSynthLog(t *testing.T, path string, frames int, pooled pooledVals, pooledFirst bool) int64 {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(f, newSynthLog(frames, pooled, pooledFirst))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// logPathArg is the shell fragment every stub here uses to find where Score told libvmaf to
// write: it parses log_path= out of the -lavfi argument exactly as stubFfmpegWritingLog does,
// so the stub writes through WHATEVER destination Score hands it - a file path, or the pipe
// descriptor's path (F2).
const logPathArg = "for a in \"$@\"; do\n" +
	"  case \"$a\" in *log_path=*)\n" +
	"    p=$(printf '%s' \"$a\" | sed -n 's/.*log_path=\\([^:]*\\).*/\\1/p')\n" +
	"  ;; esac\n" +
	"done\n"

// writeStub writes a POSIX shell stand-in for ffmpeg into dir and returns its path.
func writeStub(t *testing.T, dir, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub is POSIX")
	}
	stub := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub
}

// stubFfmpegCopyingLog is stubFfmpegWritingLog for a payload already on disk, so a log far
// too large to hold as a test string can still be written through Score's destination.
func stubFfmpegCopyingLog(t *testing.T, payload string) string {
	t.Helper()
	return writeStub(t, t.TempDir(), logPathArg+"cat '"+payload+"' > \"$p\"\n")
}

// wantPooled holds a Result bit-identical to the pooled text of a synthetic log, and every
// descriptive field to what Score has always carried back.
func wantPooled(t *testing.T, got Result, p pooledVals, r Request) {
	t.Helper()
	parse := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cb, cr := parse(p.Cb), parse(p.Cr)
	for _, c := range []struct {
		name      string
		got, want float64
	}{
		{"HarmonicMean", got.HarmonicMean, parse(p.HarmonicMean)},
		{"Min", got.Min, parse(p.Min)},
		{"ChromaMin", got.ChromaMin, math.Min(cb, cr)},
	} {
		if math.Float64bits(c.got) != math.Float64bits(c.want) {
			t.Errorf("%s = %v (bits %#x), want %v (bits %#x) - not the pooled value the log carries",
				c.name, c.got, math.Float64bits(c.got), c.want, math.Float64bits(c.want))
		}
	}
	if got.ChromaMetric != ChromaMetricName || got.PixelFormat != r.PixelFormat || got.Stream != ScoredStream ||
		got.ReferenceFilter != r.ReferenceFilter || got.DistortedFilter != r.DistortedFilter {
		t.Errorf("descriptive fields changed: got %+v for request %+v", got, r)
	}
}

// regularFileBytes is the shell fragment that prints the regular-file bytes under $TMPDIR.
const regularFileBytes = "find \"$TMPDIR\" -type f -exec cat {} + | wc -c"

// TestS0162_AC1_NoLogBytesUnderTheTempDirectory is [AC-1]: with a log of at least 8 MiB, a
// score pass leaves no more than 64 KiB of regular-file bytes under the process temp
// directory at the instant the log has just been written. The stub writes the log to the
// destination Score hands it and then measures $TMPDIR (F2). The control runs the same stub
// against the pre-change destination, a file under $TMPDIR, and must exceed 64 KiB, so a
// stub that wrote nothing could not pass.
func TestS0162_AC1_NoLogBytesUnderTheTempDirectory(t *testing.T) {
	const bound = 64 << 10
	outside := t.TempDir()
	owned := filepath.Join(t.TempDir(), "tmp")
	if err := os.Mkdir(owned, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(outside, "payload.json")
	if n := writeSynthLog(t, payload, 16000, awkwardPooled, false); n < 8<<20 {
		t.Fatalf("the payload is %d bytes, want at least 8 MiB - the fixture proves nothing", n)
	}
	record := filepath.Join(outside, "tmpdir-bytes")
	dest := filepath.Join(outside, "destination")
	stub := writeStub(t, outside, logPathArg+
		"printf '%s' \"$p\" > '"+dest+"'\n"+
		"cat '"+payload+"' > \"$p\" || exit 1\n"+
		regularFileBytes+" > '"+record+"'\n")
	t.Setenv("TMPDIR", owned)
	if os.TempDir() != owned {
		t.Fatalf("os.TempDir() = %q, want the test-owned %q", os.TempDir(), owned)
	}
	readBytes := func() int64 {
		t.Helper()
		raw, err := os.ReadFile(record)
		if err != nil {
			t.Fatalf("the stub recorded no measurement: %v", err)
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			t.Fatalf("unreadable measurement %q: %v", raw, err)
		}
		return n
	}

	r := req("d.mkv", "r.mkv")
	res, err := Score(context.Background(), stub, r)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	// The stub really wrote the 8 MiB log through the destination it was given: the pooled
	// values came back from it.
	wantPooled(t, res, awkwardPooled, r)
	if got := readBytes(); got > bound {
		t.Errorf("%d regular-file bytes under $TMPDIR right after the log was written, want at most %d", got, bound)
	}
	d, _ := os.ReadFile(dest)
	t.Logf("the stub wrote the log to %q; $TMPDIR held %d regular-file bytes", d, readBytes())

	// The control: the pre-change destination, a file under $TMPDIR, through the same stub
	// and the same measurement.
	oldDest := filepath.Join(owned, "holdfast-vmaf-control.json")
	if out, err := exec.Command(stub, "-lavfi", BuildFilter(r, oldDest)).CombinedOutput(); err != nil {
		t.Fatalf("control run: %v: %s", err, out)
	}
	if got := readBytes(); got <= bound {
		t.Fatalf("the control (log to a file under $TMPDIR) measured %d bytes, not more than %d - the "+
			"instrument does not bite, so the pass above proves nothing", got, bound)
	} else {
		t.Logf("control: a log written to a file under $TMPDIR measured %d bytes", got)
	}
}

// heapPeak measures what decode held while it read a synthetic log of `frames` entries, as
// the larger of two readings, both relative to the same process (performance PB5):
//
//   - live heap after a forced collection, sampled every MiB the decoder consumes
//     (heapmeasure), which sees anything retained as the log is read; and
//   - the largest SINGLE heap allocation made while it ran, read from a memory profile taken
//     at runtime.MemProfileRate = 1. At that rate the runtime profiles every allocation
//     (nextSample returns 0 for a rate of 1, go1.25.14 src/runtime/malloc.go), and a profile
//     bucket is keyed by stack AND size (stkbucket, src/runtime/mprof.go), so
//     AllocBytes/AllocObjects is the exact size of that bucket's allocations. A whole-file
//     buffer, however short-lived, cannot be missed (F1). Read from the pinned toolchain's
//     source, https://github.com/golang/go/blob/go1.25.14/src/runtime/malloc.go, read 2026-09-30.
//
// Between two samples the decoder consumes one MiB, so live heap that builds up out of
// many small objects is seen by the first reading, and one that comes from one big
// allocation is seen by the second.
func heapPeak(t *testing.T, what string, frames int, decode func(io.Reader) (vmafLog, error)) (peak int64, logBytes int64) {
	t.Helper()
	prev := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	defer func() { runtime.MemProfileRate = prev }()

	before := allocSizes()
	probe := heapmeasure.Start(what)
	src := newSynthLog(frames, awkwardPooled, false)
	sampler := &samplingReader{r: src, every: 1 << 20, sample: probe.Sample}
	parsed, err := decode(sampler)
	probe.Sample()
	if err != nil {
		t.Fatalf("%s: decode: %v", what, err)
	}
	if p := parsed.PooledMetrics; p.VMAF.Min == nil || p.VMAF.HarmonicMean == nil || p.PsnrCb.Min == nil || p.PsnrCr.Min == nil {
		t.Fatalf("%s: the decode lost a pooled statistic: %+v", what, p)
	}
	live, err := probe.Delta()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	largest := largestNewAllocation(before, allocSizes())
	t.Logf("%s: %d bytes of log; live heap peak %+d bytes over %d samples; largest single allocation %d bytes",
		what, src.n, live, sampler.samples, largest)
	return max(live, largest), src.n
}

type samplingReader struct {
	r       io.Reader
	every   int64
	read    int64
	next    int64
	samples int
	sample  func()
}

func (s *samplingReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.read += int64(n)
	if s.read >= s.next {
		s.sample()
		s.samples++
		s.next = s.read + s.every
	}
	return n, err
}

type allocKey struct {
	stack [32]uintptr
	size  int64
}

// allocSizes is the memory profile's allocation count per (stack, size) bucket.
func allocSizes() map[allocKey]int64 {
	// A profile reflects allocations as of the most recently completed collections.
	runtime.GC()
	runtime.GC()
	runtime.GC()
	n, _ := runtime.MemProfile(nil, true)
	var recs []runtime.MemProfileRecord
	for {
		recs = make([]runtime.MemProfileRecord, n+256)
		var ok bool
		n, ok = runtime.MemProfile(recs, true)
		if ok {
			recs = recs[:n]
			break
		}
	}
	out := make(map[allocKey]int64, len(recs))
	for _, r := range recs {
		if r.AllocObjects > 0 {
			out[allocKey{r.Stack0, r.AllocBytes / r.AllocObjects}] += r.AllocObjects
		}
	}
	return out
}

func largestNewAllocation(before, after map[allocKey]int64) int64 {
	var largest int64
	for k, n := range after {
		if n > before[k] && k.size > largest {
			largest = k.size
		}
	}
	return largest
}

// wholeFileDecode is the pre-change read path, kept test-side as the F1 control: the whole
// log read into memory and unmarshalled at once.
func wholeFileDecode(r io.Reader) (vmafLog, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return vmafLog{}, err
	}
	var parsed vmafLog
	err = json.Unmarshal(raw, &parsed)
	return parsed, err
}

// TestS0162_AC2_DecodeMemoryDoesNotGrowWithFrames is [AC-2]: the peak heap while decoding a
// 200,000-frame log and while decoding a 2,000-frame log that is otherwise the same differ by
// less than 8 MiB (O(1) memory in frames, performance PB4). F1's control: the same
// measurement over today's whole-file decode of the 200,000-frame log exceeds the bound, so
// the instrument provably bites on the very bug it guards.
func TestS0162_AC2_DecodeMemoryDoesNotGrowWithFrames(t *testing.T) {
	const bound = 8 << 20
	small, _ := heapPeak(t, "streaming decode, 2,000 frames", 2000, decodeLog)
	large, size := heapPeak(t, "streaming decode, 200,000 frames", 200000, decodeLog)
	if size <= 64<<20 {
		t.Fatalf("the 200,000-frame log is %d bytes, want well over 64 MiB - re-tune the fixture", size)
	}
	if large-small >= bound {
		t.Errorf("decoding 200,000 frames held %d bytes more than decoding 2,000 (bound %d): the parse "+
			"grows with the number of frames", large-small, bound)
	}
	control, _ := heapPeak(t, "whole-file decode (control), 200,000 frames", 200000, wholeFileDecode)
	if control-small < bound {
		t.Fatalf("the whole-file control held only %d bytes more than the 2,000-frame streaming decode, "+
			"under the %d bound - the measurement cannot see a log-sized allocation, so the pass above "+
			"proves nothing", control-small, bound)
	}
}

// TestS0162_AC3_PooledValuesAreReturnedBitIdentical is [AC-3]: through the shipped Score
// and its pipe, the pooled harmonic_mean, min and the lower chroma min come back
// bit-identical to the log's own text, whether pooled_metrics follows the frames array
// (libvmaf's order) or precedes it, and on the 200,000-frame log. Every frame in these logs
// scores far below the pooled values, so a result fed by the frames would be caught.
func TestS0162_AC3_PooledValuesAreReturnedBitIdentical(t *testing.T) {
	r := req("d.mkv", "r.mkv")
	r.ReferenceFilter = "yadif=mode=send_frame:parity=auto:deint=all"
	r.DistortedFilter = "scale=1920:1080"
	for _, tc := range []struct {
		name        string
		frames      int
		pooledFirst bool
	}{
		{"pooled after frames", 300, false},
		{"pooled before frames", 300, true},
		{"200,000 frames, pooled after", 200000, false},
		{"200,000 frames, pooled before", 200000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := filepath.Join(t.TempDir(), "log.json")
			writeSynthLog(t, payload, tc.frames, awkwardPooled, tc.pooledFirst)
			res, err := Score(context.Background(), stubFfmpegCopyingLog(t, payload), r)
			if err != nil {
				t.Fatalf("Score: %v", err)
			}
			wantPooled(t, res, awkwardPooled, r)
			if res.Min < 90 {
				t.Errorf("Min = %v: a per-frame value (every frame scores ~10) fed the result", res.Min)
			}
		})
	}
}

// TestS0162_AC4_RealFfmpegPipeScoreMatchesAnIndependentFileLog is [AC-4]: against the
// pinned ffmpeg with libvmaf, Score's figures are bit-identical to the pooled_metrics of an
// independent run over the same graph and inputs whose log went to a test-owned FILE and
// was decoded whole - the old read path as the oracle for the new one. It never skips.
func TestS0162_AC4_RealFfmpegPipeScoreMatchesAnIndependentFileLog(t *testing.T) {
	bin := ffmpegBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: ffmpeg required for the pipe-versus-file oracle: %v", err)
	}
	dir := t.TempDir()
	ref := filepath.Join(dir, "ref.mkv")
	dist := filepath.Join(dir, "dist.mkv")
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=24",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "4M", "-pix_fmt", "yuv420p", ref)
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-i", ref,
		"-c:v", "libx265", "-crf", "30", "-preset", "veryfast", "-x265-params", "log-level=error",
		"-pix_fmt", "yuv420p10le", dist)

	for _, threads := range []int{1, 3} {
		t.Run(fmt.Sprintf("threads=%d", threads), func(t *testing.T) {
			r := req(dist, ref)
			r.Threads = threads
			got, err := Score(context.Background(), bin, r)
			if err != nil {
				t.Fatalf("Score: %v", err)
			}
			// The oracle: the same argv Score runs, with the log sent to a file.
			logPath := filepath.Join(t.TempDir(), "oracle.json")
			_, graphThreads := PoolThreads(r.Threads)
			mustFF(t, bin, "-hide_banner", "-nostdin", "-loglevel", "error", "-y",
				"-filter_complex_threads", strconv.Itoa(graphThreads),
				"-i", r.Distorted, "-i", r.Reference, "-lavfi", BuildFilter(r, logPath), "-f", "null", "-")
			raw, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			var oracle vmafLog
			if err := json.Unmarshal(raw, &oracle); err != nil {
				t.Fatal(err)
			}
			p := oracle.PooledMetrics
			if p.VMAF.HarmonicMean == nil || p.VMAF.Min == nil || p.PsnrCb.Min == nil || p.PsnrCr.Min == nil {
				t.Fatalf("the oracle's log is missing a pooled statistic: %s", truncate(string(raw), 400))
			}
			for _, c := range []struct {
				name      string
				got, want float64
			}{
				{"HarmonicMean", got.HarmonicMean, *p.VMAF.HarmonicMean},
				{"Min", got.Min, *p.VMAF.Min},
				{"ChromaMin", got.ChromaMin, math.Min(*p.PsnrCb.Min, *p.PsnrCr.Min)},
			} {
				if math.Float64bits(c.got) != math.Float64bits(c.want) {
					t.Errorf("%s through the pipe = %v, the file-log oracle says %v", c.name, c.got, c.want)
				}
			}
			t.Logf("pipe: harmonic_mean=%v min=%v chroma=%v (%d-byte oracle log)",
				got.HarmonicMean, got.Min, got.ChromaMin, len(raw))
		})
	}
}

// TestS0162_AC5_SubsampleIsPassedThroughAsConfigured is [AC-5]: the n_subsample Score hands
// libvmaf is max(Request.Subsample, 1), for any request, whatever the input - Score reads
// nothing about the input's duration or frame count before it builds the graph, and the
// log route adds no sampling of its own.
func TestS0162_AC5_SubsampleIsPassedThroughAsConfigured(t *testing.T) {
	for _, n := range []int{-4, 0, 1, 2, 5, 30} {
		dir := t.TempDir()
		argv := filepath.Join(dir, "argv")
		bin := stubFfmpegRecordingArgv(t, dir, argv,
			`{"pooled_metrics":{"vmaf":{"min":98.0,"harmonic_mean":99.1},"psnr_cb":{"min":41.0},"psnr_cr":{"min":40.0}}}`)
		r := req("a-feature-film.mkv", "its-source.mkv")
		r.Subsample = n
		if _, err := Score(context.Background(), bin, r); err != nil {
			t.Fatalf("Score(subsample=%d): %v", n, err)
		}
		graph := lavfiArg(t, argv)
		want := fmt.Sprintf(":n_subsample=%d:", max(n, 1))
		if !strings.Contains(graph, want) || strings.Count(graph, "n_subsample=") != 1 {
			t.Errorf("subsample=%d: the executed graph does not carry exactly %q: %s", n, want, graph)
		}
	}
}

// TestS0162_AC6_MalformedOrTruncatedLogIsRejected is [AC-6]: an empty log, one that is not
// JSON, one cut off anywhere - inside frames, before pooled_metrics, inside it - and one
// missing any pooled statistic in either order are errors with no Result; a missing
// statistic keeps the "missing a pooled statistic" wording; a genuine 0.0 is still a score.
func TestS0162_AC6_MalformedOrTruncatedLogIsRejected(t *testing.T) {
	const pooled = `"pooled_metrics":{"vmaf":{"min":98.0,"harmonic_mean":99.1},"psnr_cb":{"min":41.0},"psnr_cr":{"min":40.0}}`
	const frame = `{"frameNum":0,"metrics":{"psnr_cb":12.0,"vmaf":9.5}}`
	for _, tc := range []struct{ name, log, want string }{
		{"empty", "", "log is empty"},
		{"not JSON", "libvmaf crashed here\n", "parse log"},
		{"a JSON array, not an object", `[` + frame + `]`, "not a JSON object"},
		{"cut inside the frames array", `{"frames":[` + frame + `,{"frameNum":1,"metr`, "parse log"},
		{"cut between frames and pooled_metrics", `{"frames":[` + frame + `],`, "parse log"},
		{"cut inside pooled_metrics", `{"frames":[` + frame + `],"pooled_metrics":{"vmaf":{"min":98.0,"harmonic_`, "parse log"},
		{"cut after pooled_metrics, before the closing brace", `{"frames":[` + frame + `],` + pooled, "parse log"},
		{"cut after pooled_metrics that came first, inside frames", `{` + pooled + `,"frames":[` + frame + `,`, "parse log"},
		{"a malformed frame entry", `{"frames":[{"frameNum":}],` + pooled + `}`, "parse log"},
		{"data after the closing brace", `{"frames":[],` + pooled + `} {}`, "after its closing brace"},
		{"min missing, pooled before frames", `{"pooled_metrics":{"vmaf":{"harmonic_mean":99.1},"psnr_cb":{"min":41.0},"psnr_cr":{"min":40.0}},"frames":[` + frame + `]}`, "missing a pooled statistic"},
		{"psnr_cr missing, pooled after frames", `{"frames":[` + frame + `],"pooled_metrics":{"vmaf":{"min":98.0,"harmonic_mean":99.1},"psnr_cb":{"min":41.0}}}`, "missing a pooled statistic"},
		{"pooled_metrics null", `{"frames":[` + frame + `],"pooled_metrics":null}`, "missing a pooled statistic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Score(context.Background(), stubFfmpegWritingLog(t, tc.log), req("d.mkv", "r.mkv"))
			if err == nil {
				t.Fatalf("Score accepted the log (got %+v)", res)
			}
			if res != (Result{}) {
				t.Errorf("an error came back WITH a result: %+v", res)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}

	// A genuine 0.0, with the frames around it, is a score and not an absence.
	zero := `{"frames":[` + frame + `],"pooled_metrics":{"vmaf":{"min":0.0,"harmonic_mean":0.0},"psnr_cb":{"min":0.0},"psnr_cr":{"min":0.0}},"aggregate_metrics":{}}`
	res, err := Score(context.Background(), stubFfmpegWritingLog(t, zero), req("d.mkv", "r.mkv"))
	if err != nil {
		t.Fatalf("a complete log of genuine 0.0 scores must parse: %v", err)
	}
	if res.Min != 0 || res.HarmonicMean != 0 || res.ChromaMin != 0 || res.ChromaMetric != ChromaMetricName {
		t.Errorf("got %+v, want zeroed scores with the metric carried back", res)
	}

	// Every proper prefix of a real-shaped log, in both orders, is refused by the decoder:
	// there is no byte at which a cut-off log reads as complete.
	for _, first := range []bool{false, true} {
		full, err := io.ReadAll(newSynthLog(3, awkwardPooled, first))
		if err != nil {
			t.Fatal(err)
		}
		whole := bytes.TrimRight(full, "\n")
		if _, err := decodeLog(bytes.NewReader(full)); err != nil {
			t.Fatalf("the complete log (pooled first=%t) does not decode: %v", first, err)
		}
		for i := 0; i < len(whole); i++ {
			if _, err := decodeLog(bytes.NewReader(whole[:i])); err == nil {
				t.Fatalf("pooled first=%t: the log cut at byte %d of %d decoded as complete: %q",
					first, i, len(whole), whole[max(0, i-40):i])
			}
		}
	}
}

// pidAlive reports whether a process with this pid still exists (a reaped child does not).
func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no pid recorded: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("bad pid %q", raw)
	}
	return pid
}

// TestS0162_AC7_NoLogOrCancellationReturnsPromptly is [AC-7]: ffmpeg exiting without ever
// writing the log (status zero or not), or the pass being cancelled, returns an error within
// 10 seconds and leaves no child process behind - the pass never waits for a log that will
// not come.
func TestS0162_AC7_NoLogOrCancellationReturnsPromptly(t *testing.T) {
	const limit = 10 * time.Second
	timed := func(t *testing.T, ctx context.Context, bin string) (time.Duration, error) {
		t.Helper()
		start := time.Now()
		done := make(chan error, 1)
		go func() {
			_, err := Score(ctx, bin, req("d.mkv", "r.mkv"))
			done <- err
		}()
		select {
		case err := <-done:
			return time.Since(start), err
		case <-time.After(3 * limit):
			t.Fatalf("Score had not returned after %s", 3*limit)
			return 0, nil
		}
	}

	t.Run("exit status zero, no log", func(t *testing.T) {
		dir := t.TempDir()
		pidf := filepath.Join(dir, "pid")
		bin := writeStub(t, dir, "echo $$ > '"+pidf+"'\nexit 0\n")
		took, err := timed(t, context.Background(), bin)
		if err == nil || !strings.Contains(err.Error(), "log is empty") {
			t.Fatalf("err = %v, want the empty-log refusal", err)
		}
		if took > limit {
			t.Errorf("took %s, want at most %s", took, limit)
		}
		if pid := readPid(t, pidf); pidAlive(pid) {
			t.Errorf("the stand-in (pid %d) is still alive", pid)
		}
	})

	t.Run("exit status non-zero, no log", func(t *testing.T) {
		dir := t.TempDir()
		bin := writeStub(t, dir, "echo 'Error opening input files: Invalid data found' >&2\nexit 1\n")
		took, err := timed(t, context.Background(), bin)
		// The failure text is as informative as CombinedOutput's was: ffmpeg's own stderr.
		if err == nil || !strings.Contains(err.Error(), "ffmpeg failed") || !strings.Contains(err.Error(), "Invalid data found") {
			t.Fatalf("err = %v, want ffmpeg's failure with its stderr", err)
		}
		if took > limit {
			t.Errorf("took %s, want at most %s", took, limit)
		}
	})

	t.Run("cancelled mid-pass", func(t *testing.T) {
		dir := t.TempDir()
		pidf := filepath.Join(dir, "pid")
		bin := writeStub(t, dir, "echo $$ > '"+pidf+".tmp' && mv '"+pidf+".tmp' '"+pidf+"'\nexec sleep 60\n")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			for i := 0; i < 500; i++ {
				if _, err := os.Stat(pidf); err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
		}()
		took, err := timed(t, ctx, bin)
		if err == nil {
			t.Fatal("a cancelled pass returned no error")
		}
		if took > limit {
			t.Errorf("took %s, want at most %s", took, limit)
		}
		if pid := readPid(t, pidf); pidAlive(pid) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("the cancelled stand-in (pid %d) was left running", pid)
		}
	})

	t.Run("a process ffmpeg spawned keeps the log pipe open", func(t *testing.T) {
		dir := t.TempDir()
		pidf := filepath.Join(dir, "pid")
		bin := writeStub(t, dir, "sleep 60 &\necho $! > '"+pidf+"'\nexit 0\n")
		t.Cleanup(func() {
			if raw, err := os.ReadFile(pidf); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
					syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		})
		took, err := timed(t, context.Background(), bin)
		if err == nil {
			t.Fatal("a pass whose log never came returned no error")
		}
		if took > limit {
			t.Errorf("took %s, want at most %s", took, limit)
		}
		t.Logf("returned after %s: %v", took, err)
	})
}

// TestS0162_AC8_LogChannelSetupFailureIsAnError is [AC-8]'s vmaf half: when the log pipe
// cannot be set up, Score returns an error and never starts ffmpeg. The engine half, that
// such an error records the job VMAF-unmeasured with the source intact, is
// engine.TestS0162_AC8_ScorePassErrorIsUnmeasuredAndSourceIntact.
func TestS0162_AC8_LogChannelSetupFailureIsAnError(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	payload := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(payload, []byte(`{"pooled_metrics":{"vmaf":{"min":98.0,"harmonic_mean":99.1},"psnr_cb":{"min":41.0},"psnr_cr":{"min":40.0}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := writeStub(t, dir, ": > '"+ran+"'\n"+logPathArg+"cat '"+payload+"' > \"$p\"\n")

	orig := newLogPipe
	t.Cleanup(func() { newLogPipe = orig })
	newLogPipe = func() (*os.File, *os.File, error) { return nil, nil, syscall.EMFILE }
	res, err := Score(context.Background(), bin, req("d.mkv", "r.mkv"))
	if err == nil {
		t.Fatalf("Score succeeded without a log channel: %+v", res)
	}
	if !strings.Contains(err.Error(), "log pipe") || !errors.Is(err, syscall.EMFILE) {
		t.Errorf("err = %v, want the log-pipe refusal wrapping the cause", err)
	}
	if _, serr := os.Stat(ran); serr == nil {
		t.Error("ffmpeg ran although its log could not have been read")
	}

	// Anti-vacuity: the same stub with the channel available scores.
	newLogPipe = orig
	if _, err := Score(context.Background(), bin, req("d.mkv", "r.mkv")); err != nil {
		t.Fatalf("the control with a working channel failed: %v", err)
	}
}

// openFDs counts this process's open descriptors.
func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("reading /proc/self/fd: %v", err)
	}
	return len(ents)
}

// TestS0162_AC9_NoLogIsLeftBehind is [AC-9]'s first half: whether a pass succeeds or fails
// in any of the ways above, it leaves no log file behind - there is none to leave, and the
// temp directory stays empty - and it leaves no descriptor of the pipe open either. The
// second half (a killed run's leftover on persistent disk) does not apply: nothing is ever
// written to disk.
func TestS0162_AC9_NoLogIsLeftBehind(t *testing.T) {
	const ok = `{"pooled_metrics":{"vmaf":{"min":98.0,"harmonic_mean":99.1},"psnr_cb":{"min":41.0},"psnr_cr":{"min":40.0}}}`
	bins := map[string]string{
		"success":        stubFfmpegWritingLog(t, ok),
		"not JSON":       stubFfmpegWritingLog(t, "garbage"),
		"missing a stat": stubFfmpegWritingLog(t, `{"pooled_metrics":{}}`),
		"empty log":      writeStub(t, t.TempDir(), "exit 0\n"),
		"ffmpeg failed":  writeStub(t, t.TempDir(), "echo boom >&2\nexit 1\n"),
	}
	owned := filepath.Join(t.TempDir(), "tmp")
	if err := os.Mkdir(owned, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", owned)
	// Warm up once so a descriptor the runtime opens lazily on first use is not counted.
	_, _ = Score(context.Background(), bins["success"], req("d.mkv", "r.mkv"))
	fds := openFDs(t)
	for name, bin := range bins {
		for i := 0; i < 5; i++ {
			_, err := Score(context.Background(), bin, req("d.mkv", "r.mkv"))
			if (err == nil) != (name == "success") {
				t.Fatalf("%s: err = %v", name, err)
			}
		}
		ents, err := os.ReadDir(owned)
		if err != nil {
			t.Fatal(err)
		}
		if len(ents) != 0 {
			t.Errorf("%s: the pass left %d entries in the temp directory: %v", name, len(ents), ents)
		}
	}
	if got := openFDs(t); got != fds {
		t.Errorf("open descriptors went from %d to %d across the passes - a pipe end leaks", fds, got)
	}
}
