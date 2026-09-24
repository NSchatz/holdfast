package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/deinterlace"
	"github.com/NSchatz/holdfast/internal/downscale"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/hdr"
	"github.com/NSchatz/holdfast/internal/probe"
)

// Encoder produces the re-encoded output file. It is an interface so tests can
// inject deterministic fakes (simulating an encode error, a corrupt output, a
// too-large output, a truncated encode, or a dropped track) without depending on
// codec/compression luck — mirroring the bash suite's TRANSCODER_TEST_HOOKS seam.
// An Encoder must write its result to out and return nil only if it believes the
// encode succeeded; the engine independently VERIFIES the output before any swap,
// so an Encoder is never trusted on its word.
//
// props is the source's probe snapshot the engine already took for its skip guards
// (TRANSCODE-PERF), threaded in so a production encoder reuses it instead of
// re-spawning ffprobe for pix_fmt and the colour tags. It may be nil (a direct caller
// — chiefly the tests — that did not pre-probe); FFmpegEncoder then takes its own
// snapshot of the same source, so behaviour is identical, just an extra probe this
// path avoids when the engine supplies one.
type Encoder interface {
	Encode(ctx context.Context, in, out string, props *probe.VideoProps) error
}

// ProfileEncoder is an Encoder whose output depends on the LIBRARY PROFILE deciding the
// file rather than on one global configuration. The engine hands it the resolved profile
// of the root the file was enumerated under and encodes with what comes back.
//
// It is a separate, optional interface rather than a parameter on Encode because an
// Encoder that has no per-root behaviour has nothing to do with a profile: a test's
// deterministic fake writes the bytes it was told to write, and forcing every one of
// them to accept and ignore a profile would say the opposite. ForProfile must return an
// Encoder equivalent to the receiver in every respect but the profile's knobs, and must
// not mutate the receiver - the engine's workers share one Encoder across goroutines.
type ProfileEncoder interface {
	Encoder
	ForProfile(prof config.Profile) Encoder
}

// StreamPlanEncoder is an Encoder whose argv is built from an INTENDED STREAM MAP - the
// set of source streams this job means the output to carry, derived once by the engine
// from the source's own probe.
//
// It is a separate optional interface for the reason ProfileEncoder is: an Encoder with
// no per-job stream behaviour has nothing to do with a plan, and a test's deterministic
// fake writes the bytes it was told to write. ForStreamPlan must return an Encoder
// equivalent to the receiver in every respect but the plan, and must not mutate the
// receiver - the engine's workers share one Encoder across goroutines.
//
// The plan is HANDED IN and never derived here, which is the whole of AC-6: the map the
// argv is built from and the map the verification gate is checked against have to be ONE
// derivation, because two of them would be two answers to whether a track was lost, and
// that answer decides whether the source is deleted.
type StreamPlanEncoder interface {
	Encoder
	ForStreamPlan(plan *StreamPlan) Encoder
}

// EncoderFunc adapts a plain function to Encoder (used by tests).
type EncoderFunc func(ctx context.Context, in, out string, props *probe.VideoProps) error

// Encode calls the wrapped function.
func (f EncoderFunc) Encode(ctx context.Context, in, out string, props *probe.VideoProps) error {
	return f(ctx, in, out, props)
}

