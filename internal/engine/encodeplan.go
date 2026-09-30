package engine

import (
	"fmt"
	"strconv"
	"sync/atomic"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/deinterlace"
	"github.com/NSchatz/holdfast/internal/downscale"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/hdr"
	"github.com/NSchatz/holdfast/internal/probe"
)

// THE ENCODE PLAN: everything one job's encode does to its source, declared once, before the
// encode runs - and the one description the command line is built from AND every acceptance
// gate is checked against (docs/design/encode-plan.md#encode-plan).
//
// Before it, each of those resolved its own answer from the same inputs through the same
// helpers: the encoder resolved the settings, the pixel format, the deinterlace and the scale
// for itself, and the engine resolved them again for the gates. Two resolutions that agree
// today are two answers waiting to disagree, and the disagreement would land exactly where it
// costs most - a gate checking an output against something other than what was encoded, and
// passing it, after which the source is deleted. So the answer is derived ONCE, here, handed
// to the encoder and to the gates, and neither of them derives any part of it again.
//
// A plan DECLARES; the command-line builder EXECUTES what it declares, and refuses what it
// cannot. Every slot a later capability fills - a hardware decode path, an audio re-encode, a
// subtitle sidecar, a crop, a dynamic HDR carrier - is declared here with the one value this
// build derives, and a plan carrying any other value is refused by name rather than built
// without it: an operation silently not performed is a replacement that is not what the plan
// says it is.

// encodePlanID mints one identity per derivation, exactly as planID does for the intended
// stream map: the only question ever asked of it is whether two plans came from ONE
// derivation.
var encodePlanID atomic.Int64

// DecodeSoftware is the one decode path this build declares: the source is decoded by
// ffmpeg's software decoders, and no -hwaccel reaches a command line.
const DecodeSoftware = "software"

// StreamAction is what an encode does to every carried stream of one type.
type StreamAction string

// CopyStreams stream-copies a carried stream: the output carries the bytes the source did.
// It is the only action this build takes on an audio or a subtitle stream.
const CopyStreams StreamAction = "copy"

// EncodePlan is one job's declared encode. It is derived once per job by deriveEncodePlan
// and read, never re-derived, by the command-line builder (args) and by every acceptance
// gate (Engine.verifyAgainst).
type EncodePlan struct {
	// id identifies the derivation. Unexported and set only by deriveEncodePlan, so a plan
	// cannot be forged with another derivation's identity.
	id int64

	// Source is the file the encode reads and Output the working file it writes: the two
	// paths the plan was derived for. A plan is never built into a command line for any
	// other pair.
	Source, Output string

	// Profile is the effective library profile that decides this file: the root's values
	// with the first matching resolution rule laid over them. Every floor a gate holds the
	// output to is read from it.
	Profile config.Profile
	// Settings are this job's effective encode settings: Profile's encode keys overlaid with
	// the first matching encode profile, and the name of the encode profile that supplied
	// them.
	Settings config.Transcode

	// Streams is the intended stream map: which source streams the output carries. It is nil
	// only on the plan a direct caller of the encoder derives without one, which carries every
	// stream but data.
	Streams *StreamPlan

	// Video is what the encode does to the video.
	Video VideoPlan
	// Audio and Subtitles are what the encode does to every carried audio and subtitle
	// stream.
	Audio, Subtitles StreamAction
	// Picture is what the encode does to the picture on its way to the encoder.
	Picture PictureOps
	// Metadata is the colour description and the metadata the output carries.
	Metadata MetadataPlan

	// container is the muxer the output is written in, named on the command line rather than
	// left to ffmpeg to infer from the output's name (see container.go).
	container outputContainer
	// coverArt is how the attached pictures the map carries reach the output.
	coverArt coverArtCarriage
}

