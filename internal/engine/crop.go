package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/crop"
	"github.com/NSchatz/holdfast/internal/dynhdr"
	"github.com/NSchatz/holdfast/internal/hdr"
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
func cropApplied(consensus *crop.Consensus, l5 *crop.L5Reading, props *probe.VideoProps, outPixFmt string) crop.Decision {
	if consensus == nil {
		return crop.Decision{}
	}
	if props == nil {
		return crop.Refuse(crop.Frame{}, crop.ReasonUnknownFrame, "the source was not probed")
	}
	in := cropInputs(*consensus, props, outPixFmt)
	in.DolbyVision.L5 = l5
	return crop.Decide(in)
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
func (e *Engine) cropConsensus(ctx context.Context, f string, props *probe.VideoProps, remux, dvCarried bool) *crop.Consensus {
	if remux {
		// The derivation records the remux's own refusal; nothing is sampled for a copy.
		c := crop.Consensus{}
		return &c
	}
	// A Dolby Vision source is sampled only where this job carries its Dolby Vision, the one
	// case its crop can be decided (docs/design/crop.md#dolby-vision).
	if !dvCarried && crop.DolbyVisionOf(props.CodecTag(), props.SideData(), props.Color("color_transfer")).Present {
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

// cropL5 reads what the Dolby Vision crop decision rests on, for a source whose Dolby Vision
// this job carries: its frame count and constant rate (an L5 zeroing writes a raw stream read
// at one rate), and every frame's L5 through dovi_tool (dynhdr.ReadL5), into working files
// beside work that removeDynamicTemps removes. Every failure is a reading the decision
// refuses by name, never an error that stops the job.
func (e *Engine) cropL5(ctx context.Context, f, work string) *crop.L5Reading {
	r := &crop.L5Reading{}
	frames, rateErr := dynhdr.RewriteFacts(ctx, e.Probe.FFprobe, absolutePath(f))
	if rateErr != nil {
		r.FrameRate = rateErr.Error()
		return r
	}
	recs, err := dynhdr.ReadL5(ctx, e.dynamicTools(), absolutePath(f),
		absolutePath(dynamicTempPath(work, dynamicSrcRPU)), absolutePath(dynamicTempPath(work, dynamicSrcL5)))
	if err != nil {
		r.Failed = err.Error()
		return r
	}
	r.Frames, r.L5 = frames, map[int]crop.Edges{}
	for _, rec := range recs {
		r.L5[rec.Frame] = crop.Edges{Left: rec.Left, Right: rec.Right, Top: rec.Top, Bottom: rec.Bottom}
	}
	return r
}

// cropBlacknessOf is the pre-encode blackness check of a decision, through the test seam
// where one is set.
func (e *Engine) cropBlacknessOf(ctx context.Context, src string, d crop.Decision, pixFmt string) error {
	if e.cropBlackness != nil {
		return e.cropBlackness(ctx, src, d, pixFmt)
	}
	return crop.Blackness(ctx, e.Probe.FFmpeg, src, d.Rect, d.Frame, pixFmt)
}

// l5Gate is the L5 gate (docs/design/crop.md#l5-gate), run on every job that cropped a Dolby
// Vision source to its RPU's own active area and on no other: the output carries exactly one
// L5 record per decoded frame and every one is 0/0/0/0, read with dovi_tool's
// `export -l level5`, which writes no record for a frame without L5, so a dropped L5 cannot
// pass as a zeroed one. A failure is TRANSIENT and remembered for the source: the next attempt
// in this process encodes it uncropped, its Dolby Vision carried as it is.
func (e *Engine) l5Gate(ctx context.Context, job *EncodePlan) (string, store.FailureClass, error) {
	fail := func(err error) (string, store.FailureClass, error) {
		e.cropL5Failed.Store(job.Source, err.Error())
		return GateDolbyVisionL5, store.FailureTransient, fmt.Errorf("%w. The source is kept, and its next attempt "+
			"is encoded uncropped", err)
	}
	counts, err := dynhdr.Count(ctx, e.Probe.FFprobe, job.Output)
	if err != nil {
		return fail(&dynhdr.GateError{Gate: GateDolbyVisionL5, Why: "the output's frames could not be counted (" + err.Error() + ")"})
	}
	recs, err := dynhdr.ReadL5(ctx, e.dynamicTools(), absolutePath(job.Output),
		absolutePath(dynamicTempPath(job.Output, dynamicOutRPU)), absolutePath(dynamicTempPath(job.Output, dynamicOutL5)))
	if err != nil {
		return fail(&dynhdr.GateError{Gate: GateDolbyVisionL5, Why: "the output's L5 could not be read (" + err.Error() + ")"})
	}
	if err := dynhdr.CheckZeroL5(recs, counts.Frames); err != nil {
		return fail(err)
	}
	return "", "", nil
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
			Frame: fmt.Sprintf("%dx%d", d.Frame.W, d.Frame.H), L5Zeroed: d.ZeroesL5()})
	}
	return store.RecordCrop(store.CropRecord{Reason: d.Reason, Detail: d.Detail})
}

