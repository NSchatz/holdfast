package main

// `holdfast run --queue-order <order>` (S0174): the configured order, overridden for one
// run. Each case names the criterion it grades (testing T1).
//
// Where the override is applied is the binding advisory F2 of the spec verdict: after
// loadConfig has refused an invalid configured queue_order, on the in-memory Config only.
// AC-15's case below is the proof that a broken configured value still exits 1 with a
// valid flag present, and AC-12's that the file is never written.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
)

// sizedH264 writes a real, transcodable h264 source whose size is set by its bitrate.
func sizedH264(t *testing.T, ffmpeg, path, bitrate string) string {
	t.Helper()
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", bitrate, "-pix_fmt", "yuv420p", "--", path).CombinedOutput()
	if err != nil {
		t.Fatalf("build fixture %s: %v\n%s", path, err, out)
	}
	return path
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// orderedLibrary builds three encodable sources of distinct sizes, named so that path order
// is neither smallest-first nor largest-first, and returns them smallest, middle, largest.
func orderedLibrary(t *testing.T, lib string) (small, mid, large string) {
	t.Helper()
	ffmpeg := envOr("HOLDFAST_FFMPEG", "ffmpeg")
	if _, err := exec.LookPath(ffmpeg); err != nil {
		t.Fatalf("::error:: ffmpeg is required here: an order that offered nothing proves nothing: %v", err)
	}
	mid = sizedH264(t, ffmpeg, filepath.Join(lib, "a_mid.mkv"), "4M")
	small = sizedH264(t, ffmpeg, filepath.Join(lib, "b_small.mkv"), "1M")
	large = sizedH264(t, ffmpeg, filepath.Join(lib, "c_large.mkv"), "12M")
	if !(fileSize(t, small) < fileSize(t, mid) && fileSize(t, mid) < fileSize(t, large)) {
		t.Fatalf("the fixture sizes are not distinct and ordered: %d %d %d",
			fileSize(t, small), fileSize(t, mid), fileSize(t, large))
	}
	return small, mid, large
}

// TestRunQueueOrder_AC11_TheFlagWinsOverTheFileAndTheEnvironment grades AC-11: with
// `queue_order: largest` configured - in the file, or in HOLDFAST_QUEUE_ORDER -
// `--queue-order smallest --limit-encodes 1` over three encodable files of distinct sizes
// encodes the smallest and no other.
func TestRunQueueOrder_AC11_TheFlagWinsOverTheFileAndTheEnvironment(t *testing.T) {
	for _, tc := range []struct{ name, extra, env string }{
		{"configured in the file", "queue_order: largest\n", ""},
		{"configured in HOLDFAST_QUEUE_ORDER", "queue_order: path\n", config.QueueOrderLargest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("HOLDFAST_QUEUE_ORDER", tc.env)
			}
			cfgPath, lib, _ := boundedLayout(t, "min_bitrate_kbps: 0\nvmaf_enable: false\nworkers: 1\n"+tc.extra)
			small, mid, large := orderedLibrary(t, lib)

			var out, errOut bytes.Buffer
			code := dispatch([]string{"run", "--config", cfgPath, "--queue-order", "smallest", "--limit-encodes", "1"}, &out, &errOut)
			if code != exitOK {
				t.Fatalf("run exited %d, want %d (stderr: %s)", code, exitOK, errOut.String())
			}
			if c := videoCodec(t, small); c == "h264" {
				t.Errorf("the smallest file %s was not encoded", small)
			}
			for _, other := range []string{mid, large} {
				if c := videoCodec(t, other); c != "h264" {
					t.Errorf("%s was encoded (now %s): the run offered something other than the smallest first", other, c)
				}
			}
		})
	}
}

// TestRunQueueOrder_AC11_AcceptsEveryValueTheKeyAccepts is AC-11's other half: the flag
// accepts exactly the queue_order key's closed set, read from config.QueueOrders, so a value
// the key learns is one the flag accepts with no edit here.
func TestRunQueueOrder_AC11_AcceptsEveryValueTheKeyAccepts(t *testing.T) {
	for _, order := range config.QueueOrders {
		t.Run(order, func(t *testing.T) {
			cfgPath := emptyLibraryConfig(t, "")
			code := -1
			logged := captureStderr(t, func() {
				var out, errOut bytes.Buffer
				code = dispatch([]string{"run", "--config", cfgPath, "--queue-order", order}, &out, &errOut)
			})
			if code != exitOK {
				t.Fatalf("run --queue-order %s exited %d:\n%s", order, code, logged)
			}
			if !strings.Contains(logged, "queue_order="+order+" ") {
				t.Errorf("the run did not put %s in force:\n%s", order, logged)
			}
		})
	}
}

