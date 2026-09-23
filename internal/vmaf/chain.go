package vmaf

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// ConversionFilter is the filter BuildFilter puts in front of libvmaf on BOTH sides: the
// one that converts each stream to the single named comparison format. It is spelled here
// so the startup check that asks whether this build provides it reads the same name the
// graph composes.
const ConversionFilter = "format"

// LibvmafFilter is the measuring filter itself.
const LibvmafFilter = "libvmaf"

// ScaleFilter is the filter that actually PERFORMS every pixel-format conversion the chain
// asks for. `format` only constrains what libavfilter negotiates; where a stream does not
// already carry the named format, libavfilter satisfies the constraint by auto-inserting a
// scale filter in front of it, and refuses the whole graph when the build has none. On the
// default path that is every gated job - `pixel_format: auto` produces a 10-bit output, so
// an 8-bit source always has to be converted - which is why the chain needs this filter
// whether or not any root scales a picture.
const ScaleFilter = "scale"

// ErrChainFilterMissing reports that this ffmpeg build does not provide a filter the
// gate's chain composes.
//
// It is a REFUSAL and never a downgrade, on exactly the terms ErrUnavailable and
// ErrModelUnresolved are. The two fallbacks available to a chain missing a filter are both
// forbidden: dropping the conversion compares in whatever format libavfilter negotiates,
// which is a different measurement nobody named or recorded, and dropping the measurement
// accepts an encode that was never scored. The source is deleted on the strength of this
// gate, so neither is a degradation this tool is allowed to choose.
var ErrChainFilterMissing = errors.New("the ffmpeg build does not provide a filter the quality gate's chain needs")

// ChainFilters is every filter the SCORING graph needs on its own, whatever the
// configuration: the per-side constraint to the named comparison format, the scaler that
// carries out the conversion that constraint demands, and libvmaf. The scaler is listed
// although the graph string never spells it, because libavfilter inserts it itself and a
// build without it cannot convert at all (see ScaleFilter). A run may add more - a
// deinterlace on the reference - and those reach RequireChain as extras, because they come
// from the configuration and refusing a build over a filter no root asked for would refuse
// a build that works.
func ChainFilters() []string { return []string{ConversionFilter, ScaleFilter, LibvmafFilter} }

// RequireChain proves this ffmpeg build provides every filter the gate's chain composes,
// and returns a loud error naming the missing one when it does not.
//
// It runs at STARTUP, before the first encode, for the reason the model preflight does:
// a chain this build cannot assemble fails every file, and discovering that after a
// library's worth of encoding costs hours and reports itself as a rejection per file. The
// extras are the filter names the configuration resolved to, so a root configured to
// deinterlace on a build without that filter stops the run here rather than at its first
// interlaced source.
func RequireChain(ctx context.Context, ffmpeg string, extras ...string) error {
	have, err := filterSet(ctx, ffmpeg)
	if err != nil {
		return fmt.Errorf("the VMAF quality gate is enabled but this build's filters could not be "+
			"listed, so holdfast cannot prove the gate's chain can be assembled: %w", err)
	}
	var missing []string
	asked := map[string]bool{}
	for _, f := range append(ChainFilters(), extras...) {
		if f == "" || have[f] || asked[f] {
			continue
		}
		asked[f] = true
		missing = append(missing, f)
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("%w: %s missing from this ffmpeg build. The gate's chain converts BOTH "+
		"streams to one named pixel format and then measures them with libvmaf, and holdfast "+
		"refuses to start rather than fall back to a chain that compares in a format nothing "+
		"named or to a run with no perceptual gate at all: install an ffmpeg providing %s, or "+
		"set vmaf_enable: false and accept that the structural checks alone pass an encode that "+
		"decodes perfectly and looks terrible",
		ErrChainFilterMissing, strings.Join(missing, ", "), strings.Join(missing, ", "))
}

// filterSet reads the build's filter listing once. Available answers one membership
// question off the same listing; this answers several, and reading the list once is what
// keeps the two from disagreeing about what this build provides.
func filterSet(ctx context.Context, ffmpeg string) (map[string]bool, error) {
	out, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-filters").Output()
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		// filter listing columns: " .. libvmaf  VV->V  Calculate the VMAF ..."
		if fields := strings.Fields(line); len(fields) >= 2 {
			have[fields[1]] = true
		}
	}
	return have, nil
}
