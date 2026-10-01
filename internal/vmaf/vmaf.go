// Package vmaf runs libvmaf (via ffmpeg) to measure the perceptual quality of a transcoded
// output against its source: the last no-loss layer. The structural gates prove an output
// exists, decodes and carries the tracks; VMAF ESTIMATES whether it still looks like the
// source. A transcode that keeps the resolution identical - which is every job on a
// configuration that sets no `max_height` - applies with no scaling at all.
//
// Three pooled statistics come back and the gate needs ALL of them (see Result): a mean
// alone averages a destroyed segment away, and luma alone misses a transcode that leaves
// every luma sample intact and still ruins the colour.
//
// libvmaf's filter takes the DISTORTED stream as its first input and the REFERENCE as its
// second; getting this backwards inverts the meaning, so it is fixed here.
//
// # Scoring across a resolution change, and the direction that is not optional
//
// Where the encode scaled the picture down, the two files are no longer the same size and
// something has to make them one. The choice is not symmetric and only one arm of it is a
// measurement of the encode:
//
//   - Scale the DISTORTED output back UP to the source's resolution and score there. Every
//     detail the downscale threw away is missing from the up-scaled output and present in
//     the reference, so the pooled statistics carry what the downscale cost. This is what
//     DistortedFilter does and it is the only direction this package offers.
//   - Scale the REFERENCE down to meet the output. The detail is then absent from BOTH
//     sides, nothing measures its loss, every downscaled encode scores near the top of the
//     scale, and the floors admit outputs they exist to refuse. The source is deleted
//     immediately afterwards, and nothing can re-run a measurement whose subject is gone.
//
// So a scaling filter reaches the reference chain through exactly one field, ReferenceFilter,
// whose whole job is to reproduce on the source a transformation the ENCODE also applied
// (today a deinterlace). The up-scale is not such a transformation and never travels there.
package vmaf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Result is the pooled VMAF over the (sub)sampled frames.
//
// What a VMAF score does and does not license: it is a regression onto a SUBJECTIVE
// opinion scale (ACR), not a measure of signal identity. 100 is a label-normalisation
// anchor, not "identical to the source", and a bit-identical file is not guaranteed to
// score it. A high score means "no worse than X against your source, under this model and
// viewing condition" and nothing stronger; the widely-repeated "~6 VMAF points = 1 JND"
// figure is practitioner folklore with no primary source, so do not repeat it.
//
// The default model extracts LUMA FEATURES ONLY: structurally blind to chroma damage, weak
// on banding and dark-region blockiness by Netflix's own account, and prone to
// OVER-predicting quality on high-motion scenes, which is the dangerous direction for a
// pass/fail gate. That blindness is why every Score also measures the chroma planes
// directly (see ChromaMin).
type Result struct {
	// HarmonicMean is the pooled mean over the (sub)sampled frames. It is an AVERAGE, and
	// Netflix documents that mean pooling "has the risk of hiding poor quality frames";
	// the harmonic mean is a weak correction, not a fix, so it must never be the sole gate.
	HarmonicMean float64
	// Min is the worst single (sub)sampled frame, the statistic that bounds LOCAL damage.
	// A short destroyed segment inside an otherwise-clean encode passes every structural
	// check and the pooled mean; only this catches it.
	Min float64

	// ChromaMin is the worst single (sub)sampled frame's PSNR over the CHROMA planes, in dB:
	// the WORSE of Cb and Cr on the worst frame each of them had, and the statistic the
	// chroma floor is enforced against. PSNR over Cb and Cr is the metric because it is
	// computed over those planes and nothing else, so a value that falls can only mean chroma
	// changed; CIEDE2000, libvmaf's other colour-aware feature, mixes lightness back in
	// through Lab and would not say WHICH thing moved. It is the raw min for the reason Min
	// is: an average lets a destroyed Cr hide behind an intact Cb.
	ChromaMin float64
	// ChromaMetric names what ChromaMin is a measurement of, and travels WITH the value
	// everywhere it is recorded: a bare number with no metric attached is not interpretable.
	ChromaMetric string

	// PixelFormat is the single pixel format BOTH streams were converted to before they were
	// compared, named by holdfast and never left to libavfilter's negotiation. It is part of
	// what was measured: an 8-bit source and a 10-bit output score differently depending on
	// which way the conversion went.
	PixelFormat string

	// Stream names WHICH video stream of each file the comparison was made against, in the
	// specifier vocabulary every ffprobe read in this program already uses (see ScoredStream).
	// On a source carrying more than one video stream, a score that does not say which stream
	// it looked at cannot be lined up against the guards that inspected the same file. "" is
	// NOT RECORDED and never a fabricated default.
	Stream string

	// ReferenceFilter is the filter expression the REFERENCE was put through before the
	// comparison, and "" where the reference was the source as it is.
	//
	// It is part of what was measured, exactly as PixelFormat is. Where the encode applied a
	// transformation the source did not carry - today a deinterlace - a score taken against
	// the untransformed source measures the difference the FILTER made and not the
	// difference the ENCODE made, and the two are not close: a deinterlaced encode scored
	// against its interlaced source scores like a damaged encode. So the filter travels with
	// the number everywhere the number goes, because a score whose reference nobody can name
	// is not interpretable.
	ReferenceFilter string

	// DistortedFilter is the filter expression the DISTORTED output was put through before
	// the comparison, and "" where it was scored as it is - which is every job that scaled
	// nothing.
	//
	// Today it carries one thing: the up-scale back to the source's resolution that a
	// downscaling job owes. It is recorded for the reason ReferenceFilter is, and with one
	// extra claim: it says WHICH SIDE was resampled. A row carrying a scale on this field
	// was scored at the source's resolution against an unfiltered reference; a row carrying
	// one on ReferenceFilter would have been scored against a reference that was degraded
	// first, and those two numbers are not comparable.
	DistortedFilter string
}

