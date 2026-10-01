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
	"github.com/NSchatz/holdfast/internal/hwdevice"
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
// (TRANSCODE-PERF). The engine derives the job's encode plan from it before the encode
// runs, so an encoder handed that plan (EncodePlanEncoder) reads nothing off it; an
// encoder deriving its own plan - a direct caller's - reuses it instead of re-spawning
// ffprobe for pix_fmt and the colour tags. It may be nil (a direct caller - chiefly the
// tests - that did not pre-probe); FFmpegEncoder then takes its own snapshot of the same
// source, so behaviour is identical, just an extra probe this path avoids when the engine
// supplies one.
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

// EncodePlanEncoder is an Encoder whose command line is built from THIS JOB's encode plan -
// the one declared description of what the encode does, derived once by the engine before
// the encode runs (see EncodePlan).
//
// It is a separate optional interface for the reason StreamPlanEncoder is: an Encoder with
// no per-job behaviour has nothing to do with a plan, and a test's deterministic fake writes
// the bytes it was told to write. ForEncodePlan must return an Encoder equivalent to the
// receiver in every respect but the plan, and must not mutate the receiver - the engine's
// workers share one Encoder across goroutines.
//
// The plan is HANDED IN and never re-derived by an encoder that was handed one: the command
// line and every gate the output is checked by read ONE plan, so no gate can check the
// output against something other than what was encoded. An Encoder that WRAPS a plan-reading
// encoder must forward ForEncodePlan as well as ForProfile and ForStreamPlan: one that does
// not leaves the wrapped encoder to derive its own plan from its own configuration, as a
// direct caller's does, which is the second derivation this interface exists to prevent.
type EncodePlanEncoder interface {
	Encoder
	ForEncodePlan(plan *EncodePlan) Encoder
}

// EncoderFunc adapts a plain function to Encoder (used by tests).
type EncoderFunc func(ctx context.Context, in, out string, props *probe.VideoProps) error

// Encode calls the wrapped function.
func (f EncoderFunc) Encode(ctx context.Context, in, out string, props *probe.VideoProps) error {
	return f(ctx, in, out, props)
}

