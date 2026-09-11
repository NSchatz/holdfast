package vmaf

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func ffmpegBin() string {
	if v := os.Getenv("HOLDFAST_FFMPEG"); v != "" {
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

	g, err := Score(context.Background(), bin, req(good, ref))
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
	b, err := Score(context.Background(), bin, req(bad, ref))
	if err != nil {
		t.Fatalf("Score(bad): %v", err)
	}
	if b.HarmonicMean >= g.HarmonicMean {
		t.Errorf("degraded (%.2f) should score below faithful (%.2f)", b.HarmonicMean, g.HarmonicMean)
	}
	// The chroma planes are measured on every pass now (GATE-4), so a faithful encode
	// must report a real chroma figure and a degraded one must report a worse figure.
	// Chroma is a MEASUREMENT, not a flag: if it ever came back constant, the floor
	// built on it would be decoration.
	if g.ChromaMin <= 0 || g.ChromaMin > 100 {
		t.Errorf("chroma PSNR %v dB out of a plausible range - psnr_cb/psnr_cr mapping likely wrong", g.ChromaMin)
	}
	if b.ChromaMin >= g.ChromaMin {
		t.Errorf("degraded chroma (%.2f dB) should measure below faithful (%.2f dB)", b.ChromaMin, g.ChromaMin)
	}
	if g.ChromaMetric != ChromaMetricName {
		t.Errorf("ChromaMetric = %q, want %q - the metric must travel with its value", g.ChromaMetric, ChromaMetricName)
	}
}

// TestBuildFilter_SelectsThePrimaryVideoStream is the proof that the gate measures the
// SAME video stream everything else in the program inspects. It makes two different
// claims with two different instruments, and it is explicit about which is which,
// because getting that backwards is how a guard comes to be believed for a reason that
// is not true.
//
// Every ffprobe property read selects `v:0` and the decode-integrity check decodes
// `0:v:0`. The filtergraph used to name its inputs with a BARE type label (`[0:v]`,
// `[1:v]`), a convention nothing else here uses - so nothing in holdfast pinned the
// measurement to the stream the guards looked at, and the recorded proof could not say
// which one it was.
//
// CLAIM 1, the regression guard: the built graph names the first video stream
// explicitly, through ScoredStream. What reds this test against the pinned tree's graph
// is the CONSTANT check and the graph-TEXT check below, NOT the measurement. That is a
// measured fact, not a shortcut: on this ffmpeg a bare `v` label resolves to the FIRST
// video stream, identically to `v:0`, so `[0:v]` and `[0:v:0]` select the same pixels
// and NO fixture can make a measurement tell them apart. Each subtest measures the bare
// label beside the two explicit ones and reports what it resolved to, so that statement
// is observed here rather than asserted from prose. The hazard the change closes is
// therefore a CONTRACT one, and this is the honest statement of it: which stream a bare
// type label resolves to is ffmpeg's rule to change, not holdfast's to depend on, and
// this gate deletes an original on the strength of what it measured.
//
// CLAIM 2, what the measurement does carry: that the shipped scoring pass really
// returns the FIRST video-stream pair's statistics on a file carrying more than one
// video stream - not a pair chosen by the fixture's content, and not one the test read
// off the graph the package produced. The fixture carries two video-stream pairs that
// score ~77 VMAF points apart, so the returned statistics alone say which pair was
// compared, and the ground truth for each pair is measured through graphs written HERE
// rather than by the package under test. It runs TWICE over MIRRORED fixtures, one whose
// first pair is the faithful one and one whose first pair is the destroyed one, so a
// ScoredStream naming any other stream reds BOTH cases in opposite directions and no
// "pick the better-looking pair" satisfies either.
//
// Claim 2 is also why the text check is not left on its own: it would pass on a label
// real ffmpeg rejects or binds somewhere else, and this package's rule is that a
// filtergraph is only correct if ffmpeg agrees. The measurement is ffmpeg agreeing -
// about the spelling that ships, which is the one that has to be right.
func TestBuildFilter_SelectsThePrimaryVideoStream(t *testing.T) {
	bin := ffmpegBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: ffmpeg required for the stream-selection proof: %v", err)
	}
	// CLAIM 1. The two conventions this criterion exists to pin together. The graph the
	// gate runs must name the SAME stream the probes read and the decode check decodes,
	// and it must name it EXPLICITLY rather than leave a bare type specifier to be
	// resolved by rules that are ffmpeg's and not holdfast's. These two checks are what
	// red against the pinned tree's graph; the measurement below cannot, and says so.
	if ScoredStream != "v:0" {
		t.Errorf("ScoredStream = %q, want the probes' own vocabulary %q", ScoredStream, "v:0")
	}
	graph := BuildFilter(req("d.mkv", "r.mkv"), "/tmp/x.json")
	for _, want := range []string{"[0:" + ScoredStream + "]", "[1:" + ScoredStream + "]"} {
		if !strings.Contains(graph, want) {
			t.Errorf("the built filtergraph does not name the first video stream as %s, so nothing pins "+
				"the comparison to the stream the probes read: %s", want, graph)
		}
	}

	for _, tc := range []struct {
		name string
		// damagedStream is the video stream index the distorted file destroys.
		damagedStream int
	}{
		{"the first video-stream pair is the faithful one", 1},
		{"the first video-stream pair is the destroyed one", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ref := filepath.Join(dir, "ref.mkv")
			dist := filepath.Join(dir, "dist.mkv")
			buildTwoVideoStreamPair(t, bin, ref, dist, tc.damagedStream)

			first := pairHarmonicMean(t, bin, dist, ref, "v:0")
			second := pairHarmonicMean(t, bin, dist, ref, "v:1")
			t.Logf("ground truth: first-stream pair harmonic_mean=%.2f  second-stream pair harmonic_mean=%.2f",
				first, second)
			const gap = 30.0
			if math.Abs(first-second) < gap {
				t.Fatalf("the fixture's two video-stream pairs are not far enough apart to identify which "+
					"was scored (first=%.2f second=%.2f, want a gap of at least %.0f) - re-tune the damage, "+
					"because nothing below decides anything until they differ", first, second, gap)
			}

			// What the bare type label the pinned tree used actually resolves to, measured
			// on THIS fixture rather than taken on trust. It is a measurement of the
			// INSTRUMENT, not an assertion about this package: no value of it makes the
			// shipped graph wrong, so it never fails the test - it records which of the two
			// statements in this test's doc comment is true of the ffmpeg in front of it.
			bare, bareErr := pairHarmonicMeanErr(bin, dist, ref, "v", dir)
			switch {
			case bareErr != nil:
				t.Logf("instrument: the bare `v` label REFUSED this graph (%v) - the old spelling is "+
					"observably broken on this build, and a measurement CAN tell the two graphs apart", bareErr)
			case math.Abs(bare-first) < math.Abs(bare-second):
				t.Logf("instrument: the bare `v` label measured %.2f, the SAME pair `v:0` names (%.2f, "+
					"against %.2f for the second) - so [0:v] and [0:v:0] select identical pixels here and "+
					"NO fixture makes this measurement red against the old graph. The constant check and "+
					"the graph-text check above are what guard this regression", bare, first, second)
			default:
				t.Logf("instrument: the bare `v` label measured %.2f, which is the SECOND pair (%.2f), not "+
					"the first (%.2f) - this build resolves a bare label to a stream the probes never read, "+
					"so the hazard this test exists for is LIVE and the doc comment above is stale",
					bare, second, first)
			}

			res, err := Score(context.Background(), bin, req(dist, ref))
			if err != nil {
				t.Fatalf("Score over a two-video-stream pair: %v", err)
			}
			// Tolerance, not equality: the shipped pass names the comparison format and
			// asks libvmaf for the chroma feature, so its numbers are its own measurement
			// of the same pair rather than a replay of the ground-truth run.
			const tol = 2.0
			if math.Abs(res.HarmonicMean-first) > tol {
				t.Errorf("Score measured harmonic_mean=%.2f, which is not the FIRST video-stream pair "+
					"(%.2f, tolerance %.1f); the second pair measures %.2f. The gate scored a stream the "+
					"probes and the decode-integrity check never inspected",
					res.HarmonicMean, first, tol, second)
			}
			if res.Stream != ScoredStream {
				t.Errorf("Score recorded stream %q, want %q - the stream the comparison was made against "+
					"travels with the score", res.Stream, ScoredStream)
			}
		})
	}
}

