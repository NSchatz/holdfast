package engine

import (
	"context"
	"fmt"
	"math"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

// vmafProof is what the VMAF gate MEASURED - carried out of verifyOutput rather than
// discarded, so the terminal ledger row can keep it (TRANSCODE-13). Its zero value
// means the gate did not run (VMAF disabled), which is why the scores are pointers:
// nil is "not measured", and 0.0 is a real, terrible score. Collapsing the two is
// exactly how a store ends up displaying a fabricated fidelity number. Every other field
// is a FACT ABOUT THE MEASUREMENT held to the same discipline: nobody reading a stored
// 98.4 without them can say which pixels were compared or whether the colour survived.
type vmafProof struct {
	Mean  *float64
	Min   *float64
	Model string

	// PixFmt is the pixel format BOTH streams were converted to before scoring, named by
	// holdfast rather than negotiated by libavfilter. "" means no measurement.
	PixFmt string
	// Stream names WHICH video stream of each file was compared, in the specifier
	// vocabulary the probes use (vmaf.ScoredStream). "" means no measurement, and is
	// never filled in with a default: a job whose gate never ran scored no stream.
	Stream string
	// ChromaMin is the worst (sub)sampled frame's chroma PSNR in dB, and ChromaMetric
	// names what that number is. nil/"" means no measurement, never a zero: 0.0 dB is
	// an obliterated plane, the most important thing this field could ever report.
	ChromaMin    *float64
	ChromaMetric string
}

// verifyOutput checks a freshly-encoded temp before it may replace the source, and is the
// heart of the no-loss contract: the source is replaced only when this returns nil. Gates run
// cheap-to-expensive, so the full decode and VMAF (TRANSCODE-4) come last. It returns the VMAF
// proof and the CLASS of the rejection beside the pass/fail error, the proof on the REJECT
// paths too: a rejection whose score is thrown away re-commits the defect TRANSCODE-13 fixes.
//
// prof is the profile of the root the source was enumerated under: every threshold comes from
// it, and targetCodec is what its own `encoder` resolves to, so a film library and a
// grainy-anime library each meet the bar their operator set. WHAT the gates check never moves.
//
// # The class, and why it is produced HERE
//
// Most of what this function refuses is a pure function of three things that do not move
// between attempts: the source bytes, the configuration, and the pinned ffmpeg build. An
// output that came out bigger comes out bigger again, and retrying it buys a full encode, hours
// at preset slow, for a verdict that was final the first time - so each rejection says which
// kind it is and the recording site parks the final ones at once (see ProcessFile, store.Finish).
// The class travels WITH the verdict rather than being re-derived from the error text: a
// classifier matching on strings drifts silently, and a file that should park retries for ever.
//
// The class is meaningful ONLY beside a non-nil error. Every rejection this function does
// NOT classify explicitly is transient, the fail-safe direction: an unrecognised rejection
// costs CPU, where a wrongly-final one costs a file nobody revisits.
func (e *Engine) verifyOutput(ctx context.Context, in, tmp string, prof config.Profile, targetCodec string) (vmafProof, store.FailureClass, error) {
	var none vmafProof

	// 1. exists & non-empty. TRANSIENT: an empty temp is what a full disk, a killed ffmpeg
	// or a write that never landed leaves behind, none of them properties of the source.
	if probe.FileSize(tmp) <= 0 {
		return none, store.FailureTransient, fmt.Errorf("temp missing or empty")
	}

	// 2. output codec must be this root profile's target codec (hevc or av1, TRANSCODE-6),
	// so a hardware or AV1 encode is held to the same bar as CPU libx265. DETERMINISTIC:
	// the encoder a profile configures produces the codec it produces.
	if oc := e.Probe.VideoCodec(ctx, tmp); oc != targetCodec {
		return none, store.FailureDeterministic, fmt.Errorf("output codec is %q, not %s", oc, targetCodec)
	}

	// 3. length: the encode must not be truncated. lengthParity classifies its own two
	// rejections (both deterministic).
	if class, err := e.lengthParity(ctx, in, tmp); err != nil {
		return none, class, err
	}

	// 4. size: reclaiming space is the whole point. DETERMINISTIC, and the headline case:
	// a source this configuration cannot beat produces the identical arithmetic every time.
	sin := probe.FileSize(in)
	sout := probe.FileSize(tmp)
	limit := float64(sin) * (1 - float64(prof.MinSavingsPercent)/100.0)
	if !(sout > 0 && float64(sout) <= limit && sout < sin) {
		return none, store.FailureDeterministic, fmt.Errorf("size-increase reject (in=%dB out=%dB min_savings=%d%%)", sin, sout, prof.MinSavingsPercent)
	}

	// 5. per-type stream-count parity: no video/audio/subtitle/attachment track dropped. Size,
	// duration and a clean decode can all pass while a track was silently lost, and video is in
	// the loop with the most at stake - a second angle or a cover picture can come out one
	// stream short with every other check green. The encode maps every stream but data, so
	// v/a/s/t counts must not fall below the source. An output whose streams cannot be counted
	// counts as ZERO: reject and keep the source rather than swap on an unknown. DETERMINISTIC.
	for _, typ := range []string{"v", "a", "s", "t"} {
		cin := e.Probe.StreamCount(ctx, in, typ)
		cout := e.Probe.StreamCount(ctx, tmp, typ)
		if cout < cin {
			return none, store.FailureDeterministic, fmt.Errorf("stream-count parity failed (type=%s in=%d out=%d — a track was dropped)", typ, cin, cout)
		}
	}

	// 6. decode-integrity healthcheck on EVERY encode. TRANSIENT: an output that does not
	// fully decode is a damaged FILE, and a filled disk, a killed process or a flipped bit
	// are conditions of the run rather than properties of the source.
	if !e.Probe.DecodeOK(ctx, tmp) {
		return none, store.FailureTransient, fmt.Errorf("decode-integrity check failed (output does not fully decode)")
	}

	// 7. VMAF perceptual-quality gate, last because it costs a second full decode. The
	// structural checks prove the output exists, decodes and carries the tracks; VMAF proves
	// it still LOOKS like the source. Unavailable libvmaf or a failed measurement REJECTS.
	if prof.VmafGate() {
		return e.vmafGate(ctx, tmp, in, prof)
	}
	return none, "", nil
}

// lengthParity is gate 3 on its own: an encode of `in` must not be TRUNCATED. It is a named
// function because the stale-temp sweep asks the identical question of a temp it finds lying
// around (strayReplacementHold), and the two must not drift: that sweep's licence to delete
// rests on anything that ever passed THIS check still passing it.
//
// It measures length twice over, deliberately. Duration is the direct measure, used whenever
// both files report one; a container that reports none (MPEG-TS is the standing example) falls
// back to video-packet count, since transcoding preserves frame count and a truncated encode
// has far fewer packets. Neither measurable is a PASS: this gate cannot convict on evidence it
// does not have, and the layers around it cover that case. Both rejections are DETERMINISTIC.
func (e *Engine) lengthParity(ctx context.Context, in, out string) (store.FailureClass, error) {
	din, okIn := e.Probe.DurationSec(ctx, in)
	dout, okOut := e.Probe.DurationSec(ctx, out)
	if okIn && okOut {
		// Both durations known: strict parity — a truncated encode is shorter.
		if math.Abs(din-dout) > e.Cfg.DurationToleranceSec {
			return store.FailureDeterministic, fmt.Errorf("duration parity failed (in=%.3fs out=%.3fs tol=%gs)", din, dout, e.Cfg.DurationToleranceSec)
		}
		return "", nil
	}
	// Duration unknown: fall back to packet parity, enforced only when the source counts.
	pin, okp := e.Probe.PacketCount(ctx, in)
	pout, okpo := e.Probe.PacketCount(ctx, out)
	if okp && pin > 0 && okpo {
		// Same tolerance as the bash: |pin-pout| <= pin*0.02 + 2.
		if math.Abs(float64(pin-pout)) > float64(pin)*0.02+2 {
			return store.FailureDeterministic, fmt.Errorf("packet-count parity failed (in=%d out=%d — truncated encode?)", pin, pout)
		}
	}
	return "", nil
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
// All three are needed. The mean is an average, so it hides local damage - Netflix documents
// exactly this ("mean pooling has the risk of hiding poor quality frames") - and measured on
// real libvmaf an encode with 4 of 240 frames destroyed to VMAF ~43 pools to ~97.5 and sails
// through (1) with every structural gate passing it too, after which the source would be
// swapped and DELETED. Only (2) sees it, and TestVmaf_MeanOnlyGateIsBlindToLocalDamage asserts
// that the mean-only gate accepts it, so the worst-frame test is not vacuous.
//
// The measured proof is returned on BOTH outcomes: a rejected encode is exactly the case where
// an operator most wants the numbers that rejected it, so it is persisted onto the failed row
// rather than living only in a log line. The three FLOORS are deterministic, as is a comparison
// format that cannot be named; the two rejections about the MEASUREMENT ITSELF, libvmaf missing
// from the build or a measurement that failed, are transient, since installing a libvmaf-capable
// build changes exactly the thing that rejected. Every floor is read off prof, so a root that
// set a stricter bar is held to its own and not the top-level one.
func (e *Engine) vmafGate(ctx context.Context, distorted, reference string, prof config.Profile) (vmafProof, store.FailureClass, error) {
	model := resolveVmafModel(prof.VmafModel, e.Probe.Height(ctx, distorted))

	// Name the comparison format BEFORE anything is measured, from the two streams' own pixel
	// formats: `pixel_format: auto` floors output depth at 10, so an 8-bit source routinely
	// meets a 10-bit output and leaving the conversion to libavfilter would make the score
	// depend on an undocumented choice nobody recorded. An unnameable pair is REJECTED.
	pixFmt, ok := vmaf.ComparisonFormat(e.Probe.PixFmt(ctx, reference), e.Probe.PixFmt(ctx, distorted))
	if !ok {
		return vmafProof{}, store.FailureDeterministic, fmt.Errorf(
			"cannot name a comparison pixel format for source pix_fmt %q and output pix_fmt %q "+
				"(refusing to score a pair whose comparison format would be chosen by filter negotiation)",
			e.Probe.PixFmt(ctx, reference), e.Probe.PixFmt(ctx, distorted))
	}

	score := e.vmafScore
	if score == nil {
		if !vmaf.Available(ctx, e.Probe.FFmpeg) {
			return vmafProof{}, store.FailureTransient, fmt.Errorf("VMAF gate enabled but libvmaf is not available in the ffmpeg build (refusing to accept an unmeasured encode)")
		}
		score = func(ctx context.Context, req vmaf.Request) (vmaf.Result, error) {
			return vmaf.Score(ctx, e.Probe.FFmpeg, req)
		}
	}

	res, err := score(ctx, vmaf.Request{
		Distorted:   distorted,
		Reference:   reference,
		Subsample:   prof.VmafSubsample,
		Model:       model,
		PixelFormat: pixFmt,
	})
	if err != nil {
		// Nothing was measured, so there is nothing to record: an empty proof, NOT a zeroed
		// one. vmaf.Score already refuses a log missing ANY pooled statistic (TRANSCODE-11
		// for the luma pair, GATE-4 for the chroma planes), so this is never a partial one.
		return vmafProof{}, store.FailureTransient, fmt.Errorf("VMAF measurement failed (refusing to accept an unmeasured encode): %w", err)
	}
	proof := vmafProof{
		Mean: &res.HarmonicMean, Min: &res.Min, Model: model,
		PixFmt: res.PixelFormat, ChromaMin: &res.ChromaMin, ChromaMetric: res.ChromaMetric,
		Stream: res.Stream,
	}

	if res.HarmonicMean < prof.MinVmaf {
		return proof, store.FailureDeterministic, fmt.Errorf("VMAF below threshold (harmonic_mean=%.2f < min_vmaf=%.2f)", res.HarmonicMean, prof.MinVmaf)
	}
	// The worst-frame floor. On by default (vmaf_min_pool=60): a locally-broken
	// encode is invisible to the mean above and to every structural check, so this
	// is the only gate standing between it and the deletion of the source.
	if prof.VmafMinPool > 0 && res.Min < prof.VmafMinPool {
		return proof, store.FailureDeterministic, fmt.Errorf(
			"VMAF worst-frame below floor (min=%.2f < vmaf_min_pool=%.2f) — the encode is locally broken: "+
				"its average is fine (harmonic_mean=%.2f) but at least one frame collapsed, so the source is kept",
			res.Min, prof.VmafMinPool, res.HarmonicMean)
	}
	// The chroma floor (GATE-4). Everything above this line is LUMA: the VMAF model
	// extracts luma features only, so an output whose colour planes were flattened or
	// desaturated clears both floors above and every structural check too. Measured on
	// real libvmaf, a 15% chroma desaturation pools to a harmonic mean of ~99 with a
	// worst frame of ~97 while its chroma PSNR falls from ~40 dB to ~26 dB. Only this
	// sees it, and the reason NAMES the metric and the floor so an operator need not
	// go to the logs to learn which of three gates fired.
	if prof.VmafMinChroma > 0 && res.ChromaMin < prof.VmafMinChroma {
		return proof, store.FailureDeterministic, fmt.Errorf(
			"chroma below floor (%s=%.2f < vmaf_min_chroma=%.2f) - the encode is damaged in its COLOUR "+
				"planes: its luma is fine (harmonic_mean=%.2f, worst frame=%.2f) and the VMAF model is "+
				"luma-only, so nothing else would have seen this; the source is kept",
			res.ChromaMetric, res.ChromaMin, prof.VmafMinChroma, res.HarmonicMean, res.Min)
	}
	return proof, "", nil
}

// resolveVmafModel maps the config VmafModel to a libvmaf model spec. The rule lives in
// internal/vmaf, beside the startup preflight that proves each candidate loads, and has to be
// ONE rule: a preflight checking other models than the gate resolves to passes, then dies hours
// into the run.
func resolveVmafModel(cfg string, height int) string {
	return vmaf.ResolveModel(cfg, height)
}
