package audio

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
)

// THE REAL-FFMPEG FIXTURES: every claim the matrix, the loudness filters and the gates rest on,
// run through the pinned ffmpeg and read back through the probe the engine reads an output
// with. Sources are lavfi-generated and a few seconds long.

func tools(t *testing.T) (string, string) {
	t.Helper()
	ffmpeg, ffprobe := os.Getenv("HOLDFAST_FFMPEG"), os.Getenv("HOLDFAST_FFPROBE")
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	for _, b := range []string{ffmpeg, ffprobe} {
		if _, err := exec.LookPath(b); err != nil {
			t.Fatalf("::error:: %s is required for the audio fixtures: %v", b, err)
		}
	}
	return ffmpeg, ffprobe
}

func ffmpegRun(t *testing.T, ffmpeg string, args ...string) {
	t.Helper()
	out, err := exec.Command(ffmpeg, append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

// mkSource writes a FLAC track of layout at rate, seconds long, into a Matroska file. The
// noise is modulated so its loudness range is above zero, which loudnorm's linear mode needs.
func mkSource(t *testing.T, ffmpeg, path, layout string, rate int, seconds float64) {
	t.Helper()
	ffmpegRun(t, ffmpeg, "-f", "lavfi", "-i",
		fmt.Sprintf("anoisesrc=c=pink:r=%d:a=0.1:d=%g:seed=7,volume=eval=frame:volume='0.25+0.2*sin(2*PI*t/2)',"+
			"aformat=channel_layouts=%s", rate, seconds, layout),
		"-c:a", "flac", "--", path)
}

func sourceOf(t *testing.T, p *probe.Prober, path string) []Source {
	t.Helper()
	streams, ok := p.Streams(context.Background(), path)
	if !ok {
		t.Fatalf("cannot enumerate %s", path)
	}
	var out []Source
	for _, s := range streams {
		if s.Type == probe.TypeAudio {
			out = append(out, Source{Index: s.Index, Codec: s.Codec, Profile: s.Profile, Channels: s.Channels,
				Layout: s.ChannelLayout, SampleRate: s.SampleRate, Language: s.Language, Commentary: s.Commentary})
		}
	}
	return out
}

// encode runs the plan's command line over in the way the engine does: every stream mapped
// and copied, the plan's arguments after it, and one report channel per normalised track.
func encode(t *testing.T, ffmpeg, in, out string, p Plan) [][]byte {
	t.Helper()
	rep, err := OpenReports(len(p.Normalised()))
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close()
	args := append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-i", in, "-map", "0", "-c", "copy"},
		p.Args()...)
	args = append(args, "--", out)
	cmd := exec.Command(ffmpeg, args...)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	cmd.ExtraFiles = rep.ExtraFiles(nil)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	rep.Started()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("encode %v: %v\n%s", args, err, errb.String())
	}
	return rep.Collect()
}

func observed(t *testing.T, p *probe.Prober, path string) []Observed {
	t.Helper()
	streams, ok := p.Streams(context.Background(), path)
	if !ok {
		t.Fatalf("cannot enumerate %s", path)
	}
	var out []Observed
	for _, s := range streams {
		if s.Type == probe.TypeAudio {
			out = append(out, Observed{Codec: s.Codec, Channels: s.Channels, Layout: s.ChannelLayout, SampleRate: s.SampleRate})
		}
	}
	return out
}

// Every cell of the matrix, in both containers: what the plan declares is what the pinned
// ffprobe reads back, the decoded length is within the duration gate's tolerance, and the full
// decode is clean.
func TestAudioFFmpeg_EveryMatrixCellReadsBackAsDeclared(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	p := probe.New(ffmpeg, ffprobe)
	dir := t.TempDir()
	ctx := context.Background()
	for _, layout := range []string{"mono", "stereo", "5.1", "5.1(side)", "7.1"} {
		in := filepath.Join(dir, "in-"+layout+".mkv")
		mkSource(t, ffmpeg, in, layout, 44100, 1.5)
		carried := sourceOf(t, p, in)
		srcLen, err := Measure(ctx, ffmpeg, in, "0:a:0", false)
		if err != nil {
			t.Fatal(err)
		}
		for _, codec := range Codecs {
			for _, ext := range []string{"mkv", "mp4"} {
				name := layout + "-" + codec + "." + ext
				plan, err := Derive(Settings{Reencode: true, Codec: codec}, carried, ext, nil)
				if err != nil {
					t.Fatal(err)
				}
				op := plan.Ops[0]
				if layout == "7.1" && (codec == CodecAC3 || codec == CodecEAC3) {
					if op.Action != ActionCopied || op.Reason != ReasonLayout {
						t.Errorf("%s: %+v, want copied for its layout", name, op)
					}
					continue
				}
				if op.Action != ActionReencoded {
					t.Fatalf("%s: %+v", name, op)
				}
				out := filepath.Join(dir, name)
				encode(t, ffmpeg, in, out, plan)
				got := observed(t, p, out)
				if len(got) != 1 {
					t.Fatalf("%s: %d audio streams", name, len(got))
				}
				if err := CheckLayout(op, got[0]); err != nil {
					t.Errorf("%s: %v", name, err)
				}
				if err := CheckSampleRate(op, got[0]); err != nil {
					t.Errorf("%s: %v", name, err)
				}
				m, err := Measure(ctx, ffmpeg, out, "0:a:0", false)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if err := CheckDuration(op, srcLen.DurationSec, m.DurationSec); err != nil {
					t.Errorf("%s: %v", name, err)
				}
			}
		}
	}
}