// Request is one scoring pass: which files, at what sampling interval, under which model,
// in which named comparison format. PixelFormat is not optional and must never be silently
// defaulted - a zero value is a REFUSAL from Score, never "let ffmpeg decide".
type Request struct {
	// Distorted is the freshly-encoded output (libvmaf's first input).
	Distorted string
	// Reference is the source it must still look like (libvmaf's second input).
	Reference string
	// Subsample is the frame-sampling interval; <1 is floored to 1 (every frame).
	//
	// It comes from operator configuration (vmaf_subsample) and from nothing else. It is
	// never derived from the file's duration or size, from the CPU count or from the CPU
	// quota, because an interval chosen by the machine changes WHAT THE FLOORS BOUND
	// without the operator having asked for it: a sampled gate bounds the worst SAMPLED
	// frame, and the unsampled ones are never seen at all. Threads is the knob that
	// answers to the machine; this one answers to the operator.
	Subsample int
	// Threads is this pass's whole thread SHARE, and it is REQUIRED on the same terms
	// PixelFormat is: Score refuses anything below 1 rather than send a value libvmaf
	// would read as its own default.
	//
	// It is the share and not the libvmaf count because a pass runs two thread pools, and
	// both are paid for out of it (see PoolThreads). Handing the whole share to each would
	// make every pass ask for twice what it was given, and the gates of every worker
	// together for twice the quota the share was divided from.
	//
	// It is a SPEED knob and must never be a measurement one. Scoring is by far the
	// slowest phase of a job and libvmaf with no thread count runs on about one CPU, so
	// the count is derived from the CPU bandwidth this process is actually allowed (see
	// internal/cpuquota) and divided across the gates that may score at once. What it
	// must not do is move the number: the score is what licenses deleting the source, so
	// TestS0160_AC3_ThreadCountDoesNotMoveTheGateFigures measures the same pair at one
	// thread and at the derived count and holds the three reported figures to 0.01 of
	// each other.
	Threads int
	// Model is the libvmaf model spec, already resolved (see ResolveModel).
	Model string
	// PixelFormat is the format both inputs are converted to before scoring. It is
	// REQUIRED: Score refuses an empty value rather than fall back to negotiation.
	PixelFormat string
	// ReferenceFilter is an ffmpeg filter expression applied to the REFERENCE before the
	// comparison, so that a reference is produced from the source by the SAME transformation
	// the encode applied. "" is the ordinary case and leaves the graph exactly as it was.
	//
	// It is a filter EXPRESSION and rides into the filtergraph unescaped, which is what lets
	// it carry its own options (`yadif=mode=send_frame:parity=auto:deint=all`). Its value
	// comes from this build's own closed registry (internal/deinterlace), followed where the
	// encode cropped by the crop's own expression (internal/crop, integers it measured), and
	// never from a path, a filename or anything else a library can influence.
	//
	// A SCALE NEVER BELONGS HERE. See the package comment: a reference resampled down to
	// meet a smaller output is a reference that lost the detail the gate exists to measure.
	ReferenceFilter string

	// DistortedFilter is an ffmpeg filter expression applied to the DISTORTED output before
	// the comparison, so that an output the encode made SMALLER is scored at the resolution
	// of the file it is about to replace. "" is the ordinary case and leaves the graph
	// exactly as it was.
	//
	// It is a filter EXPRESSION on the same terms ReferenceFilter is, and its value comes
	// from this build's own closed registry (internal/downscale) and never from anything a
	// library can influence.
	DistortedFilter string
}