// buildTwoVideoStreamPair writes a reference and a distorted file that each carry TWO
// video streams of the same picture, with the distorted file's stream `damaged` destroyed
// and its other stream faithfully encoded. The two PAIRS therefore score far apart and
// which one a measurement returns is readable from the statistics alone.
func buildTwoVideoStreamPair(t *testing.T, bin, ref, dist string, damaged int) {
	t.Helper()
	src := filepath.Join(filepath.Dir(ref), "src.mkv")
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M", "-pix_fmt", "yuv420p", src)

	// The reference carries the SAME picture twice, so neither pair is favoured by its
	// content and the only difference between them is what the distorted file did to it.
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-i", src,
		"-filter_complex", "[0:v:0]split=2[a][b]", "-map", "[a]", "-map", "[b]",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M", "-pix_fmt", "yuv420p", ref)

	// One of the distorted file's two video streams is destroyed by a round trip through
	// 40x30, the other is a faithful encode. Which one is the caller's choice, so the
	// same assertion can be made of a file whose FIRST stream is the good one and of a
	// file whose first stream is the bad one.
	firstStream, secondStream := "[a]", "[c]" // faithful first, destroyed second
	if damaged == 0 {
		firstStream, secondStream = "[c]", "[a]"
	}
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-i", src,
		"-filter_complex", "[0:v:0]split=2[a][b];[b]scale=40:30,scale=320:240:flags=neighbor[c]",
		"-map", firstStream, "-map", secondStream,
		"-c:v", "libx265", "-crf", "20", "-preset", "veryfast", "-x265-params", "log-level=error",
		"-pix_fmt", "yuv420p10le", dist)
}

