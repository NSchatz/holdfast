package hdr

import (
	"reflect"
	"strings"
	"testing"
)

// hdr10Flat is a source's side data as ffprobe prints it (flat=s=.): the frame-level
// mastering-display and content-light blocks libx265 writes for
// master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1) and
// max-cll=1000,400, then the same blocks at stream level with the fractions reduced, as the
// Matroska demuxer reports them.
const hdr10Flat = `frames.frame.0.side_data_list.side_data.0.side_data_type="Mastering display metadata"
frames.frame.0.side_data_list.side_data.0.red_x="34000/50000"
frames.frame.0.side_data_list.side_data.0.red_y="16000/50000"
frames.frame.0.side_data_list.side_data.0.green_x="13250/50000"
frames.frame.0.side_data_list.side_data.0.green_y="34500/50000"
frames.frame.0.side_data_list.side_data.0.blue_x="7500/50000"
frames.frame.0.side_data_list.side_data.0.blue_y="3000/50000"
frames.frame.0.side_data_list.side_data.0.white_point_x="15635/50000"
frames.frame.0.side_data_list.side_data.0.white_point_y="16450/50000"
frames.frame.0.side_data_list.side_data.0.min_luminance="1/10000"
frames.frame.0.side_data_list.side_data.0.max_luminance="10000000/10000"
frames.frame.0.side_data_list.side_data.1.side_data_type="Content light level metadata"
frames.frame.0.side_data_list.side_data.1.max_content=1000
frames.frame.0.side_data_list.side_data.1.max_average=400

streams.stream.0.side_data_list.side_data.0.side_data_type="Content light level metadata"
streams.stream.0.side_data_list.side_data.0.max_content=1000
streams.stream.0.side_data_list.side_data.0.max_average=400
streams.stream.0.side_data_list.side_data.1.side_data_type="Mastering display metadata"
streams.stream.0.side_data_list.side_data.1.red_x="17/25"
`

const (
	wantMD  = "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)"
	wantCLL = "1000,400"
)

func hdr10Color() Color {
	return DeriveColor("bt2020", "smpte2084", "bt2020nc", "tv", hdr10Flat)
}

