package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/audio"
	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/probe"
)

// audioStreams is a source carrying a video stream, a 5.1 FLAC track in English, an AC-3
// stereo track in French and an English FLAC commentary, as the probe would list them.
func audioStreams() []probe.Stream {
	return []probe.Stream{
		{Index: 0, Type: probe.TypeVideo, Codec: "h264"},
		{Index: 1, Type: probe.TypeAudio, Codec: "flac", Channels: 6, ChannelLayout: "5.1", SampleRate: 48000,
			Language: "eng", SourceLanguageTag: "eng"},
		{Index: 2, Type: probe.TypeAudio, Codec: "ac3", Channels: 2, ChannelLayout: "stereo", SampleRate: 48000,
			Language: "fre", SourceLanguageTag: "fre"},
		{Index: 3, Type: probe.TypeAudio, Codec: "flac", Channels: 2, ChannelLayout: "stereo", SampleRate: 44100,
			Language: "", Commentary: true},
	}
}

// remuxPlan derives a remux plan (which reads no snapshot) over audioStreams under prof.
func remuxPlan(t *testing.T, prof config.Profile, measure audio.MeasureFunc) (*EncodePlan, error) {
	t.Helper()
	yes := true
	prof.RemuxOnly = &yes
	streams := DeriveStreamPlan(audioStreams(), prof, "h264")
	return deriveEncodePlan(planInputs{prof: prof, source: "/lib/movie.mkv", output: "/lib/movie.out.mkv",
		streams: streams, measureLoudness: measure,
		snapshot: func() (*probe.VideoProps, error) {
			t.Fatal("a remux read the snapshot")
			return nil, nil
		}})
}

// With every audio key unset the plan's audio is the zero plan: no first pass runs, the
// command line is the one this build always built, nothing is added to the intended map and
// nothing is recorded.
func TestAudio_KeysUnsetChangeNoArgvNoDecisionAndRunNoProbe(t *testing.T) {
	never := func(audio.Source, string) (audio.Stats, error) {
		t.Fatal("a loudness pass ran with every audio key unset")
		return audio.Stats{}, nil
	}
	for name, prof := range map[string]config.Profile{
		"unset":            {},
		"codec alone":      {AudioCodec: "aac"},
		"loudness alone":   {AudioLoudness: config.AudioLoudnessR128, AudioCodec: "opus"},
		"explicit default": {AudioReencode: config.AudioOff, AudioDownmix: config.AudioOff},
	} {
		p, err := remuxPlan(t, prof, never)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(p.AudioTracks.Ops) != 0 || p.audioSeen != nil || len(p.addedStreams()) != 0 || p.audioRecord().Recorded() {
			t.Errorf("%s: the audio plan is not the zero plan: %+v", name, p.AudioTracks)
		}
		_, body, err := p.args(encoder.X265Parallelism{})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(body, " "); got != "-map 0 -map -0:d? -c copy" {
			t.Errorf("%s: body = %s", name, got)
		}
	}
}

