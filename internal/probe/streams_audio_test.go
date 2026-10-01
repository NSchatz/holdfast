package probe

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

// The stream-list probe reports the audio facts an audio re-encode is planned from - codec,
// profile, channels, layout and sample rate - per stream, read off a REAL file through the
// REAL ffprobe, and reports none of them on the video stream.
func TestStreams_ReportsEachAudioTracksChannelsLayoutAndRate(t *testing.T) {
	p := realProber()
	path := filepath.Join(t.TempDir(), "audio.mkv")
	out, err := exec.Command(p.FFmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=duration=1:size=64x48:rate=5",
		"-f", "lavfi", "-i", "anoisesrc=r=44100:a=0.1:d=1",
		"-f", "lavfi", "-i", "anoisesrc=r=48000:a=0.1:d=1",
		"-map", "0", "-map", "1", "-map", "2",
		"-c:v", "libx264", "-preset", "ultrafast",
		"-c:a", "flac", "-filter:a:0", "aformat=channel_layouts=mono",
		"-filter:a:1", "aformat=channel_layouts=5.1(side)", "--", path).CombinedOutput()
	if err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
	streams, ok := p.Streams(context.Background(), path)
	if !ok || len(streams) != 3 {
		t.Fatalf("Streams = %+v, %v; want three established streams", streams, ok)
	}
	v, mono, surround := streams[0], streams[1], streams[2]
	if v.Channels != 0 || v.ChannelLayout != "" || v.SampleRate != 0 {
		t.Errorf("the video stream reports audio facts: %+v", v)
	}
	if mono.Codec != "flac" || mono.Channels != 1 || mono.ChannelLayout != "mono" || mono.SampleRate != 44100 {
		t.Errorf("the mono track = %+v, want flac, 1 channel, mono, 44100", mono)
	}
	if surround.Channels != 6 || surround.ChannelLayout != "5.1(side)" || surround.SampleRate != 48000 {
		t.Errorf("the 5.1 track = %+v, want 6 channels, 5.1(side), 48000", surround)
	}
}

// A sample rate ffprobe did not print, or printed as something other than a positive whole
// number, is no rate at all.
func TestSampleRateOf_ReadsOnlyAPositiveWholeNumber(t *testing.T) {
	for in, want := range map[string]int{"48000": 48000, " 44100 ": 44100, "": 0, "0": 0, "-1": 0, "48k": 0, "N/A": 0} {
		if got := sampleRateOf(in); got != want {
			t.Errorf("sampleRateOf(%q) = %d, want %d", in, got, want)
		}
	}
}
