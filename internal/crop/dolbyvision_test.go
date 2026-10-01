package crop

import (
	"strings"
	"testing"
)

func uniformL5(frames int, e Edges) *L5Reading {
	r := &L5Reading{Frames: frames, L5: map[int]Edges{}}
	for i := 0; i < frames; i++ {
		r.L5[i] = e
	}
	return r
}

// The fixture frame: 320x240 with 40 px bars, and the consensus the 10-bit fixture measures.
var (
	dvFrame = Frame{W: 320, H: 240}
	bars40  = Consensus{Edges: Edges{Top: 40, Bottom: 40}, Samples: 10, Valid: 10}
)

func dvIn(r *L5Reading, c Consensus) Inputs {
	return Inputs{Frame: dvFrame, PixelFormats: []string{"yuv420p10le", "yuv420p10le"}, Consensus: c,
		DolbyVision: DolbyVision{Present: true, L5: r}}
}

// TestDecideDolbyVision_CropsOnlyToAnAgreeingL5 is P5 option (c) as a table: the rectangle is
// L5's own where every frame carries one identical, non-zero, aligned L5 that agrees with the
// picture within L5TolerancePx; every other case is a named refusal and no crop.
func TestDecideDolbyVision_CropsOnlyToAnAgreeingL5(t *testing.T) {
	l40 := Edges{Top: 40, Bottom: 40}
	varying := uniformL5(24, l40)
	for i := 12; i < 24; i++ {
		varying.L5[i] = Edges{Top: 20, Bottom: 20}
	}
	missing := uniformL5(24, l40)
	delete(missing.L5, 7)
	cases := []struct {
		name string
		in   Inputs
		want string
	}{
		{"no L5 read", dvIn(nil, bars40), ReasonDolbyVision},
		{"dovi_tool failed", dvIn(&L5Reading{Failed: "exit status 1"}, bars40), ReasonL5Unreadable},
		{"variable frame rate", dvIn(&L5Reading{Frames: 24, L5: uniformL5(24, l40).L5, FrameRate: "24/1 vs 16/1"}, bars40), ReasonL5FrameRate},
		{"L5 zero with bars", dvIn(uniformL5(24, Edges{}), bars40), ReasonL5Absent},
		{"L5 absent on every frame", dvIn(&L5Reading{Frames: 24, L5: map[int]Edges{}}, bars40), ReasonL5Absent},
		{"L5 absent on one frame", dvIn(missing, bars40), ReasonL5Absent},
		{"no frames", dvIn(&L5Reading{L5: map[int]Edges{0: l40}}, bars40), ReasonL5Absent},
		{"L5 varying 0-11 vs 12-23", dvIn(varying, bars40), ReasonL5Varies},
		{"odd 41/39", dvIn(uniformL5(24, Edges{Top: 41, Bottom: 39}), Consensus{Edges: Edges{Top: 41, Bottom: 39}}), ReasonL5Odd},
		{"odd left", dvIn(uniformL5(24, Edges{Left: 3, Right: 3, Top: 40, Bottom: 40}), Consensus{Edges: Edges{Left: 3, Right: 3, Top: 40, Bottom: 40}}), ReasonL5Odd},
		{"20/20 over 40 px bars", dvIn(uniformL5(24, Edges{Top: 20, Bottom: 20}), bars40), ReasonL5Disagrees},
		{"just past the tolerance", dvIn(uniformL5(24, l40), Consensus{Edges: Edges{Top: 40 - L5TolerancePx - 1, Bottom: 40}}), ReasonL5Disagrees},
		{"right past the tolerance", dvIn(uniformL5(24, Edges{Left: 8, Right: 8, Top: 40, Bottom: 40}), Consensus{Edges: Edges{Left: 8, Right: 8 + L5TolerancePx + 1, Top: 40, Bottom: 40}}), ReasonL5Disagrees},
		{"L5 past the frame", dvIn(uniformL5(24, Edges{Top: 120, Bottom: 120}), Consensus{Edges: Edges{Top: 120, Bottom: 120}}), ReasonL5Disagrees},
		{"samples disagree", dvIn(uniformL5(24, l40), Consensus{Reason: ReasonSamplesDisagree, Detail: "x"}), ReasonSamplesDisagree},
		{"unknown frame", Inputs{PixelFormats: []string{"yuv420p10le"}, Consensus: bars40, DolbyVision: DolbyVision{Present: true, L5: uniformL5(24, l40)}}, ReasonUnknownFrame},
		{"unknown pixel format", Inputs{Frame: dvFrame, PixelFormats: []string{"rgb48"}, Consensus: bars40, DolbyVision: DolbyVision{Present: true, L5: uniformL5(24, l40)}}, ReasonUnknownPixelFormat},
		{"no pixel format", Inputs{Frame: dvFrame, Consensus: bars40, DolbyVision: DolbyVision{Present: true, L5: uniformL5(24, l40)}}, ReasonUnknownPixelFormat},
	}
	for _, tc := range cases {
		d := Decide(tc.in)
		if d.Applied() || d.ZeroesL5() || d.Reason != tc.want || d.Detail == "" {
			t.Errorf("%s: %+v, want refused %s with a detail", tc.name, d, tc.want)
		}
	}
	d := Decide(dvIn(uniformL5(24, l40), bars40))
	if !d.ZeroesL5() || d.Rect.String() != "320:160:0:40" {
		t.Fatalf("L5 40/40 over 40 px bars decided %+v, want 320:160:0:40 from L5", d)
	}
	// The rectangle is L5's, not the consensus': within the tolerance, the picture's reading
	// does not move it.
	for _, c := range []Edges{{Top: 38, Bottom: 42}, {Top: 39, Bottom: 39}, {Top: 42, Bottom: 38, Left: 2, Right: 2}} {
		d := Decide(dvIn(uniformL5(24, l40), Consensus{Edges: c}))
		if !d.ZeroesL5() || d.Rect.String() != "320:160:0:40" {
			t.Errorf("consensus %v within the tolerance decided %+v, want L5's 320:160:0:40", c, d)
		}
	}
	// A 4:2:2 picture cuts rows at any offset, so 41/39 is not odd there.
	in := dvIn(uniformL5(24, Edges{Top: 41, Bottom: 39}), Consensus{Edges: Edges{Top: 41, Bottom: 39}})
	in.PixelFormats = []string{"yuv422p10le"}
	if d := Decide(in); !d.ZeroesL5() || d.Rect.String() != "320:160:0:41" {
		t.Errorf("4:2:2 41/39 decided %+v", d)
	}
	// A non-DV decision never asks for L5 to be zeroed.
	if d := Decide(Inputs{Frame: dvFrame, PixelFormats: []string{"yuv420p"}, Consensus: bars40}); !d.Applied() || d.ZeroesL5() {
		t.Errorf("a non-DV crop %+v", d)
	}
	for _, r := range []string{ReasonL5Unreadable, ReasonL5FrameRate, ReasonL5Absent, ReasonL5Varies, ReasonL5Odd,
		ReasonL5Disagrees, ReasonL5ZeroingFailed, ReasonL5GateFailed} {
		if !strings.HasPrefix(r, "dolby-vision-") {
			t.Errorf("%s does not say it is about Dolby Vision", r)
		}
		found := false
		for _, v := range Reasons {
			found = found || v == r
		}
		if !found {
			t.Errorf("%s is missing from Reasons", r)
		}
	}
}

// TestDecideDolbyVision_TheFixtureConsensusAgreesWithL5 is the tolerance's calibration: the
// 10-bit fixture's consensus reads one row of ringing into each bar (39/39) where L5 says 40,
// which the 2 px tolerance admits and a 0 px one would not.
func TestDecideDolbyVision_TheFixtureConsensusAgreesWithL5(t *testing.T) {
	d := Decide(dvIn(uniformL5(24, Edges{Top: 40, Bottom: 40}), Consensus{Edges: Edges{Top: 39, Bottom: 39}}))
	if !d.ZeroesL5() || d.Rect.String() != "320:160:0:40" {
		t.Fatalf("the fixture's consensus decided %+v", d)
	}
	if absDiff(3, 5) != 2 || absDiff(5, 3) != 2 || absDiff(4, 4) != 0 {
		t.Error("absDiff")
	}
	if _, _, ok := alignmentOf(nil); ok {
		t.Error("no pixel formats aligned")
	}
}