// faithful is an output that carries exactly what an HDR10 plan declares.
func faithful() Observed {
	tags := Tags{Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc", Range: "tv"}
	return Observed{PixFmt: "yuv420p10le", Stream: tags, Frame: tags, SideData: hdr10Flat}
}

func TestFidelityOf_DeclaresThePlansFormatTagsAndTheSourcesBlocks(t *testing.T) {
	got := FidelityOf("yuv420p10le", hdr10Color(), hdr10Flat)
	want := Fidelity{Depth: 10, Chroma: "420",
		Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc", Range: "tv",
		Mastering:    Block{Present: true, Value: wantMD},
		ContentLight: Block{Present: true, Value: wantCLL}}
	if got != want {
		t.Fatalf("FidelityOf = %+v, want %+v", got, want)
	}

	// An SDR source with no side data declares its tags and no block; a plan format this
	// build cannot take apart leaves the depth at 0.
	sdr := FidelityOf("weird", Color{Primaries: "bt709"}, "")
	if sdr != (Fidelity{Primaries: "bt709"}) {
		t.Fatalf("FidelityOf(sdr) = %+v", sdr)
	}
	// A semi-planar plan format is taken apart too.
	if p := FidelityOf("p210le", Color{}, ""); p.Depth != 10 || p.Chroma != "422" {
		t.Fatalf("FidelityOf(p210le) = %+v", p)
	}
}

func TestFidelityCheck_AFaithfulOutputHasNoMismatch(t *testing.T) {
	f := FidelityOf("yuv420p10le", hdr10Color(), hdr10Flat)
	if m := f.Check(faithful()); len(m) != 0 {
		t.Fatalf("faithful output: %v", m)
	}
	// The decoded format of a semi-planar encode (p010le in, yuv420p10le out) and the plan's
	// semi-planar name compare by layout, not by spelling.
	f2 := FidelityOf("p010le", hdr10Color(), hdr10Flat)
	if m := f2.Check(faithful()); len(m) != 0 {
		t.Fatalf("p010le plan, yuv420p10le output: %v", m)
	}
}

// TestFidelityCheck_EachFieldRedsAlone loses exactly one field at a time and requires that
// exactly that field is named, with the declared and the observed values.
func TestFidelityCheck_EachFieldRedsAlone(t *testing.T) {
	f := FidelityOf("yuv420p10le", hdr10Color(), hdr10Flat)
	stripped := func(typ string) string {
		// Drop every line of the block of that type, at frame and at stream level.
		var keep []string
		skipIdx := map[string]bool{}
		for _, l := range strings.Split(hdr10Flat, "\n") {
			if strings.Contains(l, `side_data_type="`+typ+`"`) {
				skipIdx[l[:strings.Index(l, "side_data_type")]] = true
			}
		}
		for _, l := range strings.Split(hdr10Flat, "\n") {
			drop := false
			for prefix := range skipIdx {
				if strings.HasPrefix(l, prefix) {
					drop = true
				}
			}
			if !drop {
				keep = append(keep, l)
			}
		}
		return strings.Join(keep, "\n")
	}
	cases := []struct {
		name string
		lose func(*Observed)
		want Mismatch
	}{
		{"bit depth", func(o *Observed) { o.PixFmt = "yuv420p" },
			Mismatch{FieldBitDepth, "10-bit", "8-bit (yuv420p)"}},
		{"chroma", func(o *Observed) { o.PixFmt = "yuv422p10le" },
			Mismatch{FieldChroma, "4:2:0", "4:2:2 (yuv422p10le)"}},
		{"primaries contradicted at both levels", func(o *Observed) { o.Stream.Primaries, o.Frame.Primaries = "bt709", "bt709" },
			Mismatch{FieldPrimaries, "bt2020", "bt709"}},
		{"primaries contradicted in the bitstream only", func(o *Observed) { o.Frame.Primaries = "bt709" },
			Mismatch{FieldPrimaries, "bt2020", "stream bt2020, frame bt709"}},
		{"transfer lost at both levels", func(o *Observed) { o.Stream.Transfer, o.Frame.Transfer = "", "" },
			Mismatch{FieldTransfer, "smpte2084", "none"}},
		{"transfer contradicted in the container only", func(o *Observed) { o.Stream.Transfer = "bt709" },
			Mismatch{FieldTransfer, "smpte2084", "stream bt709, frame smpte2084"}},
		{"matrix contradicted in the bitstream, unsignalled in the container", func(o *Observed) { o.Stream.Matrix, o.Frame.Matrix = "", "bt709" },
			Mismatch{FieldMatrix, "bt2020nc", "stream none, frame bt709"}},
		{"range", func(o *Observed) { o.Stream.Range, o.Frame.Range = "pc", "pc" },
			Mismatch{FieldRange, "tv", "pc"}},
		{"mastering display", func(o *Observed) { o.SideData = stripped(masteringDisplayType) },
			Mismatch{FieldMastering, wantMD, "absent"}},
		{"content light", func(o *Observed) { o.SideData = stripped(contentLightType) },
			Mismatch{FieldContentLight, wantCLL, "absent"}},
		{"mastering display values changed", func(o *Observed) {
			o.SideData = strings.Replace(hdr10Flat, `max_luminance="10000000/10000"`, `max_luminance="4000000/10000"`, 1)
		}, Mismatch{FieldMastering, wantMD, "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(4000000,1)"}},
		{"content light values changed", func(o *Observed) {
			o.SideData = strings.Replace(hdr10Flat, "max_content=1000", "max_content=900", 1)
		}, Mismatch{FieldContentLight, wantCLL, "900,400"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := faithful()
			tc.lose(&o)
			got := f.Check(o)
			if !reflect.DeepEqual(got, []Mismatch{tc.want}) {
				t.Fatalf("Check = %v, want exactly [%v]", got, tc.want)
			}
		})
	}
}

func TestFidelityCheck_WhatCannotBeEstablishedRejects(t *testing.T) {
	f := FidelityOf("yuv420p10le", Color{}, "")

	// An output pix_fmt nobody can take apart: the depth is not established.
	if got := f.Check(Observed{PixFmt: ""}); !reflect.DeepEqual(got, []Mismatch{{FieldBitDepth, "10-bit", "none"}}) {
		t.Fatalf("empty output pix_fmt: %v", got)
	}
	if got := f.Check(Observed{PixFmt: "rgb24"}); !reflect.DeepEqual(got, []Mismatch{{FieldBitDepth, "10-bit", "rgb24"}}) {
		t.Fatalf("rgb24 output: %v", got)
	}

	// A plan format nobody can take apart: nothing can be shown to match it.
	bad := FidelityOf("gray", Color{}, "")
	if got := bad.Check(Observed{PixFmt: "yuv420p10le"}); len(got) != 1 || got[0].Field != FieldBitDepth {
		t.Fatalf("unparseable plan format: %v", got)
	}

	// A present block whose values could not be read on the source side cannot be shown
	// carried, even by an output that carries a block.
	unreadable := Fidelity{Depth: 10, Chroma: "420",
		Mastering: Block{Present: true}, ContentLight: Block{Present: true}}
	got := unreadable.Check(faithful())
	want := []Mismatch{
		{FieldMastering, "present but unreadable", wantMD},
		{FieldContentLight, "present but unreadable", wantCLL},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unreadable source blocks: %v, want %v", got, want)
	}
}

func TestFidelityCheck_ATagSignalledAtOneLevelOnlyIsCarried(t *testing.T) {
	f := FidelityOf("yuv420p10le", hdr10Color(), hdr10Flat)
	for _, o := range []Observed{
		// libx265 into Matroska from frames that carry no primaries or transfer: the
		// bitstream has them, the container does not.
		{PixFmt: "yuv420p10le", SideData: hdr10Flat,
			Stream: Tags{Matrix: "bt2020nc", Range: "tv"},
			Frame:  Tags{Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc", Range: "tv"}},
		// The other way round: the container has them, the bitstream does not.
		{PixFmt: "yuv420p10le", SideData: hdr10Flat,
			Stream: Tags{Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc", Range: "tv"}},
	} {
		if got := f.Check(o); len(got) != 0 {
			t.Fatalf("Check(%+v) = %v, want none", o, got)
		}
	}
}

func TestFidelityCheck_ATagOrBlockTheSourceNeverHadIsNotRequired(t *testing.T) {
	// A source that signals no tag and carries no block has nothing to lose: whatever the
	// output signals in their place is not a loss.
	f := FidelityOf("yuv420p10le", Color{}, "")
	tags := Tags{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", Range: "tv"}
	o := Observed{PixFmt: "yuv420p10le", Stream: tags, Frame: tags, SideData: hdr10Flat}
	if got := f.Check(o); len(got) != 0 {
		t.Fatalf("undeclared fields were compared: %v", got)
	}
}

func TestFidelityCheck_EveryLostFieldIsNamedInOrder(t *testing.T) {
	f := FidelityOf("yuv422p10le", hdr10Color(), hdr10Flat)
	got := f.Check(Observed{PixFmt: "yuv420p"})
	var fields []Field
	for _, m := range got {
		fields = append(fields, m.Field)
	}
	if !reflect.DeepEqual(fields, Fields) {
		t.Fatalf("fields = %v, want %v", fields, Fields)
	}
}

func TestMismatchAndBlock_String(t *testing.T) {
	if s := (Mismatch{FieldRange, "tv", "pc"}).String(); s != "range: want tv, got pc" {
		t.Fatalf("Mismatch.String = %q", s)
	}
	for in, want := range map[[2]string]string{
		{"", ""}:            "none",
		{"tv", "tv"}:        "tv",
		{"", "tv"}:          "stream none, frame tv",
		{"pc", ""}:          "stream pc, frame none",
		{"bt709", "bt2020"}: "stream bt709, frame bt2020",
	} {
		if got := levels(in[0], in[1]); got != want {
			t.Fatalf("levels(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
	for b, want := range map[Block]string{
		{}:                          "absent",
		{Present: true}:             "present but unreadable",
		{Present: true, Value: "x"}: "x",
	} {
		if got := b.String(); got != want {
			t.Fatalf("%+v.String() = %q, want %q", b, got, want)
		}
	}
}

func TestPixelLayout(t *testing.T) {
	for in, want := range map[string]PixFmt{
		"yuv420p":     {Chroma: "420", Depth: 8},
		"yuv420p10le": {Chroma: "420", Depth: 10, Endian: "le"},
		"yuv444p12le": {Chroma: "444", Depth: 12, Endian: "le"},
		"nv12":        {Chroma: "420", Depth: 8},
		"nv16":        {Chroma: "422", Depth: 8},
		"nv24":        {Chroma: "444", Depth: 8},
		"p010le":      {Chroma: "420", Depth: 10, Endian: "le"},
		"p012le":      {Chroma: "420", Depth: 12, Endian: "le"},
		"p016be":      {Chroma: "420", Depth: 16, Endian: "be"},
		"p210le":      {Chroma: "422", Depth: 10, Endian: "le"},
		"p412le":      {Chroma: "444", Depth: 12, Endian: "le"},
	} {
		got, ok := PixelLayout(in)
		if !ok || got != want {
			t.Errorf("PixelLayout(%q) = %+v, %v; want %+v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "gray", "rgb24", "nv21", "p110le", "p010", "p008le", "xnv12", "nv12x", "vaapi"} {
		if got, ok := PixelLayout(in); ok {
			t.Errorf("PixelLayout(%q) = %+v, ok; want refused", in, got)
		}
	}
}
