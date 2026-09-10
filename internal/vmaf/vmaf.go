// Package vmaf runs libvmaf (via ffmpeg) to measure the perceptual quality of a transcoded
// output against its source: the last no-loss layer. The structural gates prove an output
// exists, decodes and carries the tracks; VMAF ESTIMATES whether it still looks like the
// source. A codec-only transcode keeps the resolution identical, so it applies with no
// scaling.
//
// Three pooled statistics come back and the gate needs ALL of them (see Result): the
// harmonic mean bounds average LUMA quality, the min bounds LOCAL luma quality, and the
// chroma min bounds damage in the colour planes the VMAF model is structurally blind to. A
// mean alone averages a destroyed segment away, and luma alone misses a transcode that
// leaves every luma sample intact and still ruins the colour.
//
// Every comparison is made in ONE pixel format holdfast NAMES (see ComparisonFormat) and
// reports back with the score. Upconverting the reference and downconverting the distorted
// stream are not the same measurement, so a score whose format was never recorded is a
// number whose meaning nobody can state afterwards.
//
// libvmaf's filter takes the DISTORTED stream as its first input and the REFERENCE as its
// second; getting this backwards inverts the meaning, so it is fixed here.
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
// What a VMAF score does and does not license: it is a regression onto a SUBJECTIVE
// opinion scale (ACR), not a measure of signal identity. 100 is a label-normalisation
// anchor, not "identical to the source", and a bit-identical file is not guaranteed to
// score it. A high score means "no worse than X against your source, under this model and
// viewing condition" and nothing stronger; the widely-repeated "~6 VMAF points = 1 JND"
// figure is practitioner folklore with no primary source, so do not repeat it.
//
// The default model extracts LUMA FEATURES ONLY: structurally blind to chroma damage, weak
// on banding and dark-region blockiness by Netflix's own account, and prone to
// OVER-predicting quality on high-motion scenes, which is the dangerous direction for a
// pass/fail gate. That blindness is why every Score also measures the chroma planes
// directly (see ChromaMin).
type Result struct {
	// HarmonicMean is the pooled mean over the (sub)sampled frames. It is an AVERAGE, and
	// Netflix documents that mean pooling "has the risk of hiding poor quality frames";
	// the harmonic mean is a weak correction, not a fix, so it must never be the sole gate.
	HarmonicMean float64
	// Min is the worst single (sub)sampled frame, the statistic that bounds LOCAL damage.
	// A short destroyed segment inside an otherwise-clean encode passes every structural
	// check and the pooled mean; only this catches it.
	Min float64

	// ChromaMin is the worst single (sub)sampled frame's PSNR over the CHROMA planes, in
	// dB: the WORSE of Cb and Cr on the worst frame each of them had, and the statistic
	// the chroma floor is enforced against.
	//
	// PSNR over Cb and Cr is the metric because it is computed over the chroma planes and
	// nothing else, so a value that falls can only mean chroma changed. CIEDE2000
	// (libvmaf's other colour-aware feature) mixes lightness back in through Lab, so a low
	// score there would not tell an operator WHICH thing moved. The two planes are
	// combined by taking the worse, and it is the raw min over frames, for the reason Min
	// is: an average would let a destroyed Cr hide behind an intact Cb, and a mean over
	// frames hides a locally-broken segment.
	ChromaMin float64
	// ChromaMetric names what ChromaMin is a measurement of, and travels WITH the value
	// everywhere it is recorded: a bare number with no metric attached is not
	// interpretable.
	ChromaMetric string

	// PixelFormat is the single pixel format BOTH streams were converted to before they
	// were compared, named by holdfast and never left to libavfilter's negotiation. It is
	// part of what was measured: an 8-bit source and a 10-bit output score differently
	// depending on which way the conversion went.
	PixelFormat string
}

// Request is one scoring pass: which files, at what sampling interval, under which model,
// in which named comparison format. PixelFormat is not optional and must never be silently
// defaulted - a zero value is a REFUSAL from Score, never "let ffmpeg decide".
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

