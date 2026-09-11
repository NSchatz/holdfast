// Package vmaf runs libvmaf (via ffmpeg) to measure the perceptual quality of a
// transcoded output against its source — the last no-loss layer (TRANSCODE-4). The
// structural gates (codec/parity/size/stream-count/decode-integrity) prove an output
// exists, decodes, and carries the tracks; VMAF ESTIMATES whether it still looks
// like the source. A codec-only transcode keeps the resolution identical, so VMAF
// applies with no scaling.
//
// Three pooled statistics come back, and the gate needs ALL of them (see Result):
// the harmonic mean bounds average LUMA quality, the min bounds LOCAL luma quality,
// and the chroma min bounds damage in the colour planes that the VMAF model itself
// is structurally blind to. A mean alone is not a gate - it averages a destroyed
// segment away - and luma alone is not a gate either, because a transcode can leave
// every luma sample intact and still ruin the colour.
//
// Every comparison is made in ONE pixel format that holdfast NAMES (see
// ComparisonFormat) and reports back with the score. Before GATE-4 the two inputs
// could disagree - `pixel_format: auto` floors output depth at 10, so an 8-bit
// source routinely meets a 10-bit output - and libavfilter negotiated the conversion
// unobserved. Upconverting the reference and downconverting the distorted stream are
// not the same measurement, so a score whose format was never recorded is a number
// whose meaning nobody can state afterwards.
//
// libvmaf's filter takes the DISTORTED stream as its first input and the REFERENCE
// as its second — getting this backwards inverts the meaning, so it is fixed here.
package vmaf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
)

// Result is the pooled VMAF over the (sub)sampled frames.
//
// What a VMAF score does and does not license: VMAF is a regression onto a
// SUBJECTIVE opinion scale (ACR), not a measure of signal identity. 100 is a
// label-normalisation anchor, not "identical to the source" — a bit-identical file
// is not guaranteed to score it. So a high score means "no worse than X against
// your source, under this model and viewing condition", and nothing stronger. It is
// not a proof of fidelity, and the widely-repeated "~6 VMAF points = 1 JND" figure
// is practitioner folklore with no primary source — do not repeat it.
//
// The default model (vmaf_v0.6.1) extracts LUMA FEATURES ONLY: it is structurally
// blind to chroma damage, and Netflix names it weak on banding and dark-region
// blockiness, and prone to OVER-predicting quality on high-motion scenes — the
// dangerous direction for a pass/fail gate. That blindness is why every Score also
// measures the chroma planes directly (see ChromaMin): before GATE-4 chroma damage
// was caught only by the structural checks, which is to say only when it also broke
// the decode.
type Result struct {
	// HarmonicMean is the pooled mean over the (sub)sampled frames. It is an
	// AVERAGE, and Netflix documents that mean pooling "has the risk of hiding poor
	// quality frames" — the harmonic mean is a weak correction, not a fix. It must
	// never be the sole gate; see Min.
	HarmonicMean float64
	// Min is the worst single (sub)sampled frame — the statistic that bounds LOCAL
	// damage, which the mean averages away. A short destroyed segment inside an
	// otherwise-clean encode passes every structural check and the pooled mean;
	// only this catches it.
	Min float64

	// ChromaMin is the worst single (sub)sampled frame's PSNR over the CHROMA
	// planes, in dB - the WORSE of the two planes (Cb and Cr) on the worst frame
	// each of them had. It is the statistic the chroma floor is enforced against
	// (GATE-4).
	//
	// PSNR over Cb and Cr is the metric because it is literally computed over the
	// chroma planes and over nothing else, so a value that falls can only mean
	// chroma changed. CIEDE2000 (libvmaf's other colour-aware feature) mixes
	// lightness back in through Lab, so a low score there would not tell an operator
	// WHICH thing moved. The two planes are combined by taking the worse for the
	// same reason the gate takes the raw min over frames: damage confined to one
	// plane is still damage, and an average over the two would let a destroyed Cr
	// hide behind an intact Cb.
	//
	// It is the raw min over frames, matching Min and for the identical reason - a
	// mean over frames hides a locally-broken segment, and a segment small enough
	// to evade a mean is by construction a small fraction of frames.
	ChromaMin float64
	// ChromaMetric names what ChromaMin is a measurement of, e.g.
	// "psnr_cb/psnr_cr min (dB)". It travels WITH the value everywhere the value is
	// recorded: a bare number with no metric attached is not interpretable, and this
	// repo has already decided once (for VmafModel) that a score whose instrument is
	// unstated is a score nobody can act on.
	ChromaMetric string

	// PixelFormat is the single pixel format BOTH streams were converted to before
	// they were compared - named by holdfast (see ComparisonFormat), never left to
	// libavfilter's automatic negotiation. It is part of what was measured, not
	// decoration: an 8-bit source and a 10-bit output score differently depending on
	// which way the conversion went, so a score without this is a number whose
	// meaning depends on an undocumented negotiation.
	PixelFormat string

	// Stream names WHICH video stream of each file the comparison was made against, in
	// the specifier vocabulary every ffprobe read in this program already uses (see
	// ScoredStream). It is a fact about the measurement in exactly the sense PixelFormat
	// and ChromaMetric are: on a source carrying more than one video stream, a score that
	// does not say which stream it looked at is a score nobody can line up against the
	// guards that inspected the same file.
	//
	// "" is NOT RECORDED and never a fabricated default - a Result no scoring pass
	// produced names no stream, because no stream was scored.
	Stream string
}

