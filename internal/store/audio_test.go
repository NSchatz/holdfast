package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// The audio record round-trips through the column, and its absence stays absence: a row of a
// job that transformed no audio reads back NOT RECORDED, never an empty list.
func TestAudioTracks_RoundTripAndAbsence(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	zero, one := 0, 1
	lufs, got := -30.25, -23.1
	tracks := []AudioTrack{
		{SourceIndex: 1, OutputIndex: &zero, Action: "reencoded", Codec: "eac3", Layout: "5.1(side)", SampleRate: 48000,
			BitrateKbps: 640, Loudness: "linear", MeasuredLUFS: &lufs, AchievedLUFS: &got},
		{SourceIndex: 2, OutputIndex: &one, Action: "copied", Reason: "not-lossless"},
		{SourceIndex: 1, Action: "downmix-skipped", Reason: "stereo-track-carried"},
	}
	for path, rec := range map[string]AudioTracks{
		"/a/recorded.mkv": RecordAudioTracks(tracks),
		"/a/empty.mkv":    RecordAudioTracks(nil),
		"/a/absent.mkv":   {},
	} {
		if ok, err := s.Claim(ctx, path, "fp", "w0", 3, sameConfig); err != nil || !ok {
			t.Fatalf("claim %s: %v %v", path, ok, err)
		}
		if err := s.Finish(ctx, path, "fp", Done, &Outcome{Encoder: "cpu", AudioTracks: rec}, 3); err != nil {
			t.Fatalf("Finish: %v", err)
		}
	}
	rows, err := s.List(ctx, []Status{Done}, 0)
	if err != nil || len(rows) != 3 {
		t.Fatalf("List: %d %v", len(rows), err)
	}
	for _, r := range rows {
		a := r.Outcome.AudioTracks
		switch r.Path {
		case "/a/recorded.mkv":
			if !a.Recorded() || !reflect.DeepEqual(a.Tracks(), tracks) {
				t.Errorf("recorded = %+v", a.Tracks())
			}
		case "/a/empty.mkv":
			if !a.Recorded() || len(a.Tracks()) != 0 || a.String() != "none" {
				t.Errorf("an empty record reads %v %v", a.Recorded(), a.Tracks())
			}
		case "/a/absent.mkv":
			if a.Recorded() || a.String() != "not recorded" {
				t.Errorf("an absent record reads as recorded: %v", a.Tracks())
			}
		}
	}
}

func TestAudioTracks_EncodeAndParse(t *testing.T) {
	if (AudioTracks{}).Encode() != "" || RecordAudioTracks(nil).Encode() != "[]" {
		t.Error("absence or emptiness encodes wrongly")
	}
	for _, bad := range []string{"", "  ", "null", "{", `{"a":1}`, "[1,2]"} {
		if ParseAudioTracks(bad).Recorded() {
			t.Errorf("%q parses as a record", bad)
		}
	}
	one := 1
	rec := RecordAudioTracks([]AudioTrack{{SourceIndex: 3, OutputIndex: &one, Action: "downmix", Codec: "aac",
		Layout: "stereo", SampleRate: 44100, BitrateKbps: 128, Loudness: "dynamic"}, {SourceIndex: 4, Action: "copied", Reason: "not-lossless"}})
	back := ParseAudioTracks(rec.Encode())
	if !reflect.DeepEqual(back.Tracks(), rec.Tracks()) {
		t.Errorf("round trip = %+v", back.Tracks())
	}
	if s := rec.String(); !strings.Contains(s, "3:downmix aac stereo 44100Hz 128k loudness dynamic") ||
		!strings.Contains(s, "4:copied (not-lossless)") {
		t.Errorf("String = %s", s)
	}
}