// ErrUnavailable indicates the ffmpeg build has no libvmaf filter, so quality cannot be
// measured. With the gate enabled the caller must treat it as a rejection: never accept an
// unmeasured encode.
var ErrUnavailable = errors.New("libvmaf is not available in this ffmpeg build")

// Available reports whether the ffmpeg build exposes the libvmaf filter. It reads the
// same listing RequireChain reads, through the same parser, so "this build has libvmaf"
// has one answer here.
func Available(ctx context.Context, ffmpeg string) bool {
	have, err := filterSet(ctx, ffmpeg)
	return err == nil && have[LibvmafFilter]
}

// vmafLog is the subset of libvmaf's JSON log we consume. EVERY pooled statistic is a
// POINTER so that "libvmaf did not emit this field" stays distinguishable from "libvmaf
// measured 0.0". That distinction is load-bearing: the worst-frame floor is enforced as
// `Min < vmaf_min_pool`, so an absent Min unmarshalling to 0.0 would either reject
// everything or, read as optional, quietly degrade the gate back to mean-only. The chroma
// planes follow the same discipline for a sharper reason: 0.0 dB is an obliterated plane.
// An unparseable or incomplete log is a REJECTION, never a fallback.
type vmafLog struct {
	PooledMetrics struct {
		VMAF struct {
			Min          *float64 `json:"min"`
			HarmonicMean *float64 `json:"harmonic_mean"`
		} `json:"vmaf"`
		PsnrCb struct {
			Min *float64 `json:"min"`
		} `json:"psnr_cb"`
		PsnrCr struct {
			Min *float64 `json:"min"`
		} `json:"psnr_cr"`
	} `json:"pooled_metrics"`
}

