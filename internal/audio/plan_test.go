package audio

import (
	"errors"
	"strings"
	"testing"
)

func src(index int, codec string, ch int, layout string, rate int, lang string) Source {
	return Source{Index: index, Codec: codec, Channels: ch, Layout: layout, SampleRate: rate, Language: lang}
}

func noMeasure(t *testing.T) MeasureFunc {
	return func(Source, string) (Stats, error) {
		t.Fatal("a first pass ran where no track is normalised")
		return Stats{}, nil
	}
}

var measured = Stats{InputI: -30.5, InputTP: -10.25, InputLRA: 4.5, InputThresh: -41, TargetOffset: 0.3, Mode: ModeDynamic}

func TestAudioPlan_InactiveSettingsDeriveTheZeroPlan(t *testing.T) {
	carried := []Source{src(1, "truehd", 8, "7.1", 48000, "eng")}
	for _, set := range []Settings{{}, {Codec: "aac"}, {Codec: "aac", Loudness: true, KeepOriginal: true}} {
		p, err := Derive(set, carried, "mkv", noMeasure(t))
		if err != nil || len(p.Ops) != 0 || p.Transforms() || len(p.Args()) != 0 {
			t.Errorf("Derive(%+v) = %+v, %v; want the zero plan", set, p, err)
		}
		if err := p.Check(carried); err != nil {
			t.Errorf("the zero plan is refused: %v", err)
		}
	}
}

// Replace by default: a lossless track is re-encoded in its own place; a lossy one is copied.
func TestAudioPlan_ReencodeReplacesALosslessTrackInPlace(t *testing.T) {
	carried := []Source{src(1, "truehd", 6, "5.1(side)", 48000, "eng"), src(2, "ac3", 6, "5.1", 48000, "fra"),
		src(3, "pcm_s24le", 2, "stereo", 96000, "")}
	p, err := Derive(Settings{Reencode: true, Codec: "eac3"}, carried, "mkv", noMeasure(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Ops) != 3 {
		t.Fatalf("ops = %+v", p.Ops)
	}
	want := []Op{
		{Source: carried[0], Action: ActionReencoded, Output: 0, Codec: "eac3", Layout: "5.1(side)", Channels: 6,
			SampleRate: 48000, BitrateKbps: 640, LoudnessIndex: -1},
		{Source: carried[1], Action: ActionCopied, Reason: ReasonNotLossless, Output: 1, LoudnessIndex: -1},
		{Source: carried[2], Action: ActionReencoded, Output: 2, Codec: "eac3", Layout: "stereo", Channels: 2,
			SampleRate: 48000, BitrateKbps: 192, LoudnessIndex: -1},
	}
	for i := range want {
		if !opEqual(p.Ops[i], want[i]) {
			t.Errorf("op %d = %+v, want %+v", i, p.Ops[i], want[i])
		}
	}
	got := strings.Join(p.Args(), " ")
	if got != "-c:a:0 eac3 -b:a:0 640k -ch_layout:a:0 5.1(side) -ar:a:0 48000 "+
		"-c:a:2 eac3 -b:a:2 192k -ch_layout:a:2 stereo -ar:a:2 48000" {
		t.Errorf("Args = %s", got)
	}
	if !p.Transforms() || len(p.Outputs()) != 3 {
		t.Errorf("Transforms=%v Outputs=%d", p.Transforms(), len(p.Outputs()))
	}
	if err := p.Check(carried); err != nil {
		t.Errorf("Check: %v", err)
	}
}

func opEqual(a, b Op) bool {
	if (a.Loudness == nil) != (b.Loudness == nil) || (a.Loudness != nil && *a.Loudness != *b.Loudness) {
		return false
	}
	a.Loudness, b.Loudness = nil, nil
	return a == b
}

// keep_original_audio: the original stays copied in place and the re-encode is appended,
// never the default track, a commentary re-encode keeping the commentary disposition.
func TestAudioPlan_KeepOriginalKeepsBoth(t *testing.T) {
	com := src(4, "flac", 2, "stereo", 44100, "eng")
	com.Commentary = true
	carried := []Source{src(1, "flac", 6, "5.1", 48000, "eng"), com}
	p, err := Derive(Settings{Reencode: true, Codec: "opus", KeepOriginal: true,
		BitratesKbps: map[Family]int{Surround51: 320}}, carried, "mp4", noMeasure(t))
	if err != nil {
		t.Fatal(err)
	}
	acts := []string{}
	for _, o := range p.Ops {
		acts = append(acts, string(o.Action)+"@"+itoa(o.Output))
	}
	if strings.Join(acts, ",") != "kept@0,kept@1,added@2,added@3" {
		t.Fatalf("ops = %v", acts)
	}
	got := strings.Join(p.Args(), " ")
	want := "-map 0:1 -map 0:4 -c:a:2 libopus -b:a:2 320k -ch_layout:a:2 5.1 -ar:a:2 48000 -disposition:a:2 0 " +
		"-c:a:3 libopus -b:a:3 128k -ch_layout:a:3 stereo -ar:a:3 48000 -disposition:a:3 comment"
	if got != want {
		t.Errorf("Args =\n%s\nwant\n%s", got, want)
	}
	if err := p.Check(carried); err != nil {
		t.Errorf("Check: %v", err)
	}
}

func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	return string(rune('0' + n))
}

