package vmaf

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/cpuquota"
	"github.com/NSchatz/holdfast/internal/encoder"
)

// The shipped libx265 knobs every S0161 candidate is encoded at, so the only thing that
// differs between them is the parallelism: crf and preset are config's defaults, and the
// 10-bit output is what pixel_format: auto derives for this 8-bit source.
const (
	s0161CRF    = "22"
	s0161Preset = "slow"
)

// s0161Derived is the parallelism a run derives here: this host's own CPU quota when it
// carries one, and otherwise the 24-CPU quota the operator measured, so the setting stays
// a quota-derived figure distinct from libx265's host default on a host with no quota.
func s0161Derived(t *testing.T) encoder.X265Parallelism {
	t.Helper()
	if q, err := cpuquota.Read(cpuquota.DefaultRoot); err == nil && q.Limited {
		return encoder.X265ParallelismFor(cpuquota.Divide(q, 1))
	}
	t.Logf("this host carries no readable CPU quota, so the derived setting is the measured 24 CPUs")
	return encoder.X265ParallelismFor(24)
}

// s0161Encode encodes ref with libx265 at the shipped knobs plus params, with vf (a video
// filter, or "") in front of it.
func s0161Encode(t *testing.T, bin, ref, out, vf, crf, preset, params string) {
	t.Helper()
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", ref}
	if vf != "" {
		args = append(args, "-vf", vf)
	}
	args = append(args, "-c:v", "libx265", "-crf", crf, "-preset", preset,
		"-x265-params", "log-level=error"+params, "-pix_fmt", comparisonPixFmt, out)
	mustFF(t, bin, args...)
}

func s0161Score(t *testing.T, bin, dist, ref string) Result {
	t.Helper()
	r, err := Score(context.Background(), bin, Request{Distorted: dist, Reference: ref, Subsample: 1,
		Threads: 1, Model: "version=vmaf_v0.6.1", PixelFormat: comparisonPixFmt})
	if err != nil {
		t.Fatalf("Score(%s): %v", filepath.Base(dist), err)
	}
	return r
}

// TestS0161_AC11_ParallelismDoesNotMoveTheGatesVerdict is the controlled experiment AC-11
// asks for: one reference source, encoded at identical profile knobs under three
// parallelism settings - one frame thread on a one-thread pool, the figure a quota derives,
// and libx265's own host default - and scored by the shipped gate at the shipped floors.
// All three carry the same verdict. The scores are logged, because they are what the
// recorded measurement quotes.
//
// Two anti-vacuity arms. The verdict they share must be PASSED: an honest encode at the
// shipped crf that the shipped gate refused would make "the same verdict" a statement about
// a gate that passes nothing. And the three candidates must not all be the same bytes, or
// the parallelism would not have been the variable at all.
func TestS0161_AC11_ParallelismDoesNotMoveTheGatesVerdict(t *testing.T) {
	bin := ffmpegBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: ffmpeg required for the parallelism verdict proof: %v", err)
	}
	dir := t.TempDir()
	ref := filepath.Join(dir, "ref.mkv")
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", fixtureContent,
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "12M", "-pix_fmt", "yuv420p", ref)

	derived := s0161Derived(t)
	settings := []struct{ name, params string }{
		{"single frame thread", encoder.X265ParallelismFor(1).Params()},
		{"derived quota figure", derived.Params()},
		{"libx265 host default", ""},
	}
	var bodies [][]byte
	var mean, pool, chroma []float64
	var verdicts []bool
	for i, s := range settings {
		out := filepath.Join(dir, "candidate-"+string(rune('a'+i))+".mkv")
		s0161Encode(t, bin, ref, out, "", s0161CRF, s0161Preset, s.params)
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, b)
		r := s0161Score(t, bin, out, ref)
		pass := passesFloors(r, shippedMinVmaf, shippedMinPool, shippedMinChroma)
		mean, pool, chroma = append(mean, r.HarmonicMean), append(pool, r.Min), append(chroma, r.ChromaMin)
		verdicts = append(verdicts, pass)
		t.Logf("%-22s -x265-params log-level=error%-32s harmonic_mean=%.4f min=%.4f chroma=%.4f -> %s",
			s.name, s.params, r.HarmonicMean, r.Min, r.ChromaMin, verdict(pass))
	}
	t.Logf("spread across the three: harmonic_mean=%.4f min=%.4f chroma=%.4f",
		spread(mean), spread(pool), spread(chroma))
	t.Logf("candidate bytes: %d / %d / %d; single == derived: %v; derived == host default: %v",
		len(bodies[0]), len(bodies[1]), len(bodies[2]), bytes.Equal(bodies[0], bodies[1]), bytes.Equal(bodies[1], bodies[2]))

	for i := range settings {
		if verdicts[i] != verdicts[0] {
			t.Errorf("the gate %s the %q candidate and %s the %q one: a parallelism setting moved the "+
				"verdict that licenses deleting a source", verdict(verdicts[0]), settings[0].name,
				verdict(verdicts[i]), settings[i].name)
		}
	}
	if !verdicts[0] {
		t.Errorf("the gate REJECTED the single-frame-thread encode at the shipped crf, so the agreement " +
			"above is agreement about a gate that passes nothing")
	}
	if bytes.Equal(bodies[0], bodies[1]) && bytes.Equal(bodies[0], bodies[2]) {
		t.Error("all three candidates are the same bytes, so the parallelism was not the variable")
	}
}

