package vmaf

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func ffmpegBin() string {
	if v := os.Getenv("TRANSCODE_FFMPEG"); v != "" {
		return v
	}
	return "ffmpeg"
}

func TestAvailable_TrueOnRealFfmpeg(t *testing.T) {
	bin := ffmpegBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: ffmpeg not found — VMAF proof requires it: %v", err)
	}
	if !Available(context.Background(), bin) {
		t.Fatal("Available() = false on a build that should ship libvmaf")
	}
}

func TestAvailable_FalseWhenFilterMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub is POSIX")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "ffmpeg")
	// A fake ffmpeg whose -filters output omits libvmaf.
	script := "#!/bin/sh\necho ' .. scale            V->V       Scale the input video size.'\necho ' .. crop             V->V       Crop the input video.'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if Available(context.Background(), stub) {
		t.Error("Available() = true for a build without the libvmaf filter")
	}
}

func mustFF(t *testing.T, bin string, args ...string) {
	t.Helper()
	if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

// TestScore locks the JSON field mapping the whole gate depends on: a faithful
// encode scores high, a degraded one scores lower, and both pooled fields
// (harmonic_mean, min) are populated in (0,100].
func TestScore(t *testing.T) {
	bin := ffmpegBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: ffmpeg required for the VMAF proof: %v", err)
	}
	dir := t.TempDir()
	ref := filepath.Join(dir, "ref.mp4")
	good := filepath.Join(dir, "good.mp4")
	bad := filepath.Join(dir, "bad.mp4")
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=320x240:rate=10", "-c:v", "libx264", "-b:v", "4M", ref)
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-i", ref,
		"-c:v", "libx265", "-crf", "22", "-x265-params", "log-level=error", good)
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-i", ref,
		"-vf", "scale=48:36,scale=320:240:flags=neighbor",
		"-c:v", "libx265", "-crf", "45", "-x265-params", "log-level=error", bad)

	g, err := Score(context.Background(), bin, good, ref, 1, "version=vmaf_v0.6.1")
	if err != nil {
		t.Fatalf("Score(good): %v", err)
	}
	for _, v := range []float64{g.HarmonicMean, g.Min} {
		if v <= 0 || v > 100 {
			t.Errorf("pooled VMAF %v out of (0,100] — JSON field mapping likely wrong", v)
		}
	}
	if g.HarmonicMean < 90 {
		t.Errorf("a faithful crf22 encode should score high; got harmonic_mean=%.2f", g.HarmonicMean)
	}
	b, err := Score(context.Background(), bin, bad, ref, 1, "version=vmaf_v0.6.1")
	if err != nil {
		t.Fatalf("Score(bad): %v", err)
	}
	if b.HarmonicMean >= g.HarmonicMean {
		t.Errorf("degraded (%.2f) should score below faithful (%.2f)", b.HarmonicMean, g.HarmonicMean)
	}
}