// FFmpegEncoder is the production encoder. It carries every stream but data
// streams (-map 0 -map -0:d?), stream-copies audio/subtitle/attachment, and
// re-encodes video per the configured Cfg.Encoder (internal/encoder.Lookup) at the
// configured CRF/preset/pixel format. Colour/HDR propagation (TRANSCODE-3) derives
// -color_* flags from the source via internal/hdr and applies to EVERY encoder
// (they are encoder-agnostic primaries/transfer/matrix/range tags); the x265
// colour params (HDR10 static-metadata master-display/max-cll) are libx265-only —
// see buildArgs. Probe is required to do the colour/pixel-format derivation, so it
// must be set in production (a nil Probe is a programmer error).
//
// CPU libx265 is the archival default; SVT-AV1 and the hardware encoders (NVENC/
// QSV/VAAPI/AMF, TRANSCODE-6) are opt-in and gated behind a runtime capability
// check (internal/encoder.Available) BEFORE the engine ever calls Encode — see
// cmd/holdfast's cmdRun. Encode itself never re-derives availability; it trusts
// the caller checked, and simply builds and runs the command for whatever Spec the
// configured Cfg.Encoder resolves to.
type FFmpegEncoder struct {
	FFmpeg string
	Cfg    config.Config
	Probe  *probe.Prober

	// Prof is the LIBRARY PROFILE this encoder builds an encode from - the resolved
	// knobs of the root the file was enumerated under. The engine sets it per file
	// through ForProfile; nil means "the top-level values of Cfg", which is what an
	// encoder constructed directly (and every configuration written before profiles
	// existed) encodes at.
	Prof *config.Profile

	// Plan is THIS JOB's intended stream map, derived once by the engine from the
	// source's probe and handed here through ForStreamPlan. It decides which source
	// streams the argv maps and whether the video is stream-copied too.
	//
	// nil means "carry every stream but data", which is the argv this repository has
	// always built and is what a direct caller of this exported type gets. It is never
	// DERIVED here: an encoder that worked out its own map would be a second derivation,
	// and the gate would then be checking a different answer than the argv was built
	// from.
	Plan *StreamPlan

	// X265 is the parallelism every libx265 encode this encoder builds is told to use:
	// a worker-pool size and a frame-thread count, joined to the -x265-params string on
	// the quality-targeted and the bitrate-targeted path alike. The run derives it once,
	// from the CPU quota or the x265_cpus key (see DeriveX265), and hands it here.
	//
	// The zero value passes neither figure, which leaves libx265's own defaults in force
	// and keeps the argv byte for byte what an encoder without this field built. No other
	// encoder reads it: pools and frame-threads are libx265 mechanisms.
	X265 encoder.X265Parallelism

	// newProgressPipe, when non-nil, replaces os.Pipe when opening the channel ffmpeg
	// writes -progress reports to. Unexported test seam (the engine tests are in this
	// package): returning an error from it is how a test drives the "progress collection
	// could not be started at all" path, which must degrade to exactly the encode this
	// package performed before progress existed. Production leaves it nil.
	newProgressPipe func() (r *os.File, w *os.File, err error)

	// argvObserver, when non-nil, receives the full ffmpeg argv immediately before the
	// subprocess starts. Unexported test seam (the engine tests are in this package),
	// nil in production, and it exists for one question no output file can answer:
	// WHICH PROFILE built this encode. Two roots at different CRFs both produce a valid
	// hevc file of the same source, so the argv is the only place the difference is
	// visible - and it is the real argv the production encoder assembled, not a
	// re-derivation of it.
	argvObserver func(args []string)

	// workDir is the directory an invocation runs in, "" for the process's own. Only the
	// picture carriage sets it (runningIn): the working output's directory, so a picture
	// file is named relative to it and its path never has to fit PATH_MAX whole.
	workDir string
}

// ForProfile returns this encoder built from prof's knobs. The receiver is a VALUE, so
// the copy is the whole of the isolation the engine's workers need: several files under
// several roots encode concurrently and none of them can see another's profile.
func (e FFmpegEncoder) ForProfile(prof config.Profile) Encoder {
	e.Prof = &prof
	return e
}

// ForStreamPlan returns this encoder built from plan's intended stream map. The receiver
// is a VALUE, so the copy is the whole of the isolation the engine's workers need, exactly
// as it is for ForProfile: several files under several roots encode concurrently and none
// of them can see another's plan.
func (e FFmpegEncoder) ForStreamPlan(plan *StreamPlan) Encoder {
	e.Plan = plan
	return e
}

// profile is the knobs this encoder builds an encode from: the library profile the
// engine handed it, or - for an encoder constructed without one - the top-level values
// of the configuration it was built with.
func (e FFmpegEncoder) profile() config.Profile {
	if e.Prof != nil {
		return *e.Prof
	}
	return e.Cfg.TopLevelProfile()
}

// Encode runs ffmpeg. It returns an error if the configured encoder is unknown, if
// ffmpeg exits non-zero, or if colour/pixel-format derivation fails (the engine
// then discards the temp and leaves the source untouched).
func (e FFmpegEncoder) Encode(ctx context.Context, in, out string, props *probe.VideoProps) error {
	return e.EncodeWithProgress(ctx, in, out, props, nil)
}

