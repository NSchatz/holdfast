package crop

import (
	"math"
	"strings"
	"testing"
)

// letterboxBox is what cropdetect reports for a frame of width w whose picture spans rows
// top..bottom inclusive: the research's 1920x1080 frame with a 1920x800 picture padded 140 px
// top and bottom reports x1:0 x2:1919 y1:140 y2:939 (research-streams-hdr.md section 4.2).
func letterboxBox(w, top, bottom int) Box { return Box{X1: 0, X2: w - 1, Y1: top, Y2: bottom} }

// allBlack is the box an all-black sample reports: cropdetect starts from x1=w-1, x2=0, y1=h-1,
// y2=0 and finds nothing to move them, which it prints as the NEGATIVE crop the research saw
// (crop=-1918:-1078:1920:1080 on a 1920x1080 frame).
func allBlack(w, h int) Box { return Box{X1: w - 1, X2: 0, Y1: h - 1, Y2: 0} }

func repeat(b Box, n int) []Box {
	out := make([]Box, n)
	for i := range out {
		out[i] = b
	}
	return out
}

var hd = Frame{W: 1920, H: 1080}

// TestDecide_TheResearchWorkedLetterbox is the consensus arithmetic checked against the
// research's worked result: a 1920x1080 frame with 140 px bars crops to 1920:800:0:140.
func TestDecide_TheResearchWorkedLetterbox(t *testing.T) {
	c := Agree(repeat(letterboxBox(1920, 140, 939), Samples), hd)
	if c.Reason != "" {
		t.Fatalf("ten agreeing samples refused: %s (%s)", c.Reason, c.Detail)
	}
	if want := (Edges{Top: 140, Bottom: 140}); c.Edges != want {
		t.Fatalf("consensus edges %v, want %v", c.Edges, want)
	}
	d := Decide(Inputs{Frame: hd, PixelFormats: []string{"yuv420p", "yuv420p10le"}, Consensus: c})
	if !d.Applied() || d.Rect.String() != "1920:800:0:140" {
		t.Fatalf("decision %v (%s: %s), want the rectangle 1920:800:0:140", d.Rect, d.Reason, d.Detail)
	}
	if got := d.Rect.Spec(); got != "crop=1920:800:0:140:exact=1" {
		t.Errorf("filter expression %q", got)
	}
	if d.String() != "1920:800:0:140" {
		t.Errorf("recorded as %q", d.String())
	}
}

// TestAgree_AllBlackSamplesAreDiscarded: the negative crops all-black frames produce are
// invalid samples, discarded rather than read as a box; enough valid ones still crop, and too
// few refuse.
func TestAgree_AllBlackSamplesAreDiscarded(t *testing.T) {
	boxes := append(repeat(allBlack(1920, 1080), 4), repeat(letterboxBox(1920, 140, 939), 6)...)
	c := Agree(boxes, hd)
	if c.Reason != "" || c.Valid != 6 || c.Discarded != 4 || c.Edges.Top != 140 {
		t.Fatalf("4 black + 6 letterbox samples: %+v", c)
	}
	few := append(repeat(allBlack(1920, 1080), 8), repeat(letterboxBox(1920, 140, 939), 2)...)
	if c := Agree(few, hd); c.Reason != ReasonTooFewSamples {
		t.Fatalf("2 valid of 10 gave %+v, want %s", c, ReasonTooFewSamples)
	}
	if c := Agree(repeat(letterboxBox(1920, 140, 939), MinValidSamples), hd); c.Reason != "" {
		t.Fatalf("exactly %d valid samples refused: %+v", MinValidSamples, c)
	}
	if c := Agree(repeat(letterboxBox(1920, 140, 939), MinValidSamples-1), hd); c.Reason != ReasonTooFewSamples {
		t.Fatalf("%d valid samples gave %+v", MinValidSamples-1, c)
	}
}

