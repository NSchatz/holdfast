package dynhdr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// L5 (docs/design/crop.md#dolby-vision): reading it, zeroing it in the pre-pass, and the L5
// gate, on real synthetic Dolby Vision letterboxes (proposal-crop-dv.md, "Test plan").

// dvLetterbox writes a profile 8.1 Matroska source: testsrc2 320x160 padded to 320x240 with 40
// px black bars, 10-bit PQ with a mastering display, an RPU from `dovi_tool generate` carrying
// level5 (a JSON object, "" for the generator's own zero L5) and level6, edited by edit (an
// editor config, "" for none) and injected, then encoded with -dolbyvision 1.
func dvLetterbox(t *testing.T, tl Tools, dir, name, level5, edit string) string {
	t.Helper()
	params := x265Colour("smpte2084")
	base := filepath.Join(dir, name+".base.hevc")
	mustRun(t, tl.FFmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=s=320x160:r=24:d=1,format=yuv420p10le,pad=320:240:0:40:black", "-pix_fmt", "yuv420p10le",
		"-c:v", "libx265", "-threads", "2", "-x265-params", params, base)
	l5 := ""
	if level5 != "" {
		l5 = `"level5":` + level5 + `,`
	}
	gen := writeFile(t, dir, name+".gen.json", `{"cm_version":"V40","profile":"8.1","length":24,`+l5+
		`"level6":{"max_display_mastering_luminance":1000,"min_display_mastering_luminance":1,`+
		`"max_content_light_level":1000,"max_frame_average_light_level":400}}`)
	rpu := filepath.Join(dir, name+".rpu.bin")
	mustRun(t, tl.DoviTool, "generate", "-j", gen, "-o", rpu)
	if edit != "" {
		edited := filepath.Join(dir, name+".edited.bin")
		mustRun(t, tl.DoviTool, "editor", "-i", rpu, "-j", writeFile(t, dir, name+".edit.json", edit), "-o", edited)
		rpu = edited
	}
	inj := filepath.Join(dir, name+".inj.hevc")
	mustRun(t, tl.DoviTool, "inject-rpu", "-i", base, "--rpu-in", rpu, "-o", inj)
	out := filepath.Join(dir, name+".mkv")
	mustRun(t, tl.FFmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "hevc", "-framerate", "24", "-i", inj,
		"-f", "lavfi", "-i", "sine=d=1", "-map", "0:v", "-map", "1:a", "-c:a", "flac",
		"-c:v", "libx265", "-dolbyvision", "1", "-threads", "2", "-pix_fmt", "yuv420p10le", "-crf", "12",
		"-x265-params", params+":vbv-maxrate=20000:vbv-bufsize=20000", out)
	return out
}

const l5Bars40 = `{"active_area_top_offset":40,"active_area_bottom_offset":40,"active_area_left_offset":0,"active_area_right_offset":0}`