// # Where the log goes: a pipe, never a file (S0162)
//
// libvmaf writes one JSON entry per scored frame, with PSNR on every one, so on a feature
// film the log runs to tens of megabytes and more. It used to be written to a file in the
// process temp directory and then read back whole. In the shipped container that directory is
// a tmpfs beside a read-only root, so the file was RAM charged to the container's memory
// limit, and reading it whole cost holdfast's own heap about as much again: enough, on a long
// film, to run the container out of memory mid-verify and end the job with no verdict.
//
// The route taken is to STREAM it. Score hands ffmpeg the write end of a pipe as descriptor
// logFD and points libvmaf's log_path at logPipePath, so there is no log file anywhere: not
// in the temp directory, not on the state or scratch filesystem, and nothing to clean up
// after a pass that returned or a process that was killed. decodeLog reads the pipe while
// ffmpeg runs, walks the top-level object token by token, skips the per-frame entries one at
// a time without keeping any, and keeps only the four pooled statistics, so what holdfast
// holds does not grow with the number of frames. The other route the defect offered, a file
// on a disk-backed directory holdfast owns, was not taken: it would still write the whole
// log, it needs a directory to be validated at startup and a killed run's leftover to be
// swept, and it would still have to be parsed without being held whole. What libvmaf itself
// holds in memory until it writes the log is outside holdfast's reach and is not changed.
//
// The score is unchanged by construction: the same filtergraph, the same libvmaf, and the
// same encoding/json number parsing of the same four fields.
// TestS0162_AC4_RealFfmpegPipeScoreMatchesAnIndependentFileLog holds the returned figures
// bit-identical to an independent run whose log went to a file and was decoded whole.

// logFD is the descriptor number the child sees the log pipe's write end as. exec.Cmd hands
// ExtraFiles[i] to the child as descriptor 3+i, and the pipe is ExtraFiles[0].
const logFD = 3

// logPipePath is the log_path libvmaf is given: the child's own descriptor logFD, reached
// through procfs. libvmaf opens its log_path with fopen(path, "w") and writes it at the end
// of the pass; opening /proc/self/fd/N re-opens the pipe that descriptor refers to, so libvmaf
// writes into the pipe exactly as it would into a file. /proc/self/fd rather than /dev/fd
// because /dev/fd is only a link to it: the OCI runtime spec has the runtime create /dev/fd
// pointing at /proc/self/fd "if the source file exists after processing mounts"
// (https://github.com/opencontainers/runtime-spec/blob/main/runtime-linux.md#dev-symbolic-links,
// read 2026-09-30), so /proc/self/fd is what both spellings depend on. Both spellings
// were tried against the pinned ffmpeg (N-125875-g5d4d3bdc61) with log_fmt=json and both
// delivered the JSON log through the descriptor.
// TestS0162_AC4_RealFfmpegPipeScoreMatchesAnIndependentFileLog is the standing proof.
var logPipePath = "/proc/self/fd/" + strconv.Itoa(logFD)

// newLogPipe creates the log channel. It is a variable only so a test can make the channel
// impossible to set up (AC-8); nothing else assigns it.
var newLogPipe = os.Pipe

// pipeCloseDelay bounds how long a pass waits, after ffmpeg has exited or been killed, for
// its stdout/stderr and its log pipe to reach EOF. Only a process ffmpeg spawned and left
// holding one of them open can make that wait at all, and at most one pipe buffer is still
// unread when ffmpeg goes, so this is a bound on a stuck descriptor and never on a parse.
// It is well inside AC-7's "within 10 seconds of ffmpeg's exit or the cancellation".
const pipeCloseDelay = 3 * time.Second

// logDecode is what the reading goroutine hands back: the pooled statistics, or why the log
// could not be read.
type logDecode struct {
	log vmafLog
	err error
}

// errEmptyLog is a log channel that reached EOF with nothing on it: ffmpeg exited, crashed or
// was killed without libvmaf ever writing its log.
var errEmptyLog = errors.New("the log is empty - libvmaf wrote nothing before ffmpeg exited")