// pairHarmonicMean measures ONE named video-stream pair through real libvmaf, with a
// graph written in the test rather than by the package under test. It is the ground
// truth the shipped pass is compared against, so the proof is not this package agreeing
// with itself.
func pairHarmonicMean(t *testing.T, bin, distorted, reference, spec string) float64 {
	t.Helper()
	got, err := pairHarmonicMeanErr(bin, distorted, reference, spec, t.TempDir())
	if err != nil {
		t.Fatalf("measuring the %s pair: %v", spec, err)
	}
	return got
}

// pairHarmonicMeanErr is pairHarmonicMean with the failure RETURNED rather than fatal,
// so a caller can measure a specifier that this build may legitimately refuse (the bare
// type label) without turning ffmpeg's opinion of it into a red test.
func pairHarmonicMeanErr(bin, distorted, reference, spec, dir string) (float64, error) {
	logPath := filepath.Join(dir, "pair-"+strings.ReplaceAll(spec, ":", "-")+".json")
	filter := fmt.Sprintf(
		"[0:%s]format=yuv420p10le[d];[1:%s]format=yuv420p10le[r];"+
			"[d][r]libvmaf=model=version=vmaf_v0.6.1:log_fmt=json:log_path=%s",
		spec, spec, escapeFilterValue(logPath))
	out, err := exec.Command(bin, "-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-i", distorted, "-i", reference, "-lavfi", filter, "-f", "null", "-").CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("ffmpeg refused the %s graph: %v: %s", spec, err, truncate(strings.TrimSpace(string(out)), 200))
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		return 0, fmt.Errorf("read the %s pair's vmaf log: %w", spec, err)
	}
	var parsed vmafLog
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0, fmt.Errorf("parse the %s pair's vmaf log: %w", spec, err)
	}
	if parsed.PooledMetrics.VMAF.HarmonicMean == nil {
		return 0, fmt.Errorf("the %s pair's vmaf log carries no pooled harmonic_mean", spec)
	}
	return *parsed.PooledMetrics.VMAF.HarmonicMean, nil
}

// TestScore_ExecutesExactlyTheBuiltFilter is AC2's observable: the -lavfi argument the
// scoring pass hands ffmpeg is character-for-character what the exported builder returns
// for that same request. A second place composing a video-stream input specifier - a
// copy of the graph inside Score, a hand-written one in a caller - would be a second
// answer to "which stream was measured", and the recorded proof would name the builder's.
func TestScore_ExecutesExactlyTheBuiltFilter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub is POSIX")
	}
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	bin := stubFfmpegRecordingArgv(t, dir, argv,
		`{"pooled_metrics":{"vmaf":{"min":98.0,"harmonic_mean":99.1},`+
			`"psnr_cb":{"min":41.0},"psnr_cr":{"min":40.0}}}`)

	r := req("d.mkv", "r.mkv")
	if _, err := Score(context.Background(), bin, r); err != nil {
		t.Fatalf("Score against the recording stub: %v", err)
	}
	got := lavfiArg(t, argv)
	// The log path is a temp Score creates and removes, so the builder is re-run against
	// the path the recorded invocation actually carried; everything else in the string
	// has to match exactly.
	want := BuildFilter(r, logPathIn(t, got))
	if got != want {
		t.Errorf("the scoring pass executed a filtergraph the builder did not produce.\n got: %s\nwant: %s", got, want)
	}
}