// TestAgree_ImplausibleAndOutOfFrameSamplesAreDiscarded: a sample removing more than a quarter
// of the frame on a side (HandBrake's rule), or reporting a box outside the frame, is not a
// sample.
func TestAgree_ImplausibleAndOutOfFrameSamplesAreDiscarded(t *testing.T) {
	cases := map[string]Box{
		"top over a quarter":    {X1: 0, X2: 1919, Y1: 271, Y2: 939},
		"bottom over a quarter": {X1: 0, X2: 1919, Y1: 140, Y2: 1080 - 1 - 271},
		"left over a quarter":   {X1: 481, X2: 1919, Y1: 0, Y2: 1079},
		"right over a quarter":  {X1: 0, X2: 1919 - 481, Y1: 0, Y2: 1079},
		"outside the frame":     {X1: 0, X2: 1920, Y1: 0, Y2: 1079},
		"negative offset":       {X1: -1, X2: 1919, Y1: 0, Y2: 1079},
	}
	for name, b := range cases {
		if _, ok := edgesOfBox(b, hd); ok {
			t.Errorf("%s: accepted as a valid sample", name)
		}
	}
	// Exactly a quarter is plausible: the bound is "more than".
	if _, ok := edgesOfBox(Box{X1: 0, X2: 1919, Y1: 270, Y2: 1079 - 270}, hd); !ok {
		t.Error("a sample removing exactly a quarter top and bottom was discarded")
	}
}

// TestAgree_DisagreeingSamplesRefuse: a mixed aspect ratio - some samples letterboxed, some
// not - is a disagreement beyond the tolerance, and no crop; samples within it agree on the
// loose (minimum per side) consensus.
func TestAgree_DisagreeingSamplesRefuse(t *testing.T) {
	mixed := append(repeat(letterboxBox(1920, 140, 939), 5), repeat(letterboxBox(1920, 0, 1079), 5)...)
	if c := Agree(mixed, hd); c.Reason != ReasonSamplesDisagree {
		t.Fatalf("mixed aspect ratio gave %+v, want %s", c, ReasonSamplesDisagree)
	}
	near := []Box{letterboxBox(1920, 140, 939), letterboxBox(1920, 140-TolerancePx, 939), letterboxBox(1920, 140, 939+TolerancePx)}
	c := Agree(near, hd)
	if c.Reason != "" || c.Edges.Top != 140-TolerancePx || c.Edges.Bottom != 140-TolerancePx {
		t.Fatalf("samples within the tolerance gave %+v, want the minimum per side", c)
	}
	over := []Box{letterboxBox(1920, 140, 939), letterboxBox(1920, 140-TolerancePx-1, 939), letterboxBox(1920, 140, 939)}
	if c := Agree(over, hd); c.Reason != ReasonSamplesDisagree {
		t.Fatalf("one pixel past the tolerance gave %+v", c)
	}
	// Every side is held to it, not only the rows.
	for _, side := range []Box{
		{X1: 0, X2: 1919 - TolerancePx - 1, Y1: 140, Y2: 939},
		{X1: TolerancePx + 1, X2: 1919, Y1: 140, Y2: 939},
		{X1: 0, X2: 1919, Y1: 140, Y2: 939 - TolerancePx - 1},
	} {
		if c := Agree([]Box{letterboxBox(1920, 140, 939), letterboxBox(1920, 140, 939), side}, hd); c.Reason != ReasonSamplesDisagree {
			t.Errorf("a sample %+v past the tolerance on one side gave %+v", side, c)
		}
	}
}