// EncodeWithProgress is Encode plus live progress reporting: while the encode runs, sink
// is called with the encoder's position in the source timeline (see scanProgressStream
// for the stream format and the measured key set). A nil sink is exactly Encode.
//
// Everything about the SUBPROCESS is unchanged by the collection, on purpose. holdfast
// deletes a source once its replacement is judged faithful, and the error this function
// returns is what the engine turns into a failed job's reason — so a collector that
// swallowed a non-zero exit would turn a failed encode into a candidate for verification,
// and one that reallocated stdout would empty the failure text an operator reads. The
// progress stream therefore goes to its OWN file descriptor (fd 3, handed to the child
// via ExtraFiles), never to stdout; stdout and stderr are still both captured into one
// buffer and truncated into the returned error exactly as CombinedOutput did. If the
// pipe cannot be opened at all, the -progress option is simply not passed and the encode
// runs precisely as it did before this existed.
func (e FFmpegEncoder) EncodeWithProgress(ctx context.Context, in, out string, props *probe.VideoProps, sink ProgressSink) error {
	// Where the output is Matroska, the plan's attached pictures travel as attachments
	// rather than through the map (see matroskaPictures); nil everywhere else.
	pics, err := e.matroskaPictures(out)
	if err != nil {
		return err
	}

	// A REMUX-ONLY job re-encodes nothing, so it needs none of what follows: no encoder
	// spec, no pixel format, no colour derivation and no quality knob, because there is no
	// encode for any of them to describe. It is the intended stream map and `-c copy`, and
	// every structural gate an encode is held to still runs against what it produces.
	if e.Plan.RemuxOnly() {
		mapArgs := e.Plan.MapArgs()
		if len(pics) > 0 {
			mapArgs = e.Plan.MapArgsWithoutPictures()
		}
		return e.runCarrying(ctx, in, out, sink, nil, append(mapArgs, "-c", "copy"), pics)
	}

	// THIS JOB's settings: the profile of the root the engine handed this encoder,
	// overlaid with the first encode profile whose pattern matches the SOURCE path
	// (TRANSCODE-PROFILES). It is a pure function of the configuration, that profile
	// and the path, so the engine - which needs the same answer for the
	// already-at-target-codec skip, the output container and the output-codec
	// acceptance check - resolves it independently from the same three inputs and
	// cannot disagree with what is built here. `in` is always the source: the encoder
	// READS the source and WRITES the working file, wherever scratch_dir puts the
	// latter.
	ts := e.Cfg.TranscodeIn(e.profile(), in)
	spec, ok := encoder.Lookup(ts.Encoder)
	if !ok {
		return fmt.Errorf("unknown encoder %q (known: %v)", ts.Encoder, encoder.Known())
	}
	if e.Probe == nil {
		return fmt.Errorf("FFmpegEncoder.Probe is nil (required to derive colour/pixel-format args from the source)")
	}
	// The engine threads in the snapshot it already probed for its skip guards
	// (TRANSCODE-PERF). A direct caller that did not pre-probe passes nil, so take one
	// here — it snapshots the same source file the guards read, so a fresh snapshot is
	// equivalent; this only avoids a duplicate probe when the engine supplies one.
	if props == nil {
		props = e.Probe.VideoProps(ctx, in)
	}

	pixFmt := ts.PixelFormat
	if ts.PixelFormatAuto() {
		derived, ok := hdr.DerivePixFmt(props.PixFmt())
		if !ok {
			// The engine's pix_fmt guard runs before Encode and should already have
			// skipped an exotic source — this is a defence-in-depth backstop so the
			// encoder itself never silently subsamples if that guard is ever bypassed.
			return fmt.Errorf("cannot derive an output pixel format for %q (unrecognized/exotic source pix_fmt)", in)
		}
		pixFmt = derived
	}

	// THE DEINTERLACE, resolved from the same profile and the same probe snapshot the
	// engine's own guards read, so the filter that runs here is the filter the perceptual
	// gate builds its reference through and the filter the terminal row records. A
	// configuration this build will not run is refused HERE as well as in the engine and in
	// config.Validate, for the reason the pixel-format derivation below is: a backstop so
	// the encoder itself can never silently transform a file when a check in front of it is
	// bypassed.
	film, err := deinterlaceApplied(e.profile(), props)
	if err != nil {
		return err
	}

	// THE RESOLUTION CEILING, resolved from the same profile and the same probe snapshot,
	// for the same reason the deinterlace above is: the scale that runs here is the scale the
	// perceptual gate scores the output back up through and the scale the terminal row
	// records. A ceiling this build cannot target is refused in config.Validate and again in
	// the engine before a temp path is chosen; a source whose dimensions the probe did not
	// establish is skipped in front of this, and resolves to NO scale here as the backstop
	// behind that - a filter built against a guessed source size would encode a library to a
	// resolution nobody measured.
	shrink := downscaleApplied(e.profile(), props)

	// Colour/HDR propagation: carry the source's primaries/transfer/matrix/range
	// forward instead of letting the encode silently drop them. These -color_* flags
	// are encoder-agnostic and applied to every Spec. x265Color (HDR10 static
	// master-display/max-cll) is libx265-only — see buildArgs. DV/HDR10+ were
	// already detected-and-skipped upstream by the engine.
	colorArgs, x265Color := hdr.DeriveColorArgsFrom(
		props.Color("color_primaries"),
		props.Color("color_transfer"),
		props.Color("color_space"),
		props.Color("color_range"),
		props.SideData(),
	)

	// The stream map, and WHICH of the mapped video streams must be pinned back to copy.
	//
	// Both come from the intended stream map when the engine supplied one, and from the
	// source's probe when a direct caller did not. They are the same question asked of one
	// derivation or of none - never of two: an encoder that worked out its own map beside
	// the one the gate checks would be the second answer this whole design exists to
	// prevent.
	//
	// An ATTACHED PICTURE is a video stream and the blanket `-c:v` below would re-encode
	// it, so each one is pinned back to copy by its own per-stream option, AFTER the
	// blanket option it overrides. The map preserves stream order, so the N of an output
	// `v:N` is the N of the video streams the output carries. A single-video-stream source
	// yields no such option and therefore byte-identical argv to the encoder that predates
	// this.
	mapArgs, pictures, errStreams := e.streamArgs(in, props)
	if errStreams != nil {
		return errStreams
	}
	if len(pics) > 0 {
		// The pictures are not in the map, so there is no mapped picture to pin to copy.
		mapArgs, pictures = e.Plan.MapArgsWithoutPictures(), nil
	}

	body := append([]string(nil), mapArgs...)
	body = append(body, "-c", "copy", "-c:v", spec.FFmpegCodec)
	for _, i := range pictures {
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
	// of it, and is "" when this encoder carries none. Every other family ignores the
	// string, so their argv cannot move with it.
	body = append(body, withDeinterlace(
		withDownscale(buildArgs(spec, ts, pixFmt, colorArgs, e.X265.Params()+x265Color), shrink), film)...)

	var pre []string
	if spec.Key == "vaapi" {
		// -vaapi_device is a GLOBAL option that must precede -i so the hwupload
		// filter (added by buildArgs) has a device to target. Every other Spec has no
		// such ordering requirement.
		pre = []string{"-vaapi_device", "/dev/dri/renderD128"}
	}
	return e.runCarrying(ctx, in, out, sink, pre, body, pics)
}

// matroskaPictureMimeTypes are the attachment mimetypes the pinned ffmpeg's Matroska
// demuxer reads back as an attached picture, keyed by the codec it then reports. An
// attachment under any other mimetype reads back as a plain attachment, which is not the
// stream the intended map carries; bmp is the MP4 cover codec that has no entry.
var matroskaPictureMimeTypes = map[string]string{
	"mjpeg": "image/jpeg",
	"png":   "image/png",
	"gif":   "image/gif",
	"tiff":  "image/tiff",
}

// pictureExtensions name a picture that arrived with no filename, by its mimetype.
var pictureExtensions = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/tiff": ".tif",
}

