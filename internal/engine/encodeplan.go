package engine

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/NSchatz/holdfast/internal/audio"
	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/deinterlace"
	"github.com/NSchatz/holdfast/internal/downscale"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/hdr"
	"github.com/NSchatz/holdfast/internal/hwdevice"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
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

// The decode paths a plan declares (VideoPlan.Decode). DecodeSoftware is every job's unless
// its root asks for hw_decode: hardware and its encoder is a hardware one; then the source is
// decoded on that encoder's vendor hardware - DecodeCUDA for NVENC, DecodeVAAPI on a render
// node for VAAPI, QSV and AMF - and every decoded frame is downloaded to system memory before
// any filter or the encoder reads it (docs/design/hardware.md#decode).
const (
	DecodeSoftware = "software"
	DecodeCUDA     = "cuda"
	DecodeVAAPI    = "vaapi"
)

// StreamAction is what an encode does to every carried stream of one type that no
// per-track declaration names.
type StreamAction string

// CopyStreams stream-copies a carried stream: the output carries the bytes the source did.
// It is the only blanket action this build takes on audio and subtitle streams; what an
// encode does to one audio track in particular is declared track by track in
// EncodePlan.AudioTracks.
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
	// stream that no per-track declaration names.
	Audio, Subtitles StreamAction
	// AudioTracks is what the encode does to the audio track by track: the re-encodes, the
	// kept originals, the added downmixes and the loudness passes the audio keys ask for
	// (docs/design/audio.md). The zero value copies every carried track and adds none, which
	// is every job's whose configuration sets none of those keys.
	AudioTracks audio.Plan
	// Picture is what the encode does to the picture on its way to the encoder.
	Picture PictureOps
	// Metadata is the colour description and the metadata the output carries.
	Metadata MetadataPlan

	// container is the muxer the output is written in, named on the command line rather than
	// left to ffmpeg to infer from the output's name (see container.go).
	container outputContainer
	// coverArt is how the attached pictures the map carries reach the output.
	coverArt coverArtCarriage
	// devices are the render-node assignment the plan's device was chosen from, so the
	// command-line builder can refuse a plan whose device is not the one assigned.
	devices hwdevice.Assignment

	// audioSeen is where the encode and the gates leave what they OBSERVED of the audio:
	// the second-pass loudness reports the encoder read back, and the output loudness the
	// gate measured. It is never part of what the plan declares, and nothing builds a
	// command line or a gate from it; it exists so the terminal row records what was
	// observed rather than what was inferred. nil on a plan that normalises nothing.
	audioSeen *audioObservations
}

// audioObservations are what one job's encode and gates observed of its audio.
type audioObservations struct {
	mu sync.Mutex
	// reports are the second-pass loudnorm reports, by report channel (audio.StatsFD).
	reports [][]byte
	// achieved are the output loudness the gate measured, by output audio index.
	achieved map[int]float64
}

// recordReports keeps the encoder's second-pass reports.
func (p *EncodePlan) recordReports(r [][]byte) {
	if p == nil || p.audioSeen == nil {
		return
	}
	p.audioSeen.mu.Lock()
	defer p.audioSeen.mu.Unlock()
	p.audioSeen.reports = r
}

// recordAchieved keeps the gate's measurement of output track n.
func (p *EncodePlan) recordAchieved(n int, lufs float64) {
	if p == nil || p.audioSeen == nil {
		return
	}
	p.audioSeen.mu.Lock()
	defer p.audioSeen.mu.Unlock()
	if p.audioSeen.achieved == nil {
		p.audioSeen.achieved = map[int]float64{}
	}
	p.audioSeen.achieved[n] = lufs
}

