package engine

import (
	"context"
	"fmt"
	"math"

	"github.com/NSchatz/transcode/internal/probe"
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
func (e *Engine) verifyOutput(ctx context.Context, in, tmp string) error {
	// 1. exists & non-empty
	if probe.FileSize(tmp) <= 0 {
		return fmt.Errorf("temp missing or empty")
	}

	// 2. output codec must be HEVC
	if oc := e.Probe.VideoCodec(ctx, tmp); oc != "hevc" {
		return fmt.Errorf("output codec is %q, not hevc", oc)
	}

	// 3. length: the encode must not be truncated.
	din, okIn := e.Probe.DurationSec(ctx, in)
	dout, okOut := e.Probe.DurationSec(ctx, tmp)
	if okIn && okOut {
		// Both durations known: strict parity — a truncated encode is shorter.
		if math.Abs(din-dout) > e.Cfg.DurationToleranceSec {
			return fmt.Errorf("duration parity failed (in=%.3fs out=%.3fs tol=%gs)", din, dout, e.Cfg.DurationToleranceSec)
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
				return fmt.Errorf("packet-count parity failed (in=%d out=%d — truncated encode?)", pin, pout)
			}
		}
	}

	// 4. size: reclaiming space is the whole point.
	sin := probe.FileSize(in)
	sout := probe.FileSize(tmp)
	limit := float64(sin) * (1 - float64(e.Cfg.MinSavingsPercent)/100.0)
	if !(sout > 0 && float64(sout) <= limit && sout < sin) {
		return fmt.Errorf("size-increase reject (in=%dB out=%dB min_savings=%d%%)", sin, sout, e.Cfg.MinSavingsPercent)
	}

	// 5. per-type stream-count parity: no audio/subtitle/attachment track dropped.
	// Size + duration + a clean decode can all pass while a track was silently lost.
	// The encode maps every stream but data, so a/s/t counts must not fall below the
	// source. Data streams are dropped on purpose and never counted.
	for _, typ := range []string{"a", "s", "t"} {
		cin := e.Probe.StreamCount(ctx, in, typ)
		cout := e.Probe.StreamCount(ctx, tmp, typ)
		if cout < cin {
			return fmt.Errorf("stream-count parity failed (type=%s in=%d out=%d — a track was dropped)", typ, cin, cout)
		}
	}

	// 6. decode-integrity healthcheck on EVERY encode (costliest, so last).
	if !e.Probe.DecodeOK(ctx, tmp) {
		return fmt.Errorf("decode-integrity check failed (output does not fully decode)")
	}
	return nil
}