// ErrUnavailable indicates the ffmpeg build has no libvmaf filter, so quality cannot be
// measured. With the gate enabled the caller must treat it as a rejection: never accept an
// unmeasured encode.
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

// vmafLog is the subset of libvmaf's JSON log we consume. EVERY pooled statistic is a
// POINTER so that "libvmaf did not emit this field" stays distinguishable from "libvmaf
// measured 0.0". That distinction is load-bearing: the worst-frame floor is enforced as
// `Min < vmaf_min_pool`, so an absent Min unmarshalling to 0.0 would either reject
// everything or, read as optional, quietly degrade the gate back to mean-only. The chroma
// planes follow the same discipline for a sharper reason: 0.0 dB is an obliterated plane.
// An unparseable or incomplete log is a REJECTION, never a fallback.
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

// ChromaMetricName is the stable token recorded beside a chroma measurement, naming what
// was measured and in what unit. It is a WIRE FORMAT - it lands in the ledger, the event
// and the API payload - so it is a closed vocabulary, changed only with the readers in mind.
const ChromaMetricName = "psnr_cb/psnr_cr min (dB)"

// chromaFeature is the libvmaf `feature` option that produces psnr_y/psnr_cb/psnr_cr
// alongside the VMAF score. `feature` takes a "|"-delimited list; one entry is all this
// gate needs.
const chromaFeature = "name=psnr"

// BuildFilter returns the exact -lavfi filtergraph a Score pass runs, writing its JSON log
// to logPath. It is exported so a test can drive the REAL graph through ffmpeg and observe
// what libavfilter did with it: a filtergraph is only correct if ffmpeg agrees.
//
// The two `format` filters convert BOTH inputs to one named format before libvmaf sees
// either, so the comparison happens in a format holdfast chose and recorded. They must name
// a format libvmaf itself accepts, or ffmpeg silently auto-inserts a scaler BETWEEN them
// and libvmaf and the named format is not the one compared after all. ComparisonFormat only
// produces the planar-YUV 8/10/12-bit formats libvmaf takes directly, and
// TestScore_NamedFormatIsWhatLibvmafCompares proves ffmpeg inserts nothing.
func BuildFilter(req Request, logPath string) string {
	sub := req.Subsample
	if sub < 1 {
		sub = 1
	}
	// [0:v] = distorted, [1:v] = reference. log_path lives INSIDE the filtergraph, where
	// ':' separates option pairs, so a path containing one must be escaped or ffmpeg
	// mis-parses the filter and the gate fails every encode. The media paths arrive as
	// separate -i argv and need none of this.
	return fmt.Sprintf(
		"[0:v]format=%s[dist];[1:v]format=%s[ref];"+
			"[dist][ref]libvmaf=model=%s:feature=%s:log_fmt=json:log_path=%s:n_subsample=%d",
		req.PixelFormat, req.PixelFormat,
		req.Model, chromaFeature, escapeFilterValue(logPath), sub)
}

// Score measures distorted against reference in the request's NAMED pixel format and
// returns the pooled luma statistics, the chroma statistic and the format the comparison
// was made in, or ErrUnavailable if the ffmpeg build lacks libvmaf.
//
// Fail-closed twice over: an empty Request.PixelFormat is refused outright, and a log
// missing ANY of the four pooled statistics is refused rather than scored on the rest.
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
	// Fail CLOSED on an incomplete log: any missing pooled statistic means the measurement
	// did not produce the numbers the gate is built on, and proceeding would either
	// misread a zero as a real score or pass the output on the metrics that did report.
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
	}, nil
}

// escapeFilterValue backslash-escapes the characters that are special inside an ffmpeg
// filtergraph option value, so a path embedded in the graph parses literally whatever it
// contains. NewReplacer applies every rule in one pass, so nothing is double-escaped.
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
