package probe

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/hdr"
)

// TRANSCODE-PERF collapses the ~15 per-file ffprobe calls into one VideoProps
// snapshot. The load-bearing safety claim is that the snapshot is
// BEHAVIOUR-PRESERVING: every accessor must return exactly what the single-field
// Prober method it replaces would return, so the skip guards, the HDR/DV classifier
// and the encoder's colour derivation cannot see any difference.
//
// The engine's real-ffmpeg fixture suite (internal/engine, cases 1–22 + a–e + the
// HDR/VMAF cases) proves that end-to-end in CI, because ProcessFile now reads every
// guard off the snapshot — a divergence would red the HDR-classification, exotic-
// pixfmt, interlace or colour-propagation fixtures. THIS test pins the same claim
// deterministically and without a real ffprobe: it drives a canned ffprobe stub so
// that batched and single-field queries return the same underlying data (exactly as a
// real ffprobe would), then asserts the snapshot and the individual methods agree
// field-for-field. It reds if the batched parse, the shared normalisation, or the
// bitrate fallback ever drifts from the single-field path.

// The stub is the test binary re-executing itself: TestMain routes a child process
// carrying fakeFFprobeEnv into fakeFFprobeMain, so Prober (which just execs its
// FFprobe with args) can be pointed at os.Args[0] and get canned, scenario-keyed
// output with no real ffprobe on the box.
const fakeFFprobeEnv = "HOLDFAST_FAKE_FFPROBE"

func TestMain(m *testing.M) {
	if os.Getenv(fakeFFprobeEnv) != "" {
		fakeFFprobeMain()
		return
	}
	os.Exit(m.Run())
}

// cannedStream is one scenario's worth of ffprobe answers. Every field is the RAW
// string a real ffprobe would print (before any normalisation), so "unknown"/"N/A"
// are represented verbatim and the test exercises the normalisation seam.
type cannedStream struct {
	codecName      string
	streamBitRate  string // stream-level bit_rate ("" = absent → format fallback)
	formatBitRate  string // container bit_rate (the fallback)
	fieldOrder     string
	codecTag       string
	pixFmt         string
	colorPrimaries string
	colorTransfer  string
	colorSpace     string
	colorRange     string
	formatDuration string // container duration ("" = the container reports none)
	frameSideData  string // flat=s=. frame side data (verbatim)
	streamSideData string // flat=s=. stream side data (verbatim)
}

const masteringFlat = `frames.frame.0.side_data_list.side_data.0.side_data_type="Mastering display metadata"
frames.frame.0.side_data_list.side_data.0.red_x="34000/50000"
frames.frame.0.side_data_list.side_data.0.red_y="16000/50000"
frames.frame.0.side_data_list.side_data.0.green_x="13250/50000"
frames.frame.0.side_data_list.side_data.0.green_y="34500/50000"
frames.frame.0.side_data_list.side_data.0.blue_x="7500/50000"
frames.frame.0.side_data_list.side_data.0.blue_y="3000/50000"
frames.frame.0.side_data_list.side_data.0.white_point_x="15635/50000"
frames.frame.0.side_data_list.side_data.0.white_point_y="16450/50000"
frames.frame.0.side_data_list.side_data.0.min_luminance="50/10000"
frames.frame.0.side_data_list.side_data.0.max_luminance="10000000/10000"
frames.frame.0.side_data_list.side_data.1.side_data_type="Content light level metadata"
frames.frame.0.side_data_list.side_data.1.max_content="1000"
frames.frame.0.side_data_list.side_data.1.max_average="400"
`

