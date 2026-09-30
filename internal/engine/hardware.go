package engine

import (
	"context"
	"errors"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/hdr"
)

// Hardware is what this run's start-time probes established about each hardware encoder the
// configuration can reach, keyed by registry key (encoder.Available, through the job's own
// command line). The engine reads it to resolve `encoder: auto` and to decide, per job,
// whether a hardware encoder may run the job's plan or hw_fallback decides instead.
//
// A nil Hardware is a run that probed nothing: every configured encoder runs as named, as it
// did before detection existed (the start-time check has already refused one that does not
// work), and `auto` finds no hardware. An encoder absent from a non-nil Hardware was not
// probed and is likewise run as named.
type Hardware map[string]encoder.Capability

// resolveEncoder is the encoder that runs a job whose settings are ts and whose plan's pixel
// format is planFmt, or the hardware-unavailable skip where hw_fallback says skip and no
// usable hardware encoder carries the plan (docs/design/hardware.md#auto,
// docs/design/hardware.md#fallback).
//
//   - `auto`: the first encoder in encoder.AutoOrder whose probe passed at the plan's depth,
//     for a 4:2:0 plan (the layout the probe encodes; a plan of another chroma subsampling is
//     never handed to hardware by `auto`), whose pixel-format list carries the plan; else cpu
//     under hw_fallback software; else the skip.
//   - a hardware encoder whose probe did not pass at the plan's depth: its software encoder
//     under hw_fallback software (cpu, or svtav1 for av1_nvenc); else the skip.
//   - anything else: the encoder named.
func (e *Engine) resolveEncoder(prof config.Profile, ts config.Transcode, planFmt, codec string) (string, sourceVerdict) {
	software := prof.HWFallbackMode() == config.HWFallbackSoftware
	skip := func(named string, why string) (string, sourceVerdict) {
		return "", sourceVerdict{guard: SkipHardwareUnavailable, codec: codec,
			log: "skip (no usable hardware encoder carries this job, and hw_fallback is skip: the file stays as it is)",
			logArgs: []any{"encoder", named, "pix_fmt", planFmt, "why", why, "lever",
				"hw_fallback: software encodes such a job with the software encoder of the same codec"}}
	}
	if ts.Encoder == encoder.Auto {
		for _, key := range encoder.AutoOrder {
			if e.hardwareCarries(key, planFmt) && autoLayout(planFmt) {
				return key, sourceVerdict{}
			}
		}
		if software {
			return encoder.SoftwareFallback(mustLookup(encoder.AutoOrder[0])).Key, sourceVerdict{}
		}
		return skip(encoder.Auto, e.autoWhy(planFmt))
	}
	spec, ok := encoder.Lookup(ts.Encoder)
	if !ok || !spec.Hardware || e.Hardware == nil {
		return ts.Encoder, sourceVerdict{}
	}
	c, probed := e.Hardware[spec.Key]
	if !probed || c.Carries(planFmt) {
		return spec.Key, sourceVerdict{}
	}
	if software {
		return encoder.SoftwareFallback(spec).Key, sourceVerdict{}
	}
	return skip(spec.Key, c.Reason)
}

// hardwareCarries reports whether key's probe passed at planFmt's depth and its pixel-format
// list carries planFmt.
func (e *Engine) hardwareCarries(key, planFmt string) bool {
	c, ok := e.Hardware[key]
	if !ok || !c.Carries(planFmt) {
		return false
	}
	_, ok = mustLookup(key).InputFormat(planFmt)
	return ok
}

// autoLayout reports whether planFmt is a layout `auto` hands to hardware: 4:2:0, the one the
// probe encodes.
func autoLayout(planFmt string) bool {
	l, ok := hdr.PixelLayout(planFmt)
	return ok && l.Chroma == "420"
}

// autoWhy says why `auto` found no hardware encoder for planFmt.
func (e *Engine) autoWhy(planFmt string) string {
	if !autoLayout(planFmt) {
		return "auto hands only 4:2:0 plans to hardware (the layout the probe encodes)"
	}
	why := ""
	for _, key := range encoder.AutoOrder {
		c, ok := e.Hardware[key]
		reason := "not probed"
		if ok {
			reason = c.Reason
			if c.Carries(planFmt) {
				reason = "its pixel formats do not carry " + planFmt
			}
		}
		if why != "" {
			why += "; "
		}
		why += key + ": " + reason
	}
	return why
}

func mustLookup(key string) encoder.Spec {
	spec, ok := encoder.Lookup(key)
	if !ok {
		panic("encoder registry has no " + key)
	}
	return spec
}

// encodeFallback is the software encoder a failed encode of job is retried with, or "" where
// none is: the encode failed (err), was not interrupted, was not aborted for memory, ran a
// hardware encoder, and the root's hw_fallback is software.
func (e *Engine) encodeFallback(ctx context.Context, prof config.Profile, job *EncodePlan, err error) string {
	if err == nil || ctx.Err() != nil || prof.HWFallbackMode() != config.HWFallbackSoftware {
		return ""
	}
	var mem *MemoryAbortError
	if errors.As(err, &mem) || !job.Video.Encoder.Hardware {
		return ""
	}
	return encoder.SoftwareFallback(job.Video.Encoder).Key
}
