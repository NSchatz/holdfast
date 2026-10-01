package crop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/dynhdr"
)

// THE DOLBY VISION CROP ON REAL FILES (proposal-crop-dv.md, "Test plan"): synthetic profile 8.1
// letterboxes - testsrc2 320x160 padded to 320x240, 10-bit PQ with a mastering display, an RPU
// from `dovi_tool generate` with level5 and level6, edited by the editor where a case needs
// frame ranges, injected and encoded with -dolbyvision 1 - read the way the engine reads them:
// the cropdetect samples, the frame facts, dovi_tool's L5, the decision, and the blackness check.

const dvColourParams = "log-level=error:pools=2:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:" +
	"master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1):max-cll=1000,400:" +
	"hdr10-opt=1:repeat-headers=1"

func dvTools(t *testing.T) dynhdr.Tools {
	t.Helper()
	ffmpeg := tools(t)
	ffprobe := os.Getenv("HOLDFAST_FFPROBE")
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	tl := dynhdr.ToolsFromEnv(os.Getenv, ffmpeg, ffprobe)
	for _, b := range []string{tl.FFprobe, tl.DoviTool} {
		if _, err := exec.LookPath(b); err != nil {
			t.Fatalf("::error:: %q not found - the Dolby Vision crop fixtures require it: %v", b, err)
		}
	}
	return tl
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// dvLetterbox writes the source: level5 is the generator's JSON object ("" for its own zero
// L5), edit an editor config ("" for none), draw is appended to the picture chain.
func dvLetterbox(t *testing.T, tl dynhdr.Tools, dir, level5, edit, draw string) string {
	t.Helper()
	chain := "testsrc2=s=320x160:r=24:d=1,format=yuv420p10le,pad=320:240:0:40:black"
	if draw != "" {
		chain += "," + draw
	}
	base := filepath.Join(dir, "base.hevc")
	run(t, tl.FFmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", chain, "-pix_fmt", "yuv420p10le",
		"-c:v", "libx265", "-threads", "2", "-x265-params", dvColourParams, base)
	l5 := ""
	if level5 != "" {
		l5 = `"level5":` + level5 + `,`
	}
	gen := write(t, dir, "gen.json", `{"cm_version":"V40","profile":"8.1","length":24,`+l5+
		`"level6":{"max_display_mastering_luminance":1000,"min_display_mastering_luminance":1,`+
		`"max_content_light_level":1000,"max_frame_average_light_level":400}}`)
	rpu := filepath.Join(dir, "rpu.bin")
	run(t, tl.DoviTool, "generate", "-j", gen, "-o", rpu)
	if edit != "" {
		edited := filepath.Join(dir, "edited.bin")
		run(t, tl.DoviTool, "editor", "-i", rpu, "-j", write(t, dir, "edit.json", edit), "-o", edited)
		rpu = edited
	}
	inj := filepath.Join(dir, "inj.hevc")
	run(t, tl.DoviTool, "inject-rpu", "-i", base, "--rpu-in", rpu, "-o", inj)
	out := filepath.Join(dir, "dv.mkv")
	run(t, tl.FFmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "hevc", "-framerate", "24", "-i", inj,
		"-c:v", "libx265", "-dolbyvision", "1", "-threads", "2", "-pix_fmt", "yuv420p10le", "-crf", "12",
		"-x265-params", dvColourParams+":vbv-maxrate=20000:vbv-bufsize=20000", out)
	return out
}

func l5Of(top, bottom, left, right int) string {
	return `{"active_area_top_offset":` + itoa(top) + `,"active_area_bottom_offset":` + itoa(bottom) +
		`,"active_area_left_offset":` + itoa(left) + `,"active_area_right_offset":` + itoa(right) + `}`
}

// decideDV reads src the way the engine does and decides its crop.
func decideDV(t *testing.T, tl dynhdr.Tools, src string) (Decision, *L5Reading) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	c := Detect(ctx, tl.FFmpeg, src, 1, sd)
	r := &L5Reading{}
	frames, rateErr := dynhdr.RewriteFacts(ctx, tl.FFprobe, src)
	if rateErr != nil {
		r.FrameRate = rateErr.Error()
	} else if recs, err := dynhdr.ReadL5(ctx, tl, src, filepath.Join(dir, "r.bin"), filepath.Join(dir, "l5.json")); err != nil {
		r.Failed = err.Error()
	} else {
		r.Frames, r.L5 = frames, map[int]Edges{}
		for _, x := range recs {
			r.L5[x.Frame] = Edges{Left: x.Left, Right: x.Right, Top: x.Top, Bottom: x.Bottom}
		}
	}
	return Decide(Inputs{Frame: sd, PixelFormats: []string{"yuv420p10le", "yuv420p10le"}, Consensus: c,
		DolbyVision: DolbyVision{Present: true, L5: r}}), r
}

// TestDolbyVisionFixture_L5MatchingTheBarsCropsToL5: L5 40/40 over 40 px bars crops to
// 320:160:0:40, L5's own rectangle, with the zeroing asked for, and the bars pass the
// blackness check.
func TestDolbyVisionFixture_L5MatchingTheBarsCropsToL5(t *testing.T) {
	tl := dvTools(t)
	src := dvLetterbox(t, tl, t.TempDir(), l5Of(40, 40, 0, 0), "", "")
	d, r := decideDV(t, tl, src)
	if !d.ZeroesL5() || d.Rect.String() != sdPicture {
		t.Fatalf("decided %+v (L5 reading frames %d, %d records), want %s from L5", d, r.Frames, len(r.L5), sdPicture)
	}
	if err := Blackness(context.Background(), tl.FFmpeg, src, d.Rect, sd, "yuv420p10le"); err != nil {
		t.Fatalf("the bars failed the blackness check: %v", err)
	}
}