// scenarios covers the field shapes the engine actually reads: a plain SDR file, an
// HDR10 file (PQ + mastering-display + content-light), a source with no stream
// bit_rate (exercising the container fallback), ffprobe non-values that must
// normalise to "", a Dolby-Vision stream-side-data block, and a container that reports
// no duration at all (ffprobe's literal "N/A" — MPEG-TS's shape).
var scenarios = map[string]cannedStream{
	"sdr.mkv": {
		codecName: "h264", streamBitRate: "8000000", fieldOrder: "progressive",
		codecTag: "avc1", pixFmt: "yuv420p",
		colorPrimaries: "bt709", colorTransfer: "bt709", colorSpace: "bt709", colorRange: "tv",
		formatDuration: "6.000000",
	},
	"hdr10.mkv": {
		codecName: "hevc", streamBitRate: "20000000", fieldOrder: "progressive",
		codecTag: "hev1", pixFmt: "yuv420p10le",
		colorPrimaries: "bt2020", colorTransfer: "smpte2084", colorSpace: "bt2020nc", colorRange: "tv",
		formatDuration: "120.500000",
		frameSideData:  masteringFlat,
	},
	"nobr.mkv": {
		codecName: "h264", streamBitRate: "", formatBitRate: "6000000", fieldOrder: "progressive",
		codecTag: "avc1", pixFmt: "yuv420p",
		colorPrimaries: "bt709", colorTransfer: "bt709", colorSpace: "bt709", colorRange: "tv",
		formatDuration: "42.000000",
	},
	"unknowncolor.mkv": {
		codecName: "h264", streamBitRate: "8000000", fieldOrder: "unknown",
		codecTag: "avc1", pixFmt: "yuv420p",
		colorPrimaries: "unknown", colorTransfer: "reserved", colorSpace: "N/A", colorRange: "",
		formatDuration: "7.250000",
	},
	"dv.mkv": {
		codecName: "hevc", streamBitRate: "18000000", fieldOrder: "progressive",
		codecTag: "dvh1", pixFmt: "yuv420p10le",
		colorPrimaries: "bt2020", colorTransfer: "smpte2084", colorSpace: "bt2020nc", colorRange: "tv",
		formatDuration: "3600.000000",
		streamSideData: `streams.stream.0.side_data_list.side_data.0.side_data_type="DOVI configuration record"` + "\n",
	},
	"nodur.ts": {
		codecName: "h264", streamBitRate: "8000000", fieldOrder: "progressive",
		codecTag: "avc1", pixFmt: "yuv420p",
		colorPrimaries: "bt709", colorTransfer: "bt709", colorSpace: "bt709", colorRange: "tv",
		formatDuration: "N/A",
	},
}

// fakeFFprobeMain answers the exact ffprobe invocations Prober issues, keyed by the
// scenario file (the last argument). It returns the SAME underlying data whether a
// field is asked for alone or in the batch, which is precisely the invariant a real
// ffprobe has and the property the snapshot relies on.
func fakeFFprobeMain() {
	args := os.Args[1:]
	var entries, of, file string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-show_entries":
			if i+1 < len(args) {
				entries = args[i+1]
			}
		case "-of":
			if i+1 < len(args) {
				of = args[i+1]
			}
		}
	}
	file = args[len(args)-1] // Prober always ends with "-- <file>"
	base := file
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	s, ok := scenarios[base]
	if !ok {
		os.Exit(1) // unknown "file" → real ffprobe would exit non-zero
	}

	val := func(field string) string {
		switch field {
		case "codec_name":
			return s.codecName
		case "bit_rate":
			return s.streamBitRate
		case "field_order":
			return s.fieldOrder
		case "codec_tag_string":
			return s.codecTag
		case "pix_fmt":
			return s.pixFmt
		case "color_primaries":
			return s.colorPrimaries
		case "color_transfer":
			return s.colorTransfer
		case "color_space":
			return s.colorSpace
		case "color_range":
			return s.colorRange
		}
		// Every other stream entry (notably stream=duration, Prober.DurationSec's
		// fallback) is absent from these fixtures' streams — matroska carries no
		// per-stream duration — so a real ffprobe would print nothing for it.
		return ""
	}
	formatVal := func(field string) string {
		switch field {
		case "bit_rate":
			return s.formatBitRate
		case "duration":
			return s.formatDuration
		}
		return ""
	}

	// -show_entries takes colon-separated SECTIONS (`stream=a,b:format=duration`), each
	// printing its own lines in order. Answering them section-by-section is what keeps
	// the batched snapshot and the single-field probes returning the same underlying
	// data, which is the whole invariant this fixture exists to hold.
	nk := strings.Contains(of, "nk=1")
	switch {
	case entries == "frame=side_data_list":
		fmt.Print(s.frameSideData)
	case entries == "stream_side_data_list":
		fmt.Print(s.streamSideData)
	default:
		for _, section := range strings.Split(entries, ":") {
			name, fields, ok := strings.Cut(section, "=")
			if !ok {
				continue
			}
			for _, f := range strings.Split(fields, ",") {
				v := ""
				switch name {
				case "stream":
					v = val(f)
				case "format":
					v = formatVal(f)
				}
				if nk {
					fmt.Println(v) // single-field, no key (matches default=nw=1:nk=1)
				} else {
					fmt.Printf("%s=%s\n", f, v) // batched, key=value (default=nw=1)
				}
			}
		}
	}
	os.Exit(0)
}

