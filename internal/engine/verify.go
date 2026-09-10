package engine

import (
	"context"
	"fmt"
	"math"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

// vmafProof is what the VMAF gate MEASURED, carried out of verifyOutput rather than
// discarded so the terminal ledger row can keep it. Its zero value means the gate did not
// run, which is why the scores are pointers: nil is "not measured" and 0.0 is a real,
// terrible score, and collapsing the two is how a store ends up displaying a fabricated
// fidelity number. Every field here is a FACT ABOUT THE MEASUREMENT - the model actually
// passed to the filter rather than the config's possibly-"auto" request, the format the
// comparison was made in, the chroma statistic - because nobody reading a stored 98.4 can
// otherwise say which pixels were compared or whether the colour survived.
type vmafProof struct {
	Mean  *float64
	Min   *float64
	Model string

	// PixFmt is the pixel format BOTH streams were converted to before scoring, named by
	// holdfast rather than negotiated by libavfilter. "" means no measurement.
	PixFmt string
	// ChromaMin is the worst (sub)sampled frame's chroma PSNR in dB and ChromaMetric names
	// what that number is. nil or "" means no measurement, never a zero: 0.0 dB is an
	// obliterated plane, the most important thing this field could ever report.
	ChromaMin    *float64
	ChromaMetric string
}

// verifyOutput checks a freshly-encoded temp file before it may replace the source, and is
// the heart of the no-loss contract: the source is replaced only when this returns nil. It
// returns an error naming the first failed gate, running them cheap-to-expensive.
//
// Why several gates and not one: decode-to-null catches many but not all corruption
// classes; per-type stream-count parity catches a dropped track that size and duration
// cannot; duration and packet parity catch truncation; strictly-smaller enforces the
// space-reclamation purpose; VMAF adds the perceptual layer. None is sufficient alone.
//
// The measured proof is returned alongside the error, on the REJECT paths too: a VMAF
// rejection whose score was thrown away would re-commit the defect this exists to fix.
func (e *Engine) verifyOutput(ctx context.Context, in, tmp string) (vmafProof, error) {
	var none vmafProof

	// 1. exists & non-empty
	if probe.FileSize(tmp) <= 0 {
		return none, fmt.Errorf("temp missing or empty")
	}

	// 2. output codec must be the engine's configured target codec, so a hardware or AV1
	// encode is held to exactly the same bar as CPU libx265.
	if oc := e.Probe.VideoCodec(ctx, tmp); oc != e.targetCodec {
		return none, fmt.Errorf("output codec is %q, not %s", oc, e.targetCodec)
	}

	// 3. length: the encode must not be truncated.
	if err := e.lengthParity(ctx, in, tmp); err != nil {
		return none, err
	}

	// 4. size: reclaiming space is the whole point.
	sin := probe.FileSize(in)
	sout := probe.FileSize(tmp)
	limit := float64(sin) * (1 - float64(e.Cfg.MinSavingsPercent)/100.0)
	if !(sout > 0 && float64(sout) <= limit && sout < sin) {
		return none, fmt.Errorf("size-increase reject (in=%dB out=%dB min_savings=%d%%)", sin, sout, e.Cfg.MinSavingsPercent)
	}

	// 5. per-type stream-count parity: size, duration and a clean decode can all pass
	// while a track was silently lost. The encode maps every stream but data, so a/s/t
	// counts must not fall below the source; data streams are dropped on purpose.
	for _, typ := range []string{"a", "s", "t"} {
		cin := e.Probe.StreamCount(ctx, in, typ)
		cout := e.Probe.StreamCount(ctx, tmp, typ)
		if cout < cin {
			return none, fmt.Errorf("stream-count parity failed (type=%s in=%d out=%d — a track was dropped)", typ, cin, cout)
		}
	}

	// 6. decode-integrity healthcheck on EVERY encode.
	if !e.Probe.DecodeOK(ctx, tmp) {
		return none, fmt.Errorf("decode-integrity check failed (output does not fully decode)")
	}

	// 7. VMAF perceptual-quality gate, costliest so last. The structural checks prove the
	// output exists, decodes and carries the tracks; VMAF proves it still LOOKS like the
	// source. An unavailable libvmaf or a failed measurement REJECTS the encode: never
	// accept an unmeasured output.
	if e.Cfg.VmafGate() {
		return e.vmafGate(ctx, tmp, in)
	}
	return none, nil
}

// lengthParity is gate 3 on its own: an encode of `in` must not be TRUNCATED. It is a
// named function because the stale-temp sweep asks the identical question of a temp it
// finds lying around (strayReplacementHold), and the two must not drift: that sweep's
// licence to delete rests on anything that ever passed THIS check still passing it.
//
// It measures length twice over. Duration is the direct measure, used whenever both files
// report one; when a container reports none (MPEG-TS is the standing example) it falls
// back to video-packet count, since transcoding preserves frame count and a truncated
// encode has far fewer packets. Neither measurable is a PASS - this gate cannot convict on
// evidence it does not have, and the layers around it cover that case.
func (e *Engine) lengthParity(ctx context.Context, in, out string) error {
	din, okIn := e.Probe.DurationSec(ctx, in)
	dout, okOut := e.Probe.DurationSec(ctx, out)
	if okIn && okOut {
		// Both durations known: strict parity — a truncated encode is shorter.
		if math.Abs(din-dout) > e.Cfg.DurationToleranceSec {
			return fmt.Errorf("duration parity failed (in=%.3fs out=%.3fs tol=%gs)", din, dout, e.Cfg.DurationToleranceSec)
		}
		return nil
	}
	// Duration unknown: fall back to packet parity, enforced only when the source counts.
	pin, okp := e.Probe.PacketCount(ctx, in)
	pout, okpo := e.Probe.PacketCount(ctx, out)
	if okp && pin > 0 && okpo {
		// Same tolerance as the bash: |pin-pout| <= pin*0.02 + 2.
		if math.Abs(float64(pin-pout)) > float64(pin)*0.02+2 {
			return fmt.Errorf("packet-count parity failed (in=%d out=%d — truncated encode?)", pin, pout)
		}
	}
	return nil
}

// vmafGate measures the output (distorted) against the source (reference), in ONE pixel
// format holdfast names rather than one libavfilter negotiates, and rejects an encode on
// ANY of three independent conditions:
//
//  1. pooled harmonic mean < MinVmaf - the average is too low;
//  2. worst (sub)sampled frame < VmafMinPool - some part of the output collapsed, however
//     good the average is; and
//  3. worst (sub)sampled frame's chroma PSNR < VmafMinChroma - the COLOUR planes were
//     damaged, which (1) and (2) cannot see at all because the VMAF model extracts luma
//     features only.
//
// All three are needed. The mean is an average, so it hides local damage - Netflix
// documents exactly this ("mean pooling has the risk of hiding poor quality frames") - and
// measured on real libvmaf an encode with 4 of 240 frames destroyed to VMAF ~43 pools to
// ~97.5 and sails through (1), while every structural gate passes it too: a degraded
// segment still decodes cleanly with the right duration, packets and streams. The source
// would then be swapped and DELETED. Only (2) sees it, and
// TestVmaf_MeanOnlyGateIsBlindToLocalDamage asserts that the mean-only gate accepts it, so
// the worst-frame test is not vacuous.
//
// The proof is returned on BOTH outcomes: a rejected encode is exactly when an operator
// wants the numbers that rejected it, so it is persisted onto the failed row.
func (e *Engine) vmafGate(ctx context.Context, distorted, reference string) (vmafProof, error) {
	model := resolveVmafModel(e.Cfg.VmafModel, e.Probe.Height(ctx, distorted))

	// Name the comparison format BEFORE anything is measured, from the two streams' own
	// pixel formats: `pixel_format: auto` floors output depth at 10, so an 8-bit source
	// routinely meets a 10-bit output and leaving the conversion to libavfilter would make
	// the score depend on an undocumented choice nobody recorded. A pair holdfast cannot
	// name a format for is REJECTED, which costs a wasted encode and keeps the source.
	pixFmt, ok := vmaf.ComparisonFormat(e.Probe.PixFmt(ctx, reference), e.Probe.PixFmt(ctx, distorted))
	if !ok {
		return vmafProof{}, fmt.Errorf(
			"cannot name a comparison pixel format for source pix_fmt %q and output pix_fmt %q "+
				"(refusing to score a pair whose comparison format would be chosen by filter negotiation)",
			e.Probe.PixFmt(ctx, reference), e.Probe.PixFmt(ctx, distorted))
	}

	score := e.vmafScore
	if score == nil {
		if !vmaf.Available(ctx, e.Probe.FFmpeg) {
			return vmafProof{}, fmt.Errorf("VMAF gate enabled but libvmaf is not available in the ffmpeg build (refusing to accept an unmeasured encode)")
		}
		score = func(ctx context.Context, req vmaf.Request) (vmaf.Result, error) {
			return vmaf.Score(ctx, e.Probe.FFmpeg, req)
		}
	}

	res, err := score(ctx, vmaf.Request{
		Distorted:   distorted,
		Reference:   reference,
		Subsample:   e.Cfg.VmafSubsample,
		Model:       model,
		PixelFormat: pixFmt,
	})
	if err != nil {
		// Nothing was measured, so there is nothing to record: an empty proof, NOT a
		// zeroed one. vmaf.Score already refuses a log missing any pooled statistic, so
		// this really is "no measurement" rather than a partial one.
		return vmafProof{}, fmt.Errorf("VMAF measurement failed (refusing to accept an unmeasured encode): %w", err)
	}
	proof := vmafProof{
		Mean: &res.HarmonicMean, Min: &res.Min, Model: model,
		PixFmt: res.PixelFormat, ChromaMin: &res.ChromaMin, ChromaMetric: res.ChromaMetric,
	}

	if res.HarmonicMean < e.Cfg.MinVmaf {
		return proof, fmt.Errorf("VMAF below threshold (harmonic_mean=%.2f < min_vmaf=%.2f)", res.HarmonicMean, e.Cfg.MinVmaf)
	}
	// The worst-frame floor. On by default (vmaf_min_pool=60): a locally-broken
	// encode is invisible to the mean above and to every structural check, so this
	// is the only gate standing between it and the deletion of the source.
	if e.Cfg.VmafMinPool > 0 && res.Min < e.Cfg.VmafMinPool {
		return proof, fmt.Errorf(
			"VMAF worst-frame below floor (min=%.2f < vmaf_min_pool=%.2f) — the encode is locally broken: "+
				"its average is fine (harmonic_mean=%.2f) but at least one frame collapsed, so the source is kept",
			res.Min, e.Cfg.VmafMinPool, res.HarmonicMean)
	}
	// The chroma floor. Everything above this line is LUMA, so an output whose colour
	// planes were flattened, shifted or desaturated clears both floors above and every
	// structural check besides. Measured on real libvmaf, a 15% chroma desaturation pools
	// to ~99 with a worst frame of ~97 while its chroma PSNR falls from ~40 dB to ~26 dB.
	// The reason NAMES the metric and the floor, because "rejected" without them sends an
	// operator to the logs to find out which of three gates fired.
	if e.Cfg.VmafMinChroma > 0 && res.ChromaMin < e.Cfg.VmafMinChroma {
		return proof, fmt.Errorf(
			"chroma below floor (%s=%.2f < vmaf_min_chroma=%.2f) - the encode is damaged in its COLOUR "+
				"planes: its luma is fine (harmonic_mean=%.2f, worst frame=%.2f) and the VMAF model is "+
				"luma-only, so nothing else would have seen this; the source is kept",
			res.ChromaMetric, res.ChromaMin, e.Cfg.VmafMinChroma, res.HarmonicMean, res.Min)
	}
	return proof, nil
}

// resolveVmafModel maps the config VmafModel to a libvmaf model spec. The rule itself
// lives in internal/vmaf, beside the startup preflight that proves each candidate actually
// loads. It has to be ONE rule: a preflight checking a different set of models from the
// ones the gate resolves to would be a preflight that passes and a run that dies hours in.
func resolveVmafModel(cfg string, height int) string {
	return vmaf.ResolveModel(cfg, height)
}
