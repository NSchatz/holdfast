package vmaf

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The shipped defaults, restated here so the fixture is graded against the gate an
// operator actually runs rather than against numbers chosen to make it pass.
const (
	shippedMinVmaf     = 95.0 // config: min_vmaf
	shippedMinPool     = 60.0 // config: vmaf_min_pool
	shippedMinChroma   = 30.0 // config: vmaf_min_chroma (GATE-4)
	comparisonPixFmt   = "yuv420p10le"
	fixtureContent     = "testsrc2=duration=5:size=320x240:rate=24"
	fixtureCRF         = "20"
	honestFixtureCRF   = "22" // the shipped default crf
	chromaDesaturation = "0.85"
)

// TestChromaOnlyDamageIsInvisibleToTheLumaGate is the EVIDENCE for GATE-4's central
// claim, and it is a controlled experiment in the shape TRANSCODE-11 established:
// build one output whose damage is confined to the CHROMA planes, show that the gate
// as it stood ACCEPTS it, then show the chroma floor REJECTS it. A fixture the old
// gate already caught would prove nothing.
//
// What the fixture is. The luma plane is re-encoded normally and left alone; only U
// and V are touched, by a 15% desaturation - the mildest chroma damage measured that
// still clears the luma gate. This is not an exotic corruption: a colour-matrix
// mistake, a range mix-up or a bad chroma-plane conversion in a transcode chain all
// land here, and every one of them is invisible to everything holdfast had:
//
//   - the structural gates pass it - it decodes cleanly, and carries the same
//     duration, packet count and stream counts as any faithful encode;
//   - the pooled harmonic mean passes it, comfortably above min_vmaf=95;
//   - the worst-frame floor passes it, comfortably above vmaf_min_pool=60;
//
// because the VMAF model extracts LUMA FEATURES ONLY. The source would then have been
// atomically swapped and DELETED, with the operator holding a file whose colour is
// wrong and no original to go back to.
//
// The honest control encoded from the same source at the same shipped crf is measured
// in the same pass. It must sit WELL ABOVE the floor: without it a rejecting chroma
// floor would be indistinguishable from a floor that rejects everything.
func TestChromaOnlyDamageIsInvisibleToTheLumaGate(t *testing.T) {
	bin := ffmpegBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: ffmpeg required for the chroma-damage proof: %v", err)
	}
	dir := t.TempDir()
	ref := filepath.Join(dir, "ref.mkv")
	honest := filepath.Join(dir, "honest.mkv")
	damaged := filepath.Join(dir, "chroma-damaged.mkv")

	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", fixtureContent,
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "12M", "-pix_fmt", "yuv420p", ref)

	// The honest control: the encode holdfast would make at its shipped crf.
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-i", ref,
		"-c:v", "libx265", "-crf", honestFixtureCRF, "-preset", "veryfast",
		"-x265-params", "log-level=error", "-pix_fmt", comparisonPixFmt, honest)

	// The fixture: identical pipeline plus `hue=s=`, which scales the U and V planes
	// and touches Y not at all. The crf is LOWER than the honest control's, so the
	// luma is if anything better encoded - the only thing wrong with this file is its
	// colour, which is exactly the claim under test.
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-i", ref,
		"-vf", "hue=s="+chromaDesaturation,
		"-c:v", "libx265", "-crf", fixtureCRF, "-preset", "veryfast",
		"-x265-params", "log-level=error", "-pix_fmt", comparisonPixFmt, damaged)

	req := func(dist string) Request {
		return Request{Distorted: dist, Reference: ref, Subsample: 1,
			Model: "version=vmaf_v0.6.1", PixelFormat: comparisonPixFmt}
	}
	good, err := Score(context.Background(), bin, req(honest))
	if err != nil {
		t.Fatalf("Score(honest): %v", err)
	}
	bad, err := Score(context.Background(), bin, req(damaged))
	if err != nil {
		t.Fatalf("Score(chroma-damaged): %v", err)
	}
	t.Logf("honest   : harmonic_mean=%.2f raw_min=%.2f chroma=%.2f dB", good.HarmonicMean, good.Min, good.ChromaMin)
	t.Logf("damaged  : harmonic_mean=%.2f raw_min=%.2f chroma=%.2f dB", bad.HarmonicMean, bad.Min, bad.ChromaMin)
	t.Logf("floors   : min_vmaf=%.0f vmaf_min_pool=%.0f vmaf_min_chroma=%.0f",
		shippedMinVmaf, shippedMinPool, shippedMinChroma)

	// 1. The luma MEAN is blind to it. If this reds, the fixture is no longer
	// chroma-only damage and proves nothing about the chroma floor.
	if bad.HarmonicMean < shippedMinVmaf {
		t.Errorf("harmonic_mean=%.2f < min_vmaf=%.0f - the fixture no longer evades the pooled "+
			"mean, so a rejection below would just be the OLD gate firing again; re-tune the damage",
			bad.HarmonicMean, shippedMinVmaf)
	}
	// 2. The worst-frame floor is blind to it too. This is the gate TRANSCODE-11 added
	// to catch LOCAL luma damage, and chroma damage is not that.
	if bad.Min < shippedMinPool {
		t.Errorf("raw min=%.2f < vmaf_min_pool=%.0f - the fixture no longer evades the worst-frame "+
			"floor, so a rejection below would just be the OLD gate firing again; re-tune the damage",
			bad.Min, shippedMinPool)
	}
	// 3. Only the chroma floor sees it.
	if bad.ChromaMin >= shippedMinChroma {
		t.Errorf("chroma=%.2f dB >= vmaf_min_chroma=%.0f - the shipped floor does NOT catch an "+
			"output whose colour planes were damaged, and the source would be deleted; the gate has a hole",
			bad.ChromaMin, shippedMinChroma)
	}
	// 4. The honest control clears the floor with real headroom. A floor that rejects
	// good encodes is not a gate, it is a tool that never finishes.
	if good.ChromaMin < shippedMinChroma {
		t.Errorf("the HONEST encode measures chroma=%.2f dB, below vmaf_min_chroma=%.0f - the floor "+
			"rejects faithful encodes and must be lowered", good.ChromaMin, shippedMinChroma)
	}
	if margin := good.ChromaMin - shippedMinChroma; margin < 5 {
		t.Errorf("the honest encode clears the chroma floor by only %.2f dB - too little headroom "+
			"for a gate that deletes originals when it passes", margin)
	}
	// 5. And the two are actually separated by the floor, in the right direction.
	if bad.ChromaMin >= good.ChromaMin {
		t.Errorf("the chroma-damaged encode measures chroma=%.2f dB, not below the honest "+
			"encode's %.2f dB - the metric is not responding to the damage",
			bad.ChromaMin, good.ChromaMin)
	}
}