// TestPoolingStatistic_OnlyRawMinSeesSubOnePercentDamage is the EVIDENCE for the
// central design decision of TRANSCODE-11, and it exists so that decision cannot rot
// into an unverifiable code comment.
//
// The roadmap proposed a low-percentile floor (1st-percentile / worst-5%) as the
// likely statistic, to be evaluated against a conservatively-set raw min. This test
// IS that evaluation, and it inverts the hypothesis. It builds one locally-broken
// encode — 240 frames, exactly ONE of them destroyed (0.42%) — and measures all
// three candidate statistics on real libvmaf:
//
//	harmonic mean ~99  → BLIND (clears min_vmaf=95 with room to spare)
//	1st percentile ~98 → BLIND (a percentile tolerates a FRACTION of frames, and
//	                     0.42% is below the 1% it discards by construction)
//	raw min       ~43  → CATCHES IT (below vmaf_min_pool=60)
//
// The percentile is not a weaker version of the raw min — for this threat it is a
// re-run of the bug. Damage small enough to evade the mean is, by construction, a
// small FRACTION of frames, which is exactly what a percentile discards. And its
// blind spot GROWS with runtime: 1% of a 2-hour film is ~1,700 frames (~72 s) of
// destroyed video it would wave through. The raw min is the only candidate whose
// guarantee does not decay with duration, which is why it is what ships.
//
// If someone later "improves" the floor into a percentile, this test reds.
func TestPoolingStatistic_OnlyRawMinSeesSubOnePercentDamage(t *testing.T) {
	bin := ffmpegBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: ffmpeg required for the pooling-statistic proof: %v", err)
	}
	dir := t.TempDir()
	ref := filepath.Join(dir, "ref.mkv")
	broken := filepath.Join(dir, "broken.mkv")

	// 240 frames (10s @ 24fps).
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=10:size=320x240:rate=24",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M", "-pix_fmt", "yuv420p", ref)
	// A high-quality encode with EXACTLY ONE frame (n=100) destroyed: 0.42% of the file.
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-i", ref,
		"-filter_complex",
		"[0:v]split=2[cl][dm];"+
			"[dm]scale=56:42,scale=320:240:flags=neighbor[bad];"+
			"[cl][bad]overlay=enable='between(n,100,100)'[v]",
		"-map", "[v]",
		"-c:v", "libx265", "-crf", "18", "-preset", "veryfast", "-x265-params", "log-level=error",
		"-pix_fmt", "yuv420p10le", broken)

	frames, pooled := scoreWithFrames(t, bin, broken, ref)
	if len(frames) < 200 {
		t.Fatalf("expected ~240 measured frames, got %d", len(frames))
	}
	p1 := percentile(frames, 1)

	const minVmaf, minPool = 95.0, 60.0 // the shipped defaults
	t.Logf("harmonic_mean=%.2f  p1=%.2f  raw_min=%.2f  (n=%d)", pooled.HarmonicMean, p1, pooled.Min, len(frames))

	// 1. The mean is blind: this is the blind spot TRANSCODE-11 exists to close.
	if pooled.HarmonicMean < minVmaf {
		t.Errorf("harmonic_mean=%.2f < %.0f — the fixture no longer evades the mean gate, so it "+
			"proves nothing about pooling; re-tune the damage", pooled.HarmonicMean, minVmaf)
	}
	// 2. The percentile is ALSO blind — the finding that rejected it as the statistic.
	if p1 < minVmaf {
		t.Errorf("1st-percentile=%.2f < %.0f — a percentile floor WOULD have caught this. That "+
			"contradicts the documented basis for shipping the raw min; re-examine the choice",
			p1, minVmaf)
	}
	// 3. Only the raw min catches it.
	if pooled.Min >= minPool {
		t.Errorf("raw min=%.2f >= floor %.0f — the shipped floor does NOT catch a destroyed frame; "+
			"the gate has a hole", pooled.Min, minPool)
	}
}

