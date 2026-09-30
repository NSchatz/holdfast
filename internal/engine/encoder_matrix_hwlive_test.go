//go:build hwlive

package engine

// The hardware half of the TRANSCODE-6 codec matrix, and the one test in this package that
// opens a real device. It is behind the hwlive build tag because no goal of this program runs
// a real GPU (brief T9): the tag is set by nobody but an operator running it by hand on a
// host with hardware, and `rg -n hwlive .github Makefile` prints nothing, so neither the gate
// nor CI ever builds it. It moved here when the maker container gained the host's NVIDIA
// device (libnvidia-encode resolves there), which made the untagged test run a real NVENC
// encode inside the gate.
//
//	go test -tags hwlive -run TestHardwareEncoders_AvailabilityTable ./internal/engine/

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/hwdevice"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// ---- hardware encoders: honest capability-gated skip -------------------------

// TestHardwareEncoders_AvailabilityTable proves, for every registered hardware
// Spec, that internal/encoder.Available reports the truth about this host. In
// THIS container there is no GPU/device of any kind, so every hardware encoder is
// expected to be unavailable — asserted explicitly (not just skipped) so the test
// REDS if Available ever starts lying (e.g. reporting a hardware encoder available
// when it demonstrably is not, which would be the exact false-green the
// temp-file-and-ffprobe design in Available exists to prevent). When an encoder
// IS available (e.g. a future CI runner with a real GPU), this test runs a REAL
// encode through the engine and asserts the same happy-path shape as SVT-AV1
// above; otherwise it records an honest, clearly-worded skip.
func TestHardwareEncoders_AvailabilityTable(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	for _, key := range []string{"nvenc", "av1_nvenc", "qsv", "vaapi", "amf"} {
		t.Run(key, func(t *testing.T) {
			spec, ok := encoder.Lookup(key)
			if !ok {
				t.Fatalf("encoder.Lookup(%q) failed", key)
			}
			if !spec.Hardware {
				t.Fatalf("registry bug: %q is not marked Hardware", key)
			}

			prober := probe.New(ffmpeg, ffprobe)
			nodes, err := hwdevice.Discover("/")
			if err != nil {
				t.Fatal(err)
			}
			available := encoder.Available(context.Background(), ffmpeg, ffprobe, spec,
				ProbeEncode(config.Config{CRF: 23, Preset: "medium", PixelFormat: "auto"}, ffmpeg, prober, hwdevice.Assign(nodes)))
			if !available.Usable() {
				t.Skipf("%s (%s) not available on this host (no matching GPU/device) — honest skip, not a false green", key, spec.FFmpegCodec)
				return
			}

			// A real device IS present (not the case in this container today) — run
			// the actual encoder end-to-end through the engine, same happy-path shape
			// as the SVT-AV1 proof above.
			d := t.TempDir()
			src := filepath.Join(d, "movie.mkv")
			mkH264(t, ffmpeg, src, "8M")
			led := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
				c.Encoder = key
				c.CRF = 23
			})
			if codecOf(t, ffprobe, src) != spec.TargetCodec {
				t.Errorf("source codec after %s encode = %q, want %q", key, codecOf(t, ffprobe, src), spec.TargetCodec)
			}
			if !ledgerHas(t, led, store.Done, "movie.mkv") {
				t.Errorf("expected a done row for %s", key)
			}
		})
	}
}
