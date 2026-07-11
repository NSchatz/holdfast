package hdr

import (
	"context"
	"strings"
	"testing"
)

// fakeProber implements the unexported prober seam with canned values, so the
// prober-backed DeriveColorArgs/Classify branches can be unit-tested without ffmpeg.
type fakeProber struct {
	tag   string
	color map[string]string
	flat  string
}

func (f fakeProber) CodecTagString(_ context.Context, _ string) string { return f.tag }
func (f fakeProber) ColorField(_ context.Context, _, field string) string {
	return f.color[field]
}
func (f fakeProber) SideDataFlat(_ context.Context, _ string) string { return f.flat }

func TestDeriveColorArgs(t *testing.T) {
	joinFlags := func(fl []string) string { return strings.Join(fl, " ") }

	t.Run("SDR bt709: tags passed through, NO HDR invented", func(t *testing.T) {
		p := fakeProber{color: map[string]string{
			"color_primaries": "bt709", "color_transfer": "bt709", "color_space": "bt709", "color_range": "tv",
		}}
		flags, x := DeriveColorArgs(context.Background(), p, "x")
		fs := joinFlags(flags)
		for _, want := range []string{"-color_primaries bt709", "-color_trc bt709", "-colorspace bt709", "-color_range tv"} {
			if !strings.Contains(fs, want) {
				t.Errorf("flags %q missing %q", fs, want)
			}
		}
		for _, forbidden := range []string{"master-display", "max-cll", "hdr10-opt"} {
			if strings.Contains(x, forbidden) {
				t.Errorf("x265 params %q wrongly invented HDR (%q) on an SDR source", x, forbidden)
			}
		}
	})

	t.Run("HDR10 under-signalled: bt2020/PQ defaulted, hdr10-opt added", func(t *testing.T) {
		// Only the PQ transfer is signalled (common with H.264 HDR); primaries/space/
		// range must be defaulted to bt2020/bt2020nc/tv, and hdr10-opt enabled.
		p := fakeProber{color: map[string]string{"color_transfer": "smpte2084"}}
		flags, x := DeriveColorArgs(context.Background(), p, "x")
		fs := joinFlags(flags)
		for _, want := range []string{"-color_primaries bt2020", "-color_trc smpte2084", "-colorspace bt2020nc"} {
			if !strings.Contains(fs, want) {
				t.Errorf("flags %q missing defaulted %q", fs, want)
			}
		}
		if !strings.Contains(x, "hdr10-opt=1:repeat-headers=1") {
			t.Errorf("x265 params %q missing hdr10-opt", x)
		}
	})

	t.Run("HDR10 with mastering-display + MaxCLL: carried into x265 params", func(t *testing.T) {
		p := fakeProber{color: map[string]string{"color_transfer": "smpte2084"}, flat: sdFlat}
		_, x := DeriveColorArgs(context.Background(), p, "x")
		if !strings.Contains(x, "master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)") {
			t.Errorf("x265 params %q missing the mapped master-display", x)
		}
		if !strings.Contains(x, "max-cll=1000,400") {
			t.Errorf("x265 params %q missing max-cll", x)
		}
	})
}

// ---- ClassFrom (bash case 20: hdr_class_from, every branch) ------------------

func TestClassFrom(t *testing.T) {
	cases := []struct {
		name          string
		codecTag      string
		flatSideData  string
		colorTransfer string
		want          string
	}{
		{"dvh1 tag -> dv", "dvh1", "", "", ClassDV},
		{"dvhe tag -> dv", "dvhe", "", "", ClassDV},
		{"dvav tag -> dv", "dvav", "", "", ClassDV},
		{"dav1 tag -> dv", "dav1", "", "", ClassDV},
		{"dvh1 tag uppercase -> dv", "DVH1", "", "", ClassDV},
		{"DOVI config record -> dv", "hev1", `side_data_type="DOVI configuration record"`, "", ClassDV},
		{"Dolby Vision string -> dv", "hev1", "Dolby Vision RPU present", "", ClassDV},
		{"SMPTE2094-40 -> hdr10plus", "hev1", "HDR Dynamic Metadata SMPTE2094-40 (HDR10+)", "", ClassHDR10Plus},
		{"HDR10+ string -> hdr10plus", "hev1", "some HDR10+ marker", "", ClassHDR10Plus},
		{"PQ transfer -> hdr10", "hev1", "", "smpte2084", ClassHDR10},
		{"mastering-display -> hdr10", "hev1", `side_data_type="Mastering display metadata"`, "bt709", ClassHDR10},
		{"plain bt709 -> other", "hev1", "", "bt709", ClassOther},
		{"nothing at all -> other", "", "", "", ClassOther},
		// codec tag takes priority even if side data / transfer would suggest otherwise
		{"dv tag wins over hdr10 signal", "dvhe", `side_data_type="Mastering display metadata"`, "smpte2084", ClassDV},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassFrom(tc.codecTag, tc.flatSideData, tc.colorTransfer)
			if got != tc.want {
				t.Errorf("ClassFrom(%q, %q, %q) = %q, want %q", tc.codecTag, tc.flatSideData, tc.colorTransfer, got, tc.want)
			}
		})
	}
}

