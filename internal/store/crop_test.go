package store

import "testing"

func TestCrop_RoundTripsAndKeepsNotRecordedApart(t *testing.T) {
	var none Crop
	if none.Recorded() || none.Encode() != "" || none.String() != "not recorded" {
		t.Fatalf("the zero record is %q / %q", none.Encode(), none)
	}
	applied := RecordCrop(CropRecord{Applied: true, Rect: "1920:800:0:140", Frame: "1920x1080"})
	refused := RecordCrop(CropRecord{Reason: "samples-disagree", Detail: "a mixed aspect ratio"})
	for _, c := range []Crop{applied, refused} {
		back := ParseCrop(c.Encode())
		if !back.Recorded() || back.Record() != c.Record() {
			t.Errorf("%q read back as %+v", c.Encode(), back.Record())
		}
	}
	if applied.String() != "1920:800:0:140 of 1920x1080" || refused.String() != "not cropped (samples-disagree)" {
		t.Errorf("rendered %q and %q", applied, refused)
	}
	for _, bad := range []string{"", "  ", "{", `{"applied":true}`, `{"applied":false}`,
		`{"applied":false,"rect":"1:1:0:0","reason":"x"}`, `[]`} {
		if ParseCrop(bad).Recorded() {
			t.Errorf("%q parsed as a record", bad)
		}
	}
}