// matroskaPicture is one attached picture as the output will carry it: an attachment
// written from a file holding the source picture's bytes, under its description. name is
// that file's name in the working output's directory (picturePath); it is only ever used
// relative to that directory (runCarrying).
type matroskaPicture struct {
	source   probe.Stream
	name     string
	mimeType string
	filename string
}

// matroskaPictures decides how this job carries its attached pictures when the output is
// Matroska, and returns nil when there is nothing to decide: an output of any other
// container, a plan carrying no picture, or no plan at all.
//
// The Matroska muxer writes a mapped picture as an ordinary video track, which reads back
// as plain video, so the intended-stream check rejects the output after the whole encode;
// it does not honour an attached_pic disposition either. What reads back as an attached
// picture is an ATTACHMENT under an image mimetype, which is how a Matroska source stores
// its cover art in the first place. So each picture is copied out of the source to a file
// beside the working output and attached from there, under the source's own filename,
// mimetype and title where it has them.
//
// A picture whose mimetype cannot be established is refused here, before anything is
// written: an attachment under a mimetype the demuxer does not map reads back as something
// other than the picture the plan intends.
func (e FFmpegEncoder) matroskaPictures(out string) ([]matroskaPicture, error) {
	if !strings.EqualFold(filepath.Ext(out), ".mkv") {
		return nil, nil
	}
	sources := e.Plan.Pictures()
	if len(sources) == 0 {
		return nil, nil
	}
	pics := make([]matroskaPicture, 0, len(sources))
	for i, s := range sources {
		mime := s.MimeType
		if mime == "" {
			mime = matroskaPictureMimeTypes[s.Codec]
		}
		if mime == "" {
			return nil, fmt.Errorf("cannot carry the attached picture at source stream %d (codec %q) "+
				"into a Matroska output: no attachment mimetype reads back as a picture in that codec",
				s.Index, s.Codec)
		}
		name := s.Filename
		if name == "" {
			name = "cover" + pictureExtensions[mime]
			if i > 0 {
				name = "cover-" + strconv.Itoa(i+1) + pictureExtensions[mime]
			}
		}
		pics = append(pics, matroskaPicture{
			source:   s,
			name:     filepath.Base(picturePath(out, i)),
			mimeType: mime,
			filename: name,
		})
	}
	return pics, nil
}

// picturePath is the file the i-th attached picture of the job writing out is copied to:
// the working output's own name plus ".picture<i>", in the working output's directory, so
// it is unique exactly as the working output is and carries the temp marker the sweeps
// recognise.
//
// Where that suffix would take the name past NAME_MAX (maxBaseName) - a long source name
// already fills it, and a scratch working name is cut to fill it exactly - the part of the
// name ahead of the temp marker is cut instead, at a character boundary, and a digest of
// the whole working name stands in for what was cut, so two long names sharing a beginning
// still get two files. A job whose working output's NAME fits always gets a picture file
// whose name fits. The PATH is the other limit: it is at least the working output's path
// plus the suffix, which can pass PATH_MAX where the working output's does not, and that is
// why the picture file is only ever reached relative to its directory (runCarrying).
func picturePath(out string, i int) string {
	dir, base := filepath.Split(out)
	tail := ".picture" + strconv.Itoa(i)
	if len(base)+len(tail) <= maxBaseName {
		return out + tail
	}
	sum := sha256.Sum256([]byte(base))
	tag := "." + hex.EncodeToString(sum[:scratchTagBytes])
	head, rest := base, ""
	if m := strings.Index(base, "."+TempMarker+"."); m > 0 {
		head, rest = base[:m], base[m:]
	}
	// The name was over the limit before tag was added, so keep is below len(head). It is
	// held at one byte or more so a stem remains ahead of the marker, which a Matroska
	// working name (a marker and an extension, a few dozen bytes) never comes near needing.
	keep := max(maxBaseName-len(tag)-len(rest)-len(tail), 1)
	for keep > 1 && !utf8.RuneStart(head[keep]) {
		keep--
	}
	return filepath.Join(dir, head[:keep]+tag+rest+tail)
}