// TestL5_TheArgvOfEveryL5Step is the golden command line of each dovi_tool step: the pre-pass
// for profile 8.1 and for a converted profile 7, the profile 7 conversion without a crop, and
// the gate's extract-rpu and export. None pairs --edit-config with -m or -c, which it would
// silently switch off.
func TestL5_TheArgvOfEveryL5Step(t *testing.T) {
	cases := map[string][]string{
		"pre-pass 8.1":         ConvertArgs(Intent{DolbyVision: true, ZeroL5: true}, "/w/raw"),
		"pre-pass 7 converted": ConvertArgs(Intent{DolbyVision: true, Convert: true, ZeroL5: true}, "/w/raw"),
		"7 converted, no crop": ConvertArgs(Intent{DolbyVision: true, Convert: true}, "/w/raw"),
	}
	want := map[string]string{
		"pre-pass 8.1":         "-m 0 -c convert - -o /w/raw",
		"pre-pass 7 converted": "-m 2 -c convert --discard - -o /w/raw",
		"7 converted, no crop": "-m 2 convert --discard - -o /w/raw",
	}
	for name, args := range cases {
		got := strings.Join(args, " ")
		if got != want[name] {
			t.Errorf("%s: %q, want %q", name, got, want[name])
		}
		if strings.Contains(got, "--edit-config") {
			t.Errorf("%s pairs --edit-config with the mode: %q", name, got)
		}
	}
	steps := L5Args("/w/rpu", "/w/l5.json")
	if len(steps) != 2 || strings.Join(steps[0], " ") != "extract-rpu - -o /w/rpu" ||
		strings.Join(steps[1], " ") != "export -i /w/rpu -l level5=/w/l5.json -f json" {
		t.Errorf("the L5 read is %q", steps)
	}
	for _, s := range steps {
		if strings.Contains(strings.Join(s, " "), " -d ") {
			t.Errorf("the L5 read uses export -d, which shows a missing L5 as zero: %q", s)
		}
	}
	// The intent's rewriting, and what it needs.
	if (Intent{DolbyVision: true}).Rewrites() || !(Intent{DolbyVision: true, ZeroL5: true}).Rewrites() ||
		(Intent{HDR10Plus: true, ZeroL5: true}).Rewrites() || !(Intent{DolbyVision: true, Convert: true}).Rewrites() {
		t.Error("Intent.Rewrites is wrong")
	}
	if !(Intent{DolbyVision: true, ZeroL5: true}).NeedsDoviTool() || (Intent{DolbyVision: true}).NeedsDoviTool() {
		t.Error("an L5 zeroing does not need dovi_tool, or a plain carry does")
	}
}

func TestL5_ParseAndTheZeroGate(t *testing.T) {
	recs, err := ParseL5([]byte(`[{"frame":0,"active_area_left_offset":0,"active_area_right_offset":0,` +
		`"active_area_top_offset":40,"active_area_bottom_offset":40},{"frame":1,"active_area_top_offset":0}]`))
	if err != nil || len(recs) != 2 || recs[0].Top != 40 || recs[0].Zero() || !recs[1].Zero() {
		t.Fatalf("ParseL5 = %+v, %v", recs, err)
	}
	for _, bad := range []string{"", "{}", "null", "[", `[{"frame":-1}]`, `[{"frame":0},{"frame":0}]`,
		`[{"frame":0,"active_area_top_offset":-2}]`, `[{"frame":0,"active_area_left_offset":-1}]`,
		`[{"frame":0,"active_area_right_offset":-1}]`, `[{"frame":0,"active_area_bottom_offset":-1}]`} {
		if _, err := ParseL5([]byte(bad)); err == nil {
			t.Errorf("ParseL5(%q) parsed", bad)
		}
	}
	zero := func(n int) []L5Record {
		out := make([]L5Record, n)
		for i := range out {
			out[i].Frame = i
		}
		return out
	}
	if err := CheckZeroL5(zero(24), 24); err != nil {
		t.Fatalf("24 zeroed records for 24 frames: %v", err)
	}
	stale := zero(24)
	stale[5].Top, stale[5].Bottom = 40, 40
	for name, c := range map[string]struct {
		recs   []L5Record
		frames int
	}{
		"a dropped L5 (no records)":    {nil, 24},
		"one frame without L5":         {zero(23), 24},
		"more records than frames":     {zero(25), 24},
		"a stale L5 on one frame":      {stale, 24},
		"frames not counted":           {zero(24), 0},
		"a record past the last frame": {append(zero(23), L5Record{Frame: 30}), 24},
	} {
		err := CheckZeroL5(c.recs, c.frames)
		var ge *GateError
		if !errors.As(err, &ge) || ge.Gate != GateDoviL5 {
			t.Errorf("%s: %v, want the L5 gate's refusal", name, err)
		}
	}
}

