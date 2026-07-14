package engine

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/NSchatz/holdfast/internal/probe"
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
type vmafProof struct {
	Mean  *float64
	Min   *float64
	Model string
}

// verifyOutput returns the VMAF proof it measured alongside its pass/fail error. The
// error is what governs the gate, exactly as before; the proof is evidence, and is
// returned on the REJECT paths too — a VMAF rejection whose score is then thrown away
// would be re-committing the very defect this phase exists to fix. The proof is the
// zero value whenever VMAF did not run.
func (e *Engine) verifyOutput(ctx context.Context, in, tmp string) (vmafProof, error) {
	var none vmafProof

	// 1. exists & non-empty
	if probe.FileSize(tmp) <= 0 {
		return none, fmt.Errorf("temp missing or empty")
	}

	// 2. output codec must be the engine's configured target codec (hevc or av1 —
	// TRANSCODE-6 generalizes this away from a hardcoded "hevc" so a hardware/AV1
	// encode is held to exactly the same bar as CPU libx265).
	if oc := e.Probe.VideoCodec(ctx, tmp); oc != e.targetCodec {
		return none, fmt.Errorf("output codec is %q, not %s", oc, e.targetCodec)
	}

	// 3. length: the encode must not be truncated.
	din, okIn := e.Probe.DurationSec(ctx, in)
	dout, okOut := e.Probe.DurationSec(ctx, tmp)
	if okIn && okOut {
		// Both durations known: strict parity — a truncated encode is shorter.
		if math.Abs(din-dout) > e.Cfg.DurationToleranceSec {
			return none, fmt.Errorf("duration parity failed (in=%.3fs out=%.3fs tol=%gs)", din, dout, e.Cfg.DurationToleranceSec)
		}
	} else {
		// Duration unknown (e.g. MPEG-TS reports N/A) — use video-packet-count
		// parity (transcoding preserves frame count, so a truncated encode has far
		// fewer packets). Only enforce when the source is countable.
		pin, okp := e.Probe.PacketCount(ctx, in)
		pout, okpo := e.Probe.PacketCount(ctx, tmp)
		if okp && pin > 0 && okpo {
			// Same tolerance as the bash: |pin-pout| <= pin*0.02 + 2.
			if math.Abs(float64(pin-pout)) > float64(pin)*0.02+2 {
				return none, fmt.Errorf("packet-count parity failed (in=%d out=%d — truncated encode?)", pin, pout)
			}
		}
	}

	// 4. size: reclaiming space is the whole point.
	sin := probe.FileSize(in)
	sout := probe.FileSize(tmp)
	limit := float64(sin) * (1 - float64(e.Cfg.MinSavingsPercent)/100.0)
	if !(sout > 0 && float64(sout) <= limit && sout < sin) {
		return none, fmt.Errorf("size-increase reject (in=%dB out=%dB min_savings=%d%%)", sin, sout, e.Cfg.MinSavingsPercent)
	}

	// 5. per-type stream-count parity: no audio/subtitle/attachment track dropped.
	// Size + duration + a clean decode can all pass while a track was silently lost.
	// The encode maps every stream but data, so a/s/t counts must not fall below the
	// source. Data streams are dropped on purpose and never counted.
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

	// 7. VMAF perceptual-quality gate (costliest — a second full decode — so last).
	// The structural checks prove the output exists/decodes/carries the tracks; VMAF
	// proves it still LOOKS like the source. Same resolution (codec-only), so no
	// scaling. When enabled and libvmaf is unavailable, or the measurement fails, the
	// encode is REJECTED — never accept an unmeasured output.
	if e.Cfg.VmafGate() {
		return e.vmafGate(ctx, tmp, in)
	}
	return none, nil
}

// vmafGate measures the output (distorted) against the source (reference) and
// rejects an encode on EITHER of two independent conditions:
//
//  1. pooled harmonic mean < MinVmaf — the average is too low; and
//  2. worst (sub)sampled frame < VmafMinPool — some part of the output collapsed,
//     however good the average is.
//
// Both are needed, and (2) is the one that closes the real hole. The mean is an
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
func (e *Engine) vmafGate(ctx context.Context, distorted, reference string) (vmafProof, error) {
	model := resolveVmafModel(e.Cfg.VmafModel, e.Probe.Height(ctx, distorted))

	score := e.vmafScore
	if score == nil {
		if !vmaf.Available(ctx, e.Probe.FFmpeg) {
			return vmafProof{}, fmt.Errorf("VMAF gate enabled but libvmaf is not available in the ffmpeg build (refusing to accept an unmeasured encode)")
		}
		score = func(ctx context.Context, d, r string, sub int, m string) (vmaf.Result, error) {
			return vmaf.Score(ctx, e.Probe.FFmpeg, d, r, sub, m)
		}
	}

	res, err := score(ctx, distorted, reference, e.Cfg.VmafSubsample, model)
	if err != nil {
		// Nothing was measured, so there is nothing to record: an empty proof, NOT a
		// zeroed one. (vmaf.Score already refuses a log missing either pooled statistic
		// — TRANSCODE-11 — so this really is "no measurement", not a partial one.)
		return vmafProof{}, fmt.Errorf("VMAF measurement failed (refusing to accept an unmeasured encode): %w", err)
	}
	proof := vmafProof{Mean: &res.HarmonicMean, Min: &res.Min, Model: model}

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
	return proof, nil
}

// resolveVmafModel maps the config VmafModel to a libvmaf model spec. "auto"/""
// picks the UHD model for output height > 1440, else the HD model; any other value
// is passed through (prefixed with "version=" when it looks like a bare version id).
func resolveVmafModel(cfg string, height int) string {
	if cfg == "" || cfg == "auto" {
		if height > 1440 {
			return "version=vmaf_4k_v0.6.1"
		}
		return "version=vmaf_v0.6.1"
	}
	if !strings.Contains(cfg, "=") {
		return "version=" + cfg
	}
	return cfg
}