// decodeLog reads a libvmaf JSON log from r without holding it: it walks the top-level
// object key by key, decodes pooled_metrics (a few hundred bytes) into the vmafLog pointers,
// and skips every other value - the frames array element by element - without keeping any
// of it. What it holds is one frame entry at a time, whatever the number of frames.
//
// It is as strict as json.Unmarshal into vmafLog was, and stricter in one direction only:
//   - pooled_metrics may come before or after frames (libvmaf writes it after);
//   - an empty log, a value that is not a JSON object, a log cut off anywhere, and anything
//     after the object's closing brace are errors, never a partial result;
//   - a pooled statistic that is absent stays nil, so the caller's "missing a pooled
//     statistic" refusal and a genuine 0.0 remain distinguishable;
//   - a top-level key is matched exactly, where json.Unmarshal also accepted a key differing
//     only in case. libvmaf writes "pooled_metrics"; any other spelling now reads as the
//     statistics being absent, which is a refusal.
//
// The per-frame values never feed the result: the pooled statistics are libvmaf's own
// pooling, and they are what the gate has always been enforced against.
func decodeLog(r io.Reader) (vmafLog, error) {
	var parsed vmafLog
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err == io.EOF {
		return vmafLog{}, errEmptyLog
	}
	if err != nil {
		return vmafLog{}, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return vmafLog{}, fmt.Errorf("the log is not a JSON object (it starts with %v)", tok)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return vmafLog{}, err
		}
		key, ok := tok.(string)
		if !ok {
			return vmafLog{}, fmt.Errorf("the log carries a non-string object key %v", tok)
		}
		switch key {
		case "pooled_metrics":
			if err := dec.Decode(&parsed.PooledMetrics); err != nil {
				return vmafLog{}, fmt.Errorf("pooled_metrics: %w", err)
			}
		case "frames":
			if err := skipElements(dec); err != nil {
				return vmafLog{}, fmt.Errorf("frames: %w", err)
			}
		default:
			if err := skipValue(dec); err != nil {
				return vmafLog{}, fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	if _, err := dec.Token(); err != nil { // the object's closing brace
		return vmafLog{}, err
	}
	switch tok, err := dec.Token(); {
	case err == io.EOF:
		return parsed, nil
	case err != nil:
		return vmafLog{}, fmt.Errorf("after the log object: %w", err)
	default:
		return vmafLog{}, fmt.Errorf("the log carries data after its closing brace (%v)", tok)
	}
}

// discardValue is a decode target that keeps nothing. The decoder still reads and
// syntax-checks the whole value, so a malformed entry is an error, but the bytes are dropped
// as soon as the value ends.
type discardValue struct{}

func (*discardValue) UnmarshalJSON([]byte) error { return nil }

// skipElements skips an array one element at a time, so what is held is the largest single
// element and never the array. A value that is not an array is skipped as skipValue would.
func skipElements(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return skipRest(dec, tok)
	}
	var discard discardValue
	for dec.More() {
		if err := dec.Decode(&discard); err != nil {
			return err
		}
	}
	_, err = dec.Token() // the closing bracket
	return err
}

// skipValue skips the next value, token by token, holding nothing whatever its size.
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	return skipRest(dec, tok)
}

// skipRest skips what is left of a value whose first token has been read: nothing for a
// scalar, and everything up to the matching close for an object or an array. It counts depth
// rather than recursing, so a deeply nested value costs no stack.
func skipRest(dec *json.Decoder, first json.Token) error {
	d, ok := first.(json.Delim)
	if !ok || d == '}' || d == ']' {
		return nil
	}
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
	}
	return nil
}

// ChromaMetricName is the stable token recorded beside a chroma measurement, naming what
// was measured and in what unit. It is a WIRE FORMAT - it lands in the ledger, the event
// and the API payload - so it is a closed vocabulary, changed only with the readers in mind.
const ChromaMetricName = "psnr_cb/psnr_cr min (dB)"

// chromaFeature is the libvmaf `feature` option that produces psnr_y/psnr_cb/psnr_cr
// alongside the VMAF score. `feature` takes a "|"-delimited list; one entry is all this
// gate needs.
const chromaFeature = "name=psnr"