// loudnessSeen is the mode the k-th normalised track's report names, and the loudness the
// gate measured on output track n where it did.
func (p *EncodePlan) loudnessSeen(k, n int) (mode string, achieved *float64) {
	mode = audio.ModeNotRecorded
	if p == nil || p.audioSeen == nil {
		return mode, nil
	}
	p.audioSeen.mu.Lock()
	defer p.audioSeen.mu.Unlock()
	if k >= 0 && k < len(p.audioSeen.reports) {
		mode = audio.Mode(p.audioSeen.reports[k])
	}
	if v, ok := p.audioSeen.achieved[n]; ok {
		achieved = &v
	}
	return mode, achieved
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
	// Device is the render node the encoder opens, and "" for every encoder this build opens
	// none for: VAAPI and QSV are told which node to use (hwdevice.Assign), and NVENC reaches
	// its device through the CUDA driver.
	Device string
	// Decode is how the source is decoded: DecodeSoftware, or - under hw_decode: hardware,
	// for a hardware encoder - DecodeCUDA or DecodeVAAPI (decodeFor).
	Decode string
	// DecodeDevice is the render node a VAAPI decode opens, and "" for every other decode:
	// the encoder's own node for VAAPI and QSV, the node assigned to VAAPI for AMF.
	DecodeDevice string
	// PixelFormat is the pixel format the video is encoded to: the one the settings force,
	// or the one derived from the source's so that its chroma subsampling is kept and its
	// bit depth is floored at 10.
	PixelFormat string
	// InputFormat is the format the encoder is handed, named explicitly on the command
	// line: a format the encoder lists that carries PixelFormat's chroma subsampling and
	// bit depth exactly (encoder.Spec.InputFormat) - the -pix_fmt, or for an encoder that
	// takes only hardware surfaces (VAAPI) the software format uploaded before it. A plan
	// whose PixelFormat the encoder cannot carry is never derived.
	InputFormat string
	// Quality is the rate control the encode runs under.
	Quality Quality
}

// HardwareDecode reports whether the source is decoded on a device (DecodeCUDA or
// DecodeVAAPI). A copy declares no decode at all, and is not one.
func (v VideoPlan) HardwareDecode() bool { return v.Decode == DecodeCUDA || v.Decode == DecodeVAAPI }

// Quality is the rate control of one video encode.
type Quality struct {
	// CRF is the job's effective crf: the software encoders' quality target, and what a
	// hardware encoder with no quality.<key> of its own inherits.
	CRF int
	// Value is the quality target the encoder is handed, on its own scale
	// (Encoder.Quality): the job's quality.<key> for a hardware encoder that has one, and
	// CRF otherwise. Every encoder family reads it in its own spelling (-crf, -cq,
	// -global_quality, -qp, -qp_i/-qp_p), and none reads it where BitrateKbps is set.
	Value int
	// BitrateKbps is the target bitrate, and 0 where the encode is quality-targeted.
	BitrateKbps int
	// Preset is the speed and efficiency word, in libx265's spelling.
	Preset string
}

// TargetsBitrate reports whether the encode runs under a target bitrate rather than the
// quality target.
func (q Quality) TargetsBitrate() bool { return q.BitrateKbps > 0 }

