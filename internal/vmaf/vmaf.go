// Package vmaf runs libvmaf (via ffmpeg) to measure the perceptual quality of a
// transcoded output against its source — the last no-loss layer (TRANSCODE-4). The
// structural gates (codec/parity/size/stream-count/decode-integrity) prove an output
// exists, decodes, and carries the tracks; VMAF ESTIMATES whether it still looks
// like the source. A codec-only transcode keeps the resolution identical, so VMAF
// applies with no scaling.
//
// Two pooled statistics come back, and the gate needs BOTH (see Result): the
// harmonic mean bounds average quality, and the min bounds LOCAL quality. A mean
// alone is not a gate — it averages a destroyed segment away.
//
// libvmaf's filter takes the DISTORTED stream as its first input and the REFERENCE
// as its second — getting this backwards inverts the meaning, so it is fixed here.
package vmaf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// dangerous direction for a pass/fail gate. Chroma corruption is caught here only
// by the structural checks, never by VMAF.
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

// vmafLog is the subset of libvmaf's JSON log we consume. Both pooled statistics
// are POINTERS so that "libvmaf did not emit this field" is distinguishable from
// "libvmaf measured 0.0". That distinction is load-bearing: the worst-frame floor
// is enforced as `Min < vmaf_min_pool`, so an absent Min silently unmarshalling to
// 0.0 would either reject everything or — if the floor were ever read as optional —
// quietly degrade the gate back to mean-only. An unparseable or incomplete log is a
// REJECTION, never a fallback.
type vmafLog struct {
	PooledMetrics struct {
		VMAF struct {
			Min          *float64 `json:"min"`
			HarmonicMean *float64 `json:"harmonic_mean"`
		} `json:"vmaf"`
	} `json:"pooled_metrics"`
}

// Score computes the pooled VMAF of distorted vs reference. model is a libvmaf
// model spec (e.g. "version=vmaf_v0.6.1" or "version=vmaf_4k_v0.6.1"); subsample>=1
// is the frame-sampling interval (1 = every frame; higher = cheaper, less precise).
// It returns ErrUnavailable (wrapped) if the ffmpeg build lacks libvmaf.
func Score(ctx context.Context, ffmpeg, distorted, reference string, subsample int, model string) (Result, error) {
	if subsample < 1 {
		subsample = 1
	}
	logf, err := os.CreateTemp("", "holdfast-vmaf-*.json")
	if err != nil {
		return Result{}, fmt.Errorf("vmaf: temp log: %w", err)
	}
	logPath := logf.Name()
	logf.Close()
	defer os.Remove(logPath)

	// [0:v] = distorted (the freshly-encoded output), [1:v] = reference (the source).
	// log_path lives INSIDE the -lavfi filtergraph, where ':' separates option pairs,
	// so a path with a ':' (or other filtergraph metachar) must be escaped or ffmpeg
	// mis-parses the filter and the gate fails every encode. The media paths are safe
	// (separate -i argv); only the filter-embedded log_path needs escaping.
	filter := fmt.Sprintf("[0:v][1:v]libvmaf=model=%s:log_fmt=json:log_path=%s:n_subsample=%d",
		model, escapeFilterValue(logPath), subsample)
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-i", distorted, "-i", reference, "-lavfi", filter, "-f", "null", "-")
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
	// Fail CLOSED on an incomplete log. Either pooled statistic missing means the
	// measurement did not actually produce the numbers the gate is built on, and the
	// caller must reject rather than proceed on a zero value it would misread as a
	// real score.
	pooled := parsed.PooledMetrics.VMAF
	if pooled.HarmonicMean == nil || pooled.Min == nil {
		return Result{}, fmt.Errorf(
			"vmaf: log is missing a pooled statistic (harmonic_mean present=%t, min present=%t) — "+
				"refusing to accept an encode whose quality was not fully measured",
			pooled.HarmonicMean != nil, pooled.Min != nil)
	}
	return Result{
		HarmonicMean: *pooled.HarmonicMean,
		Min:          *pooled.Min,
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