// TestDecide_AlignsOutwardToTheChromaSubsampling: odd edges are rounded so the rectangle keeps
// MORE, never less, and to the subsampling of every format the picture passes through.
func TestDecide_AlignsOutwardToTheChromaSubsampling(t *testing.T) {
	f := Frame{W: 320, H: 240}
	c := Consensus{Edges: Edges{Left: 3, Right: 5, Top: 41, Bottom: 39}, Samples: 10, Valid: 10}
	cases := []struct {
		fmts []string
		want string
	}{
		{[]string{"yuv420p"}, "314:162:2:40"},
		{[]string{"yuv422p10le"}, "314:160:2:41"},
		{[]string{"yuv444p"}, "312:160:3:41"},
		// The coarsest wins: a 4:4:4 source encoded to 4:2:0 aligns for 4:2:0.
		{[]string{"yuv444p", "yuv420p10le"}, "314:162:2:40"},
		{[]string{"p010le"}, "314:162:2:40"},
	}
	for _, tc := range cases {
		d := Decide(Inputs{Frame: f, PixelFormats: tc.fmts, Consensus: c})
		if !d.Applied() || d.Rect.String() != tc.want {
			t.Errorf("%v: %v (%s), want %s", tc.fmts, d.Rect, d.Reason, tc.want)
			continue
		}
		e := EdgesOf(d.Rect, f)
		if e.Left > c.Edges.Left || e.Right > c.Edges.Right || e.Top > c.Edges.Top || e.Bottom > c.Edges.Bottom {
			t.Errorf("%v: the aligned rectangle removes %v, more than the consensus %v on some side", tc.fmts, e, c.Edges)
		}
	}
}

// TestDecide_RefusesWhatItCannotAnswer: every refusal is named, and none is a rectangle.
func TestDecide_RefusesWhatItCannotAnswer(t *testing.T) {
	good := Agree(repeat(letterboxBox(1920, 140, 939), Samples), hd)
	cases := []struct {
		name string
		in   Inputs
		want string
	}{
		{"unknown frame", Inputs{Frame: Frame{}, PixelFormats: []string{"yuv420p"}, Consensus: good}, ReasonUnknownFrame},
		{"unknown pixel format", Inputs{Frame: hd, PixelFormats: []string{"rgb24"}, Consensus: good}, ReasonUnknownPixelFormat},
		{"no pixel format", Inputs{Frame: hd, Consensus: good}, ReasonUnknownPixelFormat},
		{"consensus refused", Inputs{Frame: hd, PixelFormats: []string{"yuv420p"},
			Consensus: Consensus{Reason: ReasonSamplesDisagree, Detail: "x"}}, ReasonSamplesDisagree},
		{"no bars", Inputs{Frame: hd, PixelFormats: []string{"yuv420p"}, Consensus: Consensus{Valid: 10}}, ReasonNoBars},
		{"bars round away", Inputs{Frame: Frame{W: 320, H: 240}, PixelFormats: []string{"yuv420p"},
			Consensus: Consensus{Edges: Edges{Top: 1}}}, ReasonNoBars},
		{"odd frame cannot align", Inputs{Frame: Frame{W: 321, H: 240}, PixelFormats: []string{"yuv420p"},
			Consensus: Consensus{Edges: Edges{Top: 40, Bottom: 40, Left: 1}}}, ReasonUnaligned},
	}
	for _, tc := range cases {
		d := Decide(tc.in)
		if d.Applied() || d.Reason != tc.want || !d.Rect.Empty() {
			t.Errorf("%s: %+v, want refused %s", tc.name, d, tc.want)
		}
		if d.Detail == "" {
			t.Errorf("%s: refused with no detail", tc.name)
		}
		if !strings.HasPrefix(d.String(), "not cropped (") {
			t.Errorf("%s: recorded as %q", tc.name, d.String())
		}
	}
	for _, r := range []string{ReasonNoBars, ReasonTooFewSamples, ReasonSamplesDisagree, ReasonUnknownFrame,
		ReasonUnknownPixelFormat, ReasonUnaligned, ReasonDolbyVision, ReasonBarsNotBlack, ReasonDetectFailed, ReasonRemuxOnly} {
		found := false
		for _, v := range Reasons {
			found = found || v == r
		}
		if !found {
			t.Errorf("%s is missing from Reasons", r)
		}
	}
}

