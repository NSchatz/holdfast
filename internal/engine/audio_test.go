package engine

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/audio"
	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// THE AUDIO FIXTURES (docs/design/audio.md). Each runs one oneshot pass over one tiny
// synthetic source - a 3 s 320x240 H.264 picture, grainy so any honest encode is smaller,
// and a 5.1 FLAC track in English whose loudness moves, so loudnorm can run linear - and
// grades the row, the output and the source. The re-encode, keep, downmix and loudness
// fixtures run holdfast's own encoder and must pass every gate and swap; each gate fixture
// runs an encoder that is faithful in every respect but ONE, or a measurement seam that fails
// ONE way, and must be refused by that gate with the source byte-identical.

// mkAudioSource writes the fixture at path.
func mkAudioSource(t *testing.T, ffmpeg, path string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=duration=3:size=320x240:rate=10,noise=alls=12:allf=t",
		"-f", "lavfi", "-i", "anoisesrc=c=pink:r=48000:a=0.1:d=3:seed=7,"+
			"volume=eval=frame:volume='0.25+0.2*sin(2*PI*t/2)',aformat=channel_layouts=5.1",
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "ultrafast", "-qp", "4", "-threads", "2",
		"-c:a", "flac", "-metadata:s:a:0", "language=eng", "--", path)
}

// audioRun runs one pass over a fresh fixture under the audio keys mutate sets, with
// holdfast's own encoder where mkEnc is nil, and the measurement seam where seam is set.
func audioRun(t *testing.T, mutate func(*config.Config), mkEnc func(ffmpeg string) Encoder,
	seam func(ctx context.Context, file, spec string, loudness bool) (audio.Measurement, error),
) (src, before string, st *testStore, events []Event) {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src = filepath.Join(root, "movie.mkv")
	mkAudioSource(t, ffmpeg, src)
	before = md5f(t, src)
	cfg := baseCfg(root)
	cfg.PixelFormat = "yuv420p"
	if mutate != nil {
		mutate(&cfg)
	}
	st = newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)
	var enc Encoder = FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}
	if mkEnc != nil {
		enc = mkEnc(ffmpeg)
	}
	eng := New(cfg, prober, enc, st, discardLogger())
	eng.audioMeasure = seam
	var mu sync.Mutex
	eng.Observer = func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	return src, before, st, events
}

func reencodeTo(codec string) func(*config.Config) {
	return func(c *config.Config) { c.AudioReencode, c.AudioCodec = config.AudioReencodeOn, codec }
}

// audioStreamsOf is the output's audio streams as the probe reads them.
func audioStreamsOf(t *testing.T, path string) []probe.Stream {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	streams, ok := probe.New(ffmpeg, ffprobe).Streams(context.Background(), path)
	if !ok {
		t.Fatalf("cannot enumerate %s", path)
	}
	var out []probe.Stream
	for _, s := range streams {
		if s.Type == probe.TypeAudio {
			out = append(out, s)
		}
	}
	return out
}

