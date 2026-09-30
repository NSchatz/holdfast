package engine

// The encode plan (docs/design/encode-plan.md#encode-plan): one job's encode, declared once,
// and read by the command line and by every acceptance gate. The golden argv
// (golden_argv_test.go) proves the command line the plan builds is the one this build built
// before it; the cases here prove the rule itself - that there is ONE plan per job, that the
// encoder builds from the plan it is handed and from nothing else, that it refuses what a
// plan declares and it cannot perform, and that each gate's verdict moves with the plan.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/hdr"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

// encodePlanLog records the encode plan each stage was handed.
type encodePlanLog struct {
	mu   sync.Mutex
	seen map[string][]*EncodePlan
}

func newEncodePlanLog() *encodePlanLog { return &encodePlanLog{seen: map[string][]*EncodePlan{}} }

func (l *encodePlanLog) record(stage string, plan *EncodePlan) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[stage] = append(l.seen[stage], plan)
}

func (l *encodePlanLog) only(t *testing.T, stage string) *EncodePlan {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	plans := l.seen[stage]
	if len(plans) != 1 {
		t.Fatalf("stage %q read %d encode plans, want exactly 1", stage, len(plans))
	}
	return plans[0]
}

// derivePlanFor derives an encode plan for src -> out exactly as ProcessFile does: the profile
// that decides it, the intended stream map from the source's own probe, and the snapshot.
func derivePlanFor(t *testing.T, cfg config.Config, prof config.Profile, prober *probe.Prober, src, out string) *EncodePlan {
	t.Helper()
	ctx := context.Background()
	streams, ok := prober.Streams(ctx, src)
	if !ok {
		t.Fatalf("ffprobe could not enumerate the streams of %s", src)
	}
	props := prober.VideoProps(ctx, src)
	job, err := deriveEncodePlan(planInputs{
		settings: cfg.TranscodeIn(prof, src), prof: prof, source: src, output: out,
		streams:  DeriveStreamPlan(streams, prof, props.Codec()),
		snapshot: func() (*probe.VideoProps, error) { return props, nil },
	})
	if err != nil {
		t.Fatalf("deriveEncodePlan: %v", err)
	}
	return job
}

// TestEncodePlan_TheCommandLineAndTheGatesReadOneDerivation: the plan the encoder's command
// line is built from and the plan every gate checks the output against are ONE derivation,
// on a real job that encodes, passes every gate and swaps.
//
// The assertion is on IDENTITY, as the stream map's is: two derivations agree on any fixture,
// so a test comparing what they contain would pass against exactly the build this rule exists
// to refuse. The sub-case proves the comparison says NO to a second derivation of the same
// job that agrees in every field.
func TestEncodePlan_TheCommandLineAndTheGatesReadOneDerivation(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	_, roots := twoRoots(t, "tv")
	src := filepath.Join(roots[0], "ep.mkv")
	mkSourceWithStreams(t, ffmpeg, src, audioStream("eng"), audioStream("jpn"))
	cfg := selectionCfg(t, roots[0], "    audio_languages: [eng]\n")

	prober := probe.New(ffmpeg, ffprobe)
	st := newTestStore(t, roots[0])
	eng := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, st, discardLogger())
	plans, maps := newEncodePlanLog(), newPlanLog()
	eng.encodePlanObserver, eng.planObserver = plans.record, maps.record
	eng.vmafScore = func(context.Context, vmaf.Request) (vmaf.Result, error) { return passing(), nil }
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	if !ledgerHas(t, st, store.Done, "ep.mkv") {
		t.Fatalf("the job did not swap, so the gates did not all read a plan: %+v", rowForFile(t, st, "ep.mkv"))
	}

	encoded, checked := plans.only(t, planStageEncode), plans.only(t, planStageVerify)
	if !SameEncodePlan(encoded, checked) {
		t.Fatalf("the command line was built from encode plan %d and the gates checked plan %d: two "+
			"derivations are two answers about what was encoded, and the source is deleted on the "+
			"strength of one of them", encoded.ID(), checked.ID())
	}
	// The intended stream map the gates read is the one inside that plan, not a map of its own.
	if !SameDerivation(checked.Streams, maps.only(t, planStageVerify)) ||
		!SameDerivation(encoded.Streams, maps.only(t, planStageEncode)) {
		t.Fatal("a stage read an intended stream map other than the one its encode plan carries")
	}
	if len(checked.Streams.Dropped()) != 1 {
		t.Fatalf("the plan drops %d stream(s), want the one jpn audio track - the selection did not "+
			"reach the plan", len(checked.Streams.Dropped()))
	}

	t.Run("a plan no derivation made is the same derivation as nothing", func(t *testing.T) {
		if SameEncodePlan(&EncodePlan{}, &EncodePlan{}) {
			t.Fatal("two plans no derivation produced were reported as one derivation")
		}
	})

	t.Run("a second derivation of the same job is caught", func(t *testing.T) {
		prof := cfg.RootProfiles()[0].Profile
		out := filepath.Join(t.TempDir(), "ep.mkv")
		first := derivePlanFor(t, cfg, prof, prober, src, out)
		second := derivePlanFor(t, cfg, prof, prober, src, out)
		if first.Video != second.Video || first.Settings != second.Settings ||
			len(first.Streams.Intended()) != len(second.Streams.Intended()) {
			t.Fatal("two derivations of one job disagreed, so this sub-case would pass for the wrong reason")
		}
		if SameEncodePlan(first, second) {
			t.Fatal("a SECOND derivation that agrees in every field was reported as the same plan: the " +
				"identity check above would pass against two derivations")
		}
	})
}

