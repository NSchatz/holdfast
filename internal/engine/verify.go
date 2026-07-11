package engine

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/NSchatz/transcode/internal/probe"
	"github.com/NSchatz/transcode/internal/vmaf"
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
// verifyOutput returns the measured VMAF harmonic-mean of the accepted output
// alongside its pass/fail error. The score is purely informational (it feeds the
// TRANSCODE-8 metric) — the error is what governs the gate, exactly as before; the
// score is 0 whenever VMAF did not run (disabled) and is only meaningful when the
// returned error is nil.
func (e *Engine) verifyOutput(ctx context.Context, in, tmp string) (float64, error) {
	// 1. exists & non-empty
	if probe.FileSize(tmp) <= 0 {
		return 0, fmt.Errorf("temp missing or empty")
	}

	// 2. output codec must be the engine's configured target codec (hevc or av1 —
	// TRANSCODE-6 generalizes this away from a hardcoded "hevc" so a hardware/AV1
	// encode is held to exactly the same bar as CPU libx265).
	if oc := e.Probe.VideoCodec(ctx, tmp); oc != e.targetCodec {
		return 0, fmt.Errorf("output codec is %q, not %s", oc, e.targetCodec)
	}

	// 3. length: the encode must not be truncated.
	din, okIn := e.Probe.DurationSec(ctx, in)
	dout, okOut := e.Probe.DurationSec(ctx, tmp)
	if okIn && okOut {
		// Both durations known: strict parity — a truncated encode is shorter.
		if math.Abs(din-dout) > e.Cfg.DurationToleranceSec {
			return 0, fmt.Errorf("duration parity failed (in=%.3fs out=%.3fs tol=%gs)", din, dout, e.Cfg.DurationToleranceSec)
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
				return 0, fmt.Errorf("packet-count parity failed (in=%d out=%d — truncated encode?)", pin, pout)
			}
		}
	}

	// 4. size: reclaiming space is the whole point.
	sin := probe.FileSize(in)
	sout := probe.FileSize(tmp)
	limit := float64(sin) * (1 - float64(e.Cfg.MinSavingsPercent)/100.0)
	if !(sout > 0 && float64(sout) <= limit && sout < sin) {
		return 0, fmt.Errorf("size-increase reject (in=%dB out=%dB min_savings=%d%%)", sin, sout, e.Cfg.MinSavingsPercent)
	}

	// 5. per-type stream-count parity: no audio/subtitle/attachment track dropped.
	// Size + duration + a clean decode can all pass while a track was silently lost.
	// The encode maps every stream but data, so a/s/t counts must not fall below the
	// source. Data streams are dropped on purpose and never counted.
	for _, typ := range []string{"a", "s", "t"} {
		cin := e.Probe.StreamCount(ctx, in, typ)
		cout := e.Probe.StreamCount(ctx, tmp, typ)
		if cout < cin {
			return 0, fmt.Errorf("stream-count parity failed (type=%s in=%d out=%d — a track was dropped)", typ, cin, cout)
		}
	}

	// 6. decode-integrity healthcheck on EVERY encode.
	if !e.Probe.DecodeOK(ctx, tmp) {
		return 0, fmt.Errorf("decode-integrity check failed (output does not fully decode)")
	}

	// 7. VMAF perceptual-quality gate (costliest — a second full decode — so last).
	// The structural checks prove the output exists/decodes/carries the tracks; VMAF
	// proves it still LOOKS like the source. Same resolution (codec-only), so no
	// scaling. When enabled and libvmaf is unavailable, or the measurement fails, the
	// encode is REJECTED — never accept an unmeasured output. The returned score is
	// informational (metrics); the error is the gate.
	if e.Cfg.VmafGate() {
		return e.vmafGate(ctx, tmp, in)
	}
	return 0, nil
}

// vmafGate measures the output (distorted) against the source (reference) and
// rejects an encode whose pooled harmonic-mean VMAF is below MinVmaf, or (when
// VmafMinPool > 0) whose worst-frame VMAF is below that floor. It returns the
// measured harmonic-mean (informational, for metrics) alongside the gate error; the
// score is only meaningful when the error is nil.
func (e *Engine) vmafGate(ctx context.Context, distorted, reference string) (float64, error) {
	model := resolveVmafModel(e.Cfg.VmafModel, e.Probe.Height(ctx, distorted))

	score := e.vmafScore
	if score == nil {
		if !vmaf.Available(ctx, e.Probe.FFmpeg) {
			return 0, fmt.Errorf("VMAF gate enabled but libvmaf is not available in the ffmpeg build (refusing to accept an unmeasured encode)")
		}
		score = func(ctx context.Context, d, r string, sub int, m string) (vmaf.Result, error) {
			return vmaf.Score(ctx, e.Probe.FFmpeg, d, r, sub, m)
		}
	}

	res, err := score(ctx, distorted, reference, e.Cfg.VmafSubsample, model)
	if err != nil {
		return 0, fmt.Errorf("VMAF measurement failed (refusing to accept an unmeasured encode): %w", err)
	}
	if res.HarmonicMean < e.Cfg.MinVmaf {
		return res.HarmonicMean, fmt.Errorf("VMAF below threshold (harmonic_mean=%.2f < min_vmaf=%.2f)", res.HarmonicMean, e.Cfg.MinVmaf)
	}
	if e.Cfg.VmafMinPool > 0 && res.Min < e.Cfg.VmafMinPool {
		return res.HarmonicMean, fmt.Errorf("VMAF worst-frame below floor (min=%.2f < vmaf_min_pool=%.2f)", res.Min, e.Cfg.VmafMinPool)
	}
	return res.HarmonicMean, nil
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
