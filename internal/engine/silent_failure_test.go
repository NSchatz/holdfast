package engine

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/crop"
	"github.com/NSchatz/holdfast/internal/store"
)

// A job never swaps on an ffmpeg that failed, however it failed. The hardest failure to see is
// the quietest: the process does all of its work - writes its whole output, its whole log, its
// whole stream - and then exits non-zero having printed nothing. Every invocation a job makes
// is made to fail that way here, one at a time, chosen by its KIND and its place among the
// invocations of that kind, so the choice does not depend on which of several concurrent
// processes the scheduler started first.

// The kinds of ffmpeg invocation a job makes, as kindFFmpeg tells them apart.
const (
	kindPicture       = "attached-picture-copy"
	kindCropSample    = "crop-detection-sample"
	kindCropBlackness = "crop-blackness-check"
	kindAudioMeasure  = "audio-measurement"
	kindEncode        = "encode"
	kindDecoder       = "playability-decoder"
	kindRemux         = "playability-remux"
	kindGOPRead       = "closed-gop-read"
	kindAnnexB        = "dynamic-hdr-annexb-copy"
	kindDecodeCheck   = "decode-integrity-check"
	kindStreamHash    = "video-stream-hash"
	kindVmafAvailable = "libvmaf-availability"
	kindVmafScore     = "vmaf-score"
	kindUnclassified  = "unclassified"
)

// invocationKinds classify an invocation by a marker its argv carries, the first match winning:
// the order puts each marker ahead of a broader one it would also match (an attached-picture
// copy also carries the encode's mux-queue bounds, a playability decoder the decode check's
// -err_detect).
var invocationKinds = [][2]string{
	{kindPicture, "-c copy -frames:v 1 "},
	{kindCropSample, "cropdetect"},
	{kindCropBlackness, "signalstats"},
	{kindAudioMeasure, "-progress pipe:1 -nostats"},
	{kindEncode, "-max_muxing_queue_size"},
	{kindDecoder, "-f mp4 -i pipe:0"},
	{kindRemux, "frag_keyframe"},
	{kindGOPRead, "-copyinkf -bsf:v hevc_mp4toannexb"},
	{kindAnnexB, "-bsf:v hevc_mp4toannexb"},
	{kindDecodeCheck, "-err_detect +explode -i "},
	{kindStreamHash, "-f streamhash"},
	{kindVmafAvailable, "-hide_banner -filters"},
	{kindVmafScore, "libvmaf"},
}

// kindFFmpeg writes a stand-in for the real ffmpeg into dir. Every invocation is classified
// (invocationKinds) and records itself in dir/calls as the file <kind>.<n>, n its place among
// the invocations of its kind (claimInvocation), with its argv beside it.
// The invocation numbered failOcc of the kind failKind runs the real binary to completion,
// marks dir/calls/fired, and exits 1 having printed nothing; every other invocation is the real
// binary. An invocation that cannot record itself exits 3 without running ffmpeg, which can
// only ever keep a source.
func kindFFmpeg(t *testing.T, dir, real, failKind string, failOcc int) (wrapper, calls string) {
	t.Helper()
	wrapper = filepath.Join(dir, "kind-ffmpeg.sh")
	calls = filepath.Join(dir, "calls")
	if err := os.Mkdir(calls, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\ncase \" $* \" in\n")
	for _, k := range invocationKinds {
		b.WriteString("*\"" + k[1] + "\"*) kind=" + k[0] + ";;\n")
	}
	b.WriteString("*) kind=" + kindUnclassified + ";;\nesac\n")
	b.WriteString(claimInvocation(calls + "/$kind."))
	b.WriteString("printf '%s\\n' \"$*\" > \"" + calls + "/$kind.$n.argv\"\n")
	b.WriteString("if [ \"$kind\" = \"" + failKind + "\" ] && [ \"$n\" = \"" + strconv.Itoa(failOcc) + "\" ]; then\n" +
		"  \"" + real + "\" \"$@\"\n  true > \"" + calls + "/fired\"\n  exit 1\nfi\n")
	b.WriteString("exec \"" + real + "\" \"$@\"\n")
	if err := os.WriteFile(wrapper, []byte(b.String()), 0o755); err != nil {
		t.Fatalf("write the kind-failing ffmpeg: %v", err)
	}
	return wrapper, calls
}