// scoreWithFrames runs libvmaf and returns the PER-FRAME vmaf scores alongside the
// pooled Result. Per-frame data is test-only: the shipped gate needs just the pooled
// min (see Result), and this exists solely to prove that choice was the right one.
func scoreWithFrames(t *testing.T, ffmpeg, distorted, reference string) ([]float64, Result) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "vmaf.json")
	filter := "[0:v][1:v]libvmaf=model=version=vmaf_v0.6.1:log_fmt=json:log_path=" +
		escapeFilterValue(logPath)
	mustFF(t, ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-i", distorted, "-i", reference, "-lavfi", filter, "-f", "null", "-")

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read vmaf log: %v", err)
	}
	var parsed struct {
		Frames []struct {
			Metrics struct {
				VMAF float64 `json:"vmaf"`
			} `json:"metrics"`
		} `json:"frames"`
		PooledMetrics struct {
			VMAF struct {
				Min          float64 `json:"min"`
				HarmonicMean float64 `json:"harmonic_mean"`
			} `json:"vmaf"`
		} `json:"pooled_metrics"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse vmaf log: %v", err)
	}
	scores := make([]float64, len(parsed.Frames))
	for i, f := range parsed.Frames {
		scores[i] = f.Metrics.VMAF
	}
	return scores, Result{
		HarmonicMean: parsed.PooledMetrics.VMAF.HarmonicMean,
		Min:          parsed.PooledMetrics.VMAF.Min,
	}
}

// percentile returns the q-th percentile (0-100) of scores by nearest-rank, the
// statistic a "1st-percentile floor" would gate on.
func percentile(scores []float64, q float64) float64 {
	s := append([]float64(nil), scores...)
	sort.Float64s(s)
	rank := int(math.Ceil(q/100*float64(len(s)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(s) {
		rank = len(s) - 1
	}
	return s[rank]
}

// stubFfmpegWritingLog builds a fake ffmpeg that ignores the media entirely and
// writes `body` to whatever log_path the caller embedded in the -lavfi filtergraph.
// It lets the pooled-log PARSER be tested against libvmaf output we could not
// otherwise provoke (an old/odd build that omits a pooled statistic).
func stubFfmpegWritingLog(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub is POSIX")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "ffmpeg")
	payload := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(payload, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pull log_path=... out of the filtergraph arg and copy the payload there.
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  case \"$a\" in *log_path=*)\n" +
		"    p=$(printf '%s' \"$a\" | sed -n 's/.*log_path=\\([^:]*\\).*/\\1/p')\n" +
		"    cat " + payload + " > \"$p\"\n" +
		"  ;; esac\n" +
		"done\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub
}

// TestScore_IncompleteLogIsRejected is the fail-closed proof for TRANSCODE-11.
//
// The worst-frame floor is enforced as `Min < vmaf_min_pool`. If libvmaf ever hands
// back a log WITHOUT a `min` pool, a plain unmarshal yields 0.0 — a number the gate
// cannot distinguish from a real measurement. Score must refuse instead. An
// unmeasured worst frame is an UNKNOWN, and this tool treats an unknown as a
// rejection, never as a silent fall-back to the mean-only gate it just replaced.
func TestScore_IncompleteLogIsRejected(t *testing.T) {
	cases := []struct {
		name string
		log  string
	}{
		{"min absent", `{"pooled_metrics":{"vmaf":{"harmonic_mean":99.1}}}`},
		{"harmonic_mean absent", `{"pooled_metrics":{"vmaf":{"min":98.0}}}`},
		{"vmaf section absent", `{"pooled_metrics":{}}`},
		{"empty object", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := stubFfmpegWritingLog(t, tc.log)
			res, err := Score(context.Background(), bin, "d.mkv", "r.mkv", 1, "version=vmaf_v0.6.1")
			if err == nil {
				t.Fatalf("Score accepted an incomplete libvmaf log (got %+v) — an unmeasured "+
					"worst frame must be a REJECTION, not a zero value the gate reads as real", res)
			}
			if !strings.Contains(err.Error(), "missing a pooled statistic") {
				t.Errorf("error should name the incomplete log; got: %v", err)
			}
		})
	}
}

// A complete log still parses — the fail-closed check must not reject a real
// measurement, including a legitimate 0.0 (which is a score, not an absence).
func TestScore_CompleteLogParses(t *testing.T) {
	bin := stubFfmpegWritingLog(t, `{"pooled_metrics":{"vmaf":{"min":0.0,"harmonic_mean":0.0}}}`)
	res, err := Score(context.Background(), bin, "d.mkv", "r.mkv", 1, "version=vmaf_v0.6.1")
	if err != nil {
		t.Fatalf("a complete log with genuine 0.0 scores must parse, not error: %v", err)
	}
	if res.Min != 0 || res.HarmonicMean != 0 {
		t.Errorf("got %+v, want zeroed Result", res)
	}
}

func TestEscapeFilterValue(t *testing.T) {
	cases := map[string]string{
		"/tmp/a.json":  "/tmp/a.json",
		"/t:mp/a.json": `/t\:mp/a.json`,
		`/a\b`:         `/a\\b`,
		"/a'b[1]":      `/a\'b\[1\]`,
	}
	for in, want := range cases {
		if got := escapeFilterValue(in); got != want {
			t.Errorf("escapeFilterValue(%q) = %q, want %q", in, got, want)
		}
	}
}