// A plan the audio derivation did not make is refused before a command line is built.
func TestAudio_BuildableRefusesAnAudioPlanItsDerivationCouldNotMake(t *testing.T) {
	prof := config.Profile{AudioReencode: config.AudioReencodeOn, AudioCodec: "ac3"}
	p, err := remuxPlan(t, prof, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.args(encoder.X265Parallelism{}); err != nil {
		t.Fatalf("the derived plan does not build: %v", err)
	}
	p.AudioTracks.Ops[0].Layout = "7.1"
	_, _, err = p.args(encoder.X265Parallelism{})
	var unbuildable *UnbuildablePlanError
	if !errors.As(err, &unbuildable) || !strings.Contains(err.Error(), "audio plan") {
		t.Errorf("a tampered audio plan: %v", err)
	}
}

// The derivation: re-encode in place, a kept original's re-encode and a downmix appended, the
// additions tallied for stream parity with their language and commentary, the first pass run
// through the injected seam, and a re-encode with no intended map refused.
func TestAudio_PlanDerivationAddsTalliesAndMeasures(t *testing.T) {
	var passes []string
	measure := func(s audio.Source, pre string) (audio.Stats, error) {
		passes = append(passes, s.Language+"|"+pre)
		return audio.Stats{InputI: -31, InputTP: -9, InputLRA: 5, InputThresh: -41, TargetOffset: 0.1}, nil
	}
	yes := true
	prof := config.Profile{AudioReencode: config.AudioReencodeOn, AudioCodec: "aac", KeepOriginalAudio: &yes,
		AudioDownmix: config.AudioDownmixStereo, AudioLoudness: config.AudioLoudnessR128}
	p, err := remuxPlan(t, prof, measure)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(passes, ",") != "eng|,|,eng|aformat=channel_layouts=stereo" {
		t.Errorf("first passes = %v", passes)
	}
	added := p.addedStreams()
	if len(added) != 3 || added[0].Language != "eng" || added[0].Commentary || !added[1].Commentary ||
		added[2].Language != "eng" || added[2].Commentary {
		t.Errorf("added = %+v", added)
	}
	if p.audioSeen == nil || len(p.AudioTracks.Normalised()) != 3 {
		t.Fatalf("normalised = %d", len(p.AudioTracks.Normalised()))
	}
	// The intended map with the additions accepts an output carrying them, and refuses one
	// missing the downmix.
	out := append(audioStreams(),
		probe.Stream{Type: probe.TypeAudio, Language: "eng"},
		probe.Stream{Type: probe.TypeAudio, Commentary: true},
		probe.Stream{Type: probe.TypeAudio, Language: "eng"})
	if err := p.Streams.CheckOutputAdding(out, added); err != nil {
		t.Errorf("the declared output is refused: %v", err)
	}
	if err := p.Streams.CheckOutputAdding(out[:len(out)-1], added); err == nil {
		t.Error("an output missing the downmix passed stream parity")
	}
	if err := p.Streams.CheckOutput(out); err == nil {
		t.Error("an output carrying additions passed a map that declares none")
	}
	// The record, before any report: every normalised track's mode is not recorded.
	rec := p.audioRecord().Tracks()
	if len(rec) != 6 || rec[0].Action != "kept" || rec[3].Action != "added" || rec[3].Loudness != audio.ModeNotRecorded ||
		rec[3].MeasuredLUFS == nil || *rec[3].MeasuredLUFS != -31 || rec[3].AchievedLUFS != nil || rec[1].Reason != audio.ReasonNotLossless {
		t.Errorf("record = %+v", rec)
	}
	// The observations: a report read back and a gate measurement land on the right track.
	p.recordReports([][]byte{[]byte(`{"input_i":"-31","input_tp":"-9","input_lra":"5","input_thresh":"-41",` +
		`"output_i":"-23","normalization_type":"linear","target_offset":"0.1"}`), nil})
	p.recordAchieved(3, -23.2)
	rec = p.audioRecord().Tracks()
	if rec[3].Loudness != audio.ModeLinear || rec[3].AchievedLUFS == nil || *rec[3].AchievedLUFS != -23.2 ||
		rec[4].Loudness != audio.ModeNotRecorded || rec[4].AchievedLUFS != nil {
		t.Errorf("record after the observations = %+v / %+v", rec[3], rec[4])
	}
	// No map to read the tracks from: refused rather than planned blind.
	err = (&EncodePlan{}).deriveAudio(planInputs{prof: config.Profile{AudioReencode: config.AudioReencodeOn,
		AudioCodec: "aac"}, source: "/lib/movie.mkv", output: "/lib/out.mkv"})
	if err == nil || !strings.Contains(err.Error(), "intended stream map") {
		t.Errorf("a re-encode with no map: %v", err)
	}
	// A failing first pass refuses the plan, and so the job.
	_, err = remuxPlan(t, prof, func(audio.Source, string) (audio.Stats, error) {
		return audio.Stats{}, errors.New("boom")
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("a failing first pass: %v", err)
	}
	// A plan carried over from an earlier derivation is used as it is, with no pass.
	again, err := deriveEncodePlan(planInputs{prof: p.Profile, source: "/lib/movie.mkv", output: "/lib/movie.out.mkv",
		streams: p.Streams, audio: &p.AudioTracks, measureLoudness: func(audio.Source, string) (audio.Stats, error) {
			t.Fatal("a carried-over audio plan was measured again")
			return audio.Stats{}, nil
		}})
	if err != nil || len(again.AudioTracks.Ops) != len(p.AudioTracks.Ops) || again.audioSeen == nil {
		t.Errorf("carried over: %v", err)
	}
	// Without a measurer the derivation refuses rather than normalising on nothing.
	_, err = remuxPlan(t, prof, nil)
	if err == nil || !strings.Contains(err.Error(), "no loudness measurement") {
		t.Errorf("no measurer: %v", err)
	}
}

// Every audio gate member is in the published vocabulary.
func TestAudio_GateVocabularyCarriesTheAudioGates(t *testing.T) {
	seen := map[string]bool{}
	for _, g := range GateVocabulary {
		seen[g] = true
	}
	if !seen[GateAudio] || !seen[GateLoudness] || GateAudio != "audio" || GateLoudness != "loudness" {
		t.Errorf("vocabulary = %v", GateVocabulary)
	}
	var nilPlan *EncodePlan
	nilPlan.recordReports(nil)
	nilPlan.recordAchieved(0, 0)
	if m, a := nilPlan.loudnessSeen(0, 0); m != audio.ModeNotRecorded || a != nil {
		t.Error("a nil plan observed something")
	}
	if nilPlan.audioRecord().Recorded() {
		t.Error("a nil plan records audio")
	}
}