// Why the matrix names every layout and leaves 7.1 out of AC-3 and E-AC-3: with no -ch_layout
// the E-AC-3 encoder folds a 7.1 source to six channels, exits 0 and says nothing; told the
// layout, it refuses.
func TestAudioFFmpeg_SevenOneIntoEAC3FoldsSilentlyUnlessTheLayoutIsNamed(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	p := probe.New(ffmpeg, ffprobe)
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.mkv"), filepath.Join(dir, "out.mkv")
	mkSource(t, ffmpeg, in, "7.1", 48000, 1)
	ffmpegRun(t, ffmpeg, "-i", in, "-c:a", "eac3", "-b:a", "1024k", "--", out)
	if got := observed(t, p, out); len(got) != 1 || got[0].Channels != 6 {
		t.Fatalf("eac3 without a layout wrote %+v, want the silent fold to 6 channels", got)
	}
	cmd := exec.Command(ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-i", in,
		"-c:a", "eac3", "-ch_layout:a:0", "7.1", "--", filepath.Join(dir, "named.mkv"))
	if err := cmd.Run(); err == nil {
		t.Error("eac3 told 7.1 succeeded")
	}
}

// Two-pass loudness on a re-encode and a downmix: the second pass runs linear on a programme
// whose range is under the target, the report comes back on each track's own channel, the
// output is at the declared rate (no 192 kHz), and its measured loudness meets the gate.
func TestAudioFFmpeg_TwoPassLoudnessRunsLinearAndMeetsR128(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	p := probe.New(ffmpeg, ffprobe)
	dir := t.TempDir()
	ctx := context.Background()
	in, out := filepath.Join(dir, "in.mkv"), filepath.Join(dir, "out.mkv")
	mkSource(t, ffmpeg, in, "5.1", 44100, 4)
	carried := sourceOf(t, p, in)
	measure := func(s Source, pre string) (Stats, error) { return MeasureLoudness(ctx, ffmpeg, in, s.Index, pre) }
	plan, err := Derive(Settings{Reencode: true, Downmix: true, Loudness: true, Codec: CodecAAC}, carried, "mkv", measure)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(plan.Normalised()); n != 2 {
		t.Fatalf("%d normalised tracks, want 2: %+v", n, plan.Ops)
	}
	reports := encode(t, ffmpeg, in, out, plan)
	got := observed(t, p, out)
	srcLen, err := Measure(ctx, ffmpeg, in, "0:a:0", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range plan.Normalised() {
		if mode := Mode(reports[op.LoudnessIndex]); mode != ModeLinear {
			t.Errorf("a:%d ran %q, want linear (report %s)", op.Output, mode, reports[op.LoudnessIndex])
		}
		if err := CheckSampleRate(op, got[op.Output]); err != nil || op.SampleRate != 44100 {
			t.Errorf("a:%d: %v (declared %d)", op.Output, err, op.SampleRate)
		}
		if err := CheckLayout(op, got[op.Output]); err != nil {
			t.Error(err)
		}
		m, err := Measure(ctx, ffmpeg, out, fmt.Sprintf("0:a:%d", op.Output), true)
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckLoudness(m.Loudness.InputI); err != nil {
			t.Errorf("a:%d: %v", op.Output, err)
		}
		if err := CheckDuration(op, srcLen.DurationSec, m.DurationSec); err != nil {
			t.Error(err)
		}
	}
}

// A programme loudnorm cannot normalise linearly (a steady tone measures a range of exactly
// 0, its sentinel) runs dynamic: the report says so, and the aresample after it still holds
// the declared rate where dynamic mode would have written 192 kHz.
func TestAudioFFmpeg_ADynamicFallbackIsReportedAndKeepsTheDeclaredRate(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	p := probe.New(ffmpeg, ffprobe)
	dir := t.TempDir()
	ctx := context.Background()
	in, out := filepath.Join(dir, "in.mkv"), filepath.Join(dir, "out.mkv")
	ffmpegRun(t, ffmpeg, "-f", "lavfi", "-i", "sine=f=440:r=48000:d=3,volume=0.3", "-c:a", "flac", "--", in)
	carried := sourceOf(t, p, in)
	plan, err := Derive(Settings{Reencode: true, Loudness: true, Codec: CodecOpus}, carried, "mkv",
		func(s Source, pre string) (Stats, error) { return MeasureLoudness(ctx, ffmpeg, in, s.Index, pre) })
	if err != nil {
		t.Fatal(err)
	}
	reports := encode(t, ffmpeg, in, out, plan)
	if mode := Mode(reports[0]); mode != ModeDynamic {
		t.Errorf("mode = %q, want dynamic", mode)
	}
	if got := observed(t, p, out); got[0].SampleRate != 48000 {
		t.Errorf("the output is at %d Hz, want 48000", got[0].SampleRate)
	}
	// Without the aresample, the same dynamic pass writes 192 kHz: the gate's reason to exist.
	bare := filepath.Join(dir, "bare.mkv")
	ffmpegRun(t, ffmpeg, "-i", in, "-af", "loudnorm=I=-23", "-c:a", "flac", "--", bare)
	if got := observed(t, p, bare); got[0].SampleRate != 192000 {
		t.Errorf("a dynamic loudnorm without aresample wrote %d Hz, want 192000", got[0].SampleRate)
	}
}

// The full decode reds on a damaged stream, and a truncated track reds the duration check.
func TestAudioFFmpeg_MeasureRedsOnADamagedStreamAndATruncatedOne(t *testing.T) {
	ffmpeg, _ := tools(t)
	dir := t.TempDir()
	ctx := context.Background()
	good := filepath.Join(dir, "good.mkv")
	mkSource(t, ffmpeg, good, "stereo", 48000, 3)
	b, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.mkv")
	for i := len(b) / 2; i < len(b)/2+4000 && i < len(b); i++ {
		b[i] ^= 0xA5
	}
	if err := os.WriteFile(bad, b, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Measure(ctx, ffmpeg, bad, "0:a:0", false)
	if !IsDecodeError(err) || !strings.Contains(err.Error(), "0:a:0") {
		t.Errorf("a damaged stream measured: %v", err)
	}
	if _, err := Measure(ctx, ffmpeg, filepath.Join(dir, "absent.mkv"), "0:a:0", false); !IsDecodeError(err) {
		t.Errorf("an absent file: %v", err)
	}
	full, err := Measure(ctx, ffmpeg, good, "0:a:0", true)
	if err != nil || full.DurationSec < 2.99 || full.DurationSec > 3.01 || full.Loudness == nil {
		t.Fatalf("the good track: %+v, %v", full, err)
	}
	short := filepath.Join(dir, "short.mkv")
	ffmpegRun(t, ffmpeg, "-i", good, "-t", "2", "-c:a", "aac", "--", short)
	m, err := Measure(ctx, ffmpeg, short, "0:a:0", false)
	if err != nil {
		t.Fatal(err)
	}
	op := Op{Codec: CodecAAC, SampleRate: 48000}
	if err := CheckDuration(op, full.DurationSec, m.DurationSec); err == nil {
		t.Errorf("a track of %.3fs passed against %.3fs", m.DurationSec, full.DurationSec)
	}
}

// A report channel that nothing wrote to reads as not recorded, never as a mode.
func TestAudioReports_AnUnwrittenChannelIsNotRecorded(t *testing.T) {
	rep, err := OpenReports(2)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rep.ExtraFiles(nil)); n != 3 {
		t.Fatalf("ExtraFiles = %d, want 3 (descriptor 3 then two reports)", n)
	}
	if _, err := rep.w[1].WriteString(report); err != nil {
		t.Fatal(err)
	}
	rep.Started()
	got := rep.Collect()
	rep.Close()
	if Mode(got[0]) != ModeNotRecorded || Mode(got[1]) != ModeLinear {
		t.Errorf("modes = %q, %q", Mode(got[0]), Mode(got[1]))
	}
	none, _ := OpenReports(0)
	if none.ExtraFiles(nil) != nil {
		t.Error("no reports and no progress hand over descriptors")
	}
	pr, pw, _ := os.Pipe()
	defer pr.Close()
	defer pw.Close()
	if f := none.ExtraFiles(pw); len(f) != 1 || f[0] != pw {
		t.Error("the progress channel is not descriptor 3")
	}
	var nilRep *Reports
	nilRep.Started()
	nilRep.Close()
	if nilRep.Collect() != nil || nilRep.ExtraFiles(nil) != nil {
		t.Error("a nil Reports hands something over")
	}
}
