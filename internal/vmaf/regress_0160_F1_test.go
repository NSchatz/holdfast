package vmaf

// S0160 impl-gate 1, F1 (AC-9). Refuter artifact.

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegress_0160_F1_ChainNeedsScaleButPreflightSkipsIt(t *testing.T) {
	bin := ffmpegBin()
	dist, ref := mismatchedPair(t, bin) // 8-bit source vs 10-bit output: the default path
	named, _ := ComparisonFormat(probePixFmt(t, ref), probePixFmt(t, dist))
	r := Request{Distorted: dist, Reference: ref, Subsample: 1, Threads: 1,
		Model: "version=vmaf_v0.6.1", PixelFormat: named}
	out, err := exec.Command(bin, "-hide_banner", "-nostdin", "-loglevel", "verbose", "-y",
		"-i", dist, "-i", ref, "-lavfi", BuildFilter(r, filepath.Join(t.TempDir(), "v.json")),
		"-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("real gate graph failed: %v\n%s", err, out)
	}
	n := 0
	for _, ln := range strings.Split(string(out), "\n") {
		if strings.Contains(ln, "auto-inserting filter") && strings.Contains(ln, "scale") {
			n++
			t.Log(strings.TrimSpace(ln))
		}
	}
	if n == 0 {
		t.Fatalf("precondition: no auto-inserted scale on the default path:\n%s", out)
	}
	if err := RequireChain(context.Background(), filterStub(t, ConversionFilter, LibvmafFilter)); err == nil {
		t.Fatalf("RequireChain ACCEPTED a build with no `scale`, though the real chain to %s "+
			"auto-inserts %d scale filter(s) (AC-9)", named, n)
	}
}