// streamLabelAllowMarker exempts a line that NAMES a video-stream input label without
// composing one - a comment documenting the graph's shape, or a rule prohibiting the
// bare form. It is the rename guard's idiom (scripts/check-pins.sh `rename-guard-allow`)
// and it is line-level for the same reason: a file-level exemption is one accidental
// path match away from exempting everything. Exactly one shipped line carries it.
const streamLabelAllowMarker = "stream-guard-allow"

// videoStreamLabel matches a filtergraph INPUT LABEL naming a video stream literally -
// `[0:v]`, `[1:v]`, `[0:v:0]`, `[1:v:1]`. Composed from ScoredStream, a graph never
// contains one of these as source text, which is what makes a literal hit a second
// place answering "which video stream was measured".
var videoStreamLabel = regexp.MustCompile(`\[[0-9]+:v(:[0-9]+)?]`)

// videoStreamLabelsInShippedSource lists every line of SHIPPED (non-test) Go source under root that
// spells a video-stream input label literally instead of composing it from ScoredStream,
// skipping any line carrying streamLabelAllowMarker.
func videoStreamLabelsInShippedSource(root string) ([]string, error) {
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if !videoStreamLabel.MatchString(line) || strings.Contains(line, streamLabelAllowMarker) {
				continue
			}
			offenders = append(offenders,
				fmt.Sprintf("%s:%d: %s", filepath.ToSlash(path), i+1, strings.TrimSpace(line)))
		}
		return nil
	})
	return offenders, err
}

// TestShippedCode_SpellsEveryVideoStreamLabelThroughScoredStream is AC2's absolute half:
// "with no second place in the program composing a VMAF video-stream input specifier".
//
// TestScore_ExecutesExactlyTheBuiltFilter proves the SCORING pass runs the builder's
// graph. It cannot see a different shipped graph elsewhere - the startup model preflight
// once hand-wrote `[0:v][1:v]`, drove synthetic single-stream inputs, mis-scored nothing,
// and was still a second answer to which stream a libvmaf graph looks at: the one a later
// reader copies into a path that does score. This walks the whole module instead, so the
// single-spelling property is checked where it is actually claimed rather than at the one
// call site a test happened to drive.
func TestShippedCode_SpellsEveryVideoStreamLabelThroughScoredStream(t *testing.T) {
	offenders, err := videoStreamLabelsInShippedSource(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("shipped code spells a video-stream input label literally instead of composing it "+
			"from ScoredStream (= %q), so the program carries a second answer to \"which stream was "+
			"measured\" while the ledger row carries the builder's:\n  %s\n\nCompose it from "+
			"ScoredStream, or - if the line only NAMES the label to document or prohibit it - mark "+
			"that line %q.", ScoredStream, strings.Join(offenders, "\n  "), streamLabelAllowMarker)
	}
}