// TestRunQueueOrder_AC12_LeavesTheConfigurationUntouched grades AC-12: the config file's
// bytes are identical after an overridden run, and a following run without the flag offers
// files in the configured order.
func TestRunQueueOrder_AC12_LeavesTheConfigurationUntouched(t *testing.T) {
	cfgPath, lib, _ := boundedLayout(t, "min_bitrate_kbps: 0\nvmaf_enable: false\nworkers: 1\nqueue_order: largest\n")
	small, mid, large := orderedLibrary(t, lib)
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"run", "--config", cfgPath, "--queue-order", "smallest", "--limit-encodes", "1"}, &out, &errOut); code != exitOK {
		t.Fatalf("the overridden run exited %d (stderr: %s)", code, errOut.String())
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the overridden run changed the config file:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if c := videoCodec(t, small); c == "h264" {
		t.Fatalf("the overridden run did not encode the smallest file, so the second half grades nothing")
	}

	// The next run, without the flag: the configured `largest` is what is in force again.
	out.Reset()
	errOut.Reset()
	if code := dispatch([]string{"run", "--config", cfgPath, "--limit-encodes", "1"}, &out, &errOut); code != exitOK {
		t.Fatalf("the following run exited %d (stderr: %s)", code, errOut.String())
	}
	if c := videoCodec(t, large); c == "h264" {
		t.Errorf("the following run did not encode the largest file: the override outlived its run")
	}
	if c := videoCodec(t, mid); c != "h264" {
		t.Errorf("the following run encoded %s (now %s), which the configured order does not offer first", mid, c)
	}
}

// startupRecord returns the one `holdfast starting` record line.
func startupRecord(t *testing.T, logged string) string {
	t.Helper()
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, "holdfast starting") {
			return line
		}
	}
	t.Fatalf("no startup record:\n%s", logged)
	return ""
}

// TestRunQueueOrder_AC13_TheStartupRecordNamesTheOrderAndItsSource grades AC-13: the startup
// record names the order in force and whether it came from the command line or the
// configuration, as fields of the one record.
func TestRunQueueOrder_AC13_TheStartupRecordNamesTheOrderAndItsSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"configured", nil, []string{"queue_order=largest", "queue_order_source=config"}},
		{"overridden", []string{"--queue-order", "smallest"}, []string{"queue_order=smallest", "queue_order_source=cli"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := emptyLibraryConfig(t, "queue_order: largest\n")
			code := -1
			logged := captureStderr(t, func() {
				var out, errOut bytes.Buffer
				code = dispatch(append([]string{"run", "--config", cfgPath}, tc.args...), &out, &errOut)
			})
			if code != exitOK {
				t.Fatalf("run exited %d:\n%s", code, logged)
			}
			line := startupRecord(t, logged)
			for _, f := range tc.want {
				if !strings.Contains(line, f) {
					t.Errorf("the startup record carries no %s:\n%s", f, line)
				}
			}
		})
	}
}

// TestRunQueueOrder_AC14_RefusesAValueTheKeyDoesNotAccept grades AC-14: a value outside the
// closed set exits 2, names the flag, the typed value and every accepted value, and creates
// nothing under the state directory. The empty value is TYPED, so it is refused rather than
// read as no flag.
func TestRunQueueOrder_AC14_RefusesAValueTheKeyDoesNotAccept(t *testing.T) {
	cfgPath, _, state := boundedLayout(t, "")
	for _, value := range []string{"biggest", "Smallest", "", " "} {
		t.Run("--queue-order "+strconv.Quote(value), func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := dispatch([]string{"run", "--config", cfgPath, "--queue-order", value}, &out, &errOut)
			if code != exitUsage || exitUsage != 2 {
				t.Fatalf("run --queue-order %q exited %d, want 2 (stderr: %s)", value, code, errOut.String())
			}
			msg := errOut.String()
			if !strings.Contains(msg, "--queue-order") {
				t.Errorf("the refusal did not name the flag:\n%s", msg)
			}
			if !strings.Contains(msg, strconv.Quote(value)) {
				t.Errorf("the refusal did not name the typed value %q:\n%s", value, msg)
			}
			for _, order := range config.QueueOrders {
				if !strings.Contains(msg, order) {
					t.Errorf("the refusal did not name the accepted value %q:\n%s", order, msg)
				}
			}
			mustNotExist(t, state)
		})
	}
}

// TestRunQueueOrder_AC15_AnInvalidConfiguredOrderStillRefusesToStart grades AC-15 and is the
// proof binding advisory F2 asks for: an invalid configured queue_order, in the file or in
// HOLDFAST_QUEUE_ORDER, refuses with exit 1 naming the key even with a valid --queue-order.
func TestRunQueueOrder_AC15_AnInvalidConfiguredOrderStillRefusesToStart(t *testing.T) {
	for _, tc := range []struct{ name, extra, env string }{
		{"in the file", "queue_order: biggest\n", ""},
		{"written empty in the file", "queue_order:\n", ""},
		{"in HOLDFAST_QUEUE_ORDER", "", "biggest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("HOLDFAST_QUEUE_ORDER", tc.env)
			}
			cfgPath, _, state := boundedLayout(t, tc.extra)
			for _, args := range [][]string{
				{"run", "--config", cfgPath},
				{"run", "--config", cfgPath, "--queue-order", "smallest"},
			} {
				var out, errOut bytes.Buffer
				code := dispatch(args, &out, &errOut)
				if code != exitError {
					t.Fatalf("%v exited %d, want %d (stderr: %s)", args, code, exitError, errOut.String())
				}
				if !strings.Contains(errOut.String(), "queue_order") {
					t.Errorf("%v: the refusal did not name the key:\n%s", args, errOut.String())
				}
				mustNotExist(t, state)
			}
		})
	}
}