// VideoPlan is what one encode does to the video.
type VideoPlan struct {
	// Copy says the video is stream-copied: a remux-only root re-encodes nothing, and every
	// field below but Codec is empty.
	Copy bool
	// Codec is what ffprobe must report the output's video to be in: what the encoder
	// produces, or - on a copy - what the source's video already was.
	Codec string
	// Encoder is the registry encoder that produces the video.
	Encoder encoder.Spec
	// Device is the device node the encoder opens, and "" for every encoder this build opens
	// none for: only VAAPI is told which device to use.
	Device string
	// Decode is how the source is decoded: DecodeSoftware.
	Decode string
	// PixelFormat is the pixel format the video is encoded to: the one the settings force,
	// or the one derived from the source's so that its chroma subsampling is kept and its
	// bit depth is floored at 10.
	PixelFormat string
	// Quality is the rate control the encode runs under.
	Quality Quality
}

// Quality is the rate control of one video encode.
type Quality struct {
	// CRF is the quality target. Each encoder family reads it in its own spelling (-crf,
	// -cq, -global_quality, -qp), and none reads it where BitrateKbps is set.
	CRF int
	// BitrateKbps is the target bitrate, and 0 where the encode is quality-targeted.
	BitrateKbps int
	// Preset is the speed and efficiency word, in libx265's spelling.
	Preset string
}

// TargetsBitrate reports whether the encode runs under a target bitrate rather than the
// quality target.
func (q Quality) TargetsBitrate() bool { return q.BitrateKbps > 0 }

// qualityOf is the rate control a job's effective settings resolve to, and the one place
// they are read for it.
func qualityOf(ts config.Transcode) Quality {
	return Quality{CRF: ts.CRF, BitrateKbps: ts.BitrateKbps, Preset: ts.Preset}
}

// PictureOps are the operations on the picture itself, in the order the filter chain runs
// them: the deinterlace at the source's own resolution, then the scale.
type PictureOps struct {
	// Deinterlace is the deinterlace applied: the profile's filter where the source reports
	// an interlaced field order, and none otherwise.
	Deinterlace deinterlace.Filter
	// Downscale is the resolution ceiling applied: a scale where the source is taller than
	// the profile's max_height, and none otherwise.
	Downscale downscale.Scale
	// Crop is the crop applied, as a filter expression. This build derives none, so it is
	// always "", and a plan carrying one is refused rather than encoded uncropped.
	Crop string
}

// MetadataPlan is the colour description and the metadata the output carries.
type MetadataPlan struct {
	// Color is the colour description written into the output, with the HDR10 static
	// metadata where the source carries it.
	Color hdr.Color
	// Fidelity is what the output must carry to be a faithful replacement: the bit depth
	// and chroma subsampling of PixelFormat, the colour tags of Color, and the HDR10
	// static-metadata blocks the source carries. The output fidelity gate holds the output
	// to it (docs/design/encode-plan.md#fidelity). It is zero on a stream copy, which is
	// held to bit-identity instead.
	Fidelity hdr.Fidelity
	// HDR10Plus and DolbyVision report whether the output carries that dynamic metadata.
	// This build carries neither - the guards skip a source that has either before a plan
	// is derived - so both are false, and a plan claiming either is refused rather than
	// encoded without it.
	HDR10Plus, DolbyVision bool
}

// coverArtCarriage is how the attached pictures the map carries reach the output: pinned to
// copy in the map, or - into Matroska - carried as attachments copied out of the source
// first (see matroskaPictures).
type coverArtCarriage struct {
	// pinned are the output video-relative indexes (the N of `v:N`) of the attached pictures
	// the map carries, each pinned back to copy after the blanket video codec.
	pinned []int
	// attached are the pictures carried as Matroska attachments instead of through the map.
	attached []matroskaPicture
}

// ID identifies the derivation that produced this plan: two plans share an ID only when
// they are the same derivation.
func (p *EncodePlan) ID() int64 {
	if p == nil {
		return 0
	}
	return p.id
}

// SameEncodePlan reports whether two plans came from ONE derivation. It compares IDENTITY
// and never content: two derivations that agree on a fixture are still two answers waiting
// to disagree on a file that is not the fixture. A plan no derivation produced has no
// identity, and is the same derivation as nothing.
func SameEncodePlan(a, b *EncodePlan) bool {
	return a != nil && b != nil && a.id != 0 && a.id == b.id
}