// TestDecide_ADolbyVisionSourceIsRefusedWhateverItsPicture is I7, then phase 1 of P5: a crop
// on a Dolby Vision source is refused, from the source's own probe strings, even when every
// sample agrees on a perfect letterbox - independently of the engine's DV guard.
func TestDecide_ADolbyVisionSourceIsRefusedWhateverItsPicture(t *testing.T) {
	good := Agree(repeat(letterboxBox(1920, 140, 939), Samples), hd)
	dv := []struct{ tag, side, trc string }{
		{"dvh1", "", "smpte2084"},
		{"dvhe", "", ""},
		{"DAV1", "", ""},
		{"hev1", "[SIDE_DATA]\nside_data_type=DOVI configuration record\n[/SIDE_DATA]", "smpte2084"},
		{"", "side_data_type=Dolby Vision Metadata", ""},
	}
	for _, s := range dv {
		in := Inputs{Frame: hd, PixelFormats: []string{"yuv420p10le"}, Consensus: good,
			DolbyVision: DolbyVisionOf(s.tag, s.side, s.trc)}
		d := Decide(in)
		if d.Applied() || d.Reason != ReasonDolbyVision {
			t.Errorf("DV source %+v: %+v, want refused %s", s, d, ReasonDolbyVision)
		}
		// The phase-2 seam: an L5 handed in does not, in this build, license a crop.
		in.DolbyVision.L5 = &Edges{Top: 140, Bottom: 140}
		if d := Decide(in); d.Applied() {
			t.Errorf("DV source %+v with an L5 was cropped in phase 1: %+v", s, d)
		}
	}
	// HDR10 and HDR10+ are not Dolby Vision, and their crop is decided on the picture.
	for _, s := range []struct{ tag, side, trc string }{{"hev1", "", "smpte2084"}, {"", "side_data_type=HDR Dynamic Metadata SMPTE2094-40 (HDR10+)", ""}} {
		d := Decide(Inputs{Frame: hd, PixelFormats: []string{"yuv420p10le"}, Consensus: good, DolbyVision: DolbyVisionOf(s.tag, s.side, s.trc)})
		if !d.Applied() {
			t.Errorf("non-DV source %+v refused: %+v", s, d)
		}
	}
}

func TestParseBox(t *testing.T) {
	log := "[Parsed_cropdetect_0 @ 0x1] x1:0 x2:319 y1:41 y2:199 w:320 h:160 x:0 y:40 pts:0 t:0.000000 limit:0.094118 crop=320:160:0:40\n" +
		"[Parsed_cropdetect_0 @ 0x1] x1:0 x2:319 y1:40 y2:199 w:320 h:160 x:0 y:40 pts:42 t:0.042000 limit:0.094118 crop=320:160:0:40\n" +
		"[out#0/null @ 0x2] video:1KiB"
	b, ok := ParseBox(log)
	if !ok || b != (Box{X1: 0, X2: 319, Y1: 40, Y2: 199}) {
		t.Fatalf("ParseBox = %+v, %v; want the LAST line's box", b, ok)
	}
	if _, ok := ParseBox("nothing here"); ok {
		t.Error("a log with no cropdetect line parsed")
	}
	if _, ok := ParseBox("x1:a x2:319 y1:40 y2:199 crop=1:1:0:0"); ok {
		t.Error("a non-numeric box parsed")
	}
	if _, ok := ParseBox(" x1:0 x2:319 crop=1:1:0:0"); ok {
		t.Error("a box missing two coordinates parsed")
	}
	// The all-black sample's negative crop parses, and is then invalid.
	b, ok = ParseBox(" x1:319 x2:0 y1:239 y2:0 w:-318 h:-238 x:320 y:240 crop=-318:-238:320:240")
	if !ok {
		t.Fatal("the negative crop line did not parse")
	}
	if _, valid := edgesOfBox(b, Frame{W: 320, H: 240}); valid {
		t.Error("the negative crop of an all-black sample was a valid sample")
	}
}