func fakeProber() *Prober { return &Prober{FFprobe: os.Args[0], FFmpeg: os.Args[0]} }

func TestVideoProps_MatchesPerFieldProbes(t *testing.T) {
	os.Setenv(fakeFFprobeEnv, "1")
	defer os.Unsetenv(fakeFFprobeEnv)
	ctx := context.Background()
	p := fakeProber()

	for name := range scenarios {
		f := "/lib/" + name
		vp := p.VideoProps(ctx, f)

		if got, want := vp.Codec(), p.VideoCodec(ctx, f); got != want {
			t.Errorf("%s: Codec()=%q, VideoCodec()=%q", name, got, want)
		}
		if got, want := vp.BitrateKbps(), p.BitrateKbps(ctx, f); got != want {
			t.Errorf("%s: BitrateKbps()=%d, Prober.BitrateKbps()=%d", name, got, want)
		}
		if got, want := vp.FieldOrder(), p.FieldOrder(ctx, f); got != want {
			t.Errorf("%s: FieldOrder()=%q, Prober.FieldOrder()=%q", name, got, want)
		}
		if got, want := vp.CodecTag(), p.CodecTagString(ctx, f); got != want {
			t.Errorf("%s: CodecTag()=%q, CodecTagString()=%q", name, got, want)
		}
		if got, want := vp.PixFmt(), p.PixFmt(ctx, f); got != want {
			t.Errorf("%s: PixFmt()=%q, Prober.PixFmt()=%q", name, got, want)
		}
		for _, field := range []string{"color_primaries", "color_transfer", "color_space", "color_range"} {
			if got, want := vp.Color(field), p.ColorField(ctx, f, field); got != want {
				t.Errorf("%s: Color(%q)=%q, ColorField()=%q", name, field, got, want)
			}
		}
		if got, want := vp.SideData(), p.SideDataFlat(ctx, f); got != want {
			t.Errorf("%s: SideData()=%q, SideDataFlat()=%q", name, got, want)
		}
		if got, want := vp.FrameSideData(), p.FrameSideDataFlat(ctx, f); got != want {
			t.Errorf("%s: FrameSideData()=%q, FrameSideDataFlat()=%q", name, got, want)
		}
	}
}

// TestVideoProps_NormalisesNonValues pins the specific normalisation the guards depend
// on: ffprobe's "unknown"/"reserved"/"N/A" for colour and field_order must read as ""
// through the snapshot, exactly as through ColorField/FieldOrder — a guess here would
// stamp a wrong colourspace onto the output.
func TestVideoProps_NormalisesNonValues(t *testing.T) {
	os.Setenv(fakeFFprobeEnv, "1")
	defer os.Unsetenv(fakeFFprobeEnv)
	p := fakeProber()
	vp := p.VideoProps(context.Background(), "/lib/unknowncolor.mkv")

	for _, field := range []string{"color_primaries", "color_transfer", "color_space", "color_range"} {
		if got := vp.Color(field); got != "" {
			t.Errorf("Color(%q)=%q, want \"\" (ffprobe non-value must not be guessed)", field, got)
		}
	}
	if got := vp.FieldOrder(); got != "" {
		t.Errorf("FieldOrder()=%q, want \"\" for ffprobe \"unknown\"", got)
	}
}

