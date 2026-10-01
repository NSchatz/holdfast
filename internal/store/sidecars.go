package store

import (
	"encoding/json"
	"strings"
)

// Sidecar is what ONE job recorded about one carried subtitle stream under
// `subtitle_sidecars: text` (docs/design/subtitles.md#sidecars): the stream, and either the
// sidecar file it was published to or the reason none was.
//
// Exactly one of Path and Skipped is set on a record this build writes. Path is the
// published file; Skipped is a stable reason token (see internal/subtitle), with Detail
// carrying the error text where the token names a failure rather than a decision.
type Sidecar struct {
	// Index is the stream's absolute index in the source container.
	Index int `json:"index"`
	// Codec is ffprobe's codec_name for the stream, verbatim.
	Codec string `json:"codec"`
	// Language is the stream's language tag as the source wrote it, lowercased; "" where
	// the source wrote none.
	Language string `json:"language"`
	// Forced is the stream's own forced disposition.
	Forced bool `json:"forced"`
	// Path is the sidecar this job published, "" where it published none.
	Path string `json:"path,omitempty"`
	// Skipped is the reason token where no sidecar was published.
	Skipped string `json:"skipped,omitempty"`
	// Detail is the error a failure token carries, "" otherwise.
	Detail string `json:"detail,omitempty"`
	// Events is the stream's event (packet) count the parse-back gate compared, nil where
	// it was not established.
	Events *int `json:"events,omitempty"`
	// FontsLost says the source carried font attachments an ASS sidecar cannot take with
	// it, so a player reads the sidecar with its own fonts.
	FontsLost bool `json:"fonts_lost,omitempty"`
}

// Sidecars is what ONE job recorded about its subtitle sidecars.
//
// Absence is representable and is not an empty set, exactly as for DroppedStreams: a row
// written with the key off, or before the column existed, recorded nothing, while a job
// under `subtitle_sidecars: text` whose source carried no subtitle stream recorded an
// empty set.
type Sidecars struct {
	list     []Sidecar
	recorded bool
}

// RecordSidecars records list as what a job did about its sidecars. An empty list is a
// real record.
func RecordSidecars(list []Sidecar) Sidecars {
	return Sidecars{list: append([]Sidecar(nil), list...), recorded: true}
}

// Recorded reports whether this row recorded anything at all.
func (s Sidecars) Recorded() bool { return s.recorded }

// List returns the records in the order they were recorded (the source's stream order).
func (s Sidecars) List() []Sidecar { return append([]Sidecar{}, s.list...) }

// Encode renders the record as the single TEXT value the column holds: a JSON array, or
// "" when nothing was recorded (which nullString stores as NULL). An empty record is "[]",
// never "", so it does not land as NULL.
func (s Sidecars) Encode() string {
	if !s.recorded {
		return ""
	}
	list := s.list
	if list == nil {
		list = []Sidecar{}
	}
	b, err := json.Marshal(list)
	if err != nil {
		// Every field is a plain string, int or bool: Marshal cannot fail on one. Were it
		// to, nothing is recorded rather than something wrong.
		return ""
	}
	return string(b)
}

// ParseSidecars is Encode's inverse. Anything it cannot read is NOT RECORDED: a record
// nothing can interpret is never reported as a list somebody can act on.
func ParseSidecars(text string) Sidecars {
	if strings.TrimSpace(text) == "" {
		return Sidecars{}
	}
	var list []Sidecar
	if err := json.Unmarshal([]byte(text), &list); err != nil || list == nil {
		return Sidecars{}
	}
	return RecordSidecars(list)
}