// runCarrying runs a job's encode, and where the job carries pictures as Matroska
// attachments it first copies each one out of the source to its file and then attaches
// them all to the encode.
//
// The picture files sit beside the working output, named after it (picturePath), so they
// live where the working output lives: in the scratch directory when one is set, beside the
// source otherwise, and under the temp marker either way, which is what lets the startup
// sweep take one a killed run left behind. They are removed on every return from here, the
// failed ones included: once the encode has exited nothing reads them again.
//
// The copy runs the image2 muxer with -update 1. Without it image2 reads its output name as
// an image-sequence pattern, so a `%d` anywhere in the path (a source, a library directory
// or a scratch directory named with one) sends the picture to a different name: one this
// function never removes, and one a sibling job may own.
//
// Every access to a picture file is relative to the working output's directory: the copy
// and the encode run IN that directory and name the file "./<name>", and the removal goes
// through a handle on the directory. A picture file's full path is its working output's
// plus the suffix, so where the working output's path fits PATH_MAX with fewer bytes to
// spare than that, the full path of the picture file does not, and a job the engine could
// otherwise swap would fail on its cover art. Relative to the directory the path is the
// name, which picturePath holds to NAME_MAX. The "./" also keeps ffmpeg from reading a name
// with a colon in it as a protocol.
func (e FFmpegEncoder) runCarrying(ctx context.Context, in, out string, sink ProgressSink,
	pre, body []string, pics []matroskaPicture) error {
	if len(pics) == 0 {
		return e.runFFmpeg(ctx, in, out, sink, pre, body)
	}
	in, out = absolutePath(in), absolutePath(out)
	dir := filepath.Dir(out)
	defer removePictures(dir, pics)
	e = e.runningIn(dir)
	body = append([]string(nil), body...)
	first := e.Plan.MappedAttachments()
	for i, p := range pics {
		file := "./" + p.name
		// One packet, copied: an attached picture is exactly one, and the image2 muxer
		// writes the packet's bytes as they are, to exactly the name it is given.
		if err := e.runFFmpeg(ctx, in, file, nil, nil, []string{
			"-map", "0:" + strconv.Itoa(p.source.Index), "-c", "copy", "-frames:v", "1",
			"-f", "image2", "-update", "1",
		}); err != nil {
			return fmt.Errorf("copying out the attached picture at source stream %d, to carry it as a "+
				"Matroska attachment: %w", p.source.Index, err)
		}
		spec := "-metadata:s:t:" + strconv.Itoa(first+i)
		body = append(body, "-attach", file,
			spec, "mimetype="+p.mimeType, spec, "filename="+p.filename)
		if p.source.Title != "" {
			body = append(body, spec, "title="+p.source.Title)
		}
	}
	return e.runFFmpeg(ctx, in, out, sink, pre, body)
}

// runningIn returns this encoder with its invocations run in dir. Every other path an
// invocation is handed has to keep meaning what it meant in the process's own directory:
// the caller makes the input and the output absolute, and a binary named by a relative
// path is made absolute here, because exec resolves a relative binary path against the
// directory the child runs in.
func (e FFmpegEncoder) runningIn(dir string) FFmpegEncoder {
	e.workDir = dir
	if strings.ContainsRune(e.FFmpeg, filepath.Separator) {
		e.FFmpeg = absolutePath(e.FFmpeg)
	}
	return e
}

// absolutePath is p made absolute against the process's working directory, or p itself
// when it already is one or cannot be made one.
func absolutePath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// removePictures removes the job's picture files from dir, by name through a handle on dir,
// so a picture file whose full path is past PATH_MAX goes as surely as it was written. A
// directory that cannot be opened as a handle falls back to the full paths.
func removePictures(dir string, pics []matroskaPicture) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		for _, p := range pics {
			_ = os.Remove(filepath.Join(dir, p.name))
		}
		return
	}
	defer func() { _ = root.Close() }()
	for _, p := range pics {
		_ = root.Remove(p.name)
	}
}

