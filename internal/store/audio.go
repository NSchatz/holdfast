package store

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AudioTrack is what one job did to one audio track (docs/design/audio.md): which source
// stream it came from, where it sits in the output, what was done to it and what it was
// written as.
//
// Every field follows this package's absence rule: "" and 0 and nil are NOT RECORDED.
// Codec, Layout, SampleRate and BitrateKbps are recorded on a track this job encoded, and
// not on one it copied, whose bytes are the source's. Loudness is the mode the loudness
// filter reported for this track - "linear" or "dynamic" - or "not-recorded" where the
// filter ran and no report was read back, and "" where the track was not normalised.
type AudioTrack struct {
	// SourceIndex is the source stream's absolute index.
	SourceIndex int `json:"source_index"`
	// OutputIndex is the track's audio-relative index in the output (the N of a:N), and nil
	// where the record is of a downmix that was not added.
	OutputIndex *int `json:"output_index"`
	// Action is one of the audio plan's actions: copied, reencoded, kept, added, downmix,
	// downmix-skipped.
	Action string `json:"action"`
	// Reason is why a track was copied, a downmix not added, or loudness not applied.
	Reason      string `json:"reason,omitempty"`
	Codec       string `json:"codec,omitempty"`
	Layout      string `json:"layout,omitempty"`
	SampleRate  int    `json:"sample_rate,omitempty"`
	BitrateKbps int    `json:"bitrate_kbps,omitempty"`
	Loudness    string `json:"loudness,omitempty"`
	// MeasuredLUFS is the first pass's integrated loudness of the source track, and
	// AchievedLUFS the loudness gate's measurement of the output track.
	MeasuredLUFS *float64 `json:"measured_lufs,omitempty"`
	AchievedLUFS *float64 `json:"achieved_lufs,omitempty"`
}

// AudioTracks is what ONE job recorded about its audio. Like DroppedStreams, absence is
// representable and is not an empty list: a row written before the column existed - and
// every row of a job whose configuration asked for no audio transformation, which copies
// every track exactly as this build always did - recorded nothing.
type AudioTracks struct {
	list     []AudioTrack
	recorded bool
}

// RecordAudioTracks records list as what a job did to its audio. An EMPTY list is a record:
// the audio keys applied and the source carried no audio track.
func RecordAudioTracks(list []AudioTrack) AudioTracks {
	return AudioTracks{list: append([]AudioTrack{}, list...), recorded: true}
}

// Recorded reports whether anything was recorded.
func (a AudioTracks) Recorded() bool { return a.recorded }

// Tracks returns the recorded tracks, in the order they were recorded.
func (a AudioTracks) Tracks() []AudioTrack { return append([]AudioTrack(nil), a.list...) }

// Encode renders the record as the TEXT the column holds: JSON, or "" (NULL) when nothing
// was recorded.
func (a AudioTracks) Encode() string {
	if !a.recorded {
		return ""
	}
	b, err := json.Marshal(a.list)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParseAudioTracks is Encode's inverse. Anything it cannot read is NOT RECORDED: a record
// nothing can interpret is never reported as what a job did.
func ParseAudioTracks(s string) AudioTracks {
	if strings.TrimSpace(s) == "" {
		return AudioTracks{}
	}
	var list []AudioTrack
	if err := json.Unmarshal([]byte(s), &list); err != nil || list == nil {
		return AudioTracks{}
	}
	return RecordAudioTracks(list)
}

// String renders the record for a log line, stating NOT RECORDED in words.
func (a AudioTracks) String() string {
	if !a.recorded {
		return "not recorded"
	}
	if len(a.list) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(a.list))
	for _, t := range a.list {
		p := fmt.Sprintf("%d:%s", t.SourceIndex, t.Action)
		if t.Codec != "" {
			p += fmt.Sprintf(" %s %s %dHz %dk", t.Codec, t.Layout, t.SampleRate, t.BitrateKbps)
		}
		if t.Loudness != "" {
			p += " loudness " + t.Loudness
		}
		if t.Reason != "" {
			p += " (" + t.Reason + ")"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}