// TestEncodePlan_TheEncoderBuildsFromTheHandedPlanAlone: an encoder handed a plan builds its
// command line from that plan and from nothing else about the job - not from the configuration
// it was constructed with, and not from a profile or a stream map it was handed before.
//
// The encoder's own configuration here says libx265 at crf 22 with no deinterlace; the plan
// was derived from one saying SVT-AV1 at crf 30, preset fast, yuv420p and yadif. Every one of
// those has to come out of the plan.
func TestEncodePlan_TheEncoderBuildsFromTheHandedPlanAlone(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkInterlacedLong(t, ffmpeg, src, "8M")
	prober := probe.New(ffmpeg, ffprobe)

	planned := config.Config{Encoder: "svtav1", CRF: 30, Preset: "fast", PixelFormat: "yuv420p",
		ContainerExt: "source", Deinterlace: "yadif"}
	out := filepath.Join(d, "out.mkv")
	job := derivePlanFor(t, planned, planned.TopLevelProfile(), prober, src, out)

	stub := goldenStub(t)
	rec := &goldenArgvRecorder{}
	own := config.Config{Encoder: "cpu", CRF: 22, Preset: "slow", PixelFormat: "auto", ContainerExt: "source"}
	enc := FFmpegEncoder{FFmpeg: stub, Cfg: own, Probe: prober, argvObserver: rec.record}
	var built Encoder = enc.ForProfile(own.TopLevelProfile())
	built = built.(EncodePlanEncoder).ForEncodePlan(job)
	if err := built.Encode(context.Background(), src, out, nil); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	calls := rec.all()
	if len(calls) != 1 {
		t.Fatalf("the encoder made %d invocation(s), want 1", len(calls))
	}
	argv := strings.Join(calls[0], " ")
	for _, want := range []string{"-c:v libsvtav1", "-crf 30", "-preset 10", "-pix_fmt yuv420p",
		"-vf yadif=mode=send_frame:parity=auto:deint=all"} {
		if !strings.Contains(argv, want) {
			t.Errorf("the command line does not carry %q from the plan it was handed:\n%s", want, argv)
		}
	}
	for _, own := range []string{"libx265", "-crf 22", "-preset slow"} {
		if strings.Contains(argv, own) {
			t.Errorf("the command line carries %q from the encoder's own configuration rather than the plan:\n%s", own, argv)
		}
	}

	t.Run("a plan for another job is refused", func(t *testing.T) {
		rec := &goldenArgvRecorder{}
		enc := FFmpegEncoder{FFmpeg: stub, Cfg: own, Probe: prober, argvObserver: rec.record}.ForEncodePlan(job)
		err := enc.Encode(context.Background(), src, filepath.Join(d, "elsewhere.mkv"), nil)
		if err == nil || !strings.Contains(err.Error(), "another job's plan") {
			t.Fatalf("an encode writing a path the plan was not derived for returned %v, want a refusal", err)
		}
		if n := len(rec.all()); n != 0 {
			t.Errorf("the refused encode still ran %d invocation(s)", n)
		}
	})
}