func TestSamplePositions(t *testing.T) {
	at := SamplePositions(10)
	if len(at) != Samples {
		t.Fatalf("%d positions", len(at))
	}
	for i, v := range at {
		if v < 10*EdgePercent/100.0 || v > 10-10*EdgePercent/100.0 {
			t.Errorf("position %d at %.3f s lies in the skipped first or last %d%%", i, v, EdgePercent)
		}
		if i > 0 && v <= at[i-1] {
			t.Errorf("positions not increasing: %v", at)
		}
	}
	if math.Abs(at[0]-0.95) > 1e-9 || math.Abs(at[Samples-1]-9.05) > 1e-9 {
		t.Errorf("positions %v, want 0.95 .. 9.05 for a 10 s file", at)
	}
	if SamplePositions(0) != nil || SamplePositions(-1) != nil {
		t.Error("an unknown duration produced positions")
	}
}

func TestBandsCoverExactlyTheRemovedArea(t *testing.T) {
	f := Frame{W: 320, H: 240}
	r := Rect{W: 300, H: 160, X: 8, Y: 40}
	bands := Bands(r, f)
	area := 0
	for _, b := range bands {
		if !b.Rect.Inside(f) {
			t.Errorf("band %+v outside the frame", b)
		}
		area += b.Rect.W * b.Rect.H
	}
	if want := f.W*f.H - r.W*r.H; area != want {
		t.Errorf("bands cover %d px, the crop removes %d", area, want)
	}
	if len(bands) != 4 {
		t.Errorf("%d bands, want 4: %+v", len(bands), bands)
	}
	if len(Bands(Rect{W: 320, H: 240}, f)) != 0 {
		t.Error("a crop removing nothing has bands")
	}
	// The guard leaves out the rows next to the picture, on the picture's side of each band.
	top, _ := Band{"top", Rect{W: 320, H: 40}}.inner(GuardPx)
	bottom, _ := Band{"bottom", Rect{W: 320, H: 40, Y: 200}}.inner(GuardPx)
	left, _ := Band{"left", Rect{W: 8, H: 160, Y: 40}}.inner(GuardPx)
	right, _ := Band{"right", Rect{W: 12, H: 160, X: 308, Y: 40}}.inner(GuardPx)
	if top != (Rect{W: 320, H: 36}) || bottom != (Rect{W: 320, H: 36, Y: 204}) ||
		left != (Rect{W: 4, H: 160, Y: 40}) || right != (Rect{W: 8, H: 160, X: 312, Y: 40}) {
		t.Errorf("inner regions top %v bottom %v left %v right %v", top, bottom, left, right)
	}
	if _, ok := (Band{"top", Rect{W: 320, H: GuardPx}}).inner(GuardPx); ok {
		t.Error("a band no thicker than the guard has an inner region")
	}
	if _, ok := (Band{"left", Rect{W: GuardPx, H: 10}}).inner(GuardPx); ok {
		t.Error("a band no wider than the guard has an inner region")
	}
}

func TestRegionsScaleTheirBoundsByBitDepth(t *testing.T) {
	f := Frame{W: 320, H: 240}
	r := Rect{W: 320, H: 160, Y: 40}
	for _, tc := range []struct{ depth, avg, max int }{{8, 24, 48}, {10, 96, 192}, {12, 384, 768}} {
		regs := regionsOf(r, f, tc.depth, GuardPx)
		if len(regs) != 4 {
			t.Fatalf("%d-bit: %d regions", tc.depth, len(regs))
		}
		for _, rg := range regs {
			want := float64(tc.avg)
			if rg.stat == "YMAX" {
				want = float64(tc.max)
			}
			if rg.bound != want {
				t.Errorf("%d-bit %s bound %g, want %g", tc.depth, rg.name, rg.bound, want)
			}
		}
	}
}