// PoolThreads splits ONE pass's thread share between the two thread pools a scoring pass
// runs, and returns what each is asked for: libvmaf's own pool (n_threads), and the
// filtergraph's (-filter_complex_threads), whose one thread carries out the per-side
// pixel-format conversion and feeds libvmaf its frames.
//
// The two add up to the share, so a pass asks for what it was given and no more. The
// filtergraph gets exactly one thread: its work per frame is the conversion, which is small
// beside the scoring, and one is also libavfilter's own spelling of "no worker pool" - the
// conversion runs on the graph's thread rather than being sliced across a pool the size of
// the HOST, which is what the option left unset would give it. libvmaf gets the rest,
// because scoring is the work the share exists to speed up.
//
// A share of 1 cannot be split into two pools of at least one each, and neither pool may be
// asked for 0: libvmaf reads 0 as its own default, and libavfilter reads it as "one thread
// per host CPU". So the floor is one each, which is the same floor cpuquota.Divide states for
// a job: the smallest request that can be made at all.
func PoolThreads(share int) (libvmaf, graph int) {
	graph = 1
	libvmaf = share - graph
	if libvmaf < 1 {
		libvmaf = 1
	}
	return libvmaf, graph
}

// ScoredStream is the video stream every comparison is made against, and the ONE place in the
// program that spells it: BuildFilter composes both filtergraph input labels from it, and it
// is the token recorded beside the score.
//
// It is `v:0`, the FIRST video stream, because that is the stream the rest of this program
// inspects: every ffprobe property read selects `-select_streams v:0` and the decode-integrity
// check decodes `-map 0:v:0`, so any other spelling could measure a stream no guard looked at.
// Like ChromaMetricName it is a WIRE FORMAT and deliberately NOT configurable.
const ScoredStream = "v:0"

// BuildFilter returns the exact -lavfi filtergraph a Score pass runs, writing its JSON log to
// logPath. It is exported so a test can drive the REAL graph through ffmpeg and observe what
// libavfilter did with it: a filtergraph is only correct if ffmpeg agrees.
//
// It is the SINGLE writer of the SCORING graph's two video-stream input specifiers, both from
// ScoredStream, and the startup model preflight (probeModel) names its inputs from that same
// constant, so there is ONE answer to "which stream was measured".
// TestShippedCode_SpellsEveryVideoStreamLabelThroughScoredStream walks the module and reds on
// a second spelling.
//
// The two `format` filters are the whole point of GATE-4's first criterion: they convert BOTH
// inputs to one named format before libvmaf sees either. They must name a format libvmaf itself
// accepts, or ffmpeg silently auto-inserts a scaler BETWEEN them and libvmaf and the named
// format is not the one compared after all. ComparisonFormat only ever produces the planar-YUV
// 8/10/12-bit formats libvmaf takes directly, and TestScore_NamedFormatIsWhatLibvmafCompares
// proves ffmpeg inserts nothing.
func BuildFilter(req Request, logPath string) string {
	sub := req.Subsample
	if sub < 1 {
		sub = 1
	}
	// The thread count is ALWAYS spelled, and never as 0. libvmaf's own default for
	// n_threads is 0, which the library reads as "no thread pool" rather than as a
	// request, so a graph that omits the option and a graph that sets it to 0 are the
	// same one-CPU measurement. Score refuses a request below 1 before it reaches here;
	// PoolThreads' floor is what keeps a direct BuildFilter caller from composing a graph
	// that silently means the opposite of what it says. It is libvmaf's part of the share,
	// not the whole of it: the filtergraph's thread is paid for out of the same share.
	threads, _ := PoolThreads(req.Threads)
	// [0:v:0] = distorted (the encoded output), [1:v:0] = reference - stream-guard-allow.
	// That marker exempts THIS line, which only NAMES the two labels to document the graph's
	// shape: the format string below COMPOSES both from ScoredStream, so the comparison is
	// pinned to the first video stream rather than left to ffmpeg's resolution rules.
	//
	// log_path lives INSIDE the -lavfi filtergraph, where ':' separates option pairs, so a
	// path with a ':' (or other filtergraph metachar) must be escaped or ffmpeg mis-parses the
	// filter and the gate fails every encode. The media paths are safe (separate -i argv).
	// Score passes logPipePath, which needs no escaping; a test driving this builder directly
	// passes a file of its own, and that is what the escaping is still for.
	// The REFERENCE's own chain, ahead of the format conversion: where the encode applied a
	// transformation the source did not carry, the reference is produced from the source by
	// that same transformation at those same parameters, or the score is a measurement of
	// the filter rather than of the encode. It is composed HERE, in the single writer of
	// this graph, so the reference the gate scores against cannot be built one way in one
	// call site and another way in the next.
	ref := ""
	if req.ReferenceFilter != "" {
		ref = req.ReferenceFilter + ","
	}
	// The DISTORTED's own chain, composed on exactly the same terms and in exactly the same
	// place. It carries the up-scale a downscaling job owes, and the asymmetry between the
	// two is the whole safety property of this graph: the resampling that makes two
	// differently-sized files comparable is applied to the OUTPUT, never to the source the
	// output is graded against. See the package comment for what the mirror image would cost.
	dist := ""
	if req.DistortedFilter != "" {
		dist = req.DistortedFilter + ","
	}
	return fmt.Sprintf(
		"[0:%s]%sformat=%s[dist];[1:%s]%sformat=%s[ref];"+
			"[dist][ref]libvmaf=model=%s:feature=%s:log_fmt=json:log_path=%s:n_subsample=%d:n_threads=%d",
		ScoredStream, dist, req.PixelFormat, ScoredStream, ref, req.PixelFormat,
		req.Model, chromaFeature, escapeFilterValue(logPath), sub, threads)
}