// streamArgs is the stream-selection half of the argv: which source streams are mapped,
// and the output video-relative indexes of the attached pictures among them.
//
// With an intended stream map it reads THAT and nothing else. Without one - a direct
// caller of this exported type, which is chiefly a test - it falls back to the argv this
// repository has always built and asks the source's probe which of its video streams are
// artwork, exactly as it did before stream selection existed.
//
// Whether ffprobe ESTABLISHED the source's shape is not dropped on either path. An
// encoder that could not find out what video streams its source carries cannot know
// whether one of them is artwork that must be pinned back to copy, and an unknown shape
// has to fail safe rather than default to the common one - the same posture the engine's
// own source-shape guard takes, and the same one the pixel-format derivation takes for the
// same class of unknown.
func (e FFmpegEncoder) streamArgs(in string, props *probe.VideoProps) (mapArgs []string, pictures []int, err error) {
	if e.Plan != nil {
		return e.Plan.MapArgs(), e.Plan.AttachedPictureIndexes(), nil
	}
	streams, established := props.VideoStreams()
	if !established {
		return nil, nil, fmt.Errorf("cannot establish the video streams of %q (ffprobe did not answer): "+
			"refusing to encode without knowing whether one of them is an attached picture", in)
	}
	return []string{"-map", "0", "-map", "-0:d?"}, attachedPictureCopyIndexes(streams), nil
}

// runFFmpeg assembles the full argv around a job's own options and runs the encoder.
//
// It is ONE exec path for every job this encoder performs - a re-encode and a remux
// alike - so the progress channel, the captured output, the returned error and its
// wrapping cannot drift between them. pre carries the GLOBAL options that must precede
// `-i` (today only the VAAPI device); body is everything between the input and the output.
func (e FFmpegEncoder) runFFmpeg(ctx context.Context, in, out string, sink ProgressSink, pre, body []string) error {
	// Open the progress channel BEFORE the argv is assembled: the -progress option is
	// only ever passed when there is a reader for it. A sink-less call, or a pipe we
	// could not open, produces byte-identical argv to the pre-progress encoder.
	pr, pw := e.openProgressPipe(sink)

	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}
	if pr != nil {
		// A GLOBAL option, so it belongs in this leading block. "pipe:3" is the third
		// file descriptor of the child, which is where ExtraFiles[0] lands — chosen over
		// "pipe:1" precisely because stdout is part of the captured error text.
		args = append(args, "-progress", "pipe:3")
	}
	args = append(args, pre...)
	args = append(args, "-i", in)
	args = append(args, body...)
	args = append(args, "--", out)

	if e.argvObserver != nil {
		e.argvObserver(args)
	}

	cmd := exec.CommandContext(ctx, e.FFmpeg, args...)
	cmd.Dir = e.workDir
	// exec.Cmd.CombinedOutput is exactly this: one buffer behind both streams, then
	// Run (= Start + Wait). It is spelled out rather than called because the progress
	// drain has to happen BETWEEN Start and Wait — the captured bytes, the returned
	// error and its wrapping are otherwise identical, which is the point.
	var outb bytes.Buffer
	cmd.Stdout = &outb
	cmd.Stderr = &outb
	if pr != nil {
		cmd.ExtraFiles = []*os.File{pw}
	}

	if err := cmd.Start(); err != nil {
		closeProgressPipe(pr, pw)
		return fmt.Errorf("ffmpeg encode: %w: %s", err, truncate(outb.String(), 500))
	}

	drained := make(chan struct{})
	if pr != nil {
		// The child now holds the only writer, so closing ours is what lets the reader
		// below ever see EOF.
		_ = pw.Close()
		go func() {
			defer close(drained)
			scanProgressStream(pr, func(positionSec float64) {
				sink(Progress{PositionSec: positionSec})
			})
		}()
	} else {
		close(drained)
	}

	err := cmd.Wait()
	// The encoder has exited, so its end of the progress pipe is closed and the drain
	// has finished (or is about to); waiting for it keeps the reader from outliving the
	// call and reporting progress for a job that is already terminal.
	<-drained
	if pr != nil {
		_ = pr.Close()
	}
	if err != nil {
		return fmt.Errorf("ffmpeg encode: %w: %s", err, truncate(outb.String(), 500))
	}
	return nil
}

// openProgressPipe opens the channel the encoder writes -progress reports to, or returns
// (nil, nil) when there is nothing to report to or the pipe cannot be opened. A failure
// here is NOT an encode failure: progress is a reporting nicety and the encode must run
// exactly as it did before progress collection existed, so the caller simply omits the
// option. (There is nowhere to log from here — FFmpegEncoder holds no logger — and a
// silently absent figure is already the documented "no progress reported" state.)
func (e FFmpegEncoder) openProgressPipe(sink ProgressSink) (r *os.File, w *os.File) {
	if sink == nil {
		return nil, nil
	}
	newPipe := e.newProgressPipe
	if newPipe == nil {
		newPipe = os.Pipe
	}
	pr, pw, err := newPipe()
	if err != nil || pr == nil || pw == nil {
		closeProgressPipe(pr, pw)
		return nil, nil
	}
	return pr, pw
}

func closeProgressPipe(r, w *os.File) {
	if r != nil {
		_ = r.Close()
	}
	if w != nil {
		_ = w.Close()
	}
}

