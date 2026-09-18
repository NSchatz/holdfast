// Package deinterlace is the registry of the deinterlacing filters this build will run,
// and the one place that decides what a `deinterlace` profile value MEANS.
//
// It exists for the same reason internal/encoder does: the value an operator writes has to
// resolve to one filter expression, and that ONE expression has to reach the encoder, the
// perceptual gate's reference and the terminal row. A score measured against a reference
// built by a different filter, or by the same filter at different parameters, is not a
// measurement of the encode that is about to replace somebody's file - so the expression is
// resolved once, here, and carried rather than re-derived.
//
// Everything this package accepts is FRAME-RATE-PRESERVING, and that is a property of the
// output rather than a claim about a filter name: a mode that emits one frame per FIELD
// doubles the frame count, which breaks packet-count parity and complicates duration
// parity, and neither gate may be weakened to let such an output through. The doubling
// spellings are therefore RESOLVABLE and REFUSED rather than unknown - a value this build
// can name and will not run is refused in words that say why, where an unknown key would
// send an operator looking for a typo.
package deinterlace

import "strings"

// Off is the value that means "do not deinterlace". It is spelled once here, and the empty
// string means the same thing, which is what a Profile assembled in Go rather than read
// from a file carries.
const Off = "off"

// The filters this build accepts, and the ONLY ones. Both are software filters in the
// pinned ffmpeg build, both read a single field-parity option, and both preserve the frame
// rate in their default mode.
const (
	// Yadif is the long-standing "yet another deinterlacing filter": cheap, and what most
	// libraries of pre-2010 broadcast material have been deinterlaced with for a decade.
	Yadif = "yadif"
	// Bwdif is the Bob Weaver deinterlacer: yadif's motion adaptivity with a better
	// interpolator, at more CPU. It is offered because the cost is the operator's to spend
	// and the quality difference is visible on exactly the content this feature is for.
	Bwdif = "bwdif"
)

// The four modes yadif and bwdif share, spelled as ffmpeg spells them. Two of them emit one
// frame per FIELD and are refused (see Filter.EmitsOneFramePerField); they are named here
// rather than left out because a value that is refused by name is a value an operator can
// act on.
const (
	modeSendFrame          = "send_frame"
	modeSendField          = "send_field"
	modeSendFrameNoSpatial = "send_frame_nospatial"
	modeSendFieldNoSpatial = "send_field_nospatial"
)

// modes is the accepted mode set, in the order a refusal lists them.
var modes = []string{modeSendFrame, modeSendField, modeSendFrameNoSpatial, modeSendFieldNoSpatial}

// filters is the accepted filter set, in the order a refusal lists them.
var filters = []string{Yadif, Bwdif}

// Filter is one RESOLVED deinterlace: the value as the operator wrote it, and the ffmpeg
// filter expression it means.
//
// The expression is the whole of what is applied and the whole of what is recorded. It
// carries the mode and the parity explicitly rather than relying on a default, because the
// terminal row has to say what produced the replacement and "yadif" alone does not: the
// same name at a different mode is a different transformation, and the row outlives the
// source it describes.
type Filter struct {
	// Value is the configuration value this filter was resolved from ("" when none).
	Value string
	// Name is the ffmpeg filter this build runs (yadif or bwdif), "" when disabled.
	Name string
	// Spec is the full ffmpeg filter expression, ready to be composed into a filtergraph.
	// "" when disabled.
	Spec string
}

// Enabled reports whether this filter deinterlaces anything at all.
func (f Filter) Enabled() bool { return f.Spec != "" }

// EmitsOneFramePerField reports whether this filter's expression would produce one output
// frame per input FIELD, doubling the frame rate.
//
// It is read OFF THE EXPRESSION rather than off a flag set at resolution time, and that is
// the point: the engine's refusal (and the encoder's backstop behind it) must judge what
// would actually run, so a Filter assembled by hand - by a test, by a caller that never
// went through Lookup, by whatever this package grows next - is judged by the same reading.
// A flag can be set wrongly; the expression is what ffmpeg is handed.
func (f Filter) EmitsOneFramePerField() bool {
	switch modeOf(f.Spec) {
	case modeSendField, modeSendFieldNoSpatial, "1", "3":
		return true
	default:
		return false
	}
}

// modeOf reads the `mode=` option out of a filter expression, or "" where it carries none
// (which for yadif and bwdif is send_frame, their own default).
func modeOf(spec string) string {
	_, opts, ok := strings.Cut(spec, "=")
	if !ok {
		return ""
	}
	for _, opt := range strings.Split(opts, ":") {
		if k, v, ok := strings.Cut(opt, "="); ok && strings.TrimSpace(k) == "mode" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Lookup resolves a `deinterlace` configuration value to the filter it names.
//
// The accepted spellings are `off` (and the empty string, which a Profile assembled in Go
// carries), a bare filter name, and a filter name with an explicit mode: `yadif`,
// `bwdif=send_field`. A bare name takes send_frame, which is ffmpeg's own default for both
// filters and the only one that preserves the frame rate.
//
// ok is false for a value this build cannot resolve at all. A value it CAN resolve but will
// not run - a field-doubling mode - resolves here and is refused by the caller that asks
// EmitsOneFramePerField, in words naming what is wrong with it. The two are deliberately
// different answers: "I do not know what you wrote" and "I know exactly what you wrote and
// will not do it" send an operator to different places.
func Lookup(value string) (Filter, bool) {
	v := strings.TrimSpace(value)
	if v == "" || v == Off {
		return Filter{Value: v}, true
	}
	name, mode, hasMode := strings.Cut(v, "=")
	if !known(filters, name) {
		return Filter{}, false
	}
	if !hasMode {
		mode = modeSendFrame
	}
	if !known(modes, mode) {
		return Filter{}, false
	}
	return Filter{
		Value: v,
		Name:  name,
		// parity=auto reads the field order out of the stream, which is the same answer the
		// guard in front of this read from ffprobe; deint=all deinterlaces every frame,
		// which is right for a source already established as interlaced and does not rest
		// on a per-frame flag an old container may not carry.
		Spec: name + "=mode=" + mode + ":parity=auto:deint=all",
	}, true
}

// Known returns the values this build accepts, for the refusal that has to say so. The
// mode-carrying spellings are described rather than enumerated: there are ten of them and a
// list that long in an error message is not read.
func Known() []string {
	out := []string{Off}
	for _, f := range filters {
		out = append(out, f, f+"=<mode>")
	}
	return out
}

// Modes returns the accepted modes, for the same refusal.
func Modes() []string { return append([]string(nil), modes...) }

func known(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