// TestVideoStreamLabelScan_BitesAndKnowsWhatToIgnore is the guard above tried against a
// tree built to defeat it. A walk that reports nothing is indistinguishable from a walk
// that found nothing, and this repo does not accept that difference on trust anywhere
// else (check-pins, install-ffmpeg and govulncheck each ship a selftest that proves the
// guard still bites).
func TestVideoStreamLabelScan_BitesAndKnowsWhatToIgnore(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Four kinds of line, one per rule the guard has to get right.
	write("internal/bad/bare.go", "package bad\n\nvar g = \"[0:v][1:v]libvmaf=model=x\"\n")
	write("internal/bad/explicit.go", "package bad\n\nvar g = \"[0:v:0]format=yuv420p[d]\"\n")
	write("internal/ok/composed.go", "package ok\n\nconst s = \"v:0\"\n\nvar g = \"[0:\" + s + \"]format=x\"\n")
	write("internal/ok/documented.go", "package ok\n\n// [0:v] is the bare form this refuses - "+
		streamLabelAllowMarker+"\nvar x = 1\n")
	write("internal/ok/scan_test.go", "package ok\n\nvar g = \"[0:v][1:v]\"\n")
	write("node_modules/vendored/x.go", "package vendored\n\nvar g = \"[0:v]\"\n")

	offenders, err := videoStreamLabelsInShippedSource(root)
	if err != nil {
		t.Fatalf("walking the synthetic tree: %v", err)
	}
	joined := strings.Join(offenders, "\n")
	for _, want := range []string{"internal/bad/bare.go:3", "internal/bad/explicit.go:3"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the scan did not report %s - it does not bite, so its green says nothing.\nreported:\n%s",
				want, joined)
		}
	}
	for _, never := range []string{"composed.go", "documented.go", "scan_test.go", "node_modules"} {
		if strings.Contains(joined, never) {
			t.Errorf("the scan reported %s, which it must not: composed labels, marked lines, test "+
				"files and vendored trees are not second answers.\nreported:\n%s", never, joined)
		}
	}
}

