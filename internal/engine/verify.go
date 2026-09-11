package engine

import (
	"context"
	"fmt"
	"math"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

// verifyOutput checks a freshly-encoded temp file before it may replace the source.
// It returns nil if the output passes EVERY gate, or an error naming the first
// failed gate. Checks run cheap-to-expensive so a bad encode is rejected as early
// as possible; the full decode-integrity scan (the costliest) is last and runs on
// every encode. This is the heart of the no-loss contract — the source is replaced
// only when this returns nil.
//
// The layered argument (why several gates, not one): decode-to-null catches many
// but not all corruption classes; per-type stream-count parity catches a dropped
// track that size/duration cannot; duration/packet parity catches truncation;
// strictly-smaller enforces the space-reclamation purpose. None is sufficient
// alone; VMAF (TRANSCODE-4) adds the perceptual layer.
// vmafProof is what the VMAF gate MEASURED — carried out of verifyOutput rather than
// discarded, so the terminal ledger row can keep it (TRANSCODE-13). Its zero value
// means the gate did not run (VMAF disabled), which is why the scores are pointers:
// nil is "not measured", and 0.0 is a real, terrible score. Collapsing the two is
// exactly how a store ends up displaying a fabricated fidelity number.
//
// Model is the libvmaf model spec actually passed to the filter, not the config's
// possibly-"auto" request — a score without the model that produced it is not
// interpretable, so the resolved value is the only one worth persisting.
//
// PixFmt, ChromaMin and ChromaMetric are the GATE-4 additions and follow the same
// discipline for the same reason: the format the comparison was made in and the
// chroma statistic it produced are FACTS ABOUT THE MEASUREMENT, and a score that
// travels without them cannot be interpreted afterwards - nobody reading a stored
// 98.4 can say which pixels were compared or whether the colour survived.
type vmafProof struct {
	Mean  *float64
	Min   *float64
	Model string

	// PixFmt is the pixel format BOTH streams were converted to before scoring, named
	// by holdfast rather than negotiated by libavfilter. "" means no measurement.
	PixFmt string
	// ChromaMin is the worst (sub)sampled frame's chroma PSNR in dB, and ChromaMetric
	// names what that number is. nil/"" means no measurement, never a zero: 0.0 dB is
	// an obliterated plane, which is the single most important thing this field could
	// ever have to report.
	ChromaMin    *float64
	ChromaMetric string
}

// verifyOutput returns the VMAF proof it measured, and the CLASS of the rejection,
// alongside its pass/fail error. The error is what governs the gate, exactly as before;
// the proof is evidence, and is returned on the REJECT paths too — a VMAF rejection whose
// score is then thrown away would be re-committing the very defect this phase exists to
// fix. The proof is the zero value whenever VMAF did not run.
//
// # The class, and why it is produced HERE
//
// Most of what this function refuses is a pure function of three things that do not move
// between attempts: the source bytes, the configuration, and the pinned ffmpeg build.
// An output that came out bigger than its source comes out bigger again; a source whose
// audio track the encoder drops drops it again. Retrying those costs a full encode each -
// hours on a feature-length source at preset slow - to reach a verdict that was final the
// first time. So each rejection says which kind it is, and the recording site parks the
// final ones at once (see ProcessFile, store.Finish).
//
// It is carried OUT of the gate beside the error rather than re-derived from the error
// text afterwards, and that is the whole design. A classifier that matched on strings
// would be a second, silently-drifting copy of this function's verdicts: reword a message
// here and a file that should park would retry for ever, or worse, a file that should
// retry would park - and nothing would fail. The class travels with the verdict because
// only the line that produced the verdict knows what it means.
//
// The class is meaningful ONLY beside a non-nil error; a passing gate returns the zero
// value, which nothing reads. Every rejection this function does NOT classify explicitly
// is transient, which is the fail-safe direction: an unrecognised rejection costs CPU,
// where a wrongly-final one costs a file nobody revisits.
func (e *Engine) verifyOutput(ctx context.Context, in, tmp string) (vmafProof, store.FailureClass, error) {
	var none vmafProof

	// 1. exists & non-empty. TRANSIENT: an empty temp is what a full disk, a killed
	// ffmpeg or a write that never landed leaves behind, and none of those is a
	// property of the source.
	if probe.FileSize(tmp) <= 0 {
		return none, store.FailureTransient, fmt.Errorf("temp missing or empty")
	}

	// 2. output codec must be the engine's configured target codec (hevc or av1 —
	// TRANSCODE-6 generalizes this away from a hardcoded "hevc" so a hardware/AV1
	// encode is held to exactly the same bar as CPU libx265). DETERMINISTIC: the
	// configured encoder produces the codec it produces.
	if oc := e.Probe.VideoCodec(ctx, tmp); oc != e.targetCodec {
		return none, store.FailureDeterministic, fmt.Errorf("output codec is %q, not %s", oc, e.targetCodec)
	}

	// 3. length: the encode must not be truncated. lengthParity classifies its own two
	// rejections (both deterministic).
	if class, err := e.lengthParity(ctx, in, tmp); err != nil {
		return none, class, err
	}

	// 4. size: reclaiming space is the whole point. DETERMINISTIC, and the headline
	// case: a source this configuration cannot beat - an already-efficient encode, a
	// grain-heavy master, anything at a bitrate the CRF cannot undercut - produces the
	// identical arithmetic on every attempt.
	sin := probe.FileSize(in)
	sout := probe.FileSize(tmp)
	limit := float64(sin) * (1 - float64(e.Cfg.MinSavingsPercent)/100.0)
	if !(sout > 0 && float64(sout) <= limit && sout < sin) {
		return none, store.FailureDeterministic, fmt.Errorf("size-increase reject (in=%dB out=%dB min_savings=%d%%)", sin, sout, e.Cfg.MinSavingsPercent)
	}

	// 5. per-type stream-count parity: no video/audio/subtitle/attachment track dropped.
	// Size + duration + a clean decode can all pass while a track was silently lost.
	// The encode maps every stream but data, so v/a/s/t counts must not fall below the
	// source. Data streams are dropped on purpose and never counted. DETERMINISTIC:
	// which streams this source has, and which of them this build's mapping carries
	// into this container, is fixed.
	//
	// Video is in the loop for the same reason the other three are, and with the most at
	// stake: the encode maps and re-encodes EVERY video stream, so a source carrying a
	// second angle or an attached cover picture can come out one stream short while the
	// size, duration, packet and decode checks all pass - a track dropped in exactly the
	// place this tool exists to protect. An output whose video streams cannot be counted
	// at all counts as ZERO here, which is the fail-safe direction: an uncountable answer
	// rejects and keeps the source instead of swapping on an unknown.
	for _, typ := range []string{"v", "a", "s", "t"} {
		cin := e.Probe.StreamCount(ctx, in, typ)
		cout := e.Probe.StreamCount(ctx, tmp, typ)
		if cout < cin {
			return none, store.FailureDeterministic, fmt.Errorf("stream-count parity failed (type=%s in=%d out=%d — a track was dropped)", typ, cin, cout)
		}
	}

	// 6. decode-integrity healthcheck on EVERY encode. TRANSIENT: an output that does
	// not fully decode is a damaged FILE, and the commonest ways to get one - a disk
	// that filled, a process that was killed, a bit that flipped - are conditions of
	// the run rather than properties of the source.
	if !e.Probe.DecodeOK(ctx, tmp) {
		return none, store.FailureTransient, fmt.Errorf("decode-integrity check failed (output does not fully decode)")
	}

	// 7. VMAF perceptual-quality gate (costliest — a second full decode — so last).
	// The structural checks prove the output exists/decodes/carries the tracks; VMAF
	// proves it still LOOKS like the source. Same resolution (codec-only), so no
	// scaling. When enabled and libvmaf is unavailable, or the measurement fails, the
	// encode is REJECTED — never accept an unmeasured output.
	if e.Cfg.VmafGate() {
		return e.vmafGate(ctx, tmp, in)
	}
	return none, "", nil
}

// lengthParity is gate 3 on its own: an encode of `in` must not be TRUNCATED, so its
// length has to match the source's. It is a named function rather than an inline block
// because the stale-temp sweep asks the identical question of a temp it finds lying
// around (see strayReplacementHold), and the two must not be allowed to drift: the
// sweep's licence to delete rests on the fact that anything that ever passed THIS check
// still passes it, so a second, slightly different copy of the arithmetic would be a
// second, slightly different answer about whether a file may be removed.
//
// It measures length twice over, deliberately. Duration is the direct measure and is
// used whenever both files report one. When a container reports none (MPEG-TS is the
// standing example) it falls back to video-packet count, since transcoding preserves
// frame count and a truncated encode has far fewer packets. Neither measurable is a
// PASS - this gate cannot convict on evidence it does not have, and the layers around
// it (decode integrity, stream counts, VMAF) are what cover that case.
// Both of its rejections are DETERMINISTIC: how long this source is, and how many video
// packets it holds, are properties of the file, and this build's encode of it truncates
// at the same place every time. It returns the class beside the error for the same reason
// verifyOutput does - the verdict and what it means travel together, never a text match
// afterwards - and the class is meaningful only when the error is non-nil.
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
	// Duration unknown (e.g. MPEG-TS reports N/A) — use video-packet-count parity.
	// Only enforce when the source is countable.
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

// vmafGate measures the output (distorted) against the source (reference) - in ONE
// pixel format holdfast names rather than one libavfilter negotiates - and rejects
// an encode on ANY of three independent conditions:
//
//  1. pooled harmonic mean < MinVmaf - the average is too low;
//  2. worst (sub)sampled frame < VmafMinPool — some part of the output collapsed,
//     however good the average is; and
//  3. worst (sub)sampled frame's chroma PSNR < VmafMinChroma - the COLOUR planes
//     were damaged, which (1) and (2) cannot see at all because the VMAF model
//     extracts luma features only.
//
// All three are needed, and (2) is the one that closed the first real hole. The mean is an
// average, so it hides local damage — Netflix documents exactly this ("mean pooling
// has the risk of hiding poor quality frames"). Measured on real libvmaf: an encode
// with 4 of 240 frames destroyed to VMAF ~43 pools to a harmonic mean of ~97.5 and
// sails through (1). Every structural gate passes it too: a degraded segment still
// decodes cleanly and still carries the right duration, packets and stream counts.
// The source would then be atomically swapped and DELETED. Only (2) sees it.
//
// TestVmaf_MeanOnlyGateIsBlindToLocalDamage asserts precisely that (the mean-only
// gate ACCEPTS it), which is what makes TestVmaf_WorstFrameFloorRejectsLocally-
// BrokenEncode meaningful rather than vacuous.
//
// It returns the measured proof (mean + worst frame + the model that produced them)
// alongside the gate error. The proof is returned on BOTH outcomes: a rejected encode
// is exactly the case where an operator most wants to see the numbers that rejected
// it, so it is persisted onto the failed row rather than living only in a log line.
// Its rejections split cleanly, and the split is what the class exists to record. The
// three FLOORS are deterministic: the same two files scored by the same model with the
// same subsample produce the same numbers, so a retry buys another full encode and
// another measurement to arrive at the identical comparison. The two rejections about the
// MEASUREMENT ITSELF - libvmaf missing from the ffmpeg build, or a measurement that
// failed - are transient: they are conditions of the installation and the run, and an
// operator who installs a libvmaf-capable build has changed exactly the thing that
// rejected. The unnameable comparison format is deterministic, because it is decided by
// the two files' own pixel formats before anything is measured at all.
func (e *Engine) vmafGate(ctx context.Context, distorted, reference string) (vmafProof, store.FailureClass, error) {
	model := resolveVmafModel(e.Cfg.VmafModel, e.Probe.Height(ctx, distorted))

	// Name the comparison format BEFORE anything is measured, from the two streams'
	// own pixel formats. `pixel_format: auto` floors output depth at 10, so an 8-bit
	// source routinely meets a 10-bit output and the two DO disagree on the default
	// path; leaving that conversion to libavfilter's negotiation made the score depend
	// on an undocumented choice nobody recorded. A pair whose formats holdfast cannot
	// name a comparison format for is REJECTED rather than measured in whatever
	// negotiation would have produced - the fail-closed direction costs a wasted
	// encode and keeps the source.
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
		Subsample:   e.Cfg.VmafSubsample,
		Model:       model,
		PixelFormat: pixFmt,
	})
	if err != nil {
		// Nothing was measured, so there is nothing to record: an empty proof, NOT a
		// zeroed one. (vmaf.Score already refuses a log missing ANY pooled statistic -
		// TRANSCODE-11 for the luma pair, GATE-4 for the chroma planes - so this really
		// is "no measurement", never a partial one scored on what did report.)
		return vmafProof{}, store.FailureTransient, fmt.Errorf("VMAF measurement failed (refusing to accept an unmeasured encode): %w", err)
	}
	proof := vmafProof{
		Mean: &res.HarmonicMean, Min: &res.Min, Model: model,
		PixFmt: res.PixelFormat, ChromaMin: &res.ChromaMin, ChromaMetric: res.ChromaMetric,
	}

	if res.HarmonicMean < e.Cfg.MinVmaf {
		return proof, store.FailureDeterministic, fmt.Errorf("VMAF below threshold (harmonic_mean=%.2f < min_vmaf=%.2f)", res.HarmonicMean, e.Cfg.MinVmaf)
	}
	// The worst-frame floor. On by default (vmaf_min_pool=60): a locally-broken
	// encode is invisible to the mean above and to every structural check, so this
	// is the only gate standing between it and the deletion of the source.
	if e.Cfg.VmafMinPool > 0 && res.Min < e.Cfg.VmafMinPool {
		return proof, store.FailureDeterministic, fmt.Errorf(
			"VMAF worst-frame below floor (min=%.2f < vmaf_min_pool=%.2f) — the encode is locally broken: "+
				"its average is fine (harmonic_mean=%.2f) but at least one frame collapsed, so the source is kept",
			res.Min, e.Cfg.VmafMinPool, res.HarmonicMean)
	}
	// The chroma floor (GATE-4). Everything above this line is LUMA: the VMAF model
	// extracts luma features only, so an output whose colour planes were flattened,
	// shifted or desaturated clears both floors above - and clears every structural
	// check too, because it decodes cleanly and carries the right duration, packets
	// and streams. Measured on real libvmaf, a 15% chroma desaturation pools to a
	// harmonic mean of ~99 with a worst frame of ~97 while its chroma PSNR falls from
	// ~40 dB to ~26 dB. Only this sees it. The reason NAMES the metric and the floor,
	// because "rejected" without them sends an operator to the logs to find out which
	// of three gates fired.
	if e.Cfg.VmafMinChroma > 0 && res.ChromaMin < e.Cfg.VmafMinChroma {
		return proof, store.FailureDeterministic, fmt.Errorf(
			"chroma below floor (%s=%.2f < vmaf_min_chroma=%.2f) - the encode is damaged in its COLOUR "+
				"planes: its luma is fine (harmonic_mean=%.2f, worst frame=%.2f) and the VMAF model is "+
				"luma-only, so nothing else would have seen this; the source is kept",
			res.ChromaMetric, res.ChromaMin, e.Cfg.VmafMinChroma, res.HarmonicMean, res.Min)
	}
	return proof, "", nil
}

// resolveVmafModel maps the config VmafModel to a libvmaf model spec. "auto"/""
// picks the UHD model for output height > 1440, else the HD model; any other value
// is passed through (prefixed with "version=" when it looks like a bare version id).
//
// The rule itself lives in internal/vmaf, beside the startup preflight that proves
// each candidate actually loads (GATE-4). It has to be ONE rule: a preflight that
// checked a different set of models from the ones the gate later resolves to would
// be a preflight that passes and a run that dies hours in.
func resolveVmafModel(cfg string, height int) string {
	return vmaf.ResolveModel(cfg, height)
}
