package vmaf

// S0160 impl-gate 1, F2 (AC-2). Refuter artifact.

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/cpuquota"
)

func TestRegress_0160_F2_OneGateNamesTwiceItsShare(t *testing.T) {
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	stub := filepath.Join(dir, "ffmpeg")
	sh := "#!/bin/sh\n: > '" + argv + "'\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + argv + "'; done\nexit 1\n"
	if err := os.WriteFile(stub, []byte(sh), 0o755); err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`n_threads=(\d+)`)
	for _, tc := range []struct {
		quota   float64
		workers int
	}{{4, 1}, {12, 4}, {24, 4}} {
		share := cpuquota.Divide(cpuquota.Quota{CPUs: tc.quota, Limited: true}, tc.workers)
		_, _ = Score(context.Background(), stub, Request{Distorted: "d.mkv", Reference: "r.mkv",
			Subsample: 1, Threads: share, Model: "version=vmaf_v0.6.1", PixelFormat: "yuv420p10le"})
		raw, err := os.ReadFile(argv)
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Split(strings.TrimSpace(string(raw)), "\n")
		per := 0
		for i := 0; i+1 < len(args); i++ {
			switch args[i] {
			case "-filter_complex_threads", "-filter_threads", "-threads":
				n, _ := strconv.Atoi(args[i+1])
				per += n
			case "-lavfi", "-filter_complex":
				for _, m := range re.FindAllStringSubmatch(args[i+1], -1) {
					n, _ := strconv.Atoi(m[1])
					per += n
				}
			}
		}
		if total := per * tc.workers; float64(total) > tc.quota {
			t.Errorf("quota %v, %d worker(s), share %d: each gate names %d threads, %d gates ask "+
				"for %d > quota (AC-2)", tc.quota, tc.workers, share, per, tc.workers, total)
		}
	}
}
