package dynhdr

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// recordFlat is a DOVI configuration record as ffprobe's flat output prints it, after a
// mastering-display block, with the given fields.
func recordFlat(profile, level, compat, rpu, el, bl string) string {
	p := "streams.stream.0.side_data_list.side_data.1."
	return `streams.stream.0.side_data_list.side_data.0.side_data_type="Mastering display metadata"
streams.stream.0.side_data_list.side_data.0.red_x="17/25"
` + p + `side_data_type="DOVI configuration record"
` + p + `dv_version_major=1
` + p + `dv_version_minor=0
` + p + `dv_profile=` + profile + `
` + p + `dv_level=` + level + `
` + p + `rpu_present_flag=` + rpu + `
` + p + `el_present_flag=` + el + `
` + p + `bl_present_flag=` + bl + `
` + p + `dv_bl_signal_compatibility_id=` + compat + `
` + p + `dv_md_compression="none"
`
}

func TestRecordFrom_ReadsEveryField(t *testing.T) {
	r := RecordFrom(recordFlat("8", "1", "1", "1", "0", "1"))
	want := Record{Present: true, Readable: true, Profile: 8, Level: 1, CompatID: 1, RPU: true, BL: true}
	if r != want {
		t.Fatalf("RecordFrom = %+v, want %+v", r, want)
	}
	r = RecordFrom(recordFlat("7", "6", "6", "1", "1", "1"))
	if !r.Readable || r.Profile != 7 || r.Level != 6 || r.CompatID != 6 || !r.EL || !r.RPU || !r.BL {
		t.Fatalf("profile 7 record = %+v", r)
	}
}

func TestRecordFrom_AnAbsentOrUnreadableRecordIsNeverAProfile(t *testing.T) {
	if r := RecordFrom(""); r.Present || r.Readable {
		t.Errorf("no side data reads as %+v", r)
	}
	if r := RecordFrom(`streams.stream.0.side_data_list.side_data.0.side_data_type="Mastering display metadata"`); r.Present {
		t.Errorf("a file with no record reads as %+v", r)
	}
	for name, flat := range map[string]string{
		"profile not a number": recordFlat("x", "1", "1", "1", "0", "1"),
		"negative compat":      recordFlat("8", "1", "-1", "1", "0", "1"),
		"level missing":        strings.Replace(recordFlat("8", "1", "1", "1", "0", "1"), "dv_level=1", "", 1),
		"flag not 0 or 1":      recordFlat("8", "1", "1", "2", "0", "1"),
		"el flag missing":      strings.Replace(recordFlat("8", "1", "1", "1", "0", "1"), "el_present_flag=0", "", 1),
		"bl flag empty":        recordFlat("8", "1", "1", "1", "0", ""),
	} {
		r := RecordFrom(flat)
		if !r.Present || r.Readable {
			t.Errorf("%s: %+v, want present and not readable", name, r)
		}
	}
	// A nested section under the record (a later key with a dot) is not one of its fields.
	nested := recordFlat("8", "1", "1", "1", "0", "1") +
		"streams.stream.0.side_data_list.side_data.1.sub.dv_profile=5\n"
	if r := RecordFrom(nested); r.Profile != 8 {
		t.Errorf("a nested key overwrote the profile: %+v", r)
	}
}

func TestHasHDR10Plus_ReadsTheClassifiersThreeSpellings(t *testing.T) {
	for _, s := range []string{`side_data_type="HDR Dynamic Metadata SMPTE2094-40 (HDR10+)"`, "SMPTE2094-40", "HDR10+",
		"HDR Dynamic Metadata"} {
		if !HasHDR10Plus(s) {
			t.Errorf("%q not read as HDR10+", s)
		}
	}
	if HasHDR10Plus(recordFlat("8", "1", "1", "1", "0", "1")) {
		t.Error("a DOVI record read as HDR10+")
	}
}