// qualityOf is the rate control a job's effective settings resolve to on spec's scale, and
// the one place they are read for it. The value is the job's quality.<key> where the
// configuration carries one for this encoder, and its crf otherwise. A value off the
// encoder's scale is refused naming the key that sets it and the scale - crf 0 inherited by
// NVENC would be -cq 0, "automatic", and by VAAPI -qp 0, "unset": neither is a quality
// target, and neither is sent. A target-bitrate encode passes no quality value at all, so
// nothing is checked for one.
func qualityOf(ts config.Transcode, spec encoder.Spec) (Quality, error) {
	q := Quality{CRF: ts.CRF, Value: ts.CRF, BitrateKbps: ts.BitrateKbps, Preset: ts.Preset}
	scale := spec.Quality
	key, from := scale.ConfigKey, "inherited from crf"
	if scale.PerEncoder() && ts.EncoderQualitySet {
		q.Value, from = ts.EncoderQuality, "as configured"
	}
	if !scale.PerEncoder() {
		from = "as configured"
	}
	if !q.TargetsBitrate() && !scale.Contains(q.Value) {
		return Quality{}, fmt.Errorf("%s resolves to %d (%s), outside %s's scale (%s): set %s to a value on that scale",
			key, q.Value, from, spec.FFmpegCodec, scale, key)
	}
	return q, nil
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
	// devices are the render nodes this host assigned to the encoders that open one; the zero
	// value assigns the first render node, /dev/dri/renderD128, to both.
	devices hwdevice.Assignment
	// snapshot returns the source's probe snapshot. It is called at the one point the
	// derivation first needs the source's properties, so a plan refused before then never
	// probes, and it is how the engine hands in the snapshot its guards already read.
	snapshot func() (*probe.VideoProps, error)
	// measureLoudness is the first loudness pass over one source track, called only where
	// the plan normalises a track (audio.MeasureLoudness on the source, in production).
	measureLoudness audio.MeasureFunc
	// audio, where set, is an audio plan an earlier derivation for the same job already
	// made - the software retry of a failed hardware encode - so the first passes are not
	// run twice. It is checked against this derivation's map like any other.
	audio *audio.Plan
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
// deinterlace this build refuses to run, a source whose video streams the probe could
// not establish, and - last, so every refusal above keeps the reason it always had - a pixel
// format the encoder cannot carry and a quality value off the encoder's scale.
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
		devices:   in.devices,
		coverArt:  coverArtCarriage{attached: attached},
	}

	// A REMUX-ONLY job re-encodes nothing, so it has none of what follows: no encoder, no
	// pixel format, no picture operation and no colour description, because there is no
	// encode for any of them to describe. Its video is what the source's was.
	if in.streams.RemuxOnly() {
		p.Video = VideoPlan{Copy: true, Codec: in.streams.SourceVideoCodec()}
		return p, p.deriveAudio(in)
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

	// The format the encoder is handed, chosen from its own list. The same guard skips a
	// plan the encoder cannot carry before any plan is derived; this is its backstop, so
	// ffmpeg is never left to auto-select a format that subsamples or cuts depth silently.
	inputFmt, ok := spec.InputFormat(pixFmt)
	if !ok {
		return nil, fmt.Errorf("encoder %q (%s) cannot carry the pixel format %s of %q: it lists no format of that chroma "+
			"subsampling and bit depth (supported: %s)", spec.Key, spec.FFmpegCodec, pixFmt, in.source, carriedList(spec))
	}
	quality, err := qualityOf(ts, spec)
	if err != nil {
		return nil, err
	}

	decode, decodeNode := decodeFor(spec, in.prof, in.devices)
	p.Video = VideoPlan{
		Codec:        spec.TargetCodec,
		Encoder:      spec,
		Device:       deviceFor(spec, in.devices),
		Decode:       decode,
		DecodeDevice: decodeNode,
		PixelFormat:  pixFmt,
		InputFormat:  inputFmt,
		Quality:      quality,
	}
	p.Picture = PictureOps{Deinterlace: film, Downscale: shrink}
	// The fidelity declaration names every HDR10 block the source carries, read from its side
	// data. A side-data probe that did not answer would read as "no block", and the output
	// would then be held to nothing where the source may carry a mastering display: an
	// unknown is refused rather than declared as absent, and the source is kept.
	if !props.SideDataAnswered() {
		return nil, fmt.Errorf("cannot read the side data of %q (ffprobe did not answer): refusing to "+
			"encode without knowing which HDR10 metadata the output must carry", in.source)
	}
	p.Metadata = MetadataPlan{Color: color, Fidelity: hdr.FidelityOf(pixFmt, color, props.SideData())}
	p.coverArt.pinned = pinned
	// The audio last: its first loudness passes are the one costly step of a derivation, so
	// every refusal above is met before any runs.
	return p, p.deriveAudio(in)
}

// deriveAudio declares what the encode does to the audio, from the profile's audio keys and
// the intended map's carried audio tracks (docs/design/audio.md). Keys that transform nothing
// - every configuration that sets none of them - declare the zero plan: every track copied,
// no probe, no pass. A transformation with no intended map to read the tracks from is
// refused: it cannot be planned, and an encode that silently left it out would not be what
// the configuration asked for.
func (p *EncodePlan) deriveAudio(in planInputs) error {
	set := in.prof.AudioSettings()
	if in.audio != nil {
		p.AudioTracks = *in.audio
	} else if set.Active() {
		if in.streams == nil {
			return fmt.Errorf("the audio keys ask for an audio transformation of %q, which needs the intended "+
				"stream map to plan: refusing to encode without it", in.source)
		}
		measure := in.measureLoudness
		if measure == nil {
			measure = func(audio.Source, string) (audio.Stats, error) {
				return audio.Stats{}, fmt.Errorf("no loudness measurement is available to this encode")
			}
		}
		plan, err := audio.Derive(set, carriedAudio(in.streams), containerExtOf(in.output), measure)
		if err != nil {
			return err
		}
		p.AudioTracks = plan
	}
	if len(p.AudioTracks.Normalised()) > 0 {
		p.audioSeen = &audioObservations{}
	}
	return nil
}

