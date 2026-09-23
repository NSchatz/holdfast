package vmaf

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// filterStub writes an ffmpeg whose `-filters` listing carries exactly the names given, so
// a build missing one can be presented to the check without a second ffmpeg on this host.
// The BINARY is outside this package's boundary; the subject is what RequireChain does
// with what it reads back.
func filterStub(t *testing.T, names ...string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub is POSIX")
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	for _, n := range names {
		b.WriteString("echo ' .. " + n + "             VV->V      a filter'\n")
	}
	stub := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(stub, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub
}

// TestS0160_AC9_AChainThisBuildCannotAssembleStopsTheRun: every filter the gate's graph
// composes is checked, and a build missing one is refused in words naming it. The two
// fallbacks the criterion forbids - comparing in a format nothing named, or skipping the
// gate - are exactly what the refusal exists instead of.
func TestS0160_AC9_AChainThisBuildCannotAssembleStopsTheRun(t *testing.T) {
	ctx := context.Background()

	t.Run("the conversion filter is missing", func(t *testing.T) {
		err := RequireChain(ctx, filterStub(t, LibvmafFilter, "scale", "yadif"))
		if err == nil {
			t.Fatal("RequireChain accepted a build with no format filter: the gate's graph converts " +
				"BOTH streams to one named format, and without it the comparison happens in whatever " +
				"libavfilter negotiates")
		}
		if !errors.Is(err, ErrChainFilterMissing) {
			t.Errorf("error does not wrap ErrChainFilterMissing: %v", err)
		}
		if !strings.Contains(err.Error(), ConversionFilter) {
			t.Errorf("the refusal does not name the missing filter %q: %v", ConversionFilter, err)
		}
	})

	t.Run("libvmaf is missing", func(t *testing.T) {
		err := RequireChain(ctx, filterStub(t, ConversionFilter, "scale"))
		if !errors.Is(err, ErrChainFilterMissing) {
			t.Fatalf("a build with no libvmaf was not refused: %v", err)
		}
		if !strings.Contains(err.Error(), LibvmafFilter) {
			t.Errorf("the refusal does not name %q: %v", LibvmafFilter, err)
		}
	})

	t.Run("a filter the configuration asked for is missing", func(t *testing.T) {
		err := RequireChain(ctx, filterStub(t, ConversionFilter, LibvmafFilter), "bwdif")
		if !errors.Is(err, ErrChainFilterMissing) {
			t.Fatalf("a configured deinterlace filter this build lacks was not refused: %v", err)
		}
		if !strings.Contains(err.Error(), "bwdif") {
			t.Errorf("the refusal does not name %q: %v", "bwdif", err)
		}
	})

	t.Run("control: a build providing the whole chain is accepted", func(t *testing.T) {
		if err := RequireChain(ctx, filterStub(t, ConversionFilter, LibvmafFilter, "yadif", "scale"),
			"yadif", "scale"); err != nil {
			t.Errorf("a build providing every filter the chain needs was refused: %v", err)
		}
	})

	t.Run("control: the real pinned build provides the chain", func(t *testing.T) {
		bin := ffmpegBin()
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("::error:: ffmpeg required for the chain check: %v", err)
		}
		if err := RequireChain(ctx, bin, "yadif", "bwdif", "scale"); err != nil {
			t.Errorf("the pinned build was refused: %v", err)
		}
	})

	t.Run("a build whose filters cannot be listed is refused, not assumed", func(t *testing.T) {
		if err := RequireChain(ctx, filepath.Join(t.TempDir(), "not-an-ffmpeg")); err == nil {
			t.Error("RequireChain accepted a binary it could not run at all")
		}
	})
}