// TestDolbyVisionFixture_EveryOtherSourceIsRefused: each named refusal on its own real file.
func TestDolbyVisionFixture_EveryOtherSourceIsRefused(t *testing.T) {
	tl := dvTools(t)
	cases := []struct {
		name, level5, edit, want string
	}{
		{"L5 zero with bars", "", "", ReasonL5Absent},
		{"L5 varying 0-11 vs 12-23", l5Of(40, 40, 0, 0), `{"active_area":{"presets":[{"id":0,"left":0,"right":0,` +
			`"top":40,"bottom":40},{"id":1,"left":0,"right":0,"top":20,"bottom":20}],"edits":{"0-11":0,"12-23":1}}}`, ReasonL5Varies},
		{"odd L5 41/39", l5Of(41, 39, 0, 0), "", ReasonL5Odd},
		{"L5 20/20 over 40 px bars", l5Of(20, 20, 0, 0), "", ReasonL5Disagrees},
		{"L5 dropped", l5Of(40, 40, 0, 0), `{"active_area":{"drop_l5":"all"}}`, ReasonL5Absent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := dvLetterbox(t, tl, t.TempDir(), tc.level5, tc.edit, "")
			d, _ := decideDV(t, tl, src)
			if d.Applied() || d.Reason != tc.want {
				t.Fatalf("decided %+v, want refused %s", d, tc.want)
			}
		})
	}
}

// TestDolbyVisionFixture_TextInABarIsRefusedByTheBlacknessCheck: a two-pixel-wide mark burned
// into the bottom bar on every frame is below cropdetect's row-mean limit, so the samples and
// L5 agree on 40/40 and the decision crops; the blackness check reads the mark and refuses.
func TestDolbyVisionFixture_TextInABarIsRefusedByTheBlacknessCheck(t *testing.T) {
	tl := dvTools(t)
	src := dvLetterbox(t, tl, t.TempDir(), l5Of(40, 40, 0, 0), "", "drawbox=x=150:y=220:w=2:h=6:color=white:t=fill")
	d, _ := decideDV(t, tl, src)
	if !d.ZeroesL5() {
		t.Fatalf("decided %+v: the mark was meant to pass cropdetect, so this case would not reach the blackness check", d)
	}
	err := Blackness(context.Background(), tl.FFmpeg, src, d.Rect, sd, "yuv420p10le")
	var nb *NotBlackError
	if !errors.As(err, &nb) || nb.Side != "bottom" {
		t.Fatalf("the blackness check passed a bar with a mark in it (%v)", err)
	}
}

// TestDolbyVisionFixture_VariableFrameRateIsRefused: the same letterbox, retimed so its second
// half runs at a different rate (an MP4, which keeps the DOVI record), is refused: the L5
// zeroing writes a raw stream read at one constant rate.
func TestDolbyVisionFixture_VariableFrameRateIsRefused(t *testing.T) {
	tl := dvTools(t)
	dir := t.TempDir()
	src := dvLetterbox(t, tl, dir, l5Of(40, 40, 0, 0), "", "")
	vfr := filepath.Join(dir, "vfr.mp4")
	run(t, tl.FFmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", src, "-map", "0:v", "-c", "copy", "-strict", "unofficial",
		"-tag:v", "dvh1", "-bsf:v", `setts=pts=if(lt(N\,12)\,PTS\,PTS+500):dts=if(lt(N\,12)\,DTS\,DTS+500)`, vfr)
	d, r := decideDV(t, tl, vfr)
	if d.Applied() || d.Reason != ReasonL5FrameRate || r.FrameRate == "" {
		t.Fatalf("the variable-rate source decided %+v (%q), want refused %s", d, r.FrameRate, ReasonL5FrameRate)
	}
}

// TestDolbyVisionFixture_AFailingDoviToolIsNeverACrop: a dovi_tool that exits non-zero, writes
// malformed JSON, or omits L5 is a named refusal on a source whose L5 is a perfect 40/40.
func TestDolbyVisionFixture_AFailingDoviToolIsNeverACrop(t *testing.T) {
	tl := dvTools(t)
	dir := t.TempDir()
	src := dvLetterbox(t, tl, dir, l5Of(40, 40, 0, 0), "", "")
	for _, tc := range []struct{ name, body, exit, want string }{
		{"exits non-zero", "[]", "1", ReasonL5Unreadable},
		{"malformed JSON", "{oops", "0", ReasonL5Unreadable},
		{"omits L5", "[]", "0", ReasonL5Absent},
	} {
		fake := write(t, dir, "dovi-"+tc.exit+itoa(len(tc.body)), "#!/bin/sh\ncase \"$1\" in\nextract-rpu) cat > /dev/null; : > \"$4\";;\n"+
			"export) out=$(echo \"$5\" | sed 's/^level5=//'); printf '%s' '"+tc.body+"' > \"$out\";;\nesac\nexit "+tc.exit+"\n")
		if err := os.Chmod(fake, 0o755); err != nil {
			t.Fatal(err)
		}
		ft := tl
		ft.DoviTool = fake
		d, _ := decideDV(t, ft, src)
		if d.Applied() || d.Reason != tc.want {
			t.Errorf("%s: decided %+v, want refused %s", tc.name, d, tc.want)
		}
	}
}

func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
