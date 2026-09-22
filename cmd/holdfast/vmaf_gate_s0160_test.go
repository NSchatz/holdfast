package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
)

// TestS0160_AC9_ABuildThatCannotAssembleTheGatesChainStopsBeforeTheFirstEncode.
//
// The stub is a real ffmpeg with one row removed from its filter listing, so the build
// under test differs from the pinned one in exactly the way the criterion is about, and
// everything else the run does still runs for real. The library root carries a file, so a
// preflight that did not stop the run would encode it: the assertion that the file is
// byte-identical afterwards is what "before the first encode" means here.
func TestS0160_AC9_ABuildThatCannotAssembleTheGatesChainStopsBeforeTheFirstEncode(t *testing.T) {
	real, err := exec.LookPath(envOr("HOLDFAST_FFMPEG", "ffmpeg"))
	if err != nil {
		t.Fatalf("::error:: ffmpeg required for the chain preflight proof: %v", err)
	}

	// An ffmpeg whose filter listing omits `format`, the filter the scoring graph puts in
	// front of libvmaf on BOTH sides. Everything else passes through to the real binary.
	stub := filepath.Join(t.TempDir(), "ffmpeg-without-format")
	script := "#!/bin/sh\ncase \" $* \" in\n  *\" -filters \"*) " + real +
		" \"$@\" | awk '$2 != \"format\"'; exit 0 ;;\nesac\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(lib, "movie.mkv")
	if out, err := exec.Command(real, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10", "-c:v", "libx264", "-preset", "ultrafast",
		"-b:v", "8M", src).CombinedOutput(); err != nil {
		t.Fatalf("building the library fixture: %v\n%s", err, out)
	}
	before := digest(t, src)

	state := filepath.Join(dir, "state")
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + state +
		"\nvmaf_enable: true\nencoder: cpu\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOLDFAST_FFMPEG", stub)
	var out, errOut bytes.Buffer
	code := dispatch([]string{"run", "--config", cfgPath}, &out, &errOut)
	said := errOut.String() + out.String()

	if code == 0 {
		t.Fatalf("the run exited 0 on a build that cannot assemble the gate's chain:\n%s", said)
	}
	for _, want := range []string{"format", "libvmaf", "vmaf_enable: false"} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not carry %q:\n%s", want, said)
		}
	}
	if digest(t, src) != before {
		t.Error("the library file was touched: the refusal has to land BEFORE the first encode")
	}
	if _, err := os.Stat(filepath.Join(state, "jobs.db")); err == nil {
		t.Error("a refused run left a job store behind, so it got past the preflight band")
	}
}

// TestS0160_AC11_ASampledGateSaysWhatTheFloorsThenBoundOncePerRun: a configured interval
// above 1 produces exactly one warn for the run, naming the interval and saying that the
// worst-frame and chroma floors then bound only the frames that were sampled.
//
// logConfigWarnings is the per-run emission point every command goes through, which is why
// the count is taken here rather than off a formatted string.
func TestS0160_AC11_ASampledGateSaysWhatTheFloorsThenBoundOncePerRun(t *testing.T) {
	for _, interval := range []int{2, 7} {
		cfg := loadWithSubsample(t, interval)
		var buf bytes.Buffer
		logConfigWarnings(cfg, slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

		var sampling []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("a log line is not one JSON object (%v): %s", err, line)
			}
			msg, _ := rec["msg"].(string)
			if strings.Contains(msg, "vmaf_subsample") {
				sampling = append(sampling, rec)
			}
		}
		if len(sampling) != 1 {
			t.Fatalf("vmaf_subsample: %d produced %d records about the interval, want exactly 1:\n%s",
				interval, len(sampling), buf.String())
		}
		rec := sampling[0]
		if rec["level"] != "WARN" {
			t.Errorf("the sampling record is at %v, want WARN: the run continues in a degraded "+
				"state, which is what warn means here", rec["level"])
		}
		msg, _ := rec["msg"].(string)
		if !strings.Contains(msg, strconv.Itoa(interval)) {
			t.Errorf("the warn does not name the interval %d: %s", interval, msg)
		}
		for _, floor := range []string{"vmaf_min_pool", "vmaf_min_chroma"} {
			if !strings.Contains(msg, floor) {
				t.Errorf("the warn does not say that %s then bounds only the sampled frames: %s",
					floor, msg)
			}
		}
	}

	// The control: at the shipped interval of 1 nothing is sampled away and there is
	// nothing to warn about, so the warn is not decoration on every run.
	cfg := loadWithSubsample(t, 1)
	var buf bytes.Buffer
	logConfigWarnings(cfg, slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if strings.Contains(buf.String(), "vmaf_subsample") {
		t.Errorf("scoring every frame warned about the sampling interval:\n%s", buf.String())
	}
}

func loadWithSubsample(t *testing.T, interval int) *config.Config {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + filepath.Join(dir, "state") +
		"\nvmaf_enable: true\nvmaf_subsample: " + strconv.Itoa(interval) + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("loading the configuration: %v", err)
	}
	return cfg
}

func digest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}