// recordedKinds reads back how many invocations of each kind ran, failing the test on an
// invocation no kind names (a new kind of invocation this harness must learn to fail) and on a
// kind whose numbers have a gap.
func recordedKinds(t *testing.T, calls string) map[string]int {
	t.Helper()
	entries, err := os.ReadDir(calls)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, e := range entries {
		kind, n, ok := strings.Cut(e.Name(), ".")
		if !ok || strings.HasSuffix(e.Name(), ".argv") {
			continue
		}
		if kind == kindUnclassified {
			argv, _ := os.ReadFile(filepath.Join(calls, e.Name()+".argv"))
			t.Fatalf("an ffmpeg invocation of no known kind ran: %s - add its kind to invocationKinds, so a silent "+
				"failure of it is proved to keep the source", argv)
		}
		if _, err := strconv.Atoi(n); err != nil {
			t.Fatalf("unexpected record %s", e.Name())
		}
		counts[kind]++
	}
	for kind, c := range counts {
		for n := 1; n <= c; n++ {
			if _, err := os.Stat(filepath.Join(calls, kind+"."+strconv.Itoa(n))); err != nil {
				t.Fatalf("%d invocation(s) of %s, but number %d is missing: %v", c, kind, n, err)
			}
		}
	}
	return counts
}

// invocationScenario is one job: a source, the configuration it is encoded under, and the kinds
// of ffmpeg invocation (with their counts) a passing run of it must make.
//
// decisions names the invocations that are not gates but inputs to a decision taken before the
// encode, keyed "<kind>/<n>" or "<kind>" for every one of a kind. Such an invocation that fails
// changes the decision to its conservative answer - a crop sample that did not run is a sample
// discarded, a blackness pre-check that did not run refuses the crop and the whole frame is
// encoded - and every gate then holds the output the conservative decision produced, so a swap
// may follow. The function checks the swapped row says so; it is never consulted for a gate's
// invocation, which must keep the source.
type invocationScenario struct {
	name      string
	source    func(t *testing.T, path string)
	mutate    func(*config.Config)
	scratch   bool
	want      map[string]int
	decisions map[string]func(t *testing.T, row store.Job, ran map[string]int)
}

// everyInvocationFailingKeepsTheSource runs sc once with nothing failing, which must swap and
// make exactly the invocations sc.want names, and then once per invocation with that one
// failing silently (kindFFmpeg), which must not swap and must leave nothing the job wrote.
func everyInvocationFailingKeepsTheSource(t *testing.T, sc invocationScenario) {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	job := func(t *testing.T, failKind string, failOcc int) (ts *testStore, src, scratch, calls string, before sourceState) {
		t.Helper()
		d := t.TempDir()
		src = filepath.Join(d, sc.name)
		sc.source(t, src)
		before = captureSource(t, src)
		if sc.scratch {
			scratch = t.TempDir()
		}
		wrapper, calls := kindFFmpeg(t, t.TempDir(), ffmpeg, failKind, failOcc)
		ts = run(t, wrapper, ffprobe, d, nil, func(c *config.Config) {
			c.ContainerExt = "source"
			if sc.mutate != nil {
				sc.mutate(c)
			}
			c.ScratchDir = scratch
		})
		return ts, src, scratch, calls, before
	}

	ts, _, _, calls, _ := job(t, "", 0)
	if row := onlyRow(t, ts); row.Status != store.Done {
		t.Fatalf("the passing run did not swap (%s), so its invocations are not the job's", row.Outcome.Reason)
	}
	got := recordedKinds(t, calls)
	if len(got) != len(sc.want) {
		t.Errorf("the passing run made the invocations %v, want %v", got, sc.want)
	}
	for kind, n := range sc.want {
		if got[kind] != n {
			t.Errorf("the passing run made %d %s invocation(s), want %d (all: %v)", got[kind], kind, n, got)
		}
	}
	kinds := make([]string, 0, len(got))
	for kind := range got {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		for occ := 1; occ <= got[kind]; occ++ {
			t.Run(kind+"/"+strconv.Itoa(occ), func(t *testing.T) {
				ts, src, scratch, calls, before := job(t, kind, occ)
				if _, err := os.Stat(filepath.Join(calls, "fired")); err != nil {
					t.Fatalf("the %s invocation numbered %d never ran in this run (%v): nothing was made to "+
						"fail, so the run proves nothing (invocations: %v)", kind, occ, err, recordedKinds(t, calls))
				}
				decision := sc.decisions[kind+"/"+strconv.Itoa(occ)]
				if decision == nil {
					decision = sc.decisions[kind]
				}
				if row := onlyRow(t, ts); decision != nil && row.Status == store.Done {
					decision(t, row, recordedKinds(t, calls))
					return
				}
				assertNotSwapped(t, ts, src, before, scratch)
			})
		}
	}
}