func TestJudge(t *testing.T) {
	regs := regionsOf(Rect{W: 320, H: 160, Y: 40}, Frame{W: 320, H: 240}, 8, GuardPx)
	line := func(name, stat, v string) string {
		return "[metadata@" + name + " @ 0x55] lavfi.signalstats." + stat + "=" + v + "\n"
	}
	frame := func(top, topMax string) string {
		return "[metadata@top_avg @ 0x55] frame:0 pts:0\n" + line("top_avg", "YAVG", top) + line("top_max", "YMAX", topMax) +
			line("bottom_avg", "YAVG", "16") + line("bottom_max", "YMAX", "16")
	}
	if err := judge(frame("16", "20")+frame("16.5", "48"), regs); err != nil {
		t.Fatalf("black bands refused: %v", err)
	}
	err := judge(frame("16", "20")+frame("16", "49"), regs)
	nb, ok := err.(*NotBlackError)
	if !ok || nb.Side != "top" || nb.Frame != 1 || nb.Stat != "YMAX" || nb.Value != 49 {
		t.Fatalf("a bright pixel passed or was misreported: %v", err)
	}
	if err := judge(frame("24.5", "20"), regs); err == nil {
		t.Fatal("a grey band passed")
	}
	if err := judge(frame("16", "20")+line("top_avg", "YAVG", "16"), regs); err == nil {
		t.Fatal("a band measured on more frames than another passed")
	}
	if err := judge("", regs); err == nil {
		t.Fatal("an empty log passed")
	}
	if err := judge(line("top_avg", "YAVG", "x"), regs); err == nil {
		t.Fatal("an unreadable figure passed")
	}
	if !strings.Contains((&NotBlackError{Side: "bottom", Frame: 3, Stat: "YMAX", Value: 235, Bound: 48}).Error(), "bottom band") {
		t.Error("the refusal does not name the band")
	}
}

// TestEdgesOfBox_EveryBoundIsHeld: each edge of the frame refuses a box past it, and a box
// touching every edge is the whole frame removing nothing.
func TestEdgesOfBox_EveryBoundIsHeld(t *testing.T) {
	f := Frame{W: 320, H: 240}
	for name, b := range map[string]Box{
		"x1 negative": {X1: -1, X2: 319, Y1: 0, Y2: 239},
		"y1 negative": {X1: 0, X2: 319, Y1: -1, Y2: 239},
		"x2 past":     {X1: 0, X2: 320, Y1: 0, Y2: 239},
		"y2 past":     {X1: 0, X2: 319, Y1: 0, Y2: 240},
		"x1 after x2": {X1: 10, X2: 9, Y1: 0, Y2: 239},
		"y1 after y2": {X1: 0, X2: 319, Y1: 10, Y2: 9},
		"left over":   {X1: 81, X2: 319, Y1: 0, Y2: 239},
		"top over":    {X1: 0, X2: 319, Y1: 61, Y2: 239},
		"bottom over": {X1: 0, X2: 319, Y1: 0, Y2: 239 - 61},
		"right over":  {X1: 0, X2: 319 - 81, Y1: 0, Y2: 239},
	} {
		if _, ok := edgesOfBox(b, f); ok {
			t.Errorf("%s: %+v accepted", name, b)
		}
	}
	e, ok := edgesOfBox(Box{X1: 0, X2: 319, Y1: 0, Y2: 239}, f)
	if !ok || !e.None() {
		t.Errorf("the whole frame gave %v, %v", e, ok)
	}
	e, ok = edgesOfBox(Box{X1: 80, X2: 319 - 80, Y1: 60, Y2: 239 - 60}, f)
	if !ok || e != (Edges{Left: 80, Right: 80, Top: 60, Bottom: 60}) {
		t.Errorf("a box at exactly a quarter on every side gave %v, %v", e, ok)
	}
	if got := EdgesOf(Rect{W: 300, H: 160, X: 8, Y: 40}, f); got != (Edges{Left: 8, Right: 12, Top: 40, Bottom: 40}) {
		t.Errorf("EdgesOf = %v", got)
	}
	if (Edges{Top: 1}).String() != "left 0, right 0, top 1, bottom 0" {
		t.Errorf("Edges.String = %q", Edges{Top: 1})
	}
}

