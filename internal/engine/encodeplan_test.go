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
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
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
// is handed and with nothing else. One real source and one real hevc encode of it; the plan
// the derivation produced passes every structural gate, and each case changes ONE declaration
// and gets the rejection of the gate that reads it.
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

	eng := buildEngine(t, ffmpeg, ffprobe, d, nil, nil)
	prober := probe.New(ffmpeg, ffprobe)
	prof := eng.Cfg.TopLevelProfile()
	job := derivePlanFor(t, eng.Cfg, prof, prober, src, out)
	ctx := context.Background()
	if _, gate, _, err := eng.verifyAgainst(ctx, src, out, job); err != nil {
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := *job
			tc.change(&changed)
			_, gate, _, err := eng.verifyAgainst(ctx, src, out, &changed)
			if err == nil || gate != tc.wantGate {
				t.Fatalf("with %s changed the gates returned gate %q (%v), want a %q rejection: that gate does "+
					"not read the plan", tc.name, gate, err, tc.wantGate)
			}
		})
	}

	t.Run("the perceptual gate reproduces the plan's picture operations", func(t *testing.T) {
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
		if _, gate, _, err := eng.verifyAgainst(ctx, src, out, &scored); err != nil {
			t.Fatalf("the plan with the perceptual gate on was rejected by %q: %v", gate, err)
		}
		if got.ReferenceFilter != "" || got.DistortedFilter != "" {
			t.Fatalf("a plan declaring no picture operation was scored through %q and %q",
				got.ReferenceFilter, got.DistortedFilter)
		}

		scored.Picture = PictureOps{Deinterlace: yadif, Downscale: shrink}
		got = vmaf.Request{}
		_, _, _, _ = eng.verifyAgainst(ctx, src, out, &scored)
		if got.ReferenceFilter != yadif.Spec {
			t.Errorf("the reference was produced through %q, want the plan's deinterlace %q", got.ReferenceFilter, yadif.Spec)
		}
		if got.DistortedFilter != shrink.ScoreSpec() {
			t.Errorf("the output was scaled back up through %q, want the plan's %q", got.DistortedFilter, shrink.ScoreSpec())
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