// Request is one scoring pass: which files, at what sampling interval, under which
// model, in which named comparison format.
//
// It is a struct rather than a widening positional argument list because
// PixelFormat is not optional and must never be silently defaulted - a zero value
// here is a REFUSAL from Score, not "let ffmpeg decide", which is exactly the
// behaviour GATE-4 replaced.
type Request struct {
	// Distorted is the freshly-encoded output (libvmaf's first input).
	Distorted string
	// Reference is the source it must still look like (libvmaf's second input).
	Reference string
	// Subsample is the frame-sampling interval; <1 is floored to 1 (every frame).
	Subsample int
	// Model is the libvmaf model spec, already resolved (see ResolveModel).
	Model string
	// PixelFormat is the format both inputs are converted to before scoring. It is
	// REQUIRED: Score refuses an empty value rather than fall back to negotiation.
	PixelFormat string
}

// ErrUnavailable indicates the ffmpeg build has no libvmaf filter, so quality
// cannot be measured. When the VMAF gate is enabled the caller must treat this as a
// rejection — never accept an unmeasured encode.
var ErrUnavailable = errors.New("libvmaf is not available in this ffmpeg build")

// Available reports whether the ffmpeg build exposes the libvmaf filter.
func Available(ctx context.Context, ffmpeg string) bool {
	out, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-filters").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		// filter listing columns: " .. libvmaf  VV->V  Calculate the VMAF ..."
		if fields := strings.Fields(line); len(fields) >= 2 && fields[1] == "libvmaf" {
			return true
		}
	}
	return false
}

// vmafLog is the subset of libvmaf's JSON log we consume. EVERY pooled statistic is
// a POINTER so that "libvmaf did not emit this field" is distinguishable from
// "libvmaf measured 0.0". That distinction is load-bearing: the worst-frame floor
// is enforced as `Min < vmaf_min_pool`, so an absent Min silently unmarshalling to
// 0.0 would either reject everything or — if the floor were ever read as optional —
// quietly degrade the gate back to mean-only. An unparseable or incomplete log is a
// REJECTION, never a fallback.
//
// The chroma planes (psnr_cb, psnr_cr) follow the identical discipline, and for a
// sharper reason: a PSNR of 0.0 dB is a plane that has been obliterated, so reading
// an absent field as 0 would REJECT everything, and reading it as "optional" would
// silently restore the pre-GATE-4 gate that could not see chroma at all. Absent is
// neither; absent is a refusal to score.
type vmafLog struct {
	PooledMetrics struct {
		VMAF struct {
			Min          *float64 `json:"min"`
			HarmonicMean *float64 `json:"harmonic_mean"`
		} `json:"vmaf"`
		PsnrCb struct {
			Min *float64 `json:"min"`
		} `json:"psnr_cb"`
		PsnrCr struct {
			Min *float64 `json:"min"`
		} `json:"psnr_cr"`
	} `json:"pooled_metrics"`
}