// TestS0161_AC12_ADerivedFigureNeverMakesARefusedCandidatePass: the chroma-damaged
// candidate the gate refuses today - chroma_test.go's fixture, at libx265's own default
// parallelism - is still refused when it is encoded under a quota-derived figure.
func TestS0161_AC12_ADerivedFigureNeverMakesARefusedCandidatePass(t *testing.T) {
	bin := ffmpegBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: ffmpeg required for the damaged-candidate proof: %v", err)
	}
	dir := t.TempDir()
	ref := filepath.Join(dir, "ref.mkv")
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", fixtureContent,
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "12M", "-pix_fmt", "yuv420p", ref)

	today := filepath.Join(dir, "damaged-today.mkv")
	s0161Encode(t, bin, ref, today, "hue=s="+chromaDesaturation, fixtureCRF, "veryfast", "")
	r := s0161Score(t, bin, today, ref)
	t.Logf("damaged, libx265 default: harmonic_mean=%.4f min=%.4f chroma=%.4f", r.HarmonicMean, r.Min, r.ChromaMin)
	if passesFloors(r, shippedMinVmaf, shippedMinPool, shippedMinChroma) {
		t.Fatalf("the gate PASSES the damaged candidate at libx265's default parallelism, so it is not " +
			"one the gate refuses today and this proof decides nothing")
	}

	for _, p := range []encoder.X265Parallelism{s0161Derived(t), encoder.X265ParallelismFor(24)} {
		out := filepath.Join(dir, "damaged"+p.Params()+".mkv")
		s0161Encode(t, bin, ref, out, "hue=s="+chromaDesaturation, fixtureCRF, "veryfast", p.Params())
		r := s0161Score(t, bin, out, ref)
		t.Logf("damaged, log-level=error%s: harmonic_mean=%.4f min=%.4f chroma=%.4f", p.Params(),
			r.HarmonicMean, r.Min, r.ChromaMin)
		if passesFloors(r, shippedMinVmaf, shippedMinPool, shippedMinChroma) {
			t.Errorf("the gate PASSES the damaged candidate encoded with %s: a parallelism setting made "+
				"a refused candidate deletable", p.Params())
		}
	}
}

func spread(v []float64) float64 {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, x := range v {
		lo, hi = math.Min(lo, x), math.Max(hi, x)
	}
	return hi - lo
}