// audioRecord is what the terminal row records of this plan's audio: one entry per op, with
// the loudness mode read off the encoder's own report and the loudness the gate measured. A
// plan with no audio ops records nothing.
func (p *EncodePlan) audioRecord() store.AudioTracks {
	if p == nil || len(p.AudioTracks.Ops) == 0 {
		return store.AudioTracks{}
	}
	list := make([]store.AudioTrack, 0, len(p.AudioTracks.Ops))
	for _, o := range p.AudioTracks.Ops {
		t := store.AudioTrack{SourceIndex: o.Source.Index, Action: string(o.Action), Reason: o.Reason}
		if o.InOutput() {
			n := o.Output
			t.OutputIndex = &n
		}
		if o.Transformed() {
			t.Codec, t.Layout, t.SampleRate, t.BitrateKbps = o.Codec, o.Layout, o.SampleRate, o.BitrateKbps
		}
		if o.Loudness != nil {
			measured := o.Loudness.InputI
			t.MeasuredLUFS = &measured
			t.Loudness, t.AchievedLUFS = p.loudnessSeen(o.LoudnessIndex, o.Output)
		}
		list = append(list, t)
	}
	return store.RecordAudioTracks(list)
}

// carriedAudio are the audio tracks a stream map carries, in output order, as the audio plan
// reads them.
func carriedAudio(streams *StreamPlan) []audio.Source {
	var out []audio.Source
	for _, s := range streams.Intended() {
		if s.Type != probe.TypeAudio {
			continue
		}
		lang := s.Language
		if untaggedLanguage(lang) {
			lang = ""
		}
		out = append(out, audio.Source{Index: s.Index, Codec: s.Codec, Profile: s.Profile, Channels: s.Channels,
			Layout: s.ChannelLayout, SampleRate: s.SampleRate, Language: lang, Commentary: s.Commentary})
	}
	return out
}

// carriedList is the list an encoder's input format is chosen from, for a refusal.
func carriedList(spec encoder.Spec) string {
	if spec.Uploads() {
		return spec.UploadFormats + " uploaded to " + spec.PixelFormats + " surfaces"
	}
	return spec.PixelFormats
}

// defaultRenderNode is the render node an encoder opens where the host assigned none: the
// first one, which is the only one on most hosts. A host whose detection found no usable node
// never runs VAAPI or QSV at all (their probe fails), so this is the node of an encoder handed
// a plan directly - a test, or a caller that did no detection.
const defaultRenderNode = "/dev/dri/renderD128"

// deviceFor is the render node an encoder opens: the node the host assigned to VAAPI or to
// QSV, which have to be told one, and none for every other encoder. It reads the encoder's
// API, so the H.264 and AV1 encoders of those two open the node their HEVC sibling does.
func deviceFor(spec encoder.Spec, devices hwdevice.Assignment) string {
	node := ""
	switch spec.API {
	case encoder.APIVAAPI:
		node = devices.VAAPI
	case encoder.APIQSV:
		node = devices.QSV
	default:
		return ""
	}
	if node == "" {
		node = defaultRenderNode
	}
	return node
}

// decodeFor is how a job decodes its source, and the render node a VAAPI decode opens: in
// software unless the root's hw_decode is hardware and the encoder is a hardware one, and then
// on that encoder's own vendor hardware. NVENC decodes through CUDA (NVIDIA's documented
// full-hardware form is `-hwaccel cuda` before the input:
// https://docs.nvidia.com/video-technologies/video-codec-sdk/13.0/ffmpeg-with-nvidia-gpu/index.html ,
// read 2026-09-30). VAAPI and QSV decode through VAAPI on the node their encoder opens: QSV's
// device is derived from that very VAAPI device, and the native decoders reach Intel's decode
// through VAAPI (the pinned build's QSV decode is the separate *_qsv decoders, which a
// generic -hwaccel does not select). AMF decodes through VAAPI on the node assigned to VAAPI:
// on Linux AMD's decode is Mesa's radeonsi VA driver (ASSUMED for a host install carrying
// AMF, until the AMF hardware report shows it; brief T43). A software encoder decodes in
// software: there is no hardware in the job to decode on.
func decodeFor(spec encoder.Spec, prof config.Profile, devices hwdevice.Assignment) (string, string) {
	if prof.HWDecodeMode() != config.HWDecodeHardware {
		return DecodeSoftware, ""
	}
	switch spec.API {
	case encoder.APINVENC:
		return DecodeCUDA, ""
	case encoder.APIVAAPI, encoder.APIQSV:
		return DecodeVAAPI, deviceFor(spec, devices)
	case encoder.APIAMF:
		node := devices.VAAPI
		if node == "" {
			node = defaultRenderNode
		}
		return DecodeVAAPI, node
	}
	return DecodeSoftware, ""
}

