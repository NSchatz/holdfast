package vmaf

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// The two built-in model specs "auto" chooses between. They are named here, beside
// the preflight that proves they load, rather than only at the call site that picks
// one - the preflight has to check every model a run might later resolve to, and it
// runs before any height is known.
const (
	modelHD = "version=vmaf_v0.6.1"
	model4K = "version=vmaf_4k_v0.6.1"
)

// autoHeight is the output height above which "auto" selects the UHD model.
const autoHeight = 1440

// ResolveModel maps a configured model to a libvmaf model spec. "auto"/"" picks the
// UHD model for output height > 1440 and the HD model otherwise; any other value is
// passed through, prefixed with "version=" when it looks like a bare version id.
func ResolveModel(cfg string, height int) string {
	if cfg == "" || cfg == "auto" {
		if height > autoHeight {
			return model4K
		}
		return modelHD
	}
	if !strings.Contains(cfg, "=") {
		return "version=" + cfg
	}
	return cfg
}

// CandidateModels is every spec ResolveModel could return for a configured value -
// which is what a STARTUP preflight has to check, because at startup no output
// exists and therefore no height is known. "auto" can resolve to either built-in
// depending on the file, so both must load or the refusal would depend on which
// file the library happened to reach first: a 1080p library would start cleanly and
// a 4K one would die hours in, on the same configuration and the same build.
//
// An explicitly configured model has exactly one candidate: itself.
func CandidateModels(cfg string) []string {
	if cfg == "" || cfg == "auto" {
		return []string{modelHD, model4K}
	}
	return []string{ResolveModel(cfg, 0)}
}

// ErrModelUnresolved reports that a configured libvmaf model does not load in this
// ffmpeg build. Like ErrUnavailable it is a REFUSAL, never a downgrade: a model that
// cannot be loaded cannot measure anything, and this tool does not delete an
// original on the strength of a measurement it did not take.
var ErrModelUnresolved = fmt.Errorf("libvmaf model does not resolve in this ffmpeg build")

// RequireModel proves that every model spec a run might use actually LOADS in this
// ffmpeg build, and returns a loud, self-explaining error naming the configured
// value when one does not.
//
// It is deliberately shaped like encoder.RequireAvailable, and for the same reason:
// the existence of a capability is not the same claim as the capability working.
// Available() only greps the filter list, which says the build has libvmaf and says
// nothing about whether the build ships the model that libvmaf was asked for - the
// FFmpeg documentation enumerates three `version=` values and this repo configures a
// fourth by default on tall output. So the check RUNS the filter, on two synthetic
// 64x64 frames from lavfi with no file on disk, and reads ffmpeg's exit status: a
// model that will not load fails filter-graph initialisation, which is exactly the
// failure the operator would otherwise meet after a full library's worth of encoding.
//
// Cost is a few tens of milliseconds per candidate. It is paid once per process, at
// startup, before the job store is opened.
func RequireModel(ctx context.Context, ffmpeg, configured string) error {
	if !Available(ctx, ffmpeg) {
		return fmt.Errorf("the VMAF quality gate is enabled but %w - either install an "+
			"ffmpeg built with libvmaf or set vmaf_enable: false (which turns the perceptual "+
			"gate OFF entirely; the structural checks alone pass an encode that decodes "+
			"perfectly and looks terrible)", ErrUnavailable)
	}
	for _, m := range CandidateModels(configured) {
		if out, err := probeModel(ctx, ffmpeg, m); err != nil {
			return fmt.Errorf("the VMAF quality gate is enabled with vmaf_model=%q, but the model "+
				"spec %q %w: %s. holdfast refuses to start rather than discover this after "+
				"encoding: set vmaf_model to a spec this build ships, or set vmaf_enable: false",
				configured, m, ErrModelUnresolved, truncate(strings.TrimSpace(out), 300))
		}
	}
	return nil
}

// probeModel runs libvmaf once with the given model over two synthetic frames. The
// inputs are lavfi `color` sources rather than real media so the check needs no
// fixture, touches no library file, and costs the same on every host.
func probeModel(ctx context.Context, ffmpeg, model string) (string, error) {
	const src = "color=c=black:s=64x64:r=1:d=0.1"
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", src,
		"-f", "lavfi", "-i", src,
		"-lavfi", "[0:v][1:v]libvmaf=model="+model+":n_threads=1",
		"-f", "null", "-")
	out, err := cmd.CombinedOutput()
	return string(out), err
}