// planInputs are everything one derivation of an encode plan reads, and nothing else
// reaches it.
type planInputs struct {
	// settings are the job's effective encode settings, resolved by the caller from its own
	// configuration (config.Config.TranscodeIn): the engine resolves them once per job, before
	// its guards read them, and hands the same value here.
	settings config.Transcode
	// prof is the effective library profile that decides the file.
	prof config.Profile
	// source and output are the paths the encode reads and writes.
	source, output string
	// streams is the intended stream map; nil where a direct caller of the encoder handed
	// none.
	streams *StreamPlan
	// snapshot returns the source's probe snapshot. It is called at the one point the
	// derivation first needs the source's properties, so a plan refused before then never
	// probes, and it is how the engine hands in the snapshot its guards already read.
	snapshot func() (*probe.VideoProps, error)
}

// deriveEncodePlan is THE derivation of an encode plan, and the only one in this build. The
// engine derives each job's plan here and hands the one value to the encoder and to the
// gates; a direct caller of the encoder that hands none gets its plan derived here too, by the
// same function, from the configuration, profile and stream map that encoder holds.
//
// The refusals come in the order the encoder has always met them, so a job that could not
// be encoded fails with the reason it always failed with: an output container this build
// cannot name, an attached picture Matroska cannot carry, then - for anything that
// re-encodes the video - an unknown encoder, a snapshot that cannot be taken (a direct
// caller's encoder with no prober), a source pixel format with no faithful derivation, a
// deinterlace this build refuses to run, and a source whose video streams the probe could
// not establish.
func deriveEncodePlan(in planInputs) (*EncodePlan, error) {
	container, err := outputContainerFor(in.output)
	if err != nil {
		return nil, err
	}
	attached, err := matroskaPictures(in.output, in.streams)
	if err != nil {
		return nil, err
	}
	ts := in.settings
	p := &EncodePlan{
		id:     encodePlanID.Add(1),
		Source: in.source, Output: in.output,
		Profile: in.prof, Settings: ts,
		Streams: in.streams,
		Audio:   CopyStreams, Subtitles: CopyStreams,
		container: container,
		coverArt:  coverArtCarriage{attached: attached},
	}

	// A REMUX-ONLY job re-encodes nothing, so it has none of what follows: no encoder, no
	// pixel format, no picture operation and no colour description, because there is no
	// encode for any of them to describe. Its video is what the source's was.
	if in.streams.RemuxOnly() {
		p.Video = VideoPlan{Copy: true, Codec: in.streams.SourceVideoCodec()}
		return p, nil
	}

	spec, ok := encoder.Lookup(ts.Encoder)
	if !ok {
		return nil, fmt.Errorf("unknown encoder %q (known: %v)", ts.Encoder, encoder.Known())
	}
	props, err := in.snapshot()
	if err != nil {
		return nil, err
	}

	pixFmt := ts.PixelFormat
	if ts.PixelFormatAuto() {
		derived, ok := hdr.DerivePixFmt(props.PixFmt())
		if !ok {
			// The engine's pix_fmt guard runs before any plan is derived and should already
			// have skipped an exotic source - this is a defence-in-depth backstop so the
			// encode can never silently subsample if that guard is ever bypassed.
			return nil, fmt.Errorf("cannot derive an output pixel format for %q (unrecognized/exotic source pix_fmt)", in.source)
		}
		pixFmt = derived
	}

	// The deinterlace and the scale, resolved from the profile and the source's own snapshot:
	// the filters the encode runs, the reference the perceptual gate builds and the provenance
	// the terminal row records are all read off these two fields. A deinterlace this build
	// will not run is refused here as well as in the engine and in config.Validate - a
	// backstop so no encode can transform a file when a check in front of it is bypassed.
	film, err := deinterlaceApplied(in.prof, props)
	if err != nil {
		return nil, err
	}
	shrink := downscaleApplied(in.prof, props)

	// The colour description the output is written with: the source's own tags and, for
	// HDR10, its static metadata. Dynamic metadata (Dolby Vision, HDR10+) is not carried;
	// the engine's guards skip a source that has it.
	color := hdr.DeriveColor(
		props.Color("color_primaries"),
		props.Color("color_transfer"),
		props.Color("color_space"),
		props.Color("color_range"),
		props.SideData(),
	)

	pinned, err := pinnedPictures(in.source, in.streams, props)
	if err != nil {
		return nil, err
	}
	if len(attached) > 0 {
		// The pictures are not in the map, so there is no mapped picture to pin to copy.
		pinned = nil
	}

	p.Video = VideoPlan{
		Codec:       spec.TargetCodec,
		Encoder:     spec,
		Device:      deviceFor(spec),
		Decode:      DecodeSoftware,
		PixelFormat: pixFmt,
		Quality:     qualityOf(ts),
	}
	p.Picture = PictureOps{Deinterlace: film, Downscale: shrink}
	p.Metadata = MetadataPlan{Color: color, Fidelity: hdr.FidelityOf(pixFmt, color, props.SideData())}
	p.coverArt.pinned = pinned
	return p, nil
}