// deviceArgs are the global options, before -i, that open an encoder's render node.
//
// Every VAAPI device is opened with connection_type=drm. Without it, a node that fails to
// open (missing, or not passed to the container) falls through to an X11 display
// (libavutil/hwcontext_vaapi.c:1753-1880 at 5d4d3bdc61: the DRM attempt breaks out and
// XOpenDisplay runs), and the pinned ffmpeg loads libX11 lazily through a stub that ABORTS
// the process when the library is missing, as it is in the image: exit 134 in place of an
// error naming the node (verify-hw-encode.md claim 1, measured 2026-09-29; with
// connection_type=drm the same run is a clean "No VA display found").
//
// VAAPI: -vaapi_device is `-init_hw_device vaapi:<arg>` (fftools/ffmpeg_opt.c:700-712), and
// the device string takes options after a comma (fftools/ffmpeg_hw.c:90-100); hwupload uses
// the one device defined.
//
// QSV: the encoder is handed software frames (nv12 or p010le, which hevc_qsv takes with a
// device context: HW_CONFIG_ENCODER_DEVICE(NV12, QSV) and (P010, QSV),
// libavcodec/qsvenc.c:2762-2765), and fftools hands it a QSV device of the matching type
// (hw_device_setup_for_encode, fftools/ffmpeg_enc.c:133-175), which is derived from a VAAPI
// device opened here on the assigned node, with connection_type=drm for the same reason.
// Without a device the encoder opens its own VAAPI display with no node named
// (libavcodec/qsv.c), which is the X11 fallthrough again. Sources at
// https://github.com/FFmpeg/FFmpeg/tree/5d4d3bdc61 , read 2026-09-30.
func deviceArgs(v VideoPlan) []string {
	if v.HardwareDecode() {
		return hwDecodeArgs(v)
	}
	if v.Device == "" {
		return nil
	}
	drm := v.Device + ",connection_type=drm"
	switch v.Encoder.API {
	case encoder.APIVAAPI:
		return []string{"-vaapi_device", drm}
	case encoder.APIQSV:
		return []string{"-init_hw_device", "vaapi=hfva:" + drm, "-init_hw_device", "qsv=hfqsv@hfva"}
	}
	return nil
}