// FFmpegEncoder is the production encoder. It builds its command line from THIS JOB's
// encode plan (EncodePlan): the streams the plan carries (every stream but data where no
// intended map was derived, -map 0 -map -0:d?), copied, with the video re-encoded by the
// plan's registry encoder at its pixel format and quality value, through its picture
// operations, and written with its colour description - the -color_* tags on every encoder,
// the declared primaries and transfer stamped onto the frames for every encoder but libx265,
// and the HDR10 static-metadata master-display/max-cll parameters on libx265, where every
// other encoder takes those blocks from the frames' side data (see videoArgs); the output
// fidelity gate holds whatever comes out to the plan (docs/design/encode-plan.md#fidelity).
//
// The engine derives the plan and hands it over (ForEncodePlan). An encoder that was handed
// none derives it itself, from Cfg, the profile, the stream map and the source, through the
// same derivation - which is what a direct caller of this type gets, and the only path
// that needs Probe: a nil Probe there is a programmer error, refused rather than
// dereferenced.
//
// CPU libx265 is the archival default; SVT-AV1 and the hardware encoders (NVENC/
// QSV/VAAPI/AMF, TRANSCODE-6) are opt-in and gated behind a runtime capability
// check (internal/encoder.Available) BEFORE the engine ever calls Encode - see
// cmd/holdfast's cmdRun. Encode itself never re-derives availability; it trusts
// the caller checked, and simply builds and runs the command for whatever registry
// encoder the plan names.
type FFmpegEncoder struct {
	FFmpeg string
	// Cfg is the configuration a plan this encoder derives itself is resolved from: its
	// encode profiles and its top-level bitrate target. It is not consulted when the engine
	// hands the encoder a plan - the engine derived that one from its own configuration, which
	// in production is this one.
	Cfg   config.Config
	Probe *probe.Prober

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

	// Job is THIS JOB's encode plan, derived once by the engine and handed here through
	// ForEncodePlan: the command line is built from it and from nothing else about the job,
	// and every gate reads the same value. Cfg, Prof and Plan are then not consulted at all -
	// the plan already carries everything they would have resolved to.
	//
	// nil means the encoder derives the plan itself, from Cfg, the profile, Plan and the
	// source, through the same derivation the engine uses (deriveEncodePlan) - which is what a
	// direct caller of this exported type gets.
	Job *EncodePlan

	// X265 is the parallelism every libx265 encode this encoder builds is told to use:
	// a worker-pool size and a frame-thread count, joined to the -x265-params string on
	// the quality-targeted and the bitrate-targeted path alike. The run derives it once,
	// from the CPU quota or the x265_cpus key (see DeriveX265), and hands it here.
	//
	// The zero value passes neither figure, which leaves libx265's own defaults in force
	// and keeps the argv byte for byte what an encoder without this field built. No other
	// encoder reads it: pools and frame-threads are libx265 mechanisms.
	X265 encoder.X265Parallelism

	// Devices are the render nodes this host assigned to VAAPI and QSV (hwdevice.Assign), read
	// by a plan this encoder derives itself; a plan handed in carries the engine's. The zero
	// value assigns /dev/dri/renderD128 to both.
	Devices hwdevice.Assignment

	// Memory is the resident-memory bound every invocation this encoder makes is held to:
	// while ffmpeg runs, its resident memory is sampled, and at the threshold the process
	// is terminated and the encode fails with a *MemoryAbortError. The run derives it once,
	// from the cgroup memory limit (see DeriveMemoryWatch), and hands it here.
	//
	// The zero value watches nothing, which is the encode this package ran before the
	// watchdog existed.
	Memory MemoryBound

	// procRoot, when non-empty, replaces /proc as the place the watchdog reads a process's
	// resident memory from. Unexported test seam: /proc is filesystem state outside this
	// package's boundary, and pointing it at a directory with no entry for the process is how
	// a test drives the "the sample could not be read" path. Production leaves it "".
	procRoot string

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

// ForEncodePlan returns this encoder built from plan. The receiver is a VALUE, so the copy
// is the whole of the isolation the engine's workers need, exactly as it is for ForProfile.
func (e FFmpegEncoder) ForEncodePlan(plan *EncodePlan) Encoder {
	e.Job = plan
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

// Encode runs ffmpeg. It returns an error if the plan cannot be built - a plan handed in for
// another job, an operation it declares that this build cannot perform, or, on a plan this
// encoder derives itself, any refusal of the derivation (see deriveEncodePlan) - or if
// ffmpeg exits non-zero. The engine then discards the temp and leaves the source untouched.
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
	// THE PLAN this encode is built from: the engine's, handed in, or - for a direct caller
	// that handed none - derived here from this encoder's configuration, profile and stream
	// map by the same function the engine derives its own with. Either way there is ONE
	// derivation and the command line reads nothing else about the job.
	job := e.Job
	if job == nil {
		prof := e.profile()
		derived, err := deriveEncodePlan(planInputs{
			settings: e.Cfg.TranscodeIn(prof, in), prof: prof, source: in, output: out, streams: e.Plan,
			devices: e.Devices, snapshot: e.snapshot(ctx, in, props),
		})
		if err != nil {
			return err
		}
		job = derived
	} else if job.Source != in || job.Output != out {
		return fmt.Errorf("the encode plan was derived for %s -> %s and this encode reads %s and writes %s: "+
			"refusing to build a command line from another job's plan", job.Source, job.Output, in, out)
	}
	pre, body, err := job.args(e.X265)
	if err != nil {
		return err
	}
	return e.runCarrying(ctx, in, out, job, sink, pre, body)
}

// snapshot is how a direct caller's derivation reaches the source's probe snapshot: the one
// the caller handed in, or one this encoder takes of the same source, which is equivalent -
// the engine threads in the snapshot it already probed for its skip guards (TRANSCODE-PERF)
// and this only spares a duplicate probe when it does. A nil Probe is a programmer error,
// refused rather than dereferenced.
func (e FFmpegEncoder) snapshot(ctx context.Context, in string, props *probe.VideoProps) func() (*probe.VideoProps, error) {
	return func() (*probe.VideoProps, error) {
		if e.Probe == nil {
			return nil, fmt.Errorf("FFmpegEncoder.Probe is nil (required to derive colour/pixel-format args from the source)")
		}
		if props == nil {
			props = e.Probe.VideoProps(ctx, in)
		}
		return props, nil
	}
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
//
// Whether the output is Matroska is read from the container extension out's name carries
// (containerExtOf), never from its last extension, which beside a source is TempSuffix.
func matroskaPictures(out string, plan *StreamPlan) ([]matroskaPicture, error) {
	if !strings.EqualFold(containerExtOf(out), "mkv") {
		return nil, nil
	}
	sources := plan.Pictures()
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
func picturePath(out string, i int) string { return auxTempPath(out, ".picture"+strconv.Itoa(i)) }

// sidecarTempPath is the temp the i-th subtitle sidecar of the job whose swap reads out is
// extracted to, on picturePath's terms: the working file's own name plus ".subtitle<i>", in
// its directory (the source's, so the sidecar's link(2) to its final name never crosses a
// filesystem), under the temp marker the sweeps recognise, and never a name a media server
// reads as a subtitle or a video. See docs/design/subtitles.md#sidecars.
func sidecarTempPath(out string, i int) string {
	return auxTempPath(out, ".subtitle"+strconv.Itoa(i))
}

// auxTempPath is out plus tail, shortened as picturePath describes where that would pass
// NAME_MAX.
func auxTempPath(out, tail string) string {
	dir, base := filepath.Split(out)
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
//
// It is the one funnel every encode's output goes through, re-encode and remux alike, so it
// is where the output's container is named: container's options join the job's own, and
// only the encode's - a picture's copy names its own muxer, image2.
func (e FFmpegEncoder) runCarrying(ctx context.Context, in, out string, job *EncodePlan,
	sink ProgressSink, pre, body []string) error {
	body = append(append([]string(nil), body...), job.container.args()...)
	pics := job.coverArt.attached
	if len(pics) == 0 {
		return e.runFFmpeg(ctx, in, out, sink, pre, body)
	}
	in, out = absolutePath(in), absolutePath(out)
	dir := filepath.Dir(out)
	defer removePictures(dir, pics)
	e = e.runningIn(dir)
	first := job.Streams.MappedAttachments()
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

// muxQueueBounds are the OUTPUT options that bound ffmpeg's documented mux-side queues, on
// every invocation runFFmpeg makes. They are output options with no stream specifier, so
// each applies to every output stream, and they sit after the job's own options and before
// the output path.
//
//   - -max_muxing_queue_size: the packets buffered per stream while the muxer waits for its
//     first packet of every stream.
//   - -muxing_queue_data_threshold: the bytes per stream below which that packet count is
//     not taken into account.
//   - -thread_queue_size: the packets per stream that may be queued to the muxing thread.
//
// Each figure is the pinned ffmpeg's own default for that queue (128 packets, 50 MiB and 8
// packets), so passing them never loosens a bound: it makes the bound a property of this
// command line rather than of whichever ffmpeg build happens to run it. They do not bound
// the encoder's own memory, which is what the resident-memory watchdog is for
// (docs/encode-memory.md).
var muxQueueBounds = []string{
	"-max_muxing_queue_size", "128",
	"-muxing_queue_data_threshold", "52428800",
	"-thread_queue_size", "8",
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
	args = append(args, muxQueueBounds...)
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

	// The memory watchdog runs beside the process and never through ctx: a cancelled
	// context is how the engine recognises an interruption, and an abort is a failure.
	var dog *memoryWatchdog
	if e.Memory.Armed() {
		dog = startMemoryWatchdog(e.Memory, cmd.Process, e.procRoot)
	}

	err := cmd.Wait()
	// The process has been waited for, so the watchdog has nothing left to sample; stopping
	// it here means no sampler outlives the call.
	abort := dog.stop()
	// The encoder has exited, so its end of the progress pipe is closed and the drain
	// has finished (or is about to); waiting for it keeps the reader from outliving the
	// call and reporting progress for a job that is already terminal.
	<-drained
	if pr != nil {
		_ = pr.Close()
	}
	// An aborted encode is a failed encode even where the process exited 0 after being
	// asked to stop: what it wrote is a truncated file, and verification never sees it.
	if abort != nil {
		return fmt.Errorf("ffmpeg encode: %w: %s", abort, truncate(outb.String(), 500))
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

// videoArgs assembles the per-encoder ffmpeg args (everything after `-c:v
// <codec>`, before the trailing `-- <out>`). Every Spec gets the -color_* flags and
// -fps_mode passthrough, and every Spec but VAAPI an explicit -pix_fmt (VAAPI names its
// format in the upload chain instead). Of the -color_* flags only -colorspace and
// -color_range take effect on the pinned ffmpeg: the encoder takes primaries and transfer
// from the frames, which is why EncodePlan.args stamps them (hdr.Color.SetParams) for every
// encoder but libx265. Beyond that each encoder family has its own quality-knob shape:
//
//   - libx265 (cpu): -preset/-crf plus -x265-params, which carries x265Extra: the
//     encode's pool size and frame-thread count when it has them, then the HDR10
//     static-metadata master-display/max-cll block (both libx265-only mechanisms).
//   - libsvtav1 (svtav1): -preset (numeric 0-13, mapped from the config Preset
//     word - see svtav1Preset) + -crf. No x265Params. On the pinned ffmpeg the
//     encoder takes the HDR10 mastering-display and content-light blocks from the
//     frames' side data (measured 2026-09-30: both reach the output), and the matrix
//     and range from the -color_* flags; the primaries and transfer it takes from the
//     frames, NOT from -color_primaries/-color_trc, so EncodePlan.args stamps the
//     declared ones onto the frames (hdr.Color.SetParams). Whatever an encoder fails
//     to carry, the output fidelity gate rejects (docs/design/encode-plan.md#fidelity).
//   - hevc_nvenc/av1_nvenc: -rc vbr -cq <quality> -b:v 0 + a preset.
//   - hevc_qsv: a QSV device derived from a VAAPI device on the plan's render node,
//     opened over DRM (emitted before -i - see deviceArgs) + -global_quality <quality>.
//   - hevc_vaapi: -vaapi_device <the plan's render node>,connection_type=drm (emitted
//     before -i - see deviceArgs) + -vf format=<input format>,hwupload (+ -profile:v
//     main10 for p010le) + -qp <quality>. No device is reachable here, so these shapes
//     are proven on golden argv and a stand-in ffmpeg; the startup check
//     (internal/encoder.Available, through this same builder) keeps them from running
//     on a host whose device does not encode, and a hardware report (brief T43) is what
//     shows them on a real one.
//   - hevc_amf: -rc cqp -qp_i <quality> -qp_p <quality>.
//
// <quality> is the plan's Quality.Value: the job's quality.<key> on that encoder's
// own scale, or its crf where the configuration carries none (internal/encoder's
// QualityScale).
//
// THE PIXEL FORMAT is the plan's InputFormat, named explicitly: a format the encoder
// lists that carries the plan's chroma and depth (encoder.Spec.InputFormat), so ffmpeg
// never auto-selects one behind -loglevel error. Every encoder takes it as -pix_fmt
// except VAAPI, whose encoder accepts only `vaapi` surfaces: its format is the
// software layout uploaded (`format=<fmt>,hwupload`), and it gets NO -pix_fmt. Without
// one, fftools constrains the filter graph's output to the encoder's own list
// (fftools/ffmpeg_mux_init.c:907-911 reads it into the output filter's pix_fmts,
// fftools/ffmpeg_filter.c:865-868 applies it), which for hevc_vaapi is `vaapi`, what
// hwupload produces; that is also the form of every encode example in
// https://trac.ffmpeg.org/wiki/Hardware/VAAPI (read 2026-09-30, Wayback capture of
// 2026-01-22). A software -pix_fmt there would name a format the chain does not end
// in, and the command line would contradict itself. Sources at
// https://github.com/FFmpeg/FFmpeg/tree/5d4d3bdc61 , read 2026-09-30.
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
func videoArgs(v VideoPlan, colorArgs []string, x265Extra string) []string {
	var args []string
	if !v.Encoder.Uploads() {
		args = []string{"-pix_fmt", v.InputFormat}
	}
	args = append(args, colorArgs...)
	args = append(args, "-fps_mode", "passthrough") // a VFR source is not forced to CFR

	q := v.Quality
	if q.TargetsBitrate() {
		return append(args, bitrateArgs(v, x265Extra)...)
	}

	value := strconv.Itoa(q.Value)
	switch {
	case v.Encoder.FFmpegCodec == "libx265":
		args = append(args,
			"-preset", q.Preset,
			"-crf", value,
			"-x265-params", "log-level=error"+x265Extra,
		)
	case v.Encoder.FFmpegCodec == "libsvtav1":
		args = append(args,
			"-preset", strconv.Itoa(svtav1Preset(q.Preset)),
			"-crf", value,
		)
	case v.Encoder.FFmpegCodec == "libx264":
		// libx264 takes libx265's preset words (ultrafast ... placebo are x264's own
		// names) and its -crf. It writes the HDR10 blocks from the stream's side data
		// (libavcodec/libx264.c:1036-1058 at 5d4d3bdc61, read 2026-09-30), so it needs no
		// parameter string; the output fidelity gate holds it to them like any encoder.
		args = append(args,
			"-preset", q.Preset,
			"-crf", value,
		)
	case v.Encoder.API == encoder.APINVENC:
		args = append(args,
			"-rc", "vbr",
			"-cq", value,
			"-b:v", "0",
			"-preset", "p5",
		)
	case v.Encoder.API == encoder.APIQSV:
		args = append(args, "-global_quality", value)
	case v.Encoder.API == encoder.APIVAAPI:
		// -vaapi_device itself is emitted from the plan's device (a global option that
		// must precede -i - see EncodePlan.args); here we only add the encode-side args
		// that come after -c:v.
		args = append(args, vaapiUpload(v.Encoder, v.InputFormat)...)
		args = append(args, vaapiQuality(v.Encoder, value)...)
	case v.Encoder.API == encoder.APIAMF:
		args = append(args,
			"-rc", "cqp",
			"-qp_i", value,
			"-qp_p", value,
		)
	}
	return args
}

// vaapiQuality is a VAAPI encoder's quality target. hevc_vaapi and h264_vaapi take an explicit
// -qp, which selects constant-QP rate control (libavcodec/vaapi_encode_h265.c:1084-1085,
// vaapi_encode_h264.c:1048-1049). av1_vaapi has no -qp option: constant-QP is named with
// -rc_mode CQP, which the encoder takes first and refuses where the driver lacks it
// (vaapi_encode.c:1318-1319), and the target is the generic -global_quality, read as the AV1
// quantiser index (vaapi_encode.c:1419-1425, vaapi_encode_av1.c:140). Naming the mode rather
// than leaving it to the driver keeps the value on the one scale the configuration validated
// it for: without it a driver offering ICQ would read the same number as an ICQ factor on
// 1-51 (vaapi_encode.c:1332-1335, 1527). Sources at
// https://github.com/FFmpeg/FFmpeg/tree/5d4d3bdc61 , read 2026-09-30.
func vaapiQuality(spec encoder.Spec, value string) []string {
	if spec.TargetCodec == "av1" {
		return []string{"-rc_mode", "CQP", "-global_quality", value}
	}
	return []string{"-qp", value}
}

// vaapiUpload is a VAAPI encode's carrier: the software frames converted to the plan's input
// format and uploaded to a surface of the same layout, and - for 10-bit (p010le) HEVC - the
// Main 10 profile, since hevc_vaapi's default profile is chosen for 8-bit. `main10` is a named
// value of hevc_vaapi's -profile on the pinned binary (`ffmpeg -h encoder=hevc_vaapi`, run
// 2026-09-30), and the wiki's own 10-bit HEVC encode uploads p010 with profile 2, which is
// main10 (https://trac.ffmpeg.org/wiki/Hardware/VAAPI, read 2026-09-30). av1_vaapi names no
// profile: AV1 Main covers 8 and 10 bits, and the encoder picks its entry by the surface's
// depth (libavcodec/vaapi_encode_av1.c:842-844 at 5d4d3bdc61); h264_vaapi uploads only nv12.
func vaapiUpload(spec encoder.Spec, format string) []string {
	args := []string{"-vf", "format=" + format + ",hwupload"}
	if spec.TargetCodec == "hevc" && format == "p010le" {
		args = append(args, "-profile:v", "main10")
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

// bitrateArgs is the TARGET-BITRATE half of videoArgs: everything after the
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
func bitrateArgs(v VideoPlan, x265Extra string) []string {
	q := v.Quality
	rate := strconv.Itoa(q.BitrateKbps) + "k"
	var args []string
	switch {
	case v.Encoder.FFmpegCodec == "libx265":
		args = []string{
			"-preset", q.Preset,
			"-b:v", rate,
			"-x265-params", "log-level=error" + x265Extra,
		}
	case v.Encoder.FFmpegCodec == "libsvtav1":
		args = []string{
			"-preset", strconv.Itoa(svtav1Preset(q.Preset)),
			"-b:v", rate,
		}
	case v.Encoder.FFmpegCodec == "libx264":
		args = []string{
			"-preset", q.Preset,
			"-b:v", rate,
		}
	case v.Encoder.API == encoder.APINVENC:
		args = []string{
			"-rc", "vbr",
			"-b:v", rate,
			"-preset", "p5",
		}
	case v.Encoder.API == encoder.APIQSV:
		args = []string{"-b:v", rate}
	case v.Encoder.API == encoder.APIVAAPI:
		args = append(vaapiUpload(v.Encoder, v.InputFormat), "-b:v", rate)
	case v.Encoder.API == encoder.APIAMF:
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