// stubFfmpegRecordingArgv is stubFfmpegWritingLog plus a record of the argv it was
// invoked with, one argument per line, so a test can read back exactly what the scoring
// pass handed ffmpeg.
func stubFfmpegRecordingArgv(t *testing.T, dir, argvPath, body string) string {
	t.Helper()
	stub := filepath.Join(dir, "ffmpeg")
	payload := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(payload, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		": > " + argvPath + "\n" +
		"for a in \"$@\"; do\n" +
		"  printf '%s\\n' \"$a\" >> " + argvPath + "\n" +
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

// lavfiArg returns the value the recorded invocation passed to -lavfi.
func lavfiArg(t *testing.T, argvPath string) string {
	t.Helper()
	raw, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("the stub recorded no argv: %v", err)
	}
	args := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	for i, a := range args {
		if a == "-lavfi" && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("the scoring pass passed no -lavfi argument: %v", args)
	return ""
}

// logPathIn recovers the log path Score embedded in the graph it executed, un-escaping
// the filtergraph escaping BuildFilter applied on the way in.
func logPathIn(t *testing.T, graph string) string {
	t.Helper()
	const key = "log_path="
	i := strings.Index(graph, key)
	if i < 0 {
		t.Fatalf("the executed filtergraph embeds no log_path: %s", graph)
	}
	rest := graph[i+len(key):]
	var b strings.Builder
	for j := 0; j < len(rest); j++ {
		if rest[j] == '\\' && j+1 < len(rest) {
			j++
			b.WriteByte(rest[j])
			continue
		}
		if rest[j] == ':' {
			break
		}
		b.WriteByte(rest[j])
	}
	return b.String()
}

// req is the scoring request the older tests used implicitly: every frame, the HD
// model, and an explicitly named comparison format (which Score now requires).
func req(distorted, reference string) Request {
	return Request{
		Distorted: distorted, Reference: reference,
		Subsample: 1, Model: "version=vmaf_v0.6.1", PixelFormat: "yuv420p10le",
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
	filter := "[0:" + ScoredStream + "][1:" + ScoredStream + "]" +
		"libvmaf=model=version=vmaf_v0.6.1:log_fmt=json:log_path=" +
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

// TestScore_IncompleteLogIsRejected is the fail-closed proof for TRANSCODE-11, and
// since GATE-4 for the chroma planes too.
//
// The worst-frame floor is enforced as `Min < vmaf_min_pool` and the chroma floor as
// `ChromaMin < vmaf_min_chroma`. If libvmaf ever hands back a log WITHOUT one of
// those pools, a plain unmarshal yields 0.0 - a number the gate cannot distinguish
// from a real measurement. Score must refuse instead, and it must refuse on ANY
// missing statistic rather than score the output on the ones that did report: an
// output graded on two of three metrics is an output whose third property nobody
// measured, and this tool does not delete an original on that.
func TestScore_IncompleteLogIsRejected(t *testing.T) {
	const full = `{"pooled_metrics":{"vmaf":{"min":98.0,"harmonic_mean":99.1},` +
		`"psnr_cb":{"min":41.0},"psnr_cr":{"min":40.0}}}`
	cases := []struct {
		name string
		log  string
	}{
		{"min absent", `{"pooled_metrics":{"vmaf":{"harmonic_mean":99.1},"psnr_cb":{"min":41.0},"psnr_cr":{"min":40.0}}}`},
		{"harmonic_mean absent", `{"pooled_metrics":{"vmaf":{"min":98.0},"psnr_cb":{"min":41.0},"psnr_cr":{"min":40.0}}}`},
		{"vmaf section absent", `{"pooled_metrics":{"psnr_cb":{"min":41.0},"psnr_cr":{"min":40.0}}}`},
		// The GATE-4 half: the LUMA statistics are complete and only the chroma
		// planes are missing. This is the case that would silently restore the
		// pre-GATE-4 gate - a full-looking pass on a metric nobody measured.
		{"psnr_cb absent", `{"pooled_metrics":{"vmaf":{"min":98.0,"harmonic_mean":99.1},"psnr_cr":{"min":40.0}}}`},
		{"psnr_cr absent", `{"pooled_metrics":{"vmaf":{"min":98.0,"harmonic_mean":99.1},"psnr_cb":{"min":41.0}}}`},
		{"both chroma planes absent", `{"pooled_metrics":{"vmaf":{"min":98.0,"harmonic_mean":99.1}}}`},
		{"empty object", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Anti-vacuity: the SAME stub with a complete log must be accepted, so a
			// rejection below is the missing statistic and not the stub itself.
			if _, err := Score(context.Background(), stubFfmpegWritingLog(t, full), req("d.mkv", "r.mkv")); err != nil {
				t.Fatalf("the complete-log control failed (%v) - this case proves nothing", err)
			}
			bin := stubFfmpegWritingLog(t, tc.log)
			res, err := Score(context.Background(), bin, req("d.mkv", "r.mkv"))
			if err == nil {
				t.Fatalf("Score accepted an incomplete libvmaf log (got %+v) — an unmeasured "+
					"metric must be a REJECTION, not a zero value the gate reads as real", res)
			}
			if !strings.Contains(err.Error(), "missing a pooled statistic") {
				t.Errorf("error should name the incomplete log; got: %v", err)
			}
		})
	}
}

// A complete log still parses — the fail-closed check must not reject a real
// measurement, including a legitimate 0.0 (which is a score, not an absence). 0.0 dB
// of chroma PSNR is a plane that has been obliterated, which is the single most
// important thing this field could ever report; reading it as "absent" would drop it.
func TestScore_CompleteLogParses(t *testing.T) {
	bin := stubFfmpegWritingLog(t, `{"pooled_metrics":{"vmaf":{"min":0.0,"harmonic_mean":0.0},`+
		`"psnr_cb":{"min":0.0},"psnr_cr":{"min":0.0}}}`)
	res, err := Score(context.Background(), bin, req("d.mkv", "r.mkv"))
	if err != nil {
		t.Fatalf("a complete log with genuine 0.0 scores must parse, not error: %v", err)
	}
	if res.Min != 0 || res.HarmonicMean != 0 || res.ChromaMin != 0 {
		t.Errorf("got %+v, want zeroed scores", res)
	}
	if res.PixelFormat != "yuv420p10le" || res.ChromaMetric != ChromaMetricName {
		t.Errorf("got %+v, want the named format and metric carried back with the score", res)
	}
}

// Score refuses a request with no named comparison format. There is deliberately no
// fallback: "let libavfilter negotiate it" is the behaviour GATE-4 removed, and a
// default here would quietly reinstate it for any caller that forgot the field.
func TestScore_RefusesAnUnnamedComparisonFormat(t *testing.T) {
	bin := stubFfmpegWritingLog(t, `{"pooled_metrics":{"vmaf":{"min":98.0,"harmonic_mean":99.1},`+
		`"psnr_cb":{"min":41.0},"psnr_cr":{"min":40.0}}}`)
	r := req("d.mkv", "r.mkv")
	r.PixelFormat = ""
	if res, err := Score(context.Background(), bin, r); err == nil {
		t.Fatalf("Score accepted a request with no comparison format (got %+v)", res)
	} else if !strings.Contains(err.Error(), "comparison pixel format") {
		t.Errorf("error should name the missing comparison format; got: %v", err)
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