// The downmix: one per language, from the first surround track of it, not where a stereo
// track of that language is carried, never from commentary; a 7.1 into AC-3 is copied with
// its reason but still folds to stereo.
func TestAudioPlan_DownmixIsAddedPerLanguageWithoutAStereoTrack(t *testing.T) {
	com := src(6, "ac3", 6, "5.1", 48000, "eng")
	com.Commentary = true
	carried := []Source{
		src(1, "truehd", 8, "7.1", 48000, "eng"),
		src(2, "dts", 6, "5.1(side)", 48000, "eng"),
		src(3, "ac3", 6, "5.1", 48000, "fra"),
		src(4, "aac", 2, "stereo", 48000, "fra"),
		src(5, "ac3", 6, "6 channels", 48000, "deu"),
		com,
		src(7, "aac", 1, "mono", 48000, "spa"),
	}
	carried[1].Profile = "DTS-HD MA"
	p, err := Derive(Settings{Reencode: true, Downmix: true, Codec: "ac3"}, carried, "mkv", noMeasure(t))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range p.Ops {
		got = append(got, itoa(o.Source.Index)+":"+string(o.Action)+":"+o.Reason+":"+itoa(o.Output))
	}
	want := []string{
		"1:copied:layout-not-carried:0", "2:reencoded::1", "3:copied:not-lossless:2", "4:copied:not-lossless:3",
		"5:copied:not-lossless:4", "6:copied:not-lossless:5", "7:copied:not-lossless:6",
		"1:downmix::7",
		"2:downmix-skipped:downmix-already-added:-1", "3:downmix-skipped:stereo-track-carried:-1",
		"5:downmix-skipped:layout-not-carried:-1",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("ops =\n%v\nwant\n%v", got, want)
	}
	dm := p.Ops[7]
	if dm.Layout != "stereo" || dm.Channels != 2 || dm.BitrateKbps != 192 || dm.Prefilter() != DownmixFilter {
		t.Errorf("downmix op = %+v", dm)
	}
	args := strings.Join(p.Args(), " ")
	if !strings.HasPrefix(args, "-map 0:1 ") || !strings.Contains(args,
		"-c:a:7 ac3 -b:a:7 192k -ch_layout:a:7 stereo -ar:a:7 48000 -filter:a:7 aformat=channel_layouts=stereo -disposition:a:7 0") {
		t.Errorf("Args = %s", args)
	}
	if len(p.Outputs()) != 8 {
		t.Errorf("outputs = %d, want 8", len(p.Outputs()))
	}
	if err := p.Check(carried); err != nil {
		t.Errorf("Check: %v", err)
	}
}

func TestAudioPlan_DownmixAloneAndAnUnsupportedContainer(t *testing.T) {
	carried := []Source{src(1, "eac3", 6, "5.1", 48000, "")}
	p, err := Derive(Settings{Downmix: true, Codec: "aac"}, carried, "mkv", noMeasure(t))
	if err != nil || len(p.Ops) != 2 || p.Ops[0].Reason != ReasonReencodeOff || p.Ops[1].Action != ActionDownmix ||
		p.Ops[1].BitrateKbps != 128 {
		t.Fatalf("ops = %+v, %v", p.Ops, err)
	}
	p, _ = Derive(Settings{Reencode: true, Downmix: true, Codec: "aac"},
		[]Source{src(1, "flac", 6, "5.1", 48000, "")}, "mov", noMeasure(t))
	if p.Transforms() || p.Ops[0].Reason != ReasonContainer || p.Ops[1].Reason != ReasonContainer {
		t.Errorf("an unsupported container: %+v", p.Ops)
	}
}

