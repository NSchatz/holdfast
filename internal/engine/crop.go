package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/NSchatz/holdfast/internal/crop"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// The crop, resolved in ONE place for the sites that must agree about it: the encoder that
// cuts the bars away, the perceptual gate that builds its reference through the same crop,
// the crop gate that holds the output to it, and the terminal row that records it
// (docs/design/crop.md#crop).
//
// They agree because none of them resolves it: the engine samples the source once, the encode
// plan decides once (cropApplied, from deriveEncodePlan), and every one of them reads the
// plan's Picture.Crop.

// cropApplied is the crop decision for THIS source: nothing where the root does not ask for
// one (consensus nil), and otherwise crop.Decide over the samples, the source's frame and
// pixel format, the format the encoder is handed and the source's own Dolby Vision class -
// read from the snapshot here, so a DV source is refused by the decision itself whatever the
// guards in front of it did.
//
// props may be nil (a direct caller of the encoder that did not pre-probe): the decision is
// then refused for an unknown frame, never taken against a guessed one.
func cropApplied(consensus *crop.Consensus, props *probe.VideoProps, outPixFmt string) crop.Decision {
	if consensus == nil {
		return crop.Decision{}
	}
	if props == nil {
		return crop.Refuse(crop.Frame{}, crop.ReasonUnknownFrame, "the source was not probed")
	}
	return crop.Decide(cropInputs(*consensus, props, outPixFmt))
}

// cropInputs are the decision's inputs read off a source's snapshot.
func cropInputs(consensus crop.Consensus, props *probe.VideoProps, outPixFmt string) crop.Inputs {
	var f crop.Frame
	if w, h, ok := props.Dimensions(); ok {
		f = crop.Frame{W: w, H: h}
	}
	return crop.Inputs{
		Frame:        f,
		PixelFormats: []string{props.PixFmt(), outPixFmt},
		DolbyVision:  crop.DolbyVisionOf(props.CodecTag(), props.SideData(), props.Color("color_transfer")),
		Consensus:    consensus,
	}
}

// cropBuildable refuses a crop the derivation could not have made: a rectangle outside its
// frame, or one not aligned to the chroma subsampling of the format the encoder is handed.
// The derivation never produces either; this is the builder's backstop, so a plan assembled
// any other way cannot hand the encoder a crop it would round on its own.
func (o PictureOps) cropBuildable(pixFmt string) error {
	d := o.Crop
	if !d.Applied() {
		return nil
	}
	ax, ay, ok := crop.Alignment(pixFmt)
	if !d.Rect.Inside(d.Frame) || !ok || d.Rect.W%ax != 0 || d.Rect.H%ay != 0 || d.Rect.X%ax != 0 || d.Rect.Y%ay != 0 {
		return &UnbuildablePlanError{What: fmt.Sprintf("the crop %q of a %dx%d frame for the pixel format %q",
			d.Rect, d.Frame.W, d.Frame.H, pixFmt)}
	}
	return nil
}

// withCrop composes the crop into a job's video filter chain, and is the ONE place in the
// argv where the picture is cut. A decision that crops nothing returns the arguments
// untouched, which keeps the command line of every job that crops nothing byte for byte what
// it was; one that crops is composed into the chain on withDeinterlace's terms, at its head,
// so it runs on software frames before anything uploads them to a device.
func withCrop(args []string, d crop.Decision) []string {
	if !d.Applied() {
		return args
	}
	return withHeadFilter(args, d.Rect.Spec())
}

// referenceChain is the filter expression the perceptual gate's REFERENCE is put through: the
// deinterlace the encode ran, then the crop it ran, in the order the encode ran them, so the
// reference is the source transformed exactly as the encode transformed it - and never a
// scale (see internal/vmaf).
func referenceChain(film string, cut crop.Rect) string {
	switch {
	case cut.Empty():
		return film
	case film == "":
		return cut.Spec()
	}
	return film + "," + cut.Spec()
}