// TestGates_EverySilentFFmpegFailureKeepsTheSource covers the kinds of invocation the cover-art
// job of AC-9 does not make: the closed-GOP read and the audio measurements, the crop's
// detection samples and blackness check, and a remux's video stream hashes.
func TestGates_EverySilentFFmpegFailureKeepsTheSource(t *testing.T) {
	ffmpeg, _ := tools(t)
	no := false
	for _, sc := range []invocationScenario{
		{
			name:   "audio.mkv",
			source: func(t *testing.T, path string) { mkAudioSource(t, ffmpeg, path) },
			mutate: func(c *config.Config) {
				c.X265OpenGOP = &no
				c.AudioReencode, c.AudioCodec = config.AudioReencodeOn, "opus"
				c.PixelFormat = "yuv420p"
			},
			want: map[string]int{kindAudioMeasure: 2, kindEncode: 1, kindGOPRead: 1, kindDecodeCheck: 1,
				kindRemux: 1, kindDecoder: 2},
		},
		{
			// The blackness check runs twice on a crop, in this order: before the encode, where a
			// crop whose bars it cannot show black is refused and the whole frame encoded, and as
			// the crop gate after it.
			name:   "letterbox.mkv",
			source: func(t *testing.T, path string) { mkCropLetterbox(t, ffmpeg, path, "4", "") },
			mutate: func(c *config.Config) { c.Crop = crop.Auto },
			want: map[string]int{kindCropSample: 10, kindCropBlackness: 2, kindEncode: 1, kindDecodeCheck: 1,
				kindRemux: 1, kindDecoder: 2},
			decisions: map[string]func(t *testing.T, row store.Job, ran map[string]int){
				// A sample that did not run is discarded; the rest still agree, and the crop gate
				// then ran its own blackness check on the cropped output.
				kindCropSample: func(t *testing.T, row store.Job, ran map[string]int) {
					if rec := row.Outcome.Crop.Record(); rec.Applied && ran[kindCropBlackness] != 2 {
						t.Fatalf("a crop was applied with %d blackness check(s) run, want the pre-check and the gate's",
							ran[kindCropBlackness])
					}
				},
				// The pre-check that did not run refuses the crop: the whole frame is encoded.
				kindCropBlackness + "/1": func(t *testing.T, row store.Job, ran map[string]int) {
					if rec := row.Outcome.Crop.Record(); rec.Applied || rec.Reason != crop.ReasonBarsNotBlack {
						t.Fatalf("the pre-check failed and the replacement is %+v, want the whole frame, refused as %s",
							rec, crop.ReasonBarsNotBlack)
					}
				},
			},
		},
		{
			// A remux carrying one of the source's two audio tracks: smaller, so it reaches every
			// gate, and its video held to bit-identity by hashing both files.
			name: "remux.mkv",
			source: func(t *testing.T, path string) {
				mkSourceWithStreams(t, ffmpeg, path, audioStream("eng"), audioStream("fre"))
			},
			mutate: func(c *config.Config) { c.RemuxOnly, c.AudioLanguages = boolPtr(true), []string{"eng"} },
			want:   map[string]int{kindEncode: 1, kindDecodeCheck: 1, kindStreamHash: 2},
		},
	} {
		t.Run(sc.name, func(t *testing.T) { everyInvocationFailingKeepsTheSource(t, sc) })
	}
}