func TestAudioPlan_ATrackTheMatrixCannotEncodeIsCopiedWithItsReason(t *testing.T) {
	cases := []struct {
		name string
		set  Settings
		s    Source
		want string
	}{
		{"7.1 into eac3", Settings{Codec: "eac3"}, src(1, "truehd", 8, "7.1", 48000, ""), ReasonLayout},
		{"a layout out of the matrix", Settings{Codec: "aac"}, src(1, "flac", 8, "7.1(wide)", 48000, ""), ReasonLayout},
		{"a count that disagrees with the layout", Settings{Codec: "aac"}, src(1, "flac", 2, "5.1", 48000, ""), ReasonLayout},
		{"no layout", Settings{Codec: "aac"}, src(1, "pcm_s16le", 6, "", 48000, ""), ReasonLayout},
		{"no sample rate", Settings{Codec: "aac"}, src(1, "flac", 2, "stereo", 0, ""), ReasonNoSampleRate},
		{"a bitrate past aac's clamp", Settings{Codec: "aac", BitratesKbps: map[Family]int{Mono: 300}},
			src(1, "flac", 1, "mono", 48000, ""), ReasonBitrate},
		{"a bitrate AC-3 cannot carry", Settings{Codec: "ac3", BitratesKbps: map[Family]int{Stereo: 200}},
			src(1, "flac", 2, "stereo", 48000, ""), ReasonBitrate},
		{"an unknown codec", Settings{Codec: "mp3"}, src(1, "flac", 2, "stereo", 48000, ""), ReasonLayout},
	}
	for _, c := range cases {
		c.set.Reencode = true
		p, err := Derive(c.set, []Source{c.s}, "mkv", noMeasure(t))
		if err != nil || len(p.Ops) != 1 || p.Ops[0].Action != ActionCopied || p.Ops[0].Reason != c.want {
			t.Errorf("%s: %+v, %v; want copied for %s", c.name, p.Ops, err, c.want)
		}
	}
	// A configured bitrate at the limit is taken.
	p, _ := Derive(Settings{Reencode: true, Codec: "aac", BitratesKbps: map[Family]int{Mono: 288}},
		[]Source{src(1, "flac", 1, "mono", 48000, "")}, "mkv", noMeasure(t))
	if p.Ops[0].Action != ActionReencoded || p.Ops[0].BitrateKbps != 288 {
		t.Errorf("the limit itself: %+v", p.Ops[0])
	}
}

// Loudness: one first pass per source track and prefilter, the figures handed to the second,
// a report channel per normalised track in plan order, and aresample after it.
func TestAudioPlan_TwoPassLoudnessArguments(t *testing.T) {
	carried := []Source{src(1, "flac", 6, "5.1", 44100, "eng"), src(2, "flac", 2, "stereo", 48000, "fra")}
	var calls []string
	measure := func(s Source, pre string) (Stats, error) {
		calls = append(calls, itoa(s.Index)+"|"+pre)
		return measured, nil
	}
	p, err := Derive(Settings{Reencode: true, Downmix: true, KeepOriginal: true, Loudness: true, Codec: "aac"},
		carried, "mkv", measure)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "1|,2|,1|aformat=channel_layouts=stereo" {
		t.Errorf("first passes = %v", calls)
	}
	norm := p.Normalised()
	if len(norm) != 3 || norm[0].LoudnessIndex != 0 || norm[2].LoudnessIndex != 2 || norm[2].Action != ActionDownmix {
		t.Fatalf("normalised = %+v", norm)
	}
	args := strings.Join(p.Args(), " ")
	pass2 := "loudnorm=I=-23.00:TP=-1.00:LRA=20.00:measured_I=-30.50:measured_TP=-10.25:measured_LRA=4.50:" +
		"measured_thresh=-41.00:offset=0.30:linear=true:print_format=json:stats_file=/proc/self/fd/"
	for _, want := range []string{
		"-filter:a:2 " + pass2 + "4,aresample=44100",
		"-filter:a:3 " + pass2 + "5,aresample=48000",
		"-filter:a:4 aformat=channel_layouts=stereo," + pass2 + "6,aresample=44100",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("Args lacks %q:\n%s", want, args)
		}
	}
	if err := p.Check(carried); err != nil {
		t.Errorf("Check: %v", err)
	}
	if StatsFD(0) != "/proc/self/fd/4" || FirstStatsFD != 4 {
		t.Errorf("StatsFD(0) = %s", StatsFD(0))
	}
}