// cropConsensus samples a source whose root asks for a crop, and is nil where it does not or
// where the job re-encodes nothing: a root without `crop: auto` runs no detection, probes
// nothing more and records nothing (brief I5). A Dolby Vision source is not sampled at all -
// the decision refuses it before reading the samples - so it costs nothing.
func (e *Engine) cropConsensus(ctx context.Context, f string, props *probe.VideoProps, remux bool) *crop.Consensus {
	if remux {
		// The derivation records the remux's own refusal; nothing is sampled for a copy.
		c := crop.Consensus{}
		return &c
	}
	if crop.DolbyVisionOf(props.CodecTag(), props.SideData(), props.Color("color_transfer")).Present {
		c := crop.Consensus{}
		return &c
	}
	var c crop.Consensus
	if e.cropDetect != nil {
		c = e.cropDetect(ctx, f, props)
	} else {
		var frame crop.Frame
		if w, h, ok := props.Dimensions(); ok {
			frame = crop.Frame{W: w, H: h}
		}
		dur, _ := e.Probe.DurationSec(ctx, f)
		c = crop.Detect(ctx, e.Probe.FFmpeg, f, dur, frame)
	}
	return &c
}

// cropPrecheck is the blackness check run BEFORE the encode, over the rectangle the plan
// declares: a crop whose bars are not black is refused here and the file is encoded
// uncropped in the same attempt, rather than encoded cropped and then refused.
func (e *Engine) cropPrecheck(ctx context.Context, job *EncodePlan, props *probe.VideoProps) error {
	if e.cropBlackness != nil {
		return e.cropBlackness(ctx, job.Source, job.Picture.Crop, props.PixFmt())
	}
	return crop.Blackness(ctx, e.Probe.FFmpeg, job.Source, job.Picture.Crop.Rect, job.Picture.Crop.Frame, props.PixFmt())
}

// cropGate is the crop's own gate, run on every job whose plan crops and on no other: the
// output is the size the plan declares - the crop's rectangle, or the scale's target where the
// cropped picture was then scaled - and the area the crop removed from the SOURCE is black on
// every frame, measured again over the declared rectangle (docs/design/crop.md#crop-gate).
//
// Classes: an output of the wrong size is DETERMINISTIC - the same plan builds the same
// command line. Bars that are not black, or a check that could not run, are TRANSIENT: the
// same check ran before the encode and passed, so a failure here is a property of the run,
// and the next attempt runs that check again and encodes uncropped if it fails there.
func (e *Engine) cropGate(ctx context.Context, job *EncodePlan) (string, store.FailureClass, error) {
	want := job.Picture
	ww, wh, _ := want.OutputSize()
	gw, gh, ok := e.Probe.Dimensions(ctx, job.Output)
	if !ok || gw != ww || gh != wh {
		return GateCrop, store.FailureDeterministic, fmt.Errorf("crop gate: the output is %s and its plan declares "+
			"%dx%d (the crop %s of the %dx%d source). The source is kept", sizeOf(gw, gh, ok), ww, wh,
			want.Crop.Rect, want.Crop.Frame.W, want.Crop.Frame.H)
	}
	err := crop.Blackness(ctx, e.Probe.FFmpeg, job.Source, want.Crop.Rect, want.Crop.Frame, e.Probe.PixFmt(ctx, job.Source))
	if err != nil {
		var nb *crop.NotBlackError
		what := "could not be established as black"
		if errors.As(err, &nb) {
			what = "is not black"
		}
		return GateCrop, store.FailureTransient, fmt.Errorf("crop gate: the area the crop %s removes from the "+
			"source %s (%w): the replacement would have lost picture. The source is kept", want.Crop.Rect, what, err)
	}
	return "", "", nil
}

func sizeOf(w, h int, ok bool) string {
	if !ok {
		return "of a size that could not be measured"
	}
	return fmt.Sprintf("%dx%d", w, h)
}

// cropRecord is what the terminal row records of a job's crop: nothing where the root did not
// ask for one, and otherwise the rectangle kept or the reason none was.
func (o PictureOps) cropRecord() store.Crop {
	d := o.Crop
	if !d.Applied() && d.Reason == "" {
		return store.Crop{}
	}
	if d.Applied() {
		return store.RecordCrop(store.CropRecord{Applied: true, Rect: d.Rect.String(),
			Frame: fmt.Sprintf("%dx%d", d.Frame.W, d.Frame.H)})
	}
	return store.RecordCrop(store.CropRecord{Reason: d.Reason, Detail: d.Detail})
}