// Score measures distorted against reference in the request's NAMED pixel format and
// returns the pooled luma statistics, the chroma statistic and the format the comparison
// was made in, or ErrUnavailable if the ffmpeg build lacks libvmaf.
//
// Fail-closed twice over: an empty Request.PixelFormat is refused outright, and a log
// missing ANY of the four pooled statistics is refused rather than scored on the rest.
func Score(ctx context.Context, ffmpeg string, req Request) (Result, error) {
	if req.PixelFormat == "" {
		return Result{}, fmt.Errorf("vmaf: no comparison pixel format was named - refusing to " +
			"score a pair whose comparison format would be chosen by filter negotiation and " +
			"recorded nowhere")
	}
	if req.Threads < 1 {
		return Result{}, fmt.Errorf("vmaf: no thread count was named (Threads=%d) - refusing to "+
			"score with a value libvmaf reads as its own default rather than as a request; the "+
			"count is derived from the CPU quota this process is allowed (internal/cpuquota) and "+
			"is at least 1", req.Threads)
	}
	// The log travels through a pipe and never touches a filesystem (see logPipePath). The
	// channel is set up BEFORE ffmpeg is started, so a pass that cannot have one never runs
	// a measurement whose result it could not read.
	logR, logW, err := newLogPipe()
	if err != nil {
		return Result{}, fmt.Errorf("vmaf: log pipe: cannot set up the channel libvmaf writes its "+
			"log through (refusing to score a pair whose measurement could not be read): %w", err)
	}
	defer logR.Close()

	// -filter_complex_threads bounds the OTHER thread pool this pass runs. Left unset,
	// libavfilter slices the format conversions ahead of libvmaf across as many threads as
	// the HOST advertises CPUs, which ignores the cgroup bandwidth limit exactly as an unset
	// n_threads did - and does it once per gate scoring at once. It is named from the same
	// share libvmaf's count came out of, so the two pools together ask for the share and
	// not for twice it (PoolThreads).
	_, graphThreads := PoolThreads(req.Threads)
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-filter_complex_threads", strconv.Itoa(graphThreads),
		"-i", req.Distorted, "-i", req.Reference, "-lavfi", BuildFilter(req, logPipePath), "-f", "null", "-")
	// stdout and stderr merged into one buffer, exactly what CombinedOutput collected, so
	// the failure text below is as informative as it always was.
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// ExtraFiles[0] is the child's descriptor 3 (logFD), the one logPipePath names.
	cmd.ExtraFiles = []*os.File{logW}
	// A process that exits (or is killed on cancellation) while something it spawned still
	// holds its output open must not hold this pass open with it.
	cmd.WaitDelay = pipeCloseDelay
	if err := cmd.Start(); err != nil {
		logW.Close()
		return Result{}, fmt.Errorf("vmaf: ffmpeg failed: %w", err)
	}
	// The parent's copy of the write end is closed at once, so the reader sees EOF the moment
	// ffmpeg's copy goes - by exit, by crash or by the kill a cancelled context sends -
	// whether or not libvmaf ever wrote a byte. That EOF is what keeps a pass whose log never
	// comes from waiting for it.
	logW.Close()

	decoded := make(chan logDecode, 1)
	go func() {
		p, err := decodeLog(logR)
		if err != nil {
			// Keep DRAINING to EOF: libvmaf writes the whole log at the end of the pass, and
			// a reader that stopped at the first malformed byte would leave ffmpeg blocked
			// on a full pipe forever. The error is still the answer.
			_, _ = io.Copy(io.Discard, logR)
		}
		decoded <- logDecode{log: p, err: err}
	}()
	runErr := cmd.Wait()

	var d logDecode
	select {
	case d = <-decoded:
	case <-time.After(pipeCloseDelay):
		// ffmpeg is gone and the pipe is still open, so something ffmpeg spawned inherited
		// the write end. At most a pipe buffer remained unread when ffmpeg exited, so this
		// is not a slow parse; closing the read end unblocks the reader with an error.
		logR.Close()
		d = <-decoded
		if d.err == nil {
			d.err = errors.New("the log channel was still held open after ffmpeg exited")
		}
	}

	if runErr != nil {
		msg := out.String()
		if strings.Contains(msg, "No such filter") || strings.Contains(msg, "libvmaf") && strings.Contains(msg, "not found") {
			return Result{}, ErrUnavailable
		}
		return Result{}, fmt.Errorf("vmaf: ffmpeg failed: %w: %s", runErr, truncate(msg, 300))
	}
	if d.err != nil {
		return Result{}, fmt.Errorf("vmaf: parse log: %w", d.err)
	}
	parsed := d.log
	// Fail CLOSED on an incomplete log: any missing pooled statistic means the measurement
	// did not produce the numbers the gate is built on, and proceeding would either
	// misread a zero as a real score or pass the output on the metrics that did report.
	p := parsed.PooledMetrics
	if p.VMAF.HarmonicMean == nil || p.VMAF.Min == nil || p.PsnrCb.Min == nil || p.PsnrCr.Min == nil {
		return Result{}, fmt.Errorf(
			"vmaf: log is missing a pooled statistic (harmonic_mean present=%t, min present=%t, "+
				"psnr_cb present=%t, psnr_cr present=%t) - refusing to accept an encode whose "+
				"quality was not fully measured, and refusing to score it on the metrics that did report",
			p.VMAF.HarmonicMean != nil, p.VMAF.Min != nil, p.PsnrCb.Min != nil, p.PsnrCr.Min != nil)
	}
	return Result{
		HarmonicMean:    *p.VMAF.HarmonicMean,
		Min:             *p.VMAF.Min,
		ChromaMin:       math.Min(*p.PsnrCb.Min, *p.PsnrCr.Min),
		ChromaMetric:    ChromaMetricName,
		PixelFormat:     req.PixelFormat,
		Stream:          ScoredStream,
		ReferenceFilter: req.ReferenceFilter,
		DistortedFilter: req.DistortedFilter,
	}, nil
}

// escapeFilterValue backslash-escapes the characters that are special inside an ffmpeg
// filtergraph option value, so a path embedded in the graph parses literally whatever it
// contains. NewReplacer applies every rule in one pass, so nothing is double-escaped.
func escapeFilterValue(s string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		`:`, `\:`,
		`'`, `\'`,
		`[`, `\[`,
		`]`, `\]`,
		`,`, `\,`,
		`;`, `\;`,
	).Replace(s)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