// ChromaMetricName is the stable token recorded beside a chroma measurement, naming
// what was measured and in what unit. It is a WIRE FORMAT - it lands in the ledger,
// the event and the API payload - so treat it the way the store's skip reasons are
// treated: a closed vocabulary, changed only with the readers in mind.
const ChromaMetricName = "psnr_cb/psnr_cr min (dB)"

// chromaFeature is the libvmaf `feature` option that produces psnr_y/psnr_cb/psnr_cr
// alongside the VMAF score. The FFmpeg filter documentation gives `feature` as a
// "|"-delimited list of name=... entries; one entry is all this gate needs.
const chromaFeature = "name=psnr"

// ScoredStream is the video stream every comparison is made against, and it is the ONE
// place in the program that spells it: BuildFilter composes both of the filtergraph's
// input labels from it, and it is the token recorded beside the score.
//
// It is `v:0` - the FIRST video stream - because that is the stream the rest of this
// program inspects. Every ffprobe property read selects `-select_streams v:0` and the
// decode-integrity check decodes `-map 0:v:0`, so a gate that named its inputs any other
// way could measure a stream no guard ever looked at on a file carrying more than one
// video stream, and the recorded proof would not say so.
//
// Like ChromaMetricName it is a WIRE FORMAT - it lands in the ledger, the API payload and
// the completion log - so treat it as a closed vocabulary, changed only with its readers
// in mind. It is deliberately NOT configurable: the probes fix their stream and so does
// this, for the same reason.
const ScoredStream = "v:0"

// BuildFilter returns the exact -lavfi filtergraph a Score pass runs, writing its
// JSON log to logPath. It is exported so a test can drive the REAL graph through
// ffmpeg and observe what libavfilter did with it, rather than assert against a
// string this package also produced - a filtergraph is only correct if ffmpeg agrees.
//
// It is the SINGLE writer of the graph's two video-stream input specifiers, and both
// come from ScoredStream. Nothing else in the program composes one: a second place that
// did would be a second answer to "which stream was measured", while the row the gate
// writes afterwards would carry this one.
//
// The two `format` filters are the whole point of GATE-4's first criterion: they
// convert BOTH inputs to one named format before libvmaf sees either, so the
// comparison happens in a format holdfast chose and recorded, not one libavfilter
// negotiated and nobody wrote down. They are also load-bearing in a way that is easy
// to lose: they must name a format libvmaf itself accepts, or ffmpeg silently
// auto-inserts a scaler BETWEEN them and libvmaf and the named format is not the one
// compared after all. ComparisonFormat only ever produces the planar-YUV 8/10/12-bit
// formats libvmaf takes directly, and TestScore_NamedFormatIsWhatLibvmafCompares
// proves ffmpeg inserts nothing.
func BuildFilter(req Request, logPath string) string {
	sub := req.Subsample
	if sub < 1 {
		sub = 1
	}
	// [0:v:0] = distorted (the freshly-encoded output), [1:v:0] = reference (the source).
	// Both name ScoredStream explicitly, so the comparison is pinned to the FIRST video
	// stream - the one every ffprobe read selects and the decode-integrity check decodes.
	// A bare type specifier would leave "which stream" to be resolved by rules that are
	// ffmpeg's rather than holdfast's, on a file carrying more than one video stream.
	//
	// log_path lives INSIDE the -lavfi filtergraph, where ':' separates option pairs,
	// so a path with a ':' (or other filtergraph metachar) must be escaped or ffmpeg
	// mis-parses the filter and the gate fails every encode. The media paths are safe
	// (separate -i argv); only the filter-embedded log_path needs escaping.
	return fmt.Sprintf(
		"[0:%s]format=%s[dist];[1:%s]format=%s[ref];"+
			"[dist][ref]libvmaf=model=%s:feature=%s:log_fmt=json:log_path=%s:n_subsample=%d",
		ScoredStream, req.PixelFormat, ScoredStream, req.PixelFormat,
		req.Model, chromaFeature, escapeFilterValue(logPath), sub)
}