// TestEncodePlan_RefusesWhatItCannotPerform: a plan declaring an operation this build has no
// way to perform - a slot a later capability fills - is refused by name before anything runs,
// never built without it. Each case starts from a plan the derivation produced and changes
// the one declaration.
func TestEncodePlan_RefusesWhatItCannotPerform(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "3M")
	prober := probe.New(ffmpeg, ffprobe)
	out := filepath.Join(d, "out.mkv")
	cfg := config.Config{Encoder: "cpu", CRF: 22, Preset: "slow", PixelFormat: "auto", ContainerExt: "source"}
	stub := goldenStub(t)

	cases := []struct {
		name   string
		remux  bool
		change func(*EncodePlan)
		names  string
	}{
		{"an audio action other than copy", false, func(p *EncodePlan) { p.Audio = "reencode" }, `audio action "reencode"`},
		{"a subtitle action other than copy", false, func(p *EncodePlan) { p.Subtitles = "sidecar" }, `subtitle action "sidecar"`},
		{"a crop", false, func(p *EncodePlan) { p.Picture.Crop = "crop=320:200:0:20" }, `crop "crop=320:200:0:20"`},
		{"HDR10+ carried", false, func(p *EncodePlan) { p.Metadata.HDR10Plus = true }, "HDR10+"},
		{"a Dolby Vision RPU carried", false, func(p *EncodePlan) { p.Metadata.DolbyVision = true }, "Dolby Vision"},
		{"a hardware decode path", false, func(p *EncodePlan) { p.Video.Decode = "cuda" }, `decode path "cuda"`},
		{"a device for an encoder this build opens none for", false,
			func(p *EncodePlan) { p.Video.Device = "/dev/dri/renderD129" }, `device "/dev/dri/renderD129"`},
		{"a remux declaring an audio action other than copy", true, func(p *EncodePlan) { p.Audio = "downmix" }, `audio action "downmix"`},
		{"a remux declaring a picture operation", true, func(p *EncodePlan) {
			p.Picture.Downscale = config.Profile{MaxHeight: 120}.DownscaleFor(320, 240)
		}, "a picture operation on a stream copy"},
		{"a plan no derivation made", false, func(p *EncodePlan) { p.id = 0 }, "no derivation this build made"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg
			if tc.remux {
				c.RemuxOnly = boolPtr(true)
			}
			job := derivePlanFor(t, c, c.TopLevelProfile(), prober, src, out)
			// The unchanged plan builds: the refusal below is about the one declaration.
			if _, _, err := job.args(encoder.X265Parallelism{}); err != nil {
				t.Fatalf("the derived plan does not build before the change: %v", err)
			}
			tc.change(job)
			rec := &goldenArgvRecorder{}
			enc := FFmpegEncoder{FFmpeg: stub, Cfg: c, Probe: prober, argvObserver: rec.record}.ForEncodePlan(job)
			err := enc.Encode(context.Background(), src, out, nil)
			var unbuildable *UnbuildablePlanError
			if !errors.As(err, &unbuildable) {
				t.Fatalf("the encode returned %v, want an *UnbuildablePlanError", err)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the refusal %q does not name %q", err, tc.names)
			}
			if n := len(rec.all()); n != 0 {
				t.Errorf("the refused plan still ran %d invocation(s): an operation silently left out is an "+
					"output that is not what its plan says", n)
			}
		})
	}
}