// vaapiDevice is the render node a VAAPI encode opens.
const vaapiDevice = "/dev/dri/renderD128"

// deviceFor is the device node an encoder opens: the render node for VAAPI, which has to be
// told, and none for every other encoder.
func deviceFor(spec encoder.Spec) string {
	if spec.Key == "vaapi" {
		return vaapiDevice
	}
	return ""
}

// pinnedPictures are the output video-relative indexes of the attached pictures a job's map
// carries, which a blanket -c:v would re-encode and which must be pinned back to copy.
//
// With an intended stream map they are read off THAT and nothing else. Without one - a
// direct caller of the encoder, chiefly a test - they come from the source's probe, exactly
// as they did before stream selection existed, and a probe that could not establish the
// source's video streams refuses the encode: an encoder that could not find out what video
// streams its source carries cannot know whether one of them is artwork that must be pinned
// back to copy, and an unknown shape fails safe rather than defaulting to the common one.
func pinnedPictures(source string, streams *StreamPlan, props *probe.VideoProps) ([]int, error) {
	if streams != nil {
		return streams.AttachedPictureIndexes(), nil
	}
	vs, established := props.VideoStreams()
	if !established {
		return nil, fmt.Errorf("cannot establish the video streams of %q (ffprobe did not answer): "+
			"refusing to encode without knowing whether one of them is an attached picture", source)
	}
	return attachedPictureCopyIndexes(vs), nil
}

// UnbuildablePlanError refuses a plan that declares an operation this build cannot perform.
// Nothing has run when it is returned: the command line is refused before it is assembled,
// because an operation silently left out is an output that is not what the plan says.
type UnbuildablePlanError struct {
	// What names the declared operation.
	What string
}

func (e *UnbuildablePlanError) Error() string {
	return "the encode plan declares " + e.What + ", which this build cannot perform: nothing was encoded"
}