// ---- MasterDisplay / MaxCLL numeric mapping (bash case 21) --------------------

// sdFlat is the known blob from bash case 21 (lines ~411-430): a complete
// mastering-display + content-light block.
const sdFlat = `frames.frame.0.side_data_list.side_data.0.side_data_type="Mastering display metadata"
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
frames.frame.0.side_data_list.side_data.1.max_average=400`

func TestMasterDisplay_KnownBlob(t *testing.T) {
	got := MasterDisplay(sdFlat)
	want := "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)"
	if got != want {
		t.Errorf("MasterDisplay = %q, want %q", got, want)
	}
}

func TestMaxCLL_KnownBlob(t *testing.T) {
	got := MaxCLL(sdFlat)
	want := "1000,400"
	if got != want {
		t.Errorf("MaxCLL = %q, want %q", got, want)
	}
}

func TestMasterDisplay_PartialIsEmpty(t *testing.T) {
	// Missing white_point_x/white_point_y (bash: `grep -v white_point`) — a
	// half-signalled display volume is worse than none.
	partial := filterOutLines(sdFlat, "white_point")
	if got := MasterDisplay(partial); got != "" {
		t.Errorf("MasterDisplay(partial) = %q, want empty", got)
	}
}

func TestMaxCLL_PartialIsEmpty(t *testing.T) {
	partial := filterOutLines(sdFlat, "max_average")
	if got := MaxCLL(partial); got != "" {
		t.Errorf("MaxCLL(partial) = %q, want empty", got)
	}
}

func TestHdrScaled(t *testing.T) {
	cases := []struct {
		name  string
		r     string
		scale int
		want  string
	}{
		{"empty -> empty", "", 50000, ""},
		{"rational", "34000/50000", 50000, "34000"},
		{"rational rounds", "1/10000", 10000, "1"},
		{"plain number", "42", 1, "42"},
		{"plain decimal", "0.5", 100, "50"},
		{"zero denominator -> empty", "1/0", 100, ""},
		{"garbage -> empty", "not-a-number", 100, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hdrScaled(tc.r, tc.scale); got != tc.want {
				t.Errorf("hdrScaled(%q, %d) = %q, want %q", tc.r, tc.scale, got, tc.want)
			}
		})
	}
}

// ---- StaticMetadataIncomplete (bash case 21 fail-safe predicate) -------------

func TestStaticMetadataIncomplete(t *testing.T) {
	partialMD := filterOutLines(sdFlat, "white_point")
	partialCLL := filterOutLines(sdFlat, "max_average")
	allZeroCLL := `side_data_type="Content light level metadata"
frames.frame.0.side_data_list.side_data.0.max_content=0
frames.frame.0.side_data_list.side_data.0.max_average=0`
	noHDRBlock := `side_data_type="Display Matrix"`

	cases := []struct {
		name string
		flat string
		want bool
	}{
		{"partial mastering-display -> incomplete", partialMD, true},
		{"partial content-light -> incomplete", partialCLL, true},
		{"complete static metadata -> not incomplete", sdFlat, false},
		{"all-zero MaxCLL parses -> not incomplete", allZeroCLL, false},
		{"no HDR static block -> not incomplete", noHDRBlock, false},
		{"empty flat -> not incomplete", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StaticMetadataIncomplete(tc.flat); got != tc.want {
				t.Errorf("StaticMetadataIncomplete(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// ---- small string helpers for the test file only ------------------------------

// filterOutLines drops every line of s containing substr, mirroring the bash
// suite's `grep -v` fixture mutation, and rejoins with newlines.
func filterOutLines(s, substr string) string {
	var kept []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.Contains(l, substr) {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// ---- DerivePixFmt --------------------------------------------------------------

func TestDerivePixFmt(t *testing.T) {
	cases := []struct {
		src    string
		want   string
		wantOk bool
	}{
		{"yuv420p", "yuv420p10le", true},
		{"yuvj420p", "yuv420p10le", true},
		{"yuv422p", "yuv422p10le", true},
		{"yuv444p", "yuv444p10le", true},
		{"yuv420p10le", "yuv420p10le", true},
		{"yuv420p12le", "yuv420p12le", true},
		{"yuv422p10le", "yuv422p10le", true},
		{"yuv444p10le", "yuv444p10le", true},
		{"yuv420p16le", "", false}, // >12-bit: libx265 can't; skip, never silently reduce
		{"yuv444p12le", "yuv444p12le", true},
		{"rgb24", "", false},
		{"pal8", "", false},
		{"yuv411p", "", false},
		{"gray", "", false},
		{"", "", false},
		{"nv12", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			got, ok := DerivePixFmt(tc.src)
			if ok != tc.wantOk || got != tc.want {
				t.Errorf("DerivePixFmt(%q) = (%q, %v), want (%q, %v)", tc.src, got, ok, tc.want, tc.wantOk)
			}
		})
	}
}