// p81 is a carried profile 8.1 source's facts.
func p81() Source {
	return Source{DolbyVision: true, Record: RecordFrom(recordFlat("8", "1", "1", "1", "0", "1")),
		Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc", MasterDisplay: "G(1,2)B(3,4)R(5,6)WP(7,8)L(9,10)"}
}

func cpu() Settings { return Settings{Encoder: "cpu", P7: P7Skip} }

func TestDecide_NothingDynamicIsNeitherCarriedNorSkipped(t *testing.T) {
	if v := Decide(Source{}, cpu()); v.Skipped() || v.Intent.Carries() {
		t.Errorf("a source with no dynamic metadata: %+v", v)
	}
}

func TestDecide_OnlyTheCpuEncoderCarries(t *testing.T) {
	h10 := Source{HDR10Plus: true}
	for _, enc := range []string{"svtav1", "nvenc", "qsv", "vaapi", "amf", "x264", "auto", ""} {
		set := Settings{Encoder: enc, P7: P7Convert}
		v := Decide(p81(), set)
		if v.Skip != ReasonDolbyVision || len(v.Inputs) != 1 || v.Inputs[0] != InputEncoder || !strings.Contains(v.Why, "cpu") {
			t.Errorf("Dolby Vision under %q: %+v", enc, v)
		}
		v = Decide(h10, set)
		if v.Skip != ReasonHDR10Plus || len(v.Inputs) != 1 || v.Inputs[0] != InputEncoder {
			t.Errorf("HDR10+ under %q: %+v", enc, v)
		}
	}
	for _, src := range []Source{p81(), h10} {
		v := Decide(src, Settings{Encoder: "cpu", RemuxOnly: true})
		if !v.Skipped() || len(v.Inputs) != 0 || !strings.Contains(v.Why, "remux") {
			t.Errorf("remux-only: %+v", v)
		}
	}
}

func TestDecide_CarriesProfile81AndHDR10PlusOnCpu(t *testing.T) {
	v := Decide(p81(), cpu())
	if v.Skipped() || !v.Intent.DolbyVision || v.Intent.SourceProfile != 8 || v.Intent.Convert || v.Intent.HDR10Plus {
		t.Errorf("profile 8.1: %+v", v)
	}
	v = Decide(Source{HDR10Plus: true, Transfer: "smpte2084"}, cpu())
	if v.Skipped() || v.Intent.DolbyVision || !v.Intent.HDR10Plus {
		t.Errorf("HDR10+: %+v", v)
	}
	both := p81()
	both.HDR10Plus = true
	v = Decide(both, cpu())
	if v.Skipped() || !v.Intent.DolbyVision || !v.Intent.HDR10Plus {
		t.Errorf("both: %+v", v)
	}
	if !v.Intent.NeedsHDR10PlusTool() || v.Intent.NeedsDoviTool() {
		t.Errorf("both needs hdr10plus_tool and not dovi_tool: %+v", v.Intent)
	}
}

func TestDecide_Profile7IsConvertedOnlyWhereOptedIn(t *testing.T) {
	src := p81()
	src.Record = RecordFrom(recordFlat("7", "6", "6", "1", "1", "1"))
	v := Decide(src, cpu())
	if v.Skip != ReasonProfile7 || len(v.Inputs) != 2 || v.Inputs[0] != InputP7 || v.Inputs[1] != InputEncoder {
		t.Errorf("profile 7 under skip: %+v", v)
	}
	v = Decide(src, Settings{Encoder: "cpu"})
	if v.Skip != ReasonProfile7 {
		t.Errorf("profile 7 under an unset key: %+v", v)
	}
	v = Decide(src, Settings{Encoder: "cpu", P7: P7Convert})
	if v.Skipped() || !v.Intent.Convert || v.Intent.SourceProfile != 7 || !v.Intent.DolbyVision || !v.Intent.NeedsDoviTool() {
		t.Errorf("profile 7 under convert: %+v", v)
	}
	src.MasterDisplay = ""
	if v := Decide(src, Settings{Encoder: "cpu", P7: P7Convert}); v.Skip != ReasonNoMasteringDisplay {
		t.Errorf("profile 7 with no mastering display: %+v", v)
	}
}

func TestDecide_EveryOtherDolbyVisionStaysSkipped(t *testing.T) {
	for name, rec := range map[string]string{
		"profile 5":         recordFlat("5", "6", "0", "1", "0", "1"),
		"profile 8.4 (HLG)": recordFlat("8", "1", "4", "1", "0", "1"),
		"profile 8.2 (SDR)": recordFlat("8", "1", "2", "1", "0", "1"),
		"profile 8 with EL": recordFlat("8", "1", "1", "1", "1", "1"),
		"profile 8 no RPU":  recordFlat("8", "1", "1", "0", "0", "1"),
		"profile 8 no BL":   recordFlat("8", "1", "1", "1", "0", "0"),
		"profile 4":         recordFlat("4", "1", "2", "1", "1", "1"),
		"profile 9":         recordFlat("9", "1", "2", "1", "0", "1"),
		"profile 10":        recordFlat("10", "1", "1", "1", "0", "1"),
		"no record":         "",
		"unreadable record": recordFlat("8", "1", "1", "x", "0", "1"),
	} {
		src := p81()
		src.Record = RecordFrom(rec)
		v := Decide(src, Settings{Encoder: "cpu", P7: P7Convert})
		if v.Skip != ReasonDolbyVision || len(v.Inputs) != 0 {
			t.Errorf("%s: %+v, want the dolby-vision skip reading no key", name, v)
		}
	}
	for name, mutate := range map[string]func(*Source){
		"bt709 primaries": func(s *Source) { s.Primaries = "bt709" },
		"HLG transfer":    func(s *Source) { s.Transfer = "arib-std-b67" },
		"bt709 matrix":    func(s *Source) { s.Matrix = "bt709" },
	} {
		src := p81()
		mutate(&src)
		if v := Decide(src, cpu()); v.Skip != ReasonDolbyVision || !strings.Contains(v.Why, "compatibility id 1") {
			t.Errorf("%s: %+v", name, v)
		}
	}
	src := p81()
	src.MasterDisplay = ""
	if v := Decide(src, cpu()); v.Skip != ReasonNoMasteringDisplay || len(v.Inputs) != 1 || v.Inputs[0] != InputEncoder {
		t.Errorf("no mastering display: %+v", v)
	}
}

// The level table is H.265 Table A.8 as x265 carries it; each case is a worked example
// against it (level.cpp, read 2026-10-01).
func TestVBVFor_TakesTheLowestAdmittingLevelsCeiling(t *testing.T) {
	for _, c := range []struct {
		w, h     int
		rate     string
		level    string
		max, buf int
	}{
		{320, 240, "24/1", "2", 1500, 1500},           // 76800 ps, 1843200 sr
		{352, 288, "30/1", "2", 1500, 1500},           // 101376 ps, 3041280 sr
		{640, 360, "30/1", "3", 6000, 6000},           // 230400 ps > 2's 122880... 2.1 sr 6912000 ok -> but ps fits 2.1
		{1280, 720, "30/1", "3.1", 10000, 10000},      // 921600 ps
		{1920, 1080, "24000/1001", "4", 30000, 30000}, // high tier of level 4
		{1920, 1080, "60/1", "4.1", 50000, 50000},     // 124416000 sr
		{3840, 2160, "24/1", "5", 100000, 100000},     // 199065600 sr
		{3840, 2160, "60/1", "5.1", 160000, 160000},   // 497664000 sr
		{3840, 2160, "120/1", "5.2", 240000, 240000},  // 995328000 sr
		{7680, 4320, "30/1", "6", 240000, 240000},     // 995328000 sr, 33177600 ps
		{7680, 4320, "120/1", "6.2", 800000, 800000},  // 3981312000 sr
	} {
		r, ok := ParseRate(c.rate)
		if !ok {
			t.Fatalf("rate %q", c.rate)
		}
		v, err := VBVFor(c.w, c.h, r)
		if c.w == 640 {
			// 230400 > level 2's 122880 and <= 2.1's 245760, at 6912000 <= 7372800.
			c.level, c.max, c.buf = "2.1", 3000, 3000
		}
		if err != nil || v.Level != c.level || v.MaxrateKbps != c.max || v.BufsizeKbit != c.buf {
			t.Errorf("%dx%d@%s = %+v, %v; want level %s %d/%d", c.w, c.h, c.rate, v, err, c.level, c.max, c.buf)
		}
	}
	// Exactly at a boundary is admitted: 1920x1080 is 2073600 <= 2228224, and 2228224 x 30 =
	// 66846720 is level 4's MaxLumaSr exactly.
	if v, err := VBVFor(2228224, 1, Rate{30, 1}); err != nil || v.Level != "4" {
		t.Errorf("the level-4 boundary: %+v %v", v, err)
	}
	if v, err := VBVFor(2228224, 1, Rate{3000001, 100000}); err != nil || v.Level != "4.1" {
		t.Errorf("just past the level-4 rate: %+v %v", v, err)
	}
	for name, c := range map[string]struct {
		w, h int
		r    Rate
	}{
		"8K at 240":   {7680, 4320, Rate{240, 1}},
		"16K":         {15360, 8640, Rate{24, 1}},
		"zero width":  {0, 1080, Rate{24, 1}},
		"zero height": {1920, 0, Rate{24, 1}},
		"no rate":     {1920, 1080, Rate{}},
		"zero den":    {1920, 1080, Rate{24, 0}},
	} {
		if v, err := VBVFor(c.w, c.h, c.r); err == nil {
			t.Errorf("%s: %+v, want a refusal", name, v)
		}
	}
}

func TestRates_ParseAndConstancy(t *testing.T) {
	for in, want := range map[string]Rate{"24/1": {24, 1}, "24000/1001": {24000, 1001}, " 30/1 ": {30, 1}} {
		if r, ok := ParseRate(in); !ok || r != want {
			t.Errorf("ParseRate(%q) = %v %v", in, r, ok)
		}
	}
	for _, in := range []string{"0/0", "24", "", "a/b", "-24/1", "24/-1", "24/0", "0/1"} {
		if r, ok := ParseRate(in); ok {
			t.Errorf("ParseRate(%q) = %v, want none", in, r)
		}
	}
	if r, err := ConstantRate("24000/1001", "24000/1001"); err != nil || r.String() != "24000/1001" {
		t.Errorf("constant: %v %v", r, err)
	}
	if r, err := ConstantRate("48/2", "24/1"); err != nil || r != (Rate{48, 2}) {
		t.Errorf("equal by value: %v %v", r, err)
	}
	if _, err := ConstantRate("24/1", "2997/125"); err == nil || !strings.Contains(err.Error(), "not constant") {
		t.Errorf("variable: %v", err)
	}
	if _, err := ConstantRate("24/1", "0/0"); err == nil || !strings.Contains(err.Error(), "could not be established") {
		t.Errorf("unknown average: %v", err)
	}
	for in, want := range map[string]bool{"0.000000": true, "0": true, "0.041708": false, "N/A": false, "": false, "-0.5": false} {
		if StartsAtZero(in) != want {
			t.Errorf("StartsAtZero(%q) != %v", in, want)
		}
	}
}

func TestValidateHDR10Plus_HoldsTheFileToTheSource(t *testing.T) {
	entries := func(n int) string {
		s := make([]string, n)
		for i := range s {
			s[i] = `{"SceneFrameIndex":0}`
		}
		return `{"JSONInfo":{"HDR10plusProfile":"B","Version":"1.0"},"SceneInfo":[` + strings.Join(s, ",") + `]}`
	}
	if err := ValidateHDR10Plus([]byte(entries(24)), 24); err != nil {
		t.Errorf("a whole file: %v", err)
	}
	for name, c := range map[string]struct {
		data   string
		frames int
		want   string
	}{
		"fewer entries": {entries(23), 24, "23 frame entries"},
		"more entries":  {entries(25), 24, "25 frame entries"},
		"not JSON":      {"Dynamic HDR10+ metadata detected.", 24, "does not parse"},
		"no JSONInfo":   {`{"SceneInfo":[{}]}`, 1, "no JSONInfo"},
		"no SceneInfo":  {`{"JSONInfo":{}}`, 0, "no SceneInfo"},
		"empty":         {"", 1, "does not parse"},
		"truncated":     {entries(24)[:40], 24, "does not parse"},
	} {
		err := ValidateHDR10Plus([]byte(c.data), c.frames)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestCountsFrom_CountsFramesOnceEach(t *testing.T) {
	data := `{"frames":[
 {"media_type":"video","side_data_list":[{"side_data_type":"Dolby Vision RPU Data"},{"side_data_type":"Dolby Vision Metadata"},{"side_data_type":"HDR Dynamic Metadata SMPTE2094-40 (HDR10+)"}]},
 {"media_type":"video","side_data_list":[{"side_data_type":"Dolby Vision RPU Data"},{"side_data_type":"Dolby Vision RPU Data"}]},
 {"media_type":"video"},
 {"media_type":"audio","side_data_list":[{"side_data_type":"Dolby Vision RPU Data"}]}
]}`
	c, err := CountsFrom([]byte(data))
	if err != nil || c != (Counts{Frames: 3, DoviRPU: 2, HDR10Plus: 1}) {
		t.Errorf("CountsFrom = %+v, %v", c, err)
	}
	if _, err := CountsFrom([]byte("not json")); err == nil {
		t.Error("garbage counted")
	}
}

func TestCheck_EachFailureNamesItsGate(t *testing.T) {
	good := RecordFrom(recordFlat("8", "1", "1", "1", "0", "1"))
	dv := Expectation{DolbyVision: true, Profile: 8, CompatID: 1}
	both := Expectation{DolbyVision: true, Profile: 8, CompatID: 1, HDR10Plus: true}
	plus := Expectation{HDR10Plus: true}
	full := Counts{Frames: 24, DoviRPU: 24, HDR10Plus: 24}
	if err := Check(both, good, full); err != nil {
		t.Errorf("a faithful output: %v", err)
	}
	if err := Check(Expectation{}, Record{}, Counts{}); err != nil {
		t.Errorf("a plan carrying nothing: %v", err)
	}
	for name, c := range map[string]struct {
		want Expectation
		rec  Record
		n    Counts
		gate string
	}{
		"no record":         {dv, Record{}, full, GateDoviRecord},
		"unreadable record": {dv, RecordFrom(recordFlat("8", "1", "1", "x", "0", "1")), full, GateDoviRecord},
		"another profile":   {dv, RecordFrom(recordFlat("5", "1", "1", "1", "0", "1")), full, GateDoviRecord},
		"another compat id": {dv, RecordFrom(recordFlat("8", "1", "4", "1", "0", "1")), full, GateDoviRecord},
		"an EL flagged":     {dv, RecordFrom(recordFlat("8", "1", "1", "1", "1", "1")), full, GateDoviRecord},
		"no RPU flagged":    {dv, RecordFrom(recordFlat("8", "1", "1", "0", "0", "1")), full, GateDoviRecord},
		"no BL flagged":     {dv, RecordFrom(recordFlat("8", "1", "1", "1", "0", "0")), full, GateDoviRecord},
		"one RPU short":     {dv, good, Counts{Frames: 24, DoviRPU: 23}, GateDoviRPU},
		"no RPU":            {dv, good, Counts{Frames: 24}, GateDoviRPU},
		"no frames, DV":     {dv, good, Counts{}, GateDoviRPU},
		"no frames, HDR10+": {plus, Record{}, Counts{}, GateHDR10Plus},
		"one HDR10+ short":  {both, good, Counts{Frames: 24, DoviRPU: 24, HDR10Plus: 23}, GateHDR10Plus},
		"HDR10+ dropped":    {plus, Record{}, Counts{Frames: 24}, GateHDR10Plus},
	} {
		err := Check(c.want, c.rec, c.n)
		var ge *GateError
		if !errors.As(err, &ge) || ge.Gate != c.gate || !strings.Contains(err.Error(), "The source is kept") {
			t.Errorf("%s: %v, want a rejection by %s", name, err, c.gate)
		}
	}
}

func TestELTypeFrom_ReadsTheBracketedLayerTypes(t *testing.T) {
	for in, want := range map[string]string{
		"Summary:\n  Frames: 24\n  Profile: 7 (FEL)\n  DM version: 1":       "FEL",
		"Summary:\n  Frames: 24\n  Profile: 7 (MEL)\n":                      "MEL",
		"Summary:\n  Frames: 24\n  Profiles: 7 (FEL, MEL), 8\n":             "FEL, MEL",
		"Summary:\n  Frames: 24\n  Profile: 8\n  DM version: 2 (CM v4.0)\n": "",
		"nothing":           "",
		"  Profile: 7 (FEL": "",
	} {
		if got := ELTypeFrom(in); got != want {
			t.Errorf("ELTypeFrom(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEscapeX265Value_KeepsAPathOneValue(t *testing.T) {
	for in, want := range map[string]string{
		"/lib/a.json":            "/lib/a.json",
		"/lib/Film: Part 2.json": `/lib/Film\:\ Part\ 2.json`,
		`/l/a=b'c\d`:             `/l/a\=b\'c\\d`,
		"/l/a\tb":                "/l/a\\\tb",
	} {
		if got := EscapeX265Value(in); got != want {
			t.Errorf("EscapeX265Value(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrepared_CommandLineParts(t *testing.T) {
	var none *Prepared
	if none.X265Params() != "" || none.CodecArgs() != nil || none.InputArgs() != nil || none.Expect() != (Expectation{}) {
		t.Error("a nil pre-pass adds something")
	}
	dv := &Prepared{Intent: Intent{DolbyVision: true, SourceProfile: 8}, VBV: VBV{MaxrateKbps: 30000, BufsizeKbit: 30000}}
	if got := dv.X265Params(); got != ":vbv-maxrate=30000:vbv-bufsize=30000" {
		t.Errorf("DV params %q", got)
	}
	if got := strings.Join(dv.CodecArgs(), " "); got != "-dolbyvision 1" {
		t.Errorf("DV codec args %q", got)
	}
	if dv.InputArgs() != nil {
		t.Error("an unconverted plan reads a second input")
	}
	if dv.Expect() != (Expectation{DolbyVision: true, Profile: 8, CompatID: 1}) {
		t.Errorf("DV expectation %+v", dv.Expect())
	}
	plus := &Prepared{Intent: Intent{HDR10Plus: true}, HDR10PlusJSON: "/w/a:b.json"}
	if got := plus.X265Params(); got != `:dhdr10-info=/w/a\:b.json` {
		t.Errorf("HDR10+ params %q", got)
	}
	if plus.CodecArgs() != nil || plus.Expect() != (Expectation{HDR10Plus: true}) {
		t.Errorf("HDR10+ codec args %v expectation %+v", plus.CodecArgs(), plus.Expect())
	}
	conv := &Prepared{Intent: Intent{DolbyVision: true, Convert: true, SourceProfile: 7},
		VBV: VBV{MaxrateKbps: 1500, BufsizeKbit: 1500}, RawVideo: "/w/raw", FrameRate: Rate{24000, 1001}}
	if got := strings.Join(conv.InputArgs(), " "); got != "-f hevc -framerate 24000/1001 -i /w/raw" {
		t.Errorf("converted input %q", got)
	}
	if conv.Expect() != (Expectation{DolbyVision: true, Profile: 8, CompatID: 1}) {
		t.Errorf("a converted profile 7 is held to 8.1: %+v", conv.Expect())
	}
	both := &Prepared{Intent: Intent{DolbyVision: true, HDR10Plus: true}, VBV: VBV{MaxrateKbps: 1, BufsizeKbit: 2},
		HDR10PlusJSON: "/j"}
	if got := both.X265Params(); got != ":vbv-maxrate=1:vbv-bufsize=2:dhdr10-info=/j" {
		t.Errorf("both params %q", got)
	}
}

func TestTools_FromEnvAndMissing(t *testing.T) {
	env := map[string]string{EnvDoviTool: "/opt/dt"}
	tl := ToolsFromEnv(func(k string) string { return env[k] }, "ff", "fp")
	if tl.DoviTool != "/opt/dt" || tl.HDR10Plus != DefaultHDR10PlusTool || tl.FFmpeg != "ff" || tl.FFprobe != "fp" {
		t.Errorf("ToolsFromEnv = %+v", tl)
	}
	dir := t.TempDir()
	present := filepath.Join(dir, "present")
	if err := os.WriteFile(present, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "absent")
	conv := Intent{DolbyVision: true, Convert: true}
	plus := Intent{HDR10Plus: true}
	p8 := Intent{DolbyVision: true}
	for name, c := range map[string]struct {
		tools Tools
		in    Intent
		want  string
	}{
		"profile 8 needs no tool":     {Tools{}, p8, ""},
		"convert, dovi_tool present":  {Tools{DoviTool: present}, conv, ""},
		"convert, dovi_tool absent":   {Tools{DoviTool: absent, HDR10Plus: present}, conv, absent},
		"HDR10+, tool present":        {Tools{HDR10Plus: present}, plus, ""},
		"HDR10+, tool absent":         {Tools{DoviTool: present, HDR10Plus: absent}, plus, absent},
		"HDR10+, tool unnamed":        {Tools{}, plus, "(unnamed)"},
		"both, the second one absent": {Tools{DoviTool: present, HDR10Plus: absent}, Intent{Convert: true, HDR10Plus: true}, absent},
	} {
		if got := c.tools.Missing(c.in); got != c.want {
			t.Errorf("%s: Missing = %q, want %q", name, got, c.want)
		}
	}
}

// fakeTool writes a stand-in binary running script.
func fakeTool(t *testing.T, dir, name, script string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A pre-pass that cannot complete refuses with the reason it names, whichever step stopped,
// and never hands back a half-prepared carriage.
func TestPrepare_EveryFailingStepRefusesByName(t *testing.T) {
	dir := t.TempDir()
	facts := "width=320\nheight=240\nr_frame_rate=24/1\navg_frame_rate=24/1\nstart_time=0.000000\nnb_read_packets=24"
	n := 0
	next := func(prefix string) string { n++; return prefix + strconv.Itoa(n) }
	probe := func(out string) string { return fakeTool(t, dir, next("ffprobe-"), "printf '"+out+"\\n'") }
	goodProbe := fakeTool(t, dir, "ffprobe", "printf '"+facts+"\\n'")
	ffmpeg := fakeTool(t, dir, "ffmpeg", "printf 'raw-hevc'")
	failing := fakeTool(t, dir, "failing", "echo 'boom' >&2; exit 3")
	json24 := `{"JSONInfo":{},"SceneInfo":[` + strings.Repeat(`{},`, 23) + `{}]}`
	writeJSON := func(body string) string {
		return fakeTool(t, dir, next("h10p-"),
			`cat >/dev/null; while [ "$1" != "-o" ]; do shift; done; printf '%s' '`+body+`' > "$2"`)
	}
	goodH10 := writeJSON(json24)
	conv := fakeTool(t, dir, "dovi-ok", `cat >/dev/null; for a; do last=$a; done; case "$1" in extract-rpu|info) exit 1;; esac; printf 'converted' > "$last"`)
	convEmpty := fakeTool(t, dir, "dovi-empty", `cat >/dev/null; for a; do last=$a; done; case "$1" in extract-rpu|info) exit 1;; esac; : > "$last"`)
	req := func(tl Tools, in Intent) Request {
		return Request{Tools: tl, Intent: in, Source: filepath.Join(dir, "src.mkv"),
			JSONPath: filepath.Join(dir, "m.json"), RawPath: filepath.Join(dir, "raw"), HeadPath: filepath.Join(dir, "head")}
	}
	dv := Intent{DolbyVision: true, SourceProfile: 8}
	plus := Intent{HDR10Plus: true}
	p7 := Intent{DolbyVision: true, SourceProfile: 7, Convert: true}
	for name, c := range map[string]struct {
		tools Tools
		in    Intent
		want  string
	}{
		"probe fails, DV":           {Tools{FFprobe: failing}, dv, ReasonFrameRate},
		"probe fails, HDR10+":       {Tools{FFprobe: failing}, plus, ReasonHDR10PlusUnreadable},
		"no frame count":            {Tools{FFprobe: probe("width=320\nheight=240\nr_frame_rate=24/1")}, plus, ReasonHDR10PlusUnreadable},
		"no rate for the level":     {Tools{FFprobe: probe("width=320\nheight=240\nr_frame_rate=0/0\nnb_read_packets=24")}, dv, ReasonFrameRate},
		"beyond level 6.2":          {Tools{FFprobe: probe("width=15360\nheight=8640\nr_frame_rate=24/1\nnb_read_packets=24")}, dv, ReasonFrameRate},
		"variable rate, converting": {Tools{FFprobe: probe("width=320\nheight=240\nr_frame_rate=24/1\navg_frame_rate=23/1\nstart_time=0\nnb_read_packets=24")}, p7, ReasonFrameRate},
		"late start, converting":    {Tools{FFprobe: probe("width=320\nheight=240\nr_frame_rate=24/1\navg_frame_rate=24/1\nstart_time=0.5\nnb_read_packets=24")}, p7, ReasonFrameRate},
		"extract fails":             {Tools{FFprobe: goodProbe, FFmpeg: ffmpeg, HDR10Plus: failing}, plus, ReasonHDR10PlusUnreadable},
		"ffmpeg fails extracting":   {Tools{FFprobe: goodProbe, FFmpeg: failing, HDR10Plus: goodH10}, plus, ReasonHDR10PlusUnreadable},
		"malformed JSON":            {Tools{FFprobe: goodProbe, FFmpeg: ffmpeg, HDR10Plus: writeJSON("{nope")}, plus, ReasonHDR10PlusUnreadable},
		"a frame short":             {Tools{FFprobe: goodProbe, FFmpeg: ffmpeg, HDR10Plus: writeJSON(`{"JSONInfo":{},"SceneInfo":[{}]}`)}, plus, ReasonHDR10PlusUnreadable},
		"conversion fails":          {Tools{FFprobe: goodProbe, FFmpeg: ffmpeg, DoviTool: failing}, p7, ReasonConversionFailed},
		"conversion writes nothing": {Tools{FFprobe: goodProbe, FFmpeg: ffmpeg, DoviTool: convEmpty}, p7, ReasonConversionFailed},
	} {
		_ = os.Remove(filepath.Join(dir, "raw"))
		_ = os.Remove(filepath.Join(dir, "m.json"))
		p, err := Prepare(context.Background(), req(c.tools, c.in))
		r, ok := AsRefusal(err)
		if p != nil || !ok || r.Reason != c.want || !strings.HasPrefix(err.Error(), c.want+": ") || errors.Unwrap(err) == nil {
			t.Errorf("%s: %v %v, want a %s refusal", name, p, err, c.want)
		}
	}
	nonJSON := req(Tools{FFprobe: goodProbe, FFmpeg: ffmpeg, HDR10Plus: goodH10}, plus)
	nonJSON.JSONPath = filepath.Join(dir, "m.dynhdr0")
	if _, err := Prepare(context.Background(), nonJSON); err == nil || !strings.Contains(err.Error(), ".json") {
		t.Errorf("a metadata path x265 would abort on: %v", err)
	}
	// And the steps that complete hand back exactly what they established.
	p, err := Prepare(context.Background(), req(Tools{FFprobe: goodProbe, FFmpeg: ffmpeg, HDR10Plus: goodH10, DoviTool: conv},
		Intent{DolbyVision: true, SourceProfile: 7, Convert: true, HDR10Plus: true}))
	if err != nil {
		t.Fatalf("a whole pre-pass: %v", err)
	}
	if p.Frames != 24 || p.VBV.Level != "2" || p.HDR10PlusJSON == "" || p.RawVideo == "" || p.FrameRate != (Rate{24, 1}) || p.ELType != "" {
		t.Errorf("a whole pre-pass established %+v", p)
	}
	p, err = Prepare(context.Background(), req(Tools{FFprobe: goodProbe}, dv))
	if err != nil || p.VBV.Level != "2" || p.FrameRate.Valid() || p.RawVideo != "" || p.HDR10PlusJSON != "" {
		t.Errorf("a profile 8 pre-pass: %+v %v", p, err)
	}
}

func TestELType_ReadsTheSourcesHeadThroughTheTool(t *testing.T) {
	dir := t.TempDir()
	tool := fakeTool(t, dir, "dovi", `case "$1" in
extract-rpu) [ "$2" = "-l" ] && [ "$3" = "48" ] || exit 9; for a; do last=$a; done; : > "$last";;
info) printf 'Summary:\n  Frames: 48\n  Profile: 7 (MEL)\n';;
esac`)
	if got := ELType(context.Background(), Tools{DoviTool: tool}, "src", filepath.Join(dir, "head")); got != "MEL" {
		t.Errorf("ELType = %q", got)
	}
	bad := fakeTool(t, dir, "dovi-info-fails", `case "$1" in extract-rpu) exit 0;; info) exit 1;; esac`)
	if got := ELType(context.Background(), Tools{DoviTool: bad}, "src", filepath.Join(dir, "head")); got != "" {
		t.Errorf("a failed info read %q", got)
	}
}

func TestPipeline_ReportsTheConsumerFirst(t *testing.T) {
	dir := t.TempDir()
	ok := fakeTool(t, dir, "ok", "printf data")
	fail := fakeTool(t, dir, "fail", "cat >/dev/null; echo consumer-said >&2; exit 4")
	pfail := fakeTool(t, dir, "pfail", "echo producer-said >&2; exit 5")
	sink := fakeTool(t, dir, "sink", "cat >/dev/null")
	cmd := func(p string) *exec.Cmd { return exec.Command(p) }
	if err := pipeline(cmd(ok), cmd(sink)); err != nil {
		t.Errorf("a clean pipe: %v", err)
	}
	if err := pipeline(cmd(ok), cmd(fail)); err == nil || !strings.Contains(err.Error(), "consumer-said") {
		t.Errorf("consumer failure: %v", err)
	}
	if err := pipeline(cmd(pfail), cmd(sink)); err == nil || !strings.Contains(err.Error(), "producer-said") {
		t.Errorf("producer failure: %v", err)
	}
	if err := pipeline(cmd(filepath.Join(dir, "nope")), cmd(sink)); err == nil {
		t.Error("a producer that cannot start")
	}
	if err := pipeline(cmd(ok), cmd(filepath.Join(dir, "nope"))); err == nil {
		t.Error("a consumer that cannot start")
	}
}
