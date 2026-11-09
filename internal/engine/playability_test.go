package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/playability/playabilitytest"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// THE PLAYABILITY GATE, END TO END (issue #200, docs/design/playability.md). The job's own encode
// runs through the real FFmpegEncoder with x265 held to a keyframe every second, open GOPs and
// four B-frames, so every keyframe after the first is followed by RASL pictures. The control
// swaps. The bite relabels those RASL pictures RADL in the encoder's output: the linear decode
// is the same frames, so every faithfulness gate - codec, length, size, streams, fidelity, the
// decode-integrity gate and VMAF - passes it, and a decoder starting at any of those keyframes
// decodes pictures that reference one it never saw. The gate refuses it, deterministically, with
// the VMAF proof that everything before it passed on the row, and the source byte-identical.

// openGOPEncoder is the job's own encoder with libx265 held to playabilitytest.OpenGOPParams,
// through a stand-in that appends them to the -x265-params the job builds; mislabel relabels the
// output's RASL pictures after the encode.
func openGOPEncoder(t *testing.T, ffmpeg, ffprobe string, cfg config.Config, prober *probe.Prober, mislabel bool) (Encoder, *int) {
	t.Helper()
	real, err := exec.LookPath(ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "ffmpeg")
	script := `#!/bin/sh
first=1
for a in "$@"; do
  if [ $first = 1 ]; then set --; first=0; fi
  case "$a" in log-level=error*) a="$a:` + playabilitytest.OpenGOPParams + `" ;; esac
  set -- "$@" "$a"
done
exec ` + real + ` "$@"
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	relabelled := new(int)
	var mu sync.Mutex
	return EncoderFunc(func(ctx context.Context, in, out string, props *probe.VideoProps) error {
		if err := (FFmpegEncoder{FFmpeg: stub, Cfg: cfg, Probe: prober}).Encode(ctx, in, out, props); err != nil {
			return err
		}
		if !mislabel {
			return nil
		}
		bad := out + ".mislabelled"
		n, err := playabilitytest.Mislabel(ffmpeg, ffprobe, out, bad, "matroska")
		if err != nil {
			return err
		}
		mu.Lock()
		*relabelled += n
		mu.Unlock()
		return os.Rename(bad, out)
	}), relabelled
}

func playabilityRun(t *testing.T, mislabel bool) (src, before string, st *testStore, events []Event, relabelled int) {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src = filepath.Join(root, "movie.mkv")
	mkH264Long(t, ffmpeg, src, "8M")
	before = md5f(t, src)
	cfg := baseCfg(root)
	everyGate(&cfg)
	st = newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)
	enc, n := openGOPEncoder(t, ffmpeg, ffprobe, cfg, prober, mislabel)
	eng := New(cfg, prober, enc, st, discardLogger())
	var mu sync.Mutex
	eng.Observer = func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	return src, before, st, events, *n
}

// The control: the same job's valid open-GOP output - RASL pictures and all - passes the
// playability gate and replaces the source. The gate must not refuse a file every conforming
// player plays.
func TestPlayability_AValidOpenGOPOutputSwaps(t *testing.T) {
	src, before, st, _, _ := playabilityRun(t, false)
	doneWithVmaf(t, st)
	if md5f(t, src) == before {
		t.Fatal("the source was not replaced")
	}
}

// THE BITE: the output passes every gate before this one and is refused by it.
func TestPlayability_LeadingPicturesThatNeedAPriorPictureKeepTheSource(t *testing.T) {
	src, before, st, events, relabelled := playabilityRun(t, true)
	if relabelled == 0 {
		t.Fatal("the encode carried no RASL picture to relabel: the case would prove nothing")
	}
	if md5f(t, src) != before {
		t.Fatal("the source was modified by an output that does not play from a cold start")
	}
	row := onlyRow(t, st)
	o := row.Outcome
	if row.Status != store.Failed || o.FailureClass != store.FailureDeterministic {
		t.Fatalf("row %q class %q (%s), want failed and deterministic", row.Status, o.FailureClass, o.Reason)
	}
	if !strings.Contains(o.Reason, "playability gate: a decode started cold at keyframe") ||
		!strings.Contains(o.Reason, "followed by leading pictures") || !strings.Contains(o.Reason, "The source is kept") {
		t.Errorf("the reason does not say where the cold start failed: %s", o.Reason)
	}
	// Every faithfulness gate passed it: the perceptual gate, which runs before this one, scored
	// it above its floors and the score is on the failed row.
	if o.VmafMean == nil || *o.VmafMean < 90 || o.VmafMin == nil || *o.VmafMin < 50 {
		t.Errorf("the rejected row carries no passing VMAF measurement (%v, %v): the case must pass every "+
			"gate but this one", o.VmafMean, o.VmafMin)
	}
	var gates []string
	for _, ev := range events {
		if ev.Status == store.Failed {
			gates = append(gates, ev.Gate)
		}
	}
	if len(gates) != 1 || gates[0] != GatePlayability {
		t.Errorf("the failure was attributed to %v, want [%s]", gates, GatePlayability)
	}
	if nTemp(t, filepath.Dir(src)) != 0 {
		t.Error("the refused output was left beside the source")
	}
}

func TestPlayability_GateIsInTheVocabulary(t *testing.T) {
	for _, g := range GateVocabulary {
		if g == GatePlayability {
			return
		}
	}
	t.Errorf("GatePlayability is missing from GateVocabulary %v", GateVocabulary)
}

// playabilityHardwareArgs carries the decode half of each hardware path only.
func TestPlayability_HardwareArgs(t *testing.T) {
	for _, tc := range []struct {
		v    VideoPlan
		want string
	}{
		{VideoPlan{Decode: DecodeSoftware}, ""},
		{VideoPlan{Decode: DecodeCUDA}, "-hwaccel cuda"},
		{VideoPlan{Decode: DecodeVAAPI, DecodeDevice: "/dev/dri/renderD129"},
			"-init_hw_device vaapi=hfva:/dev/dri/renderD129,connection_type=drm -hwaccel vaapi -hwaccel_device hfva"},
	} {
		if got := strings.Join(playabilityHardwareArgs(tc.v), " "); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.v.Decode, got, tc.want)
		}
	}
}
