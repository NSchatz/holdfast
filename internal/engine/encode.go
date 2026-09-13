package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/NSchatz/holdfast/internal/config"
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
}

// ForProfile returns this encoder built from prof's knobs. The receiver is a VALUE, so
// the copy is the whole of the isolation the engine's workers need: several files under
// several roots encode concurrently and none of them can see another's profile.
func (e FFmpegEncoder) ForProfile(prof config.Profile) Encoder {
	e.Prof = &prof
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
	if spec.Key == "vaapi" {
		// -vaapi_device is a GLOBAL option that must precede -i so the hwupload
		// filter (added below by buildArgs) has a device to target. Every other
		// Spec has no such ordering requirement.
		args = append(args, "-vaapi_device", "/dev/dri/renderD128")
	}
	args = append(args, "-i", in,
		"-map", "0", "-map", "-0:d?",
		"-c", "copy", "-c:v", spec.FFmpegCodec,
	)
	// An ATTACHED PICTURE is a video stream and `-c:v` above would re-encode it, so each
	// one is pinned back to copy by its own per-stream option. It must come AFTER the
	// blanket -c:v, which is what it overrides; `-map 0` preserves stream order, so the
	// N of an output `v:N` is the N of the source's. A single-video-stream source yields
	// no such option and therefore byte-identical argv to the encoder that predates this.
	//
	// Whether ffprobe ESTABLISHED that shape is not dropped. An encoder that could not
	// find out what video streams its source carries cannot know whether one of them is
	// artwork that must be pinned back to copy, and an unknown shape has to fail safe
	// rather than default to the common one - the same posture the engine's own
	// source-shape guard takes, and the same one the pixel-format derivation above takes
	// for the same class of unknown. Through the engine this is unreachable: that guard
	// skipped the file already and hands this call the snapshot it read. It is the
	// backstop for a direct caller of this exported type, which builds its own.
	streams, established := props.VideoStreams()
	if !established {
		return fmt.Errorf("cannot establish the video streams of %q (ffprobe did not answer): "+
			"refusing to encode without knowing whether one of them is an attached picture", in)
	}
	for _, i := range attachedPictureCopyIndexes(streams) {
		args = append(args, "-c:v:"+strconv.Itoa(i), "copy")
	}
	args = append(args, buildArgs(spec, ts, pixFmt, colorArgs, x265Color)...)
	args = append(args, "--", out)

	if e.argvObserver != nil {
		e.argvObserver(args)
	}

	cmd := exec.CommandContext(ctx, e.FFmpeg, args...)
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
//   - libx265 (cpu): -preset/-crf plus x265Params (HDR10 static-metadata
//     master-display/max-cll — a libx265-only mechanism).
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
// x265Color block are the same on both paths, so a bitrate-targeted encode carries
// exactly the same source fidelity as a quality-targeted one.
func buildArgs(spec encoder.Spec, ts config.Transcode, pixFmt string, colorArgs []string, x265Color string) []string {
	args := []string{"-pix_fmt", pixFmt}
	args = append(args, colorArgs...)
	args = append(args, "-fps_mode", "passthrough") // a VFR source is not forced to CFR

	if ts.TargetsBitrate() {
		return append(args, bitrateArgs(spec, ts, x265Color)...)
	}

	switch spec.Key {
	case "cpu":
		args = append(args,
			"-preset", ts.Preset,
			"-crf", strconv.Itoa(ts.CRF),
			"-x265-params", "log-level=error"+x265Color,
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
//     -x265-params HDR10 block stay exactly as they are on the quality path.
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
func bitrateArgs(spec encoder.Spec, ts config.Transcode, x265Color string) []string {
	rate := strconv.Itoa(ts.BitrateKbps) + "k"
	switch spec.Key {
	case "cpu":
		return []string{
			"-preset", ts.Preset,
			"-b:v", rate,
			"-x265-params", "log-level=error" + x265Color,
		}
	case "svtav1":
		return []string{
			"-preset", strconv.Itoa(svtav1Preset(ts.Preset)),
			"-b:v", rate,
		}
	case "nvenc", "av1_nvenc":
		return []string{
			"-rc", "vbr",
			"-b:v", rate,
			"-preset", "p5",
		}
	case "qsv":
		return []string{"-b:v", rate}
	case "vaapi":
		return []string{
			"-vf", "format=nv12,hwupload",
			"-b:v", rate,
		}
	case "amf":
		return []string{
			"-rc", "vbr_peak",
			"-b:v", rate,
		}
	}
	// A Spec this build ships but this function does not name would silently lose
	// the operator's target, so it gets the codec-independent option and nothing
	// else rather than the quality knob it did not ask for.
	return []string{"-b:v", rate}
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