// TestChromaDamageLeavesLumaIntact is the other half of "chroma-only": it reads the
// PER-PLANE PSNR out of the same libvmaf pass and shows the fixture's damage is
// confined to Cb/Cr, with Y no worse than the honest control's.
//
// Without it, "chroma-only damage" would rest on the claim that `hue=s=` touches no
// luma - true, but an argument rather than a measurement, and this repo's rule is
// that a design decision which cannot rot into an unverifiable comment should not be
// one.
func TestChromaDamageLeavesLumaIntact(t *testing.T) {
	bin := ffmpegBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: ffmpeg required for the chroma-damage proof: %v", err)
	}
	dir := t.TempDir()
	ref := filepath.Join(dir, "ref.mkv")
	honest := filepath.Join(dir, "honest.mkv")
	damaged := filepath.Join(dir, "chroma-damaged.mkv")

	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", fixtureContent,
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "12M", "-pix_fmt", "yuv420p", ref)
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-i", ref,
		"-c:v", "libx265", "-crf", honestFixtureCRF, "-preset", "veryfast",
		"-x265-params", "log-level=error", "-pix_fmt", comparisonPixFmt, honest)
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-i", ref,
		"-vf", "hue=s="+chromaDesaturation,
		"-c:v", "libx265", "-crf", fixtureCRF, "-preset", "veryfast",
		"-x265-params", "log-level=error", "-pix_fmt", comparisonPixFmt, damaged)

	goodY, goodCb, goodCr := planePSNR(t, bin, honest, ref)
	badY, badCb, badCr := planePSNR(t, bin, damaged, ref)
	t.Logf("honest  : psnr_y=%.2f psnr_cb=%.2f psnr_cr=%.2f (dB)", goodY, goodCb, goodCr)
	t.Logf("damaged : psnr_y=%.2f psnr_cb=%.2f psnr_cr=%.2f (dB)", badY, badCb, badCr)

	if badY < goodY {
		t.Errorf("the fixture's LUMA (psnr_y=%.2f dB) is worse than the honest encode's (%.2f dB) - "+
			"the damage is not confined to the chroma planes, so this fixture does not test what it claims",
			badY, goodY)
	}
	for _, p := range []struct {
		name      string
		bad, good float64
	}{{"psnr_cb", badCb, goodCb}, {"psnr_cr", badCr, goodCr}} {
		if p.bad >= p.good {
			t.Errorf("%s: damaged=%.2f dB is not below honest=%.2f dB - the chroma planes were "+
				"not actually damaged", p.name, p.bad, p.good)
		}
	}
}

// planePSNR reads the per-plane pooled PSNR minima out of one libvmaf pass. Y is
// test-only: the shipped gate needs the chroma pair (see Result.ChromaMin), and the
// luma plane is read here solely to prove the fixture damaged what it claims to.
func planePSNR(t *testing.T, bin, distorted, reference string) (y, cb, cr float64) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "vmaf.json")
	filter := "[0:v]format=" + comparisonPixFmt + "[d];[1:v]format=" + comparisonPixFmt + "[r];" +
		"[d][r]libvmaf=model=version=vmaf_v0.6.1:feature=name=psnr:log_fmt=json:log_path=" +
		escapeFilterValue(logPath)
	mustFF(t, bin, "-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-i", distorted, "-i", reference, "-lavfi", filter, "-f", "null", "-")

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read vmaf log: %v", err)
	}
	var parsed struct {
		PooledMetrics struct {
			PsnrY  struct{ Min float64 } `json:"psnr_y"`
			PsnrCb struct{ Min float64 } `json:"psnr_cb"`
			PsnrCr struct{ Min float64 } `json:"psnr_cr"`
		} `json:"pooled_metrics"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse vmaf log: %v", err)
	}
	p := parsed.PooledMetrics
	return p.PsnrY.Min, p.PsnrCb.Min, p.PsnrCr.Min
}