// Score measures distorted against reference in the request's NAMED pixel format and
// returns the pooled luma statistics, the chroma statistic, and the format the
// comparison was actually made in. It returns ErrUnavailable (wrapped) if the ffmpeg
// build lacks libvmaf.
//
// Fail-closed twice over: an empty Request.PixelFormat is refused outright (there is
// no "let ffmpeg decide" path any more), and a log missing ANY of the four pooled
// statistics the gate is built on is refused rather than scored on the ones that did
// report.
func Score(ctx context.Context, ffmpeg string, req Request) (Result, error) {
	if req.PixelFormat == "" {
		return Result{}, fmt.Errorf("vmaf: no comparison pixel format was named - refusing to " +
			"score a pair whose comparison format would be chosen by filter negotiation and " +
			"recorded nowhere")
	}
	logf, err := os.CreateTemp("", "holdfast-vmaf-*.json")
	if err != nil {
		return Result{}, fmt.Errorf("vmaf: temp log: %w", err)
	}
	logPath := logf.Name()
	logf.Close()
	defer os.Remove(logPath)

	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-i", req.Distorted, "-i", req.Reference, "-lavfi", BuildFilter(req, logPath), "-f", "null", "-")
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := string(out)
		if strings.Contains(msg, "No such filter") || strings.Contains(msg, "libvmaf") && strings.Contains(msg, "not found") {
			return Result{}, ErrUnavailable
		}
		return Result{}, fmt.Errorf("vmaf: ffmpeg failed: %w: %s", err, truncate(msg, 300))
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		return Result{}, fmt.Errorf("vmaf: read log: %w", err)
	}
	var parsed vmafLog
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Result{}, fmt.Errorf("vmaf: parse log: %w", err)
	}
	// Fail CLOSED on an incomplete log. ANY pooled statistic missing means the
	// measurement did not actually produce the numbers the gate is built on, and the
	// caller must reject rather than proceed on a zero value it would misread as a
	// real score - or, worse, score the output on the metrics that DID report and
	// call the result a pass.
	p := parsed.PooledMetrics
	if p.VMAF.HarmonicMean == nil || p.VMAF.Min == nil || p.PsnrCb.Min == nil || p.PsnrCr.Min == nil {
		return Result{}, fmt.Errorf(
			"vmaf: log is missing a pooled statistic (harmonic_mean present=%t, min present=%t, "+
				"psnr_cb present=%t, psnr_cr present=%t) - refusing to accept an encode whose "+
				"quality was not fully measured, and refusing to score it on the metrics that did report",
			p.VMAF.HarmonicMean != nil, p.VMAF.Min != nil, p.PsnrCb.Min != nil, p.PsnrCr.Min != nil)
	}
	return Result{
		HarmonicMean: *p.VMAF.HarmonicMean,
		Min:          *p.VMAF.Min,
		ChromaMin:    math.Min(*p.PsnrCb.Min, *p.PsnrCr.Min),
		ChromaMetric: ChromaMetricName,
		PixelFormat:  req.PixelFormat,
		Stream:       ScoredStream,
	}, nil
}

// escapeFilterValue backslash-escapes the characters that are special inside an
// ffmpeg filtergraph option value, so a file path embedded in the graph (the VMAF
// log_path) parses literally regardless of what it contains. Backslash is escaped
// first (NewReplacer applies all rules in a single pass, so no double-escaping).
func escapeFilterValue(s string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		`:`, `\:`,
		`'`, `\'`,
		`[`, `\[`,
		`]`, `\]`,
		`,`, `\,`,
		`;`, `\;`,
	).Replace(s)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