// dispositionDefault reads the default disposition of every audio stream of path.
func dispositionDefault(t *testing.T, path string) []bool {
	t.Helper()
	_, ffprobe := tools(t)
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "a", "-show_entries",
		"stream_disposition=default", "-of", "json", "--", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Streams []struct {
			Disposition map[string]int `json:"disposition"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	var d []bool
	for _, s := range parsed.Streams {
		d = append(d, s.Disposition["default"] == 1)
	}
	return d
}

func requireAudioDone(t *testing.T, st *testStore, src, before string) store.Job {
	t.Helper()
	row := rowFor(t, st, src)
	if row.Status != store.Done {
		t.Fatalf("row status %q, want done (reason %q)", row.Status, row.Outcome.Reason)
	}
	if md5f(t, src) == before {
		t.Fatal("the source was not replaced")
	}
	if !row.Outcome.AudioTracks.Recorded() {
		t.Fatal("the row records nothing about the audio")
	}
	return row
}

// Replace by default: the FLAC track is re-encoded in its own place and the original is gone.
func TestAudio_ReencodeReplacesTheTrackByDefault(t *testing.T) {
	src, before, st, _ := audioRun(t, reencodeTo("ac3"), nil, nil)
	row := requireAudioDone(t, st, src, before)
	got := audioStreamsOf(t, src)
	if len(got) != 1 || got[0].Codec != "ac3" || got[0].ChannelLayout != "5.1(side)" || got[0].SampleRate != 48000 ||
		got[0].Language != "eng" {
		t.Fatalf("output audio = %+v, want one ac3 5.1(side) 48 kHz eng track", got)
	}
	tracks := row.Outcome.AudioTracks.Tracks()
	if len(tracks) != 1 || tracks[0].Action != "reencoded" || tracks[0].Codec != "ac3" || tracks[0].BitrateKbps != 640 ||
		tracks[0].Layout != "5.1(side)" || tracks[0].SampleRate != 48000 || *tracks[0].OutputIndex != 0 || tracks[0].Loudness != "" {
		t.Errorf("recorded = %+v", tracks)
	}
}

// keep_original_audio: the original is kept in place and its re-encode added, not default.
func TestAudio_KeepOriginalKeepsBothTracks(t *testing.T) {
	src, before, st, _ := audioRun(t, func(c *config.Config) {
		reencodeTo("opus")(c)
		c.KeepOriginalAudio = boolPtr(true)
	}, nil, nil)
	row := requireAudioDone(t, st, src, before)
	got := audioStreamsOf(t, src)
	if len(got) != 2 || got[0].Codec != "flac" || got[1].Codec != "opus" || got[1].ChannelLayout != "5.1" ||
		got[1].Language != "eng" {
		t.Fatalf("output audio = %+v, want the flac original then an opus 5.1 eng re-encode", got)
	}
	if d := dispositionDefault(t, src); len(d) != 2 || d[1] {
		t.Errorf("default dispositions = %v, the added track must not be default", d)
	}
	tracks := row.Outcome.AudioTracks.Tracks()
	if len(tracks) != 2 || tracks[0].Action != "kept" || tracks[1].Action != "added" || *tracks[1].OutputIndex != 1 {
		t.Errorf("recorded = %+v", tracks)
	}
}

// The stereo downmix is added beside the 5.1 track, which is copied.
func TestAudio_StereoDownmixIsAdded(t *testing.T) {
	src, before, st, _ := audioRun(t, func(c *config.Config) {
		c.AudioDownmix, c.AudioCodec = config.AudioDownmixStereo, "aac"
	}, nil, nil)
	row := requireAudioDone(t, st, src, before)
	got := audioStreamsOf(t, src)
	if len(got) != 2 || got[0].Codec != "flac" || got[0].Channels != 6 || got[1].Codec != "aac" ||
		got[1].ChannelLayout != "stereo" || got[1].Language != "eng" {
		t.Fatalf("output audio = %+v, want the flac 5.1 then an aac stereo eng downmix", got)
	}
	tracks := row.Outcome.AudioTracks.Tracks()
	if len(tracks) != 2 || tracks[0].Action != "copied" || tracks[0].Reason != audio.ReasonReencodeOff ||
		tracks[1].Action != "downmix" || tracks[1].Layout != "stereo" {
		t.Errorf("recorded = %+v", tracks)
	}
}

// Two-pass loudness: the second pass ran linear, read off the encoder's own report and
// recorded; the gate measured the output within R 128's tolerance and recorded it.
func TestAudio_TwoPassLoudnessRunsLinearAndIsRecorded(t *testing.T) {
	src, before, st, _ := audioRun(t, func(c *config.Config) {
		reencodeTo("aac")(c)
		c.AudioLoudness = config.AudioLoudnessR128
	}, nil, nil)
	row := requireAudioDone(t, st, src, before)
	tracks := row.Outcome.AudioTracks.Tracks()
	if len(tracks) != 1 {
		t.Fatalf("recorded = %+v", tracks)
	}
	tr := tracks[0]
	if tr.Loudness != audio.ModeLinear || tr.MeasuredLUFS == nil || tr.AchievedLUFS == nil {
		t.Fatalf("recorded = %+v, want linear with both figures", tr)
	}
	if *tr.MeasuredLUFS > -30 || audio.CheckLoudness(*tr.AchievedLUFS) != nil {
		t.Errorf("measured %v LUFS, achieved %v LUFS", *tr.MeasuredLUFS, *tr.AchievedLUFS)
	}
	if got := audioStreamsOf(t, src); got[0].SampleRate != 48000 {
		t.Errorf("the output is at %d Hz", got[0].SampleRate)
	}
}

// audioLossyEncoder is an encoder faithful to the ac3 re-encode plan in every respect but the
// options in change, which follow the faithful ones; pre are input options for the audio's
// own input (the source read a second time).
func audioLossyEncoder(pre []string, change ...string) func(string) Encoder {
	return func(ffmpeg string) Encoder {
		return EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
			args := []string{"-hide_banner", "-nostdin", "-v", "error", "-y", "-i", in}
			args = append(args, pre...)
			args = append(args, "-i", in, "-map", "0:v", "-map", "1:a",
				"-c:v", "libx265", "-preset", "ultrafast", "-crf", "30", "-pix_fmt", "yuv420p",
				"-x265-params", "log-level=error",
				"-c:a", "ac3", "-b:a", "640k", "-ch_layout", "5.1(side)", "-ar", "48000")
			args = append(args, change...)
			args = append(args, "-f", "matroska", "--", out)
			return exec.CommandContext(ctx, ffmpeg, args...).Run()
		})
	}
}

// requireAudioReject asserts the job was refused at gate naming want, of class class, with
// the source byte-identical and no temp left.
func requireAudioReject(t *testing.T, gate, want string, class store.FailureClass, src, before string,
	st *testStore, events []Event) {
	t.Helper()
	if got := md5f(t, src); got != before {
		t.Fatalf("the source changed (md5 %s -> %s): a rejected encode must leave it byte-identical", before, got)
	}
	if n := nTemp(t, filepath.Dir(src)); n != 0 {
		t.Errorf("%d temp file(s) left behind", n)
	}
	row := rowFor(t, st, src)
	if row.Status != store.Failed {
		t.Fatalf("row status %q, want failed (reason %q)", row.Status, row.Outcome.Reason)
	}
	var failed []Event
	for _, ev := range events {
		if ev.Status == store.Failed {
			failed = append(failed, ev)
		}
	}
	if len(failed) != 1 || failed[0].Gate != gate {
		t.Fatalf("failed events %+v, want exactly one at gate %q (reason %q)", failed, gate, row.Outcome.Reason)
	}
	if !strings.Contains(row.Outcome.Reason, want) {
		t.Errorf("the reason does not name %q: %q", want, row.Outcome.Reason)
	}
	if row.Outcome.FailureClass != class {
		t.Errorf("failure class %q, want %q", row.Outcome.FailureClass, class)
	}
	if !row.Outcome.AudioTracks.Recorded() {
		t.Error("a rejected job records nothing about its audio")
	}
}

// The control: the lossy encoder with no change is faithful, so every red below differs from
// a passing encode by its one option.
func TestAudioGate_LossyControlPasses(t *testing.T) {
	src, before, st, _ := audioRun(t, reencodeTo("ac3"), audioLossyEncoder(nil), nil)
	requireAudioDone(t, st, src, before)
}

// A track that decodes a third as long as its source is refused by the duration check.
func TestAudioGate_DurationReds(t *testing.T) {
	src, before, st, events := audioRun(t, reencodeTo("ac3"), audioLossyEncoder([]string{"-t", "1"}), nil)
	requireAudioReject(t, GateAudio, "audio duration check failed", store.FailureDeterministic, src, before, st, events)
}

// A 5.1 track written as stereo is refused by the channel-layout check.
func TestAudioGate_ChannelLayoutReds(t *testing.T) {
	src, before, st, events := audioRun(t, reencodeTo("ac3"), audioLossyEncoder(nil, "-ch_layout", "stereo", "-b:a", "192k"), nil)
	requireAudioReject(t, GateAudio, "audio channel check failed", store.FailureDeterministic, src, before, st, events)
}

// A track at 44.1 kHz where the plan keeps 48 kHz is refused by the sample-rate check.
func TestAudioGate_SampleRateReds(t *testing.T) {
	src, before, st, events := audioRun(t, reencodeTo("ac3"), audioLossyEncoder(nil, "-ar", "44100"), nil)
	requireAudioReject(t, GateAudio, "audio sample-rate check failed", store.FailureDeterministic, src, before, st, events)
}

// An output audio stream that does not decode to the end is refused by the full decode, as
// transient as the video's.
func TestAudioGate_FullDecodeReds(t *testing.T) {
	ffmpeg, _ := tools(t)
	seam := func(ctx context.Context, file, spec string, loudness bool) (audio.Measurement, error) {
		if strings.Contains(filepath.Base(file), TempMarker) {
			return audio.Measurement{}, &audio.DecodeError{Stream: spec, Detail: "a damaged frame (stand-in)",
				Err: exec.ErrNotFound}
		}
		return audio.Measure(ctx, ffmpeg, file, spec, loudness)
	}
	src, before, st, events := audioRun(t, reencodeTo("ac3"), nil, seam)
	requireAudioReject(t, GateDecode, "audio decode-integrity check failed", store.FailureTransient, src, before, st, events)
}

// A normalised track that was never normalised misses the target and is refused by the
// loudness check: the encoder writes the aac track at the source's own level, about -45 LUFS.
func TestAudioGate_LoudnessReds(t *testing.T) {
	mk := func(ffmpeg string) Encoder {
		return EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
			return exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-v", "error", "-y", "-i", in,
				"-map", "0:v", "-map", "0:a", "-c:v", "libx265", "-preset", "ultrafast", "-crf", "30",
				"-pix_fmt", "yuv420p", "-x265-params", "log-level=error",
				"-c:a", "aac", "-b:a", "384k", "-ch_layout", "5.1", "-ar", "48000", "-f", "matroska", "--", out).Run()
		})
	}
	src, before, st, events := audioRun(t, func(c *config.Config) {
		reencodeTo("aac")(c)
		c.AudioLoudness = config.AudioLoudnessR128
	}, mk, nil)
	requireAudioReject(t, GateLoudness, "loudness check failed", store.FailureDeterministic, src, before, st, events)
	tr := rowFor(t, st, src).Outcome.AudioTracks.Tracks()
	if len(tr) != 1 || tr[0].Loudness != audio.ModeNotRecorded || tr[0].AchievedLUFS == nil || *tr[0].AchievedLUFS > -30 {
		t.Errorf("recorded = %+v, want no mode read (no report came back) and the quiet figure the gate measured", tr)
	}
}