// TestVideoProps_DurationComesFromTheSnapshotAndIsNeverGuessed pins the source length a
// live progress figure is measured against (S0030). Two properties matter and neither is
// negotiable: the duration must ride the snapshot probe that was already being taken (no
// second ffprobe per job), and a container that reports none must read as UNKNOWN — never
// as 0, which downstream would mean "a zero-length source", i.e. an instantly-finished
// encode. It deliberately does NOT apply Prober.DurationSec's stream-level fallback,
// because that fallback is a second subprocess on exactly the files that need it.
func TestVideoProps_DurationComesFromTheSnapshotAndIsNeverGuessed(t *testing.T) {
	t.Setenv(fakeFFprobeEnv, "1")
	p := fakeProber()
	ctx := context.Background()

	sec, ok := p.VideoProps(ctx, "/lib/sdr.mkv").DurationSec()
	if !ok || sec != 6 {
		t.Errorf("sdr.mkv DurationSec() = (%v, %v), want (6, true) from the container", sec, ok)
	}
	sec, ok = p.VideoProps(ctx, "/lib/hdr10.mkv").DurationSec()
	if !ok || sec != 120.5 {
		t.Errorf("hdr10.mkv DurationSec() = (%v, %v), want (120.5, true)", sec, ok)
	}
	// The container reports ffprobe's literal "N/A".
	sec, ok = p.VideoProps(ctx, "/lib/nodur.ts").DurationSec()
	if ok {
		t.Errorf("nodur.ts DurationSec() = (%v, true), want ok=false — an unknown duration must not be reported as known", sec)
	}
	if sec != 0 || ok {
		// The zero is the Go zero value that accompanies ok=false, and callers are
		// required to read ok, never the number. Stated here so the contract is explicit.
		t.Logf("nodur.ts returns (%v, %v) — callers must branch on ok", sec, ok)
	}
}

// TestVideoProps_BitrateContainerFallback pins that a source with no stream bit_rate
// falls back to the container bit_rate (in kbps), identically to Prober.BitrateKbps —
// the low-bitrate skip must fire off the same number whether read from the snapshot or
// a direct probe.
func TestVideoProps_BitrateContainerFallback(t *testing.T) {
	os.Setenv(fakeFFprobeEnv, "1")
	defer os.Unsetenv(fakeFFprobeEnv)
	p := fakeProber()
	vp := p.VideoProps(context.Background(), "/lib/nobr.mkv")
	if got := vp.BitrateKbps(); got != 6000 {
		t.Fatalf("BitrateKbps()=%d, want 6000 (6000000 container bit_rate / 1000)", got)
	}
}

// TestVideoProps_FeedsHDRClassifierAndColourArgsIdentically proves the two consumers
// that actually gate data-safety — the DV/HDR10+ classifier and the encoder's colour
// derivation — get identical results from the snapshot and from the prober-backed
// path. If these diverged, a DV source could slip the skip guard or an HDR encode
// could drop its colour signalling.
func TestVideoProps_FeedsHDRClassifierAndColourArgsIdentically(t *testing.T) {
	os.Setenv(fakeFFprobeEnv, "1")
	defer os.Unsetenv(fakeFFprobeEnv)
	ctx := context.Background()
	p := fakeProber()

	for name := range scenarios {
		f := "/lib/" + name
		vp := p.VideoProps(ctx, f)

		gotClass := hdr.ClassFrom(vp.CodecTag(), vp.SideData(), vp.Color("color_transfer"))
		wantClass := hdr.Classify(ctx, p, f)
		if gotClass != wantClass {
			t.Errorf("%s: class via props=%q, via prober=%q", name, gotClass, wantClass)
		}

		gotFlags, gotX := hdr.DeriveColorArgsFrom(
			vp.Color("color_primaries"), vp.Color("color_transfer"),
			vp.Color("color_space"), vp.Color("color_range"), vp.SideData())
		wantFlags, wantX := hdr.DeriveColorArgs(ctx, p, f)
		if strings.Join(gotFlags, " ") != strings.Join(wantFlags, " ") || gotX != wantX {
			t.Errorf("%s: color args via props=(%v, %q), via prober=(%v, %q)",
				name, gotFlags, gotX, wantFlags, wantX)
		}
	}
}