// TestEncodePlan_EveryGateReadsThePlan: each acceptance gate's verdict moves with the plan it
// is handed - the files it measures included, which the gates take from the plan too (Source,
// Output). One real source and one real hevc encode of it; the plan the derivation produced
// passes every gate, and each case changes ONE thing the plan declares and gets the rejection
// of the gate that reads it: the exists, codec, length, size, stream-parity and decode gates,
// the perceptual gate's three floors (with the measurement stubbed, since the floors are what
// is under test), and - on a plan for a stream copy - the video-identity check that stands in
// for the perceptual gate. The perceptual gate is also shown to build its comparison through
// the plan's picture operations. The only thresholds not on the plan are the length gate's
// run-wide tolerance and its packet-count bound, a constant.
func TestEncodePlan_EveryGateReadsThePlan(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	out := filepath.Join(d, "out.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", src, "-map", "0",
		"-c:v", "libx265", "-preset", "ultrafast", "-crf", "28", "-x265-params", "log-level=error",
		"-pix_fmt", "yuv420p10le", "--", out)
	withAudio := filepath.Join(d, "with-audio.mkv")
	mkSourceWithStreams(t, ffmpeg, withAudio, audioStream("eng"))

	// The files the three gates with no job-specific expectation measure: an output that is
	// not there to measure, a source twice the output's length, and an output whose video does
	// not decode. The damaged one is an hevc elementary stream of the source - encoded without
	// B-frames, so a raw stream muxes back with timestamps - with bytes in its middle flipped,
	// muxed at its own frame rate; the same mux of the undamaged stream passes every gate, so
	// the damage is the one thing the decode gate can be rejecting.
	empty := filepath.Join(d, "empty.mkv")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatalf("write the empty output: %v", err)
	}
	long := filepath.Join(d, "long.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=4:size=320x240:rate=10", "-c:v", "libx264", "-preset", "ultrafast",
		"-b:v", "8M", "-pix_fmt", "yuv420p", "--", long)
	elementary := filepath.Join(d, "out.hevc")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", src, "-map", "0:v",
		"-c:v", "libx265", "-preset", "ultrafast", "-crf", "28", "-x265-params", "bframes=0:log-level=error",
		"-pix_fmt", "yuv420p10le", "-f", "hevc", "--", elementary)
	raw, err := os.ReadFile(elementary)
	if err != nil {
		t.Fatalf("read the elementary stream: %v", err)
	}
	if len(raw) < 8192 {
		t.Fatalf("the elementary stream is %d bytes - too small to damage its middle", len(raw))
	}
	for i := len(raw) / 2; i < len(raw)/2+len(raw)/8; i++ {
		raw[i] ^= 0xff
	}
	damagedStream := filepath.Join(d, "damaged.hevc")
	if err := os.WriteFile(damagedStream, raw, 0o644); err != nil {
		t.Fatalf("write the damaged elementary stream: %v", err)
	}
	remux := func(stream, to string) string {
		ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-framerate", "10", "-i", stream,
			"-c", "copy", "--", to)
		return to
	}
	cleanMux, damaged := remux(elementary, filepath.Join(d, "clean.mkv")), remux(damagedStream, filepath.Join(d, "damaged.mkv"))

	eng := buildEngine(t, ffmpeg, ffprobe, d, nil, nil)
	prober := probe.New(ffmpeg, ffprobe)
	prof := eng.Cfg.TopLevelProfile()
	job := derivePlanFor(t, eng.Cfg, prof, prober, src, out)
	ctx := context.Background()
	if _, gate, _, err := eng.verifyAgainst(ctx, job); err != nil {
		t.Fatalf("the derived plan's own output was rejected by %q: %v - the cases below would prove nothing", gate, err)
	}

	cases := []struct {
		name     string
		change   func(*EncodePlan)
		wantGate string
	}{
		{"the declared codec", func(p *EncodePlan) { p.Video.Codec = "av1" }, GateCodec},
		{"a declared copy of the source's video", func(p *EncodePlan) {
			p.Video = VideoPlan{Copy: true, Codec: p.Streams.SourceVideoCodec()}
		}, GateCodec},
		{"the profile's savings floor", func(p *EncodePlan) { p.Profile.MinSavingsPercent = 100 }, GateSize},
		{"the intended streams", func(p *EncodePlan) {
			p.Streams = derivePlanFor(t, eng.Cfg, prof, prober, withAudio, out).Streams
		}, GateStreamParity},
		{"the declared bit depth", func(p *EncodePlan) { p.Metadata.Fidelity.Depth = 12 }, GateFidelity},
		{"the declared chroma subsampling", func(p *EncodePlan) { p.Metadata.Fidelity.Chroma = "444" }, GateFidelity},
		{"a declared colour tag", func(p *EncodePlan) { p.Metadata.Fidelity.Transfer = "smpte2084" }, GateFidelity},
		{"a declared HDR10 block", func(p *EncodePlan) {
			p.Metadata.Fidelity.ContentLight = hdr.Block{Present: true, Value: "1000,400"}
		}, GateFidelity},
		{"the output the plan names: nothing there", func(p *EncodePlan) { p.Output = empty }, GateEncode},
		{"the source the plan names: twice the output's length", func(p *EncodePlan) { p.Source = long }, GateLength},
		{"the output the plan names: a stream that does not decode", func(p *EncodePlan) { p.Output = damaged }, GateDecode},
	}
	clean := *job
	clean.Output = cleanMux
	if _, gate, _, err := eng.verifyAgainst(ctx, &clean); err != nil {
		t.Fatalf("the undamaged remux of the same stream was rejected by %q: %v - the decode case would not be "+
			"about the damage", gate, err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := *job
			tc.change(&changed)
			_, gate, _, err := eng.verifyAgainst(ctx, &changed)
			if err == nil || gate != tc.wantGate {
				t.Fatalf("with %s changed the gates returned gate %q (%v), want a %q rejection: that gate does "+
					"not read the plan", tc.name, gate, err, tc.wantGate)
			}
		})
	}

	t.Run("the perceptual gate takes its floors and its comparison from the plan", func(t *testing.T) {
		yadif, ok := config.Profile{Deinterlace: "yadif"}.DeinterlaceFilter()
		if !ok || !yadif.Enabled() {
			t.Fatal("yadif did not resolve to a filter; the case would prove nothing")
		}
		shrink := config.Profile{MaxHeight: 120}.DownscaleFor(320, 240)
		if !shrink.Enabled() {
			t.Fatal("a 120-line ceiling does not scale a 240-line picture; the case would prove nothing")
		}
		var got vmaf.Request
		eng.vmafScore = func(_ context.Context, r vmaf.Request) (vmaf.Result, error) { got = r; return passing(), nil }
		defer func() { eng.vmafScore = nil }()

		scored := *job
		scored.Profile.VmafEnable, scored.Profile.MinVmaf = boolPtr(true), 1
		if _, gate, _, err := eng.verifyAgainst(ctx, &scored); err != nil {
			t.Fatalf("the plan with the perceptual gate on was rejected by %q: %v", gate, err)
		}
		if got.ReferenceFilter != "" || got.DistortedFilter != "" {
			t.Fatalf("a plan declaring no picture operation was scored through %q and %q",
				got.ReferenceFilter, got.DistortedFilter)
		}

		// The three floors, each set just above the stubbed measurement on the plan's own
		// profile and nowhere else.
		m := passing()
		for _, fl := range []struct {
			name     string
			raise    func(*config.Profile)
			wantGate string
		}{
			{"the mean floor", func(p *config.Profile) { p.MinVmaf = m.HarmonicMean + 1 }, GateVmafMean},
			{"the worst-frame floor", func(p *config.Profile) { p.VmafMinPool = m.Min + 1 }, GateVmafMin},
			{"the chroma floor", func(p *config.Profile) { p.VmafMinChroma = m.ChromaMin + 1 }, GateVmafChroma},
		} {
			floored := scored
			fl.raise(&floored.Profile)
			if _, gate, _, err := eng.verifyAgainst(ctx, &floored); err == nil || gate != fl.wantGate {
				t.Errorf("with %s raised on the plan's profile the gates returned %q (%v), want a %q rejection",
					fl.name, gate, err, fl.wantGate)
			}
		}

		scored.Picture = PictureOps{Deinterlace: yadif, Downscale: shrink}
		got = vmaf.Request{}
		_, _, _, _ = eng.verifyAgainst(ctx, &scored)
		if got.ReferenceFilter != yadif.Spec {
			t.Errorf("the reference was produced through %q, want the plan's deinterlace %q", got.ReferenceFilter, yadif.Spec)
		}
		if got.DistortedFilter != shrink.ScoreSpec() {
			t.Errorf("the output was scaled back up through %q, want the plan's %q", got.DistortedFilter, shrink.ScoreSpec())
		}
	})

	t.Run("a plan for a stream copy is held to video identity with the source it names", func(t *testing.T) {
		// Two different encodes of the same picture, each muxed with index padding a remux does
		// not reproduce, so a stream copy of either comes out smaller than both and the size
		// gate passes; the copy is of the first.
		padded := func(name, bitrate string) string {
			path := filepath.Join(d, name)
			ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
				"-i", "testsrc2=duration=2:size=320x240:rate=10", "-c:v", "libx264", "-preset", "ultrafast",
				"-b:v", bitrate, "-pix_fmt", "yuv420p", "-reserve_index_space", "65536", "--", path)
			return path
		}
		first, other := padded("first.mkv", "3M"), padded("other.mkv", "5M")
		copied := filepath.Join(d, "copied.mkv")
		ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", first, "-map", "0", "-c", "copy", "--", copied)

		remux := eng.Cfg
		remux.RemuxOnly = boolPtr(true)
		copyJob := derivePlanFor(t, remux, remux.TopLevelProfile(), prober, first, copied)
		if !copyJob.Video.Copy {
			t.Fatalf("a remux-only profile derived a plan that re-encodes (%+v)", copyJob.Video)
		}
		proof, gate, _, err := eng.verifyAgainst(ctx, copyJob)
		if err != nil || proof.Skipped != VmafSkippedRemuxOnly {
			t.Fatalf("a stream copy of the plan's own source was rejected by %q (%v) or not held to identity "+
				"(skipped %q) - the case below would prove nothing", gate, err, proof.Skipped)
		}
		changed := *copyJob
		changed.Source = other
		if _, gate, _, err := eng.verifyAgainst(ctx, &changed); err == nil || gate != GateOther ||
			!strings.Contains(err.Error(), "NOT identical") {
			t.Fatalf("a copy checked against a different source the plan names returned %q (%v), want the "+
				"video-identity rejection", gate, err)
		}
	})
}