// roomIntent is the dynamic-HDR intent the room check sizes a job for: a Dolby Vision source
// under `crop: auto` may have its L5 zeroed into a raw stream as large as its video, which is
// decided only after the check, so the check counts it whenever it could happen.
func roomIntent(in dynhdr.Intent, prof config.Profile) dynhdr.Intent {
	if prof.CropEnabled() && in.DolbyVision {
		in.ZeroL5 = true
	}
	return in
}

// cropBeforePrePass samples a source under `crop: auto` and, where this job carries its Dolby
// Vision, reads its L5 and decides the crop then, because the pre-pass that runs next is what
// zeroes L5: a crop to L5's own rectangle whose removed area is black asks the pre-pass for
// the zeroing (intent.ZeroL5); anything else leaves the intent as it was and the source is
// encoded uncropped with its Dolby Vision carried. The derivation takes the same decision
// again from the same inputs and refuses a plan where the two differ.
//
// A source whose earlier attempt in this process failed the L5 gate is refused the crop.
func (e *Engine) cropBeforePrePass(ctx context.Context, f, work string, props *probe.VideoProps, ts config.Transcode,
	remux bool, intent dynhdr.Intent) (*crop.Consensus, *crop.L5Reading, dynhdr.Intent) {
	cropIn := e.cropConsensus(ctx, f, props, remux, intent.DolbyVision)
	if remux || !intent.DolbyVision {
		return cropIn, nil, intent
	}
	if why, failed := e.cropL5Failed.Load(f); failed {
		c := crop.Consensus{Reason: crop.ReasonL5GateFailed, Detail: "an earlier attempt's cropped output failed the L5 gate: " +
			fmt.Sprint(why)}
		return &c, e.cropL5(ctx, f, work), intent
	}
	l5 := e.cropL5(ctx, f, work)
	outFmt := ts.PixelFormat
	if ts.PixelFormatAuto() {
		outFmt, _ = hdr.DerivePixFmt(props.PixFmt())
	}
	in := cropInputs(*cropIn, props, outFmt)
	in.DolbyVision.L5 = l5
	d := crop.Decide(in)
	if !d.ZeroesL5() {
		return cropIn, l5, intent
	}
	if err := e.cropBlacknessOf(ctx, f, d, props.PixFmt()); err != nil {
		e.Log.Info("crop refused: the area it would remove is not black; encoding uncropped", "file", f,
			"crop", d.Rect.String(), "err", err)
		c := crop.Consensus{Reason: crop.ReasonBarsNotBlack, Detail: err.Error()}
		return &c, l5, intent
	}
	intent.ZeroL5 = true
	return cropIn, l5, intent
}