// TestRealFixture_TheL5ZeroingCropsAndItsGateRedsWithoutTheFlag is the pre-pass end to end
// and the bite. The source's L5 reads 40/40 on all 24 frames; the pre-pass with -c writes a
// raw stream whose cropped encode carries 0/0/0/0 on every frame with an RPU on every frame,
// and the L5 gate passes it. The same pre-pass with -c dropped leaves L5 stale at 40/40 (the
// observed ffmpeg behaviour, P5 finding 2), and the L5 gate reds on it.
func TestRealFixture_TheL5ZeroingCropsAndItsGateRedsWithoutTheFlag(t *testing.T) {
	tl := realTools(t)
	dir := t.TempDir()
	ctx := context.Background()
	src := dvLetterbox(t, tl, dir, "lb", l5Bars40, "")
	recs, err := ReadL5(ctx, tl, src, filepath.Join(dir, "src.rpu"), filepath.Join(dir, "src.l5.json"))
	if err != nil || len(recs) != fixtureFrames {
		t.Fatalf("reading the source's L5: %d records, %v", len(recs), err)
	}
	for _, r := range recs {
		if r.Top != 40 || r.Bottom != 40 || r.Left != 0 || r.Right != 0 {
			t.Fatalf("source frame %d's L5 is %+v, want 40/40", r.Frame, r)
		}
	}
	frames, rateErr := RewriteFacts(ctx, tl.FFprobe, src)
	if rateErr != nil || frames != fixtureFrames {
		t.Fatalf("RewriteFacts = %d, %v", frames, rateErr)
	}
	in := Intent{DolbyVision: true, SourceProfile: 8, ZeroL5: true}
	p, err := Prepare(ctx, Request{Tools: tl, Intent: in, Source: src, RawPath: filepath.Join(dir, "raw.hevc"),
		JSONPath: filepath.Join(dir, "x.json"), HeadPath: filepath.Join(dir, "head.bin")})
	if err != nil || p.RawVideo == "" || p.FrameRate.String() != "24/1" {
		t.Fatalf("Prepare: %+v, %v", p, err)
	}
	encodeCropped := func(out string, p *Prepared) {
		args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-i", src}
		args = append(append(args, p.InputArgs()...), "-map", "1:v:0", "-map", "0:a", "-c", "copy",
			"-c:v", "libx265", "-threads", "2", "-pix_fmt", "yuv420p10le", "-preset", "ultrafast", "-crf", "30",
			"-vf", "crop=320:160:0:40:exact=1", "-x265-params", x265Colour("smpte2084")+p.X265Params())
		mustRun(t, tl.FFmpeg, append(append(args, p.CodecArgs()...), "-f", "matroska", out)...)
	}
	out := filepath.Join(dir, "out.mkv")
	encodeCropped(out, p)
	if err := gate(t, tl, out, p.Expect()); err != nil {
		t.Fatalf("the cropped output fails the RPU gates: %v", err)
	}
	counts, err := Count(ctx, tl.FFprobe, out)
	if err != nil || counts.Frames != fixtureFrames || counts.DoviRPU != fixtureFrames {
		t.Fatalf("counts %+v, %v", counts, err)
	}
	got, err := ReadL5(ctx, tl, out, filepath.Join(dir, "out.rpu"), filepath.Join(dir, "out.l5.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckZeroL5(got, counts.Frames); err != nil {
		t.Fatalf("the zeroed output fails the L5 gate: %v", err)
	}

	// THE BITE: the same rewrite without -c.
	stale := filepath.Join(dir, "stale.hevc")
	if err := pipeline(annexB(ctx, tl.FFmpeg, src), exec.CommandContext(ctx, tl.DoviTool, "-m", "0", "convert", "-", "-o", stale)); err != nil {
		t.Fatal(err)
	}
	ps := *p
	ps.RawVideo = stale
	staleOut := filepath.Join(dir, "stale.mkv")
	encodeCropped(staleOut, &ps)
	got, err = ReadL5(ctx, tl, staleOut, filepath.Join(dir, "stale.rpu"), filepath.Join(dir, "stale.l5.json"))
	if err != nil {
		t.Fatal(err)
	}
	err = CheckZeroL5(got, counts.Frames)
	var ge *GateError
	if !errors.As(err, &ge) || !strings.Contains(err.Error(), "0/0/40/40") {
		t.Fatalf("a pre-pass without -c passed the L5 gate (%v): the gate does not bite", err)
	}
}

// TestRealFixture_ADroppedL5IsRefusedAlthoughExportDReadsZero: an RPU whose L5 the editor
// dropped (`drop_l5`) carries no L5 block. `export -d level5` writes it as 0/0/0/0, exactly
// like a zeroed one; `export -l level5` writes no record, and the gate refuses it.
func TestRealFixture_ADroppedL5IsRefusedAlthoughExportDReadsZero(t *testing.T) {
	tl := realTools(t)
	dir := t.TempDir()
	ctx := context.Background()
	src := dvLetterbox(t, tl, dir, "dropped", l5Bars40, `{"active_area":{"drop_l5":"all"}}`)
	rpu := filepath.Join(dir, "d.rpu")
	recs, err := ReadL5(ctx, tl, src, rpu, filepath.Join(dir, "d.l5.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("a dropped L5 exported %d records", len(recs))
	}
	if err := CheckZeroL5(recs, fixtureFrames); err == nil {
		t.Fatal("the L5 gate passed a file with no L5")
	}
	// The contrast that decides the reader: -d shows it as zero.
	dpath := filepath.Join(dir, "d.export.json")
	mustRun(t, tl.DoviTool, "export", "-i", rpu, "-d", "level5="+dpath)
	var cfg struct {
		ActiveArea struct {
			Presets []struct{ Left, Right, Top, Bottom int } `json:"presets"`
		} `json:"active_area"`
	}
	data, _ := os.ReadFile(dpath)
	if err := json.Unmarshal(data, &cfg); err != nil || len(cfg.ActiveArea.Presets) != 1 ||
		cfg.ActiveArea.Presets[0] != (struct{ Left, Right, Top, Bottom int }{}) {
		t.Fatalf("export -d did not show the dropped L5 as 0/0/0/0 (%s, %v): the quirk this reader avoids moved", data, err)
	}
}

// TestL5_AFailingOrLyingDoviToolIsAnError: a dovi_tool that exits non-zero, writes malformed
// JSON, or writes nothing parseable is an error, never a reading; one that omits L5 is a
// reading with no records, which the crop decision refuses as absent.
func TestL5_AFailingOrLyingDoviToolIsAnError(t *testing.T) {
	real := realTools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	mustRun(t, real.FFmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=s=64x64:r=24:d=0.2",
		"-c:v", "libx265", "-x265-params", "log-level=error", src)
	fake := func(name, exportBody string, exit int) Tools {
		p := filepath.Join(dir, name)
		script := "#!/bin/sh\ncase \"$1\" in\nextract-rpu) cat > /dev/null; : > \"$4\";;\nexport) out=$(echo \"$5\" | sed 's/^level5=//'); " +
			"printf '%s' '" + exportBody + "' > \"$out\";;\nesac\nexit " + string(rune('0'+exit)) + "\n"
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return Tools{DoviTool: p, FFmpeg: real.FFmpeg}
	}
	ctx := context.Background()
	for name, tl := range map[string]Tools{
		"exits non-zero": fake("fail", "[]", 1),
		"malformed JSON": fake("garbage", "{not json", 0),
		"not a list":     fake("object", `{"frame":0}`, 0),
		"no binary":      {DoviTool: filepath.Join(dir, "absent"), FFmpeg: real.FFmpeg},
	} {
		if recs, err := ReadL5(ctx, tl, src, filepath.Join(dir, name+".rpu"), filepath.Join(dir, name+".json")); err == nil {
			t.Errorf("%s: read %d records and no error", name, len(recs))
		}
	}
	recs, err := ReadL5(ctx, fake("omits", "[]", 0), src, filepath.Join(dir, "o.rpu"), filepath.Join(dir, "o.json"))
	if err != nil || len(recs) != 0 {
		t.Errorf("a tool omitting L5 gave %d records, %v; want an empty reading", len(recs), err)
	}
}