// TestEncodePlan_ARemuxRecordsNoPictureOperationItDidNotApply: a remux-only job under a
// `max_height` ceiling below its source re-encodes nothing and scales nothing, and its row
// says so - Downscaled false, no scaler named - because the row records the picture
// operations its plan declares, and a stream copy declares none.
//
// Before the plan, this row recorded Downscaled true and a scaler for exactly this job: the
// engine resolved the ceiling for the row on its own, against the profile and the source,
// without asking whether anything would re-encode the video, while the deinterlace beside it
// already excluded a remux (deinterlaceWanted). That was a provenance claim about a picture
// nobody transformed, on the record that outlives the source; the plan corrected it, and this
// case holds the correction. The command line never carried a scale for a remux, before or
// after (the golden argv's remux cases).
func TestEncodePlan_ARemuxRecordsNoPictureOperationItDidNotApply(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	_, roots := twoRoots(t, "tv")
	src := filepath.Join(roots[0], "tall.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=640x480:rate=10", "-c:v", "libx264", "-preset", "ultrafast",
		"-b:v", "8M", "-pix_fmt", "yuv420p", "--", src)
	cfg := selectionCfg(t, roots[0], "    remux_only: true\n    max_height: 240\n    downscale_acknowledged: true\n")

	prober := probe.New(ffmpeg, ffprobe)
	st := newTestStore(t, roots[0])
	argv := newArgvLog()
	eng := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober, argvObserver: argv.record},
		st, discardLogger())
	plans := newEncodePlanLog()
	eng.encodePlanObserver = plans.record
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	job := plans.only(t, planStageVerify)
	if !job.Video.Copy {
		t.Fatalf("the job was not a remux (%+v), so this case proves nothing about one", job.Video)
	}
	if args := strings.Join(argv.forSource(t, src), " "); strings.Contains(args, "scale=") {
		t.Fatalf("a remux carried a scale on its command line: %s", args)
	}
	row := rowForFile(t, st, "tall.mkv")
	if row.Outcome.Downscaled == nil || *row.Outcome.Downscaled {
		t.Errorf("the row records Downscaled=%v for a stream copy that scaled nothing: want an explicit false",
			pointerText(row.Outcome.Downscaled))
	}
	if row.Outcome.DownscaleScaler != "" {
		t.Errorf("the row names the scaler %q for a job that scaled nothing", row.Outcome.DownscaleScaler)
	}
	if row.Outcome.Deinterlaced != nil && *row.Outcome.Deinterlaced {
		t.Error("the row records a deinterlace for a stream copy")
	}
}

// pointerText renders a recorded flag for a failure message: "nil" where nothing was recorded.
func pointerText(b *bool) string {
	if b == nil {
		return "nil"
	}
	if *b {
		return "true"
	}
	return "false"
}
