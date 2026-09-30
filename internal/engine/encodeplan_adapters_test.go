package engine

import (
	"context"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/deinterlace"
	"github.com/NSchatz/holdfast/internal/downscale"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/hdr"
	"github.com/NSchatz/holdfast/internal/store"
)

// The two signatures the encode plan replaced, kept for the tests written against them.
//
// Before the plan, the per-family option builder and the acceptance gates took a job's facts
// one argument at a time. Production now hands each of them the plan instead, and the tests
// that grade them are unchanged: each adapter below assembles the facts a test hands it into
// the part of a plan the production function reads, through the same conversions the
// derivation uses (qualityOf, encoder.Spec.InputFormat), and calls the production function. What those tests assert is
// therefore still asserted of the one builder and the one gate the engine runs - no assertion
// was removed or relaxed to fit the plan, and none of those tests changed.

// buildArgs is videoArgs under its signature before the plan: the encoder, the job's
// settings and the pixel format, as a VideoPlan.
//
// The input format and the quality value are resolved exactly as the derivation resolves
// them; a pixel format the encoder cannot carry, or a quality off its scale, is a fixture the
// derivation would have refused, and panics here rather than building a command line the
// production path never could.
func buildArgs(spec encoder.Spec, ts config.Transcode, pixFmt string, colorArgs []string, x265Extra string) []string {
	input, ok := spec.InputFormat(pixFmt)
	if !ok {
		panic("buildArgs: encoder " + spec.Key + " cannot carry " + pixFmt + ", which the derivation refuses")
	}
	q, err := qualityOf(ts, spec)
	if err != nil {
		panic("buildArgs: " + err.Error())
	}
	return videoArgs(VideoPlan{Encoder: spec, PixelFormat: pixFmt, InputFormat: input, Quality: q}, colorArgs, x265Extra)
}

// verifyOutput is verifyAgainst under its signature before the plan: the profile, the target
// codec, the intended stream map and the two picture operations, as the plan the gates read.
// A remux's codec expectation is the source's, exactly as the derivation declares it.
func (e *Engine) verifyOutput(ctx context.Context, in, tmp string, prof config.Profile, targetCodec string,
	plan *StreamPlan, film deinterlace.Filter, shrink downscale.Scale) (vmafProof, string, store.FailureClass, error) {
	video := VideoPlan{Codec: targetCodec}
	if plan.RemuxOnly() {
		video = VideoPlan{Copy: true, Codec: plan.SourceVideoCodec()}
	}
	job := &EncodePlan{
		Source: in, Output: tmp,
		Profile: prof, Streams: plan, Video: video,
		Audio: CopyStreams, Subtitles: CopyStreams,
		Picture: PictureOps{Deinterlace: film, Downscale: shrink},
	}
	if !video.Copy {
		job.Metadata.Fidelity = e.adapterFidelity(ctx, in)
	}
	return e.verifyAgainst(ctx, job)
}

// adapterFidelity is the output fidelity declaration the derivation makes for a re-encode of
// in under this engine's configuration: the configured pixel format, or the one derived from
// the source's; the colour description derived from the source's tags and side data; and the
// source's HDR10 blocks. The signature before the plan carried no pixel format or colour
// description, so the adapter declares what deriveEncodePlan would, from the same inputs,
// rather than hand the gate an empty declaration it would rightly refuse.
func (e *Engine) adapterFidelity(ctx context.Context, in string) hdr.Fidelity {
	props := e.Probe.VideoProps(ctx, in)
	pixFmt := e.Cfg.PixelFormat
	if e.Cfg.PixelFormatAuto() {
		pixFmt, _ = hdr.DerivePixFmt(props.PixFmt())
	}
	color := hdr.DeriveColor(props.Color("color_primaries"), props.Color("color_transfer"),
		props.Color("color_space"), props.Color("color_range"), props.SideData())
	return hdr.FidelityOf(pixFmt, color, props.SideData())
}