// hwDecodeArgs are the global options of a hardware decode: the encoder's device where it
// opens one, the decode device, and -hwaccel naming it. No -hwaccel_output_format is given, so
// ffmpeg downloads every decoded frame to system memory in the frames' own software layout
// (p010 for a 10-bit 4:2:0 source) with its properties - colour description and side data,
// the HDR10 blocks among them - copied onto it (fftools/ffmpeg_demux.c:1687-1688 leaves the
// output format unset; ffmpeg_dec.c:389-393 downloads a hardware frame and 370 copies its
// props). The filter chain and the encoder therefore see exactly the frames a software decode
// hands them, in depth, chroma and metadata: the colour stamp, a deinterlace, a scale and a
// VAAPI upload run on them unchanged, and no gate ever reads a hardware frame.
//
// A decoder with no hardware configuration for the device (an FFV1 source, say), or a profile
// the device cannot decode, is decoded in software instead: get_format falls through to the
// first software format when no hardware format of the device's type is offered
// (ffmpeg_dec.c:1329-1376). A device that cannot be opened fails the decoder, and so the job
// (ffmpeg_dec.c:1451-1535, the refusal at 1531), which hw_fallback then decides like any
// failed hardware encode.
//
// Every VAAPI device is named (hfva) and opened with connection_type=drm, for the reason
// deviceArgs gives; a VAAPI encode's upload is pointed at it with -filter_hw_device, since a
// decode device is otherwise indistinguishable from the one hwupload should use. Sources at
// https://github.com/FFmpeg/FFmpeg/tree/5d4d3bdc61 , read 2026-09-30.
func hwDecodeArgs(v VideoPlan) []string {
	vaapi := []string{"-hwaccel", "vaapi", "-hwaccel_device", "hfva"}
	switch v.Decode {
	case DecodeCUDA:
		return []string{"-hwaccel", "cuda"}
	case DecodeVAAPI:
		va := []string{"-init_hw_device", "vaapi=hfva:" + v.DecodeDevice + ",connection_type=drm"}
		switch v.Encoder.API {
		case encoder.APIVAAPI:
			return append(append(va, "-filter_hw_device", "hfva"), vaapi...)
		case encoder.APIQSV:
			return append(append(va, "-init_hw_device", "qsv=hfqsv@hfva"), vaapi...)
		}
		return append(va, vaapi...)
	}
	return nil
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
	if err := p.AudioTracks.Check(carriedAudio(p.Streams)); err != nil {
		return &UnbuildablePlanError{What: "an audio plan its derivation could not have made (" + err.Error() + ")"}
	}
	wantDecode, wantNode := decodeFor(p.Video.Encoder, p.Profile, p.devices)
	if p.Video.Copy {
		// A copy re-encodes nothing, so no picture operation can run on it: a plan claiming
		// one would be a replacement recorded as transformed that is the source's picture.
		if p.Picture.Deinterlace.Enabled() || p.Picture.Downscale.Enabled() {
			return &UnbuildablePlanError{What: "a picture operation on a stream copy of the video"}
		}
		return nil
	}
	switch {
	case p.Video.Decode != wantDecode || p.Video.DecodeDevice != wantNode:
		return &UnbuildablePlanError{What: fmt.Sprintf("the decode path %q on %q for encoder %q",
			p.Video.Decode, p.Video.DecodeDevice, p.Video.Encoder.Key)}
	case p.Video.Device != deviceFor(p.Video.Encoder, p.devices):
		return &UnbuildablePlanError{What: fmt.Sprintf("the device %q for encoder %q", p.Video.Device, p.Video.Encoder.Key)}
	}
	// The input format is the one the encoder's own list gives for the plan's pixel format,
	// and nothing else: a plan naming another would hand the encoder a format that does not
	// carry what the plan says the output is.
	if want, ok := p.Video.Encoder.InputFormat(p.Video.PixelFormat); !ok || want != p.Video.InputFormat {
		return &UnbuildablePlanError{What: fmt.Sprintf("the input format %q for the pixel format %q on encoder %q",
			p.Video.InputFormat, p.Video.PixelFormat, p.Video.Encoder.Key)}
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
		return nil, append(append(body, "-c", "copy"), p.AudioTracks.Args()...), nil
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
	//
	// Every encoder but libx265 takes its colour primaries and transfer from the frames it is
	// handed, not from -color_primaries/-color_trc, so the declared ones are stamped onto the
	// frames (hdr.Color.SetParams) at the head of the chain the picture operations prepend
	// to - after them, and before any upload to a hardware surface. libx265 writes them from
	// its own parameters, and its command line does not change.
	video := videoArgs(p.Video, p.Metadata.Color.FFmpegFlags(), x265.Params()+p.Metadata.Color.X265Params())
	if p.Video.Encoder.FFmpegCodec != "libx265" {
		video = withHeadFilter(video, p.Metadata.Color.SetParams())
	}
	body = append(body, withDeinterlace(withDownscale(video, p.Picture.Downscale), p.Picture.Deinterlace)...)
	// The audio tracks the plan transforms, after everything else: the zero plan adds
	// nothing, so a job whose configuration sets no audio key builds the command line it
	// always did.
	body = append(body, p.AudioTracks.Args()...)

	// The device options are GLOBAL and must precede -i, so the hwupload filter (added by
	// videoArgs for VAAPI) and the QSV encoder have a device to target.
	return deviceArgs(p.Video), body, nil
}

// observeEncodePlan announces the encode plan one stage is about to read. Production leaves
// the observer nil and this is a nil check.
func (e *Engine) observeEncodePlan(stage string, plan *EncodePlan) {
	if e.encodePlanObserver != nil {
		e.encodePlanObserver(stage, plan)
	}
}
