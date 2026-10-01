package store

import (
	"encoding/json"
	"strings"
)

// CropRecord is what one job did about cropping its source (docs/design/crop.md#crop): the
// rectangle it kept, or the reason it kept the whole frame.
//
// Every field follows this package's absence rule: "" is NOT RECORDED. Rect and Frame are
// recorded where the job cropped, Reason and Detail where it did not.
type CropRecord struct {
	// Applied says the replacement is the cropped picture.
	Applied bool `json:"applied"`
	// Rect is the rectangle of the source kept, as `W:H:X:Y` (cropdetect's spelling).
	Rect string `json:"rect,omitempty"`
	// Frame is the source's frame the rectangle was taken from, as `WxH`.
	Frame string `json:"frame,omitempty"`
	// L5Zeroed says the source was Dolby Vision, the rectangle is its RPU's own active area
	// (level 5), and the output's L5 was zeroed and gated (docs/design/crop.md#dolby-vision).
	L5Zeroed bool `json:"l5_zeroed,omitempty"`
	// Reason is the token saying why nothing was cropped (crop.Reasons).
	Reason string `json:"reason,omitempty"`
	// Detail says the same in words.
	Detail string `json:"detail,omitempty"`
}

// Crop is what ONE job recorded about its crop. Absence is representable: a row written
// before the column existed, and every row of a job whose root does not set `crop: auto`,
// recorded nothing.
type Crop struct {
	rec      CropRecord
	recorded bool
}

// RecordCrop records rec as what a job did about its crop.
func RecordCrop(rec CropRecord) Crop { return Crop{rec: rec, recorded: true} }

// Recorded reports whether anything was recorded.
func (c Crop) Recorded() bool { return c.recorded }

// Record returns what was recorded.
func (c Crop) Record() CropRecord { return c.rec }

// Encode renders the record as the TEXT the column holds: JSON, or "" (NULL) when nothing
// was recorded.
func (c Crop) Encode() string {
	if !c.recorded {
		return ""
	}
	b, err := json.Marshal(c.rec)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParseCrop is Encode's inverse. Anything it cannot read is NOT RECORDED: a record nothing
// can interpret is never reported as what a job did.
func ParseCrop(s string) Crop {
	if strings.TrimSpace(s) == "" {
		return Crop{}
	}
	var rec CropRecord
	if err := json.Unmarshal([]byte(s), &rec); err != nil {
		return Crop{}
	}
	if rec.Applied == (rec.Rect == "") || (!rec.Applied && rec.Reason == "") {
		return Crop{}
	}
	return RecordCrop(rec)
}

// String renders the record for a log line, stating NOT RECORDED in words.
func (c Crop) String() string {
	switch {
	case !c.recorded:
		return "not recorded"
	case c.rec.Applied:
		return c.rec.Rect + " of " + c.rec.Frame
	}
	return "not cropped (" + c.rec.Reason + ")"
}