func TestAudioPlan_LoudnessOfSilenceIsSkippedAndAFailedPassRefuses(t *testing.T) {
	carried := []Source{src(1, "flac", 2, "stereo", 48000, "")}
	silent := measured
	silent.InputI = -99.5
	p, err := Derive(Settings{Reencode: true, Loudness: true, Codec: "opus"}, carried, "mkv",
		func(Source, string) (Stats, error) { return silent, nil })
	if err != nil || p.Ops[0].Loudness != nil || p.Ops[0].Reason != ReasonLoudnessSilence ||
		p.Ops[0].LoudnessIndex != -1 || len(p.Normalised()) != 0 {
		t.Errorf("silence: %+v, %v", p.Ops, err)
	}
	if strings.Contains(strings.Join(p.Args(), " "), "loudnorm") {
		t.Error("a silent track is normalised")
	}
	boom := errors.New("boom")
	_, err = Derive(Settings{Reencode: true, Loudness: true, Codec: "opus"}, carried, "mkv",
		func(Source, string) (Stats, error) { return Stats{}, boom })
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "source stream 1") {
		t.Errorf("a failed first pass: %v", err)
	}
}

// Check refuses every plan its derivation could not have made.
func TestAudioPlan_CheckRefusesWhatTheDerivationCouldNotMake(t *testing.T) {
	carried := []Source{src(1, "flac", 6, "5.1", 48000, "eng"), src(2, "ac3", 2, "stereo", 48000, "fra")}
	derive := func() Plan {
		p, err := Derive(Settings{Reencode: true, Downmix: true, KeepOriginal: true, Loudness: true, Codec: "ac3"},
			carried, "mkv", func(Source, string) (Stats, error) { return measured, nil })
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := derive().Check(carried); err != nil {
		t.Fatalf("the derived plan is refused: %v", err)
	}
	cases := map[string]func(p *Plan) []Source{
		"fewer carried":       func(p *Plan) []Source { return carried[:1] },
		"another source":      func(p *Plan) []Source { p.Ops[0].Source.Index = 9; return carried },
		"a changed source":    func(p *Plan) []Source { p.Ops[2].Source.Channels = 8; return carried },
		"out of place":        func(p *Plan) []Source { p.Ops[0].Output = 1; return carried },
		"appended in place":   func(p *Plan) []Source { p.Ops[0].Action = ActionAdded; return carried },
		"appended misplaced":  func(p *Plan) []Source { p.Ops[2].Output = 5; return carried },
		"copy normalised":     func(p *Plan) []Source { p.Ops[1].Loudness = &measured; return carried },
		"an unknown action":   func(p *Plan) []Source { p.Ops[2].Action = "upmix"; return carried },
		"a layout":            func(p *Plan) []Source { p.Ops[2].Layout = "5.1"; return carried },
		"a channel count":     func(p *Plan) []Source { p.Ops[2].Channels = 2; return carried },
		"a codec":             func(p *Plan) []Source { p.Ops[2].Codec = "mp3"; return carried },
		"a rate":              func(p *Plan) []Source { p.Ops[2].SampleRate = 44100; return carried },
		"a bitrate":           func(p *Plan) []Source { p.Ops[2].BitrateKbps = 0; return carried },
		"a bitrate too high":  func(p *Plan) []Source { p.Ops[2].BitrateKbps = 768; return carried },
		"an AC-3 bitrate":     func(p *Plan) []Source { p.Ops[2].BitrateKbps = 600; return carried },
		"unusable figures":    func(p *Plan) []Source { s := measured; s.InputI = 5; p.Ops[2].Loudness = &s; return carried },
		"a channel misnumber": func(p *Plan) []Source { p.Ops[3].LoudnessIndex = 0; return carried },
		"a channel unused":    func(p *Plan) []Source { p.Ops[2].Loudness = nil; return carried },
		"a skipped in place": func(p *Plan) []Source {
			p.Ops = append(p.Ops, Op{Source: carried[0], Action: ActionReencoded, Output: -1})
			return carried
		},
		"a skipped out of place": func(p *Plan) []Source {
			p.Ops[0].Action = ActionDownmixSkipped
			return carried
		},
	}
	for name, mut := range cases {
		p := derive()
		c := mut(&p)
		if err := p.Check(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