// buildable refuses every declaration the command-line builder has no way to honour.
func (p *EncodePlan) buildable() error {
	switch {
	case p.id == 0:
		return &UnbuildablePlanError{What: "no derivation this build made (an encode plan comes only from deriveEncodePlan)"}
	case p.Audio != CopyStreams:
		return &UnbuildablePlanError{What: fmt.Sprintf("the audio action %q", p.Audio)}
	case p.Subtitles != CopyStreams:
		return &UnbuildablePlanError{What: fmt.Sprintf("the subtitle action %q", p.Subtitles)}
	case p.Picture.Crop != "":
		return &UnbuildablePlanError{What: fmt.Sprintf("the crop %q", p.Picture.Crop)}
	case p.Metadata.HDR10Plus:
		return &UnbuildablePlanError{What: "HDR10+ dynamic metadata carried into the output"}
	case p.Metadata.DolbyVision:
		return &UnbuildablePlanError{What: "a Dolby Vision RPU carried into the output"}
	}
	if p.Video.Copy {
		// A copy re-encodes nothing, so no picture operation can run on it: a plan claiming
		// one would be a replacement recorded as transformed that is the source's picture.
		if p.Picture.Deinterlace.Enabled() || p.Picture.Downscale.Enabled() {
			return &UnbuildablePlanError{What: "a picture operation on a stream copy of the video"}
		}
		return nil
	}
	switch {
	case p.Video.Decode != DecodeSoftware:
		return &UnbuildablePlanError{What: fmt.Sprintf("the decode path %q", p.Video.Decode)}
	case p.Video.Device != deviceFor(p.Video.Encoder):
		return &UnbuildablePlanError{What: fmt.Sprintf("the device %q for encoder %q", p.Video.Device, p.Video.Encoder.Key)}
	}
	return nil
}

// args is the plan's command line between the input and the output: every job-specific
// option is read off the plan, and the one other input is x265, the run's libx265
// parallelism, which the plan does not declare - it decides how the encode is scheduled on
// this machine and nothing about what the output carries, exactly as the progress channel and
// the mux queue bounds do. pre carries the global options that must precede -i, body
// everything after it.
func (p *EncodePlan) args(x265 encoder.X265Parallelism) (pre, body []string, err error) {
	if err := p.buildable(); err != nil {
		return nil, nil, err
	}
	// Where the output is Matroska, the attached pictures travel as attachments rather than
	// through the map, so they are left out of it.
	mapArgs := p.Streams.MapArgs()
	if len(p.coverArt.attached) > 0 {
		mapArgs = p.Streams.MapArgsWithoutPictures()
	}
	body = append([]string(nil), mapArgs...)
	if p.Video.Copy {
		// The intended stream map and `-c copy`, and nothing else: every structural gate an
		// encode is held to still runs against what it produces.
		return nil, append(body, "-c", "copy"), nil
	}

	// Every carried stream is copied - that is the whole of what this build does to audio,
	// subtitles and attachments - and the video is encoded. An ATTACHED PICTURE is a video
	// stream the blanket `-c:v` would re-encode, so each one is pinned back to copy by its own
	// per-stream option, after the blanket option it overrides.
	body = append(body, "-c", "copy", "-c:v", p.Video.Encoder.FFmpegCodec)
	for _, i := range p.coverArt.pinned {
		body = append(body, "-c:v:"+strconv.Itoa(i), "copy")
	}
	// The two filters go on in this order so the CHAIN reads deinterlace, then scale, then
	// whatever the encoder family built: each helper prepends to the head, so composing the
	// scale first and the deinterlace second puts the deinterlace in front of it. That order
	// is the right one and not an accident - a deinterlacer interpolates from the fields the
	// source carried, so it has to see them at the resolution they were shot at, and a
	// resampler run first would have blended two fields into every line it produced.
	//
	// The libx265 parallelism joins the same -x265-params string as the colour block, ahead
	// of it, and is "" when the run carries none. Every other family ignores the string, so
	// their argv cannot move with it.
	body = append(body, withDeinterlace(withDownscale(
		videoArgs(p.Video, p.Metadata.Color.FFmpegFlags(), x265.Params()+p.Metadata.Color.X265Params()),
		p.Picture.Downscale), p.Picture.Deinterlace)...)

	if p.Video.Device != "" {
		// -vaapi_device is a GLOBAL option that must precede -i so the hwupload filter
		// (added by videoArgs) has a device to target. No other encoder is told a device.
		pre = []string{"-vaapi_device", p.Video.Device}
	}
	return pre, body, nil
}

// observeEncodePlan announces the encode plan one stage is about to read. Production leaves
// the observer nil and this is a nil check.
func (e *Engine) observeEncodePlan(stage string, plan *EncodePlan) {
	if e.encodePlanObserver != nil {
		e.encodePlanObserver(stage, plan)
	}
}