// buildArgs assembles the per-encoder ffmpeg args (everything after `-c:v
// <codec>`, before the trailing `-- <out>`). The -pix_fmt, -color_* flags and
// -fps_mode passthrough are UNIVERSAL — every Spec gets them, since they carry
// source fidelity independent of which codec/encoder produces the bytes. Beyond
// that each encoder family has its own quality-knob shape:
//
//   - libx265 (cpu): -preset/-crf plus -x265-params, which carries x265Extra: the
//     encode's pool size and frame-thread count when it has them, then the HDR10
//     static-metadata master-display/max-cll block (both libx265-only mechanisms).
//   - libsvtav1 (svtav1): -preset (numeric 0-13, mapped from the config Preset
//     word — see svtav1Preset) + -crf. No x265Params: AV1 HDR10 static-metadata
//     carriage would need svt-av1-params mastering-display/content-light options,
//     which is OUT OF SCOPE here (see the package doc / CLAUDE.md) — colour
//     PRIMARIES/TRANSFER/MATRIX/RANGE tags still carry via the universal
//     -color_* flags, just not the mastering-display block. This mirrors the
//     existing NVENC limitation the bash transcoder already documented.
//   - hevc_nvenc/av1_nvenc: -rc vbr -cq <CRF> -b:v 0 (CRF reused as the CQ
//     target) + a preset.
//   - hevc_qsv: -global_quality <CRF>.
//   - hevc_vaapi: -vaapi_device (emitted by Encode, before -i — see Encode's
//     vaapi special case) + -vf format=nv12,hwupload + -qp <CRF>. This is the
//     fiddliest of the set and untestable in this environment (no VAAPI
//     device) — capability detection (internal/encoder.Available) keeps it from
//     ever running unless a real device is present; the arg shape is reasonable
//     but not battle-tested.
//   - hevc_amf: -rc cqp -qp_i <CRF> -qp_p <CRF>.
//
// A job whose effective settings carry a positive BitrateKbps takes the
// TARGET-BITRATE shape instead, per family (see bitrateArgs). The quality knob is
// then not passed AT ALL - no -crf, -cq, -global_quality, -qp or -qp_i/-qp_p, and
// no -rc cqp - because a rate control and a quality target are two different
// instructions and passing both leaves which one wins to the encoder's own
// precedence rules rather than to the operator. Everything else is unchanged: the
// pixel format, the colour tags, -fps_mode passthrough and the libx265 preset and
// x265Extra block are the same on both paths, so a bitrate-targeted encode carries
// exactly the same source fidelity, and the same parallelism, as a quality-targeted one.
func buildArgs(spec encoder.Spec, ts config.Transcode, pixFmt string, colorArgs []string, x265Extra string) []string {
	args := []string{"-pix_fmt", pixFmt}
	args = append(args, colorArgs...)
	args = append(args, "-fps_mode", "passthrough") // a VFR source is not forced to CFR

	if ts.TargetsBitrate() {
		return append(args, bitrateArgs(spec, ts, x265Extra)...)
	}

	switch spec.Key {
	case "cpu":
		args = append(args,
			"-preset", ts.Preset,
			"-crf", strconv.Itoa(ts.CRF),
			"-x265-params", "log-level=error"+x265Extra,
		)
	case "svtav1":
		args = append(args,
			"-preset", strconv.Itoa(svtav1Preset(ts.Preset)),
			"-crf", strconv.Itoa(ts.CRF),
		)
	case "nvenc", "av1_nvenc":
		args = append(args,
			"-rc", "vbr",
			"-cq", strconv.Itoa(ts.CRF),
			"-b:v", "0",
			"-preset", "p5",
		)
	case "qsv":
		args = append(args, "-global_quality", strconv.Itoa(ts.CRF))
	case "vaapi":
		// -vaapi_device itself is emitted by Encode (a global option that must
		// precede -i — see Encode's doc comment on the vaapi special case); here we
		// only add the encode-side args that come after -c:v.
		args = append(args,
			"-vf", "format=nv12,hwupload",
			"-qp", strconv.Itoa(ts.CRF),
		)
	case "amf":
		args = append(args,
			"-rc", "cqp",
			"-qp_i", strconv.Itoa(ts.CRF),
			"-qp_p", strconv.Itoa(ts.CRF),
		)
	}
	return args
}

// withDeinterlace composes the deinterlace into a job's video filter chain, and is the ONE
// place in the argv where it is applied.
//
// A disabled filter returns the arguments untouched, which is what keeps the command line
// of every configuration written before this key existed byte for byte what it was. An
// enabled one is composed INTO a chain the encoder family already built rather than added
// beside it: ffmpeg takes one -vf per output, so a second would silently replace the first
// and a VAAPI job would upload frames to the GPU with no deinterlace, or deinterlace and
// never upload. It goes at the HEAD of that chain because the software filter works on
// software frames and has to run before anything that uploads them to a device.
func withDeinterlace(args []string, film deinterlace.Filter) []string {
	return withHeadFilter(args, film.Spec)
}