func TestRectAndFrame(t *testing.T) {
	f := Frame{W: 320, H: 240}
	if !f.Known() || (Frame{W: 320}).Known() || (Frame{H: 240}).Known() {
		t.Error("Frame.Known is wrong")
	}
	var zero Rect
	if !zero.Empty() || zero.String() != "" || zero.Spec() != "" || zero.Inside(f) {
		t.Error("the zero rectangle is not empty everywhere")
	}
	for _, r := range []Rect{{W: 321, H: 240}, {W: 320, H: 241}, {W: 10, H: 10, X: -1}, {W: 10, H: 10, Y: -1},
		{W: 0, H: 10}, {W: 10, H: 0}, {W: 20, H: 10, X: 301}, {W: 10, H: 20, Y: 221}} {
		if r.Inside(f) {
			t.Errorf("%+v is inside %v", r, f)
		}
	}
	if !(Rect{W: 320, H: 240}).Inside(f) || !(Rect{W: 20, H: 20, X: 300, Y: 220}).Inside(f) {
		t.Error("a rectangle touching the frame's edges is not inside it")
	}
	d := Decision{Rect: Rect{W: 2, H: 2}}
	if !d.Applied() || (Decision{Rect: Rect{W: 2, H: 2}, Reason: ReasonNoBars}).Applied() || (Decision{}).Applied() {
		t.Error("Decision.Applied is wrong")
	}
}

func TestTail(t *testing.T) {
	if tail("  short  ") != "short" {
		t.Error("tail trims")
	}
	long := strings.Repeat("a", 400)
	if tail(long) != long {
		t.Error("400 characters were cut")
	}
	if got := tail(long + "bc"); got != "..."+long[2:]+"bc" || len(got) != 403 {
		t.Errorf("tail of 402 characters is %d long", len(got))
	}
}

func TestMetadataLine(t *testing.T) {
	name, v, ok := metadataLine("[metadata@top_max @ 0x5555] lavfi.signalstats.YMAX=149")
	if !ok || name != "top_max" || v != "149" {
		t.Errorf("parsed %q %q %v", name, v, ok)
	}
	for _, bad := range []string{"", "[metadata@top_max] frame:0", "[Parsed_crop_1 @ 0x1] lavfi.signalstats.YMAX=1",
		"[metadata@x", "[metadata@x @ 0x1] lavfi.signalstats.YMAX"} {
		if _, _, ok := metadataLine(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestBlacknessArgs(t *testing.T) {
	regs := regionsOf(Rect{W: 320, H: 160, Y: 40}, Frame{W: 320, H: 240}, 8, GuardPx)
	args := BlacknessArgs("/m/src.mkv", regs)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-i /m/src.mkv", "split=4[s0][s1][s2][s3]",
		"[s0]crop=320:40:0:0:exact=1,signalstats,metadata@top_avg=mode=print:key=lavfi.signalstats.YAVG[o0]",
		"[s1]crop=320:36:0:0:exact=1,signalstats,metadata@top_max=mode=print:key=lavfi.signalstats.YMAX[o1]",
		"[s3]crop=320:36:0:204:exact=1", "-map [o3] -f null -"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the blackness command line lacks %q:\n%s", want, joined)
		}
	}
	if n := strings.Count(joined, "-f null -"); n != 4 {
		t.Errorf("%d null outputs for 4 regions", n)
	}
	sample := strings.Join(SampleArgs("/m/src.mkv", 1.25, Limit), " ")
	for _, want := range []string{"-ss 1.250 -i /m/src.mkv", "-frames:v 4",
		"cropdetect=limit=0.0941176:round=2:skip=0:reset=0", "-f null -"} {
		if !strings.Contains(sample, want) {
			t.Errorf("the sample command line lacks %q:\n%s", want, sample)
		}
	}
}