// withDownscale composes the resolution ceiling's scale into a job's video filter chain, and
// is the ONE place in the argv where the picture is made smaller.
//
// A disabled scale returns the arguments untouched, which is what keeps the command line of
// every configuration that names no ceiling byte for byte what it was. An enabled one is
// composed into the chain on exactly withDeinterlace's terms and for exactly its reason:
// ffmpeg takes one -vf per output, so a second would silently replace the first.
//
// It goes at the HEAD of the chain for the VAAPI case above all - the scale is a software
// filter and has to run before anything uploads frames to a device - and the caller puts the
// deinterlace in front of it afterwards, so a job doing both deinterlaces at the source's own
// resolution and only then resamples.
func withDownscale(args []string, s downscale.Scale) []string {
	return withHeadFilter(args, s.Spec())
}

// withHeadFilter prepends one filter expression to a job's -vf chain, or starts the chain
// with it where the encoder family built none. An empty expression is a no-op, which is what
// makes an unconfigured filter leave the argv exactly as it was.
//
// It is shared by the two composers above so there is ONE answer to "how does a filter reach
// this argv": two copies of this loop would be two places for a second -vf to appear, and a
// second -vf silently replaces the first rather than erroring.
func withHeadFilter(args []string, spec string) []string {
	if spec == "" {
		return args
	}
	for i, a := range args {
		if a == "-vf" && i+1 < len(args) {
			out := append([]string(nil), args...)
			out[i+1] = spec + "," + out[i+1]
			return out
		}
	}
	return append(args, "-vf", spec)
}

// bitrateArgs is the TARGET-BITRATE half of buildArgs: everything after the
// universal pixel-format/colour/fps block, for a job whose effective settings carry
// a positive BitrateKbps.
//
// `-b:v <n>k` is the target in every family - it is ffmpeg's own codec-independent
// bitrate option - and what varies is only the rate-control MODE each family needs
// told, because several of them default to a constant-quality mode that would
// otherwise ignore the target:
//
//   - libx265 (cpu): -b:v alone selects libx265's ABR mode. -preset and the
//     -x265-params string - the parallelism and the HDR10 block - stay exactly as
//     they are on the quality path.
//   - libsvtav1 (svtav1): -b:v alone selects SVT-AV1's VBR mode; the numeric
//     preset stays.
//   - hevc_nvenc/av1_nvenc: -rc vbr with a real -b:v. The quality path passes
//     `-cq <CRF> -b:v 0`, which is NVENC's constant-quality spelling; here the
//     -cq is dropped entirely and the 0 replaced by the target.
//   - hevc_qsv: -b:v alone. -global_quality is what selects ICQ and is dropped.
//   - hevc_vaapi: the hwupload filter chain is unchanged; -qp is dropped and the
//     target passed. Untestable in this environment (no VAAPI device), exactly as
//     the quality path is.
//   - hevc_amf: -rc vbr_peak with the target, in place of -rc cqp and the two QP
//     values. AMF's cqp is a fixed-quantiser mode that ignores -b:v outright.
func bitrateArgs(spec encoder.Spec, ts config.Transcode, x265Extra string) []string {
	rate := strconv.Itoa(ts.BitrateKbps) + "k"
	var args []string
	switch spec.Key {
	case "cpu":
		args = []string{
			"-preset", ts.Preset,
			"-b:v", rate,
			"-x265-params", "log-level=error" + x265Extra,
		}
	case "svtav1":
		args = []string{
			"-preset", strconv.Itoa(svtav1Preset(ts.Preset)),
			"-b:v", rate,
		}
	case "nvenc", "av1_nvenc":
		args = []string{
			"-rc", "vbr",
			"-b:v", rate,
			"-preset", "p5",
		}
	case "qsv":
		args = []string{"-b:v", rate}
	case "vaapi":
		args = []string{
			"-vf", "format=nv12,hwupload",
			"-b:v", rate,
		}
	case "amf":
		args = []string{
			"-rc", "vbr_peak",
			"-b:v", rate,
		}
	default:
		// A Spec this build ships but this function does not name would silently lose
		// the operator's target, so it gets the codec-independent option and nothing
		// else rather than the quality knob it did not ask for.
		args = []string{"-b:v", rate}
	}
	return args
}

// svtav1Preset maps the config Preset word to SVT-AV1's numeric 0-13 preset scale
// (0 = slowest/best, 13 = fastest/worst — the opposite direction from libx265's
// naming but the same "slower is smaller" intuition). Unrecognized/empty values
// fall back to 8 (SVT-AV1's own default), the same "don't guess extremes" posture
// as picking a documented middle ground rather than silently defaulting to fastest
// or slowest.
func svtav1Preset(p string) int {
	switch p {
	case "veryslow", "placebo":
		return 2
	case "slower":
		return 4
	case "slow":
		return 6
	case "medium":
		return 8
	case "fast":
		return 10
	case "faster", "veryfast":
		return 11
	case "superfast", "ultrafast":
		return 12
	default:
		return 8
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
