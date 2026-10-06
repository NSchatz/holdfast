package hwreport

// These tests drive scripts/hw-report.sh for real: a holdfast binary built from this tree, the
// pinned ffmpeg and ffprobe on PATH, and a real `holdfast run` over the script's throwaway
// library, so every gate runs on the software encoder. What is faked is only what would
// describe a real machine: `hostname`, `nvidia-smi` and `vainfo` are stand-ins first on PATH
// that print planted host names, user names, home paths, GPU identities, serials and MAC
// addresses, and HOME, USER and LOGNAME are planted values. No GPU is touched: the software
// encoder never opens a device, and the one test that names a hardware encoder runs behind an
// ffmpeg that refuses any VAAPI argument before the real one sees it (brief T9).
//
// A missing jq, ffmpeg, ffprobe or bash FAILS these tests; it never skips them. A grader that
// skips is a false green (docs/development.md).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"math/big"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/corpus"
)

// planted is one set of identities no report may carry, unique to the test that made it.
type planted struct {
	host, user, home, gpuUUID, serial, mac string
}

func randFrom(t *testing.T, alphabet string, n int) string {
	t.Helper()
	b := make([]byte, n)
	for i := range b {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			t.Fatalf("random: %v", err)
		}
		b[i] = alphabet[k.Int64()]
	}
	return string(b)
}

func newPlanted(t *testing.T) planted {
	t.Helper()
	letters := "abcdefghijklmnopqrstuvwxyz"
	hex := "0123456789abcdef"
	home := filepath.Join(t.TempDir(), "home-"+randFrom(t, letters, 10))
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	return planted{
		host: "plantedhost" + randFrom(t, letters, 8),
		user: "planteduser" + randFrom(t, letters, 8),
		home: home,
		gpuUUID: "GPU-" + randFrom(t, hex, 8) + "-" + randFrom(t, hex, 4) + "-" + randFrom(t, hex, 4) + "-" +
			randFrom(t, hex, 4) + "-" + randFrom(t, hex, 12),
		serial: "1" + randFrom(t, "0123456789", 12),
		mac: randFrom(t, hex, 2) + ":" + randFrom(t, hex, 2) + ":" + randFrom(t, hex, 2) + ":" +
			randFrom(t, hex, 2) + ":" + randFrom(t, hex, 2) + ":" + randFrom(t, hex, 2),
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := corpus.RepoRoot(wd)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// needTool fails the test, never skips it, when a tool the script needs is not on PATH.
func needTool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("%s is not on PATH: hw-report.sh needs it, and a test that skipped here would be a false green", name)
	}
	return p
}

// buildHoldfast builds this tree's holdfast into a temporary directory.
func buildHoldfast(t *testing.T, root string) string {
	t.Helper()
	gobin := needTool(t, "go")
	bin := filepath.Join(t.TempDir(), "holdfast")
	cmd := exec.Command(gobin, "build", "-o", bin, "./cmd/holdfast")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/holdfast: %v\n%s", err, out)
	}
	return bin
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeTools writes the stand-in device tools and returns their directory. Every one of them
// prints planted identities inside the very fields the script copies into the report.
func fakeTools(t *testing.T, p planted) string {
	t.Helper()
	dir := t.TempDir()
	writeExec(t, filepath.Join(dir, "hostname"), "#!/bin/sh\necho '"+p.host+".planted.example'\n")
	writeExec(t, filepath.Join(dir, "nvidia-smi"), "#!/bin/sh\n"+
		"echo '580.95.05 "+p.gpuUUID+", NVIDIA Planted Card serial "+p.serial+" on "+p.host+
		" for "+p.user+" at "+p.home+" mac "+p.mac+"'\n")
	writeExec(t, filepath.Join(dir, "vainfo"), "#!/bin/sh\n"+
		"echo 'libva info: VA-API version 1.22.0'\n"+
		"echo 'vainfo: Driver version: Intel iHD driver for Intel(R) Gen Graphics - 24.1.0 ("+
		p.host+" "+p.user+" "+p.home+"/.local "+p.gpuUUID+" "+p.serial+")'\n")
	return dir
}

// scriptEnv is the environment the script runs in: the fakes first on PATH, and HOME, USER and
// LOGNAME planted.
func scriptEnv(p planted, pathFirst ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		switch strings.SplitN(kv, "=", 2)[0] {
		case "PATH", "HOME", "USER", "LOGNAME":
			continue
		}
		env = append(env, kv)
	}
	path := strings.Join(append(pathFirst, os.Getenv("PATH")), string(os.PathListSeparator))
	return append(env, "PATH="+path, "HOME="+p.home, "USER="+p.user, "LOGNAME="+p.user)
}

func runScript(t *testing.T, root string, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	// A hang is a failure with a message, never a package timeout: the whole script run is
	// bounded well inside the test binary's own clock.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(root, "scripts", "hw-report.sh"), args...)
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = env
	cmd.Dir = t.TempDir()
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("hw-report.sh %v did not finish within its bound:\n%s", args, e.String())
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return o.String(), e.String(), ee.ExitCode()
		}
		t.Fatalf("running hw-report.sh: %v", err)
	}
	return o.String(), e.String(), 0
}

// forbidden is every token no report may carry: the planted ones, and this machine's own.
func forbidden(t *testing.T, p planted, extra ...string) map[string]string {
	t.Helper()
	f := map[string]string{
		"planted host name":      p.host,
		"planted user name":      p.user,
		"planted home":           p.home,
		"planted GPU UUID":       p.gpuUUID,
		"planted GPU UUID's hex": strings.TrimPrefix(p.gpuUUID, "GPU-"),
		"planted serial":         p.serial,
		"planted MAC":            p.mac,
		"real HOME":              os.Getenv("HOME"),
		"t.TempDir root":         filepath.Dir(t.TempDir()),
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		f["real host name"] = h
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		f["real user name"] = u.Username
	}
	for i, e := range extra {
		f["extra "+string(rune('a'+i))] = e
	}
	return f
}

func assertAbsent(t *testing.T, report []byte, tokens map[string]string) {
	t.Helper()
	for what, tok := range tokens {
		if tok == "" {
			t.Fatalf("the %s is empty, so its absence would prove nothing", what)
		}
		if bytes.Contains(report, []byte(tok)) {
			t.Errorf("the report carries the %s (%q)", what, tok)
		}
	}
}

// assertNoWorkDirLeft: the throwaway library under HOME is removed at the end.
func assertNoWorkDirLeft(t *testing.T, home string) {
	t.Helper()
	left, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range left {
		t.Errorf("the script left %q under HOME", e.Name())
	}
}

type clipReport struct {
	Clip   string `json:"clip"`
	Source struct {
		Codec  string `json:"codec"`
		PixFmt string `json:"pix_fmt"`
		Bytes  int64  `json:"bytes"`
	} `json:"source"`
	Outcome struct {
		Status      string   `json:"status"`
		Reason      string   `json:"reason"`
		EncoderRan  string   `json:"encoder_ran"`
		VmafMean    *float64 `json:"vmaf_mean"`
		VmafMinPool *float64 `json:"vmaf_min_pool"`
		SourceBytes *int64   `json:"source_bytes"`
		OutputBytes *int64   `json:"output_bytes"`
		Savings     *int64   `json:"savings_bytes"`
		EncodeMs    *int64   `json:"encode_ms"`
	} `json:"outcome"`
	Output *struct {
		Codec            string `json:"codec"`
		PixFmt           string `json:"pix_fmt"`
		ColorPrimaries   string `json:"color_primaries"`
		ColorTransfer    string `json:"color_transfer"`
		MasteringDisplay bool   `json:"mastering_display"`
		ContentLight     bool   `json:"content_light"`
	} `json:"output"`
	Decode string `json:"decode"`
	Timing struct {
		WallMs int64 `json:"wall_ms"`
	} `json:"timing"`
}

type report struct {
	Encoder struct {
		Requested string   `json:"requested"`
		Ran       []string `json:"ran"`
	} `json:"encoder"`
	HWDecode        string `json:"hw_decode"`
	PixelFormat     string `json:"pixel_format"`
	HoldfastVersion string `json:"holdfast_version"`
	FFmpeg          string `json:"ffmpeg"`
	Device          struct {
		Nvidia *struct {
			DriverVersion string `json:"driver_version"`
			Name          string `json:"name"`
		} `json:"nvidia"`
		VainfoDriver *string `json:"vainfo_driver"`
	} `json:"device"`
	Clips []clipReport `json:"clips"`
}

// TestHWReport_CPUReportCarriesTheFiguresAndNoHostIdentity runs the whole script on the
// software encoder and holds the report to both halves of its contract: it carries the
// encoder, the ffmpeg pin, the holdfast version, the gate figures of both clips and their
// timing, and it carries none of the planted identities, the device tools' planted serials,
// or this machine's host name, user, home, work directory or temporary directories.
func TestHWReport_CPUReportCarriesTheFiguresAndNoHostIdentity(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "ffmpeg", "ffprobe"} {
		needTool(t, tool)
	}
	root := repoRoot(t)
	bin := buildHoldfast(t, root)
	p := newPlanted(t)
	fakes := fakeTools(t, p)
	out := filepath.Join(t.TempDir(), "cpu-report.json")

	_, stderr, code := runScript(t, root, scriptEnv(p, fakes), "--encoder", "cpu", "--holdfast", bin, "--out", out)
	if code != 0 {
		t.Fatalf("hw-report.sh exited %d:\n%s", code, stderr)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("no report was written: %v\n%s", err, stderr)
	}
	var r report
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, raw)
	}

	if r.Encoder.Requested != "cpu" || len(r.Encoder.Ran) != 1 || r.Encoder.Ran[0] != "cpu" {
		t.Errorf("encoder = %+v, want requested cpu and ran [cpu]", r.Encoder)
	}
	if !strings.HasPrefix(r.FFmpeg, "ffmpeg version ") {
		t.Errorf("ffmpeg pin = %q, want the first line of ffmpeg -version", r.FFmpeg)
	}
	if !strings.HasPrefix(r.HoldfastVersion, "holdfast ") {
		t.Errorf("holdfast_version = %q", r.HoldfastVersion)
	}
	// The device tools' fields were copied (so their absence below is a redaction, not an
	// omission), and only the planted identities in them are gone.
	if r.Device.Nvidia == nil || !strings.HasPrefix(r.Device.Nvidia.DriverVersion, "580.95.05") ||
		!strings.Contains(r.Device.Nvidia.Name, "NVIDIA Planted Card") {
		t.Errorf("device.nvidia = %+v, want the fake nvidia-smi's driver version and name, redacted", r.Device.Nvidia)
	}
	if r.Device.VainfoDriver == nil || !strings.Contains(*r.Device.VainfoDriver, "Intel iHD driver") {
		t.Errorf("device.vainfo_driver = %v, want the fake vainfo's driver line, redacted", r.Device.VainfoDriver)
	}

	if len(r.Clips) != 2 || r.Clips[0].Clip != "sdr8" || r.Clips[1].Clip != "hdr10" {
		t.Fatalf("clips = %+v, want sdr8 then hdr10", r.Clips)
	}
	for _, c := range r.Clips {
		o := c.Outcome
		if c.Source.Codec != "ffv1" || c.Source.Bytes <= 0 {
			t.Errorf("%s: source = %+v, want an FFV1 clip with a size", c.Clip, c.Source)
		}
		if o.Status != "done" || o.EncoderRan != "cpu" {
			t.Errorf("%s: outcome status %q (%s) encoder %q, want done by cpu", c.Clip, o.Status, o.Reason, o.EncoderRan)
		}
		if o.VmafMean == nil || *o.VmafMean <= 0 || o.VmafMinPool == nil || *o.VmafMinPool <= 0 {
			t.Errorf("%s: VMAF mean %v min pool %v, want both measured", c.Clip, o.VmafMean, o.VmafMinPool)
		}
		if o.SourceBytes == nil || o.OutputBytes == nil || *o.OutputBytes <= 0 || *o.OutputBytes >= *o.SourceBytes ||
			o.Savings == nil || *o.Savings != *o.SourceBytes-*o.OutputBytes {
			t.Errorf("%s: bytes source %v output %v savings %v, want a smaller output and their difference",
				c.Clip, o.SourceBytes, o.OutputBytes, o.Savings)
		}
		if o.EncodeMs == nil || c.Timing.WallMs <= 0 {
			t.Errorf("%s: encode_ms %v wall_ms %d, want both timed", c.Clip, o.EncodeMs, c.Timing.WallMs)
		}
		if c.Output == nil || c.Output.Codec != "hevc" {
			t.Errorf("%s: output = %+v, want the HEVC file the swap left", c.Clip, c.Output)
		}
	}
	if hdr := r.Clips[1]; hdr.Source.PixFmt != "yuv420p10le" || hdr.Output == nil ||
		hdr.Output.PixFmt != "yuv420p10le" || hdr.Output.ColorTransfer != "smpte2084" ||
		hdr.Output.ColorPrimaries != "bt2020" || !hdr.Output.MasteringDisplay || !hdr.Output.ContentLight {
		t.Errorf("hdr10: source %+v output %+v, want 10-bit PQ BT.2020 with both HDR10 blocks carried", hdr.Source, hdr.Output)
	}

	assertAbsent(t, raw, forbidden(t, p, bin, filepath.Dir(bin), fakes, root))
	if bytes.Contains(raw, []byte("GPU-")) {
		t.Errorf("the report carries a GPU identity prefix")
	}
	if !bytes.Contains(raw, []byte("<redacted>")) {
		t.Errorf("the report carries no redaction marker, so the planted fields were not copied and nothing was proven")
	}
	assertNoWorkDirLeft(t, p.home)

	// The same report again is refused, and the first one is untouched.
	_, stderr, code = runScript(t, root, scriptEnv(p, fakes), "--encoder", "cpu", "--holdfast", bin, "--out", out)
	if code == 0 || !strings.Contains(stderr, "refusing to overwrite") {
		t.Errorf("a second run onto the same --out exited %d, want a refusal to overwrite:\n%s", code, stderr)
	}
	if again, _ := os.ReadFile(out); !bytes.Equal(again, raw) {
		t.Errorf("the refused run changed the existing report")
	}

	// The committed-report check passes this report on this host.
	if _, stderr, code := runScript(t, root, scriptEnv(p, fakes), "--verify", out); code != 0 {
		t.Errorf("--verify refused the report the script itself wrote (exit %d):\n%s", code, stderr)
	}
}

// TestHWReport_VerifyRefusesAReportCarryingAForbiddenToken proves the final check bites: a
// report whose string fields carry this host's name, the user, the home directory, a GPU
// UUID, a serial, a MAC, a PCI address or an absolute path is refused, and the same report
// without them is accepted. It is the check the script runs before it writes any report.
func TestHWReport_VerifyRefusesAReportCarryingAForbiddenToken(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		needTool(t, tool)
	}
	root := repoRoot(t)
	p := newPlanted(t)
	fakes := fakeTools(t, p)
	env := scriptEnv(p, fakes)
	dir := t.TempDir()

	write := func(name, field string) string {
		b, err := json.Marshal(map[string]any{
			"encoder": map[string]any{"requested": "nvenc"},
			"device":  map[string]any{"nvidia": map[string]any{"driver_version": "580.95.05", "name": field}},
		})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	if _, stderr, code := runScript(t, root, env, "--verify", write("clean", "NVIDIA Planted Card")); code != 0 {
		t.Fatalf("--verify refused a clean report (exit %d):\n%s", code, stderr)
	}
	cases := map[string]string{
		"host name (from the hostname tool)": "card on " + p.host,
		"user name (from USER)":              "owned by " + p.user,
		"home directory (from HOME)":         "at " + p.home,
		"GPU UUID":                           "NVIDIA Planted Card " + p.gpuUUID,
		"bare UUID":                          strings.TrimPrefix(p.gpuUUID, "GPU-"),
		"serial":                             "serial " + p.serial,
		"MAC":                                "mac " + p.mac,
		"PCI address":                        "bus 0000:01:00.0",
		"absolute path":                      "driver at /usr/lib/x86_64-linux-gnu/dri",
	}
	for what, field := range cases {
		path := write(strings.NewReplacer(" ", "-", "(", "", ")", "").Replace(what), field)
		_, stderr, code := runScript(t, root, env, "--verify", path)
		if code == 0 {
			t.Errorf("--verify accepted a report carrying a %s", what)
			continue
		}
		if strings.Contains(stderr, field) {
			t.Errorf("--verify echoed the forbidden %s it refused", what)
		}
	}
}

// TestHWReport_UnavailableEncoderWritesNoReport: an encoder the start-time probe cannot use is
// a refusal under `hw_fallback: skip`, never a silent software encode. The script says the
// encoder is unavailable, exits non-zero, writes no report and leaves no work directory.
//
// The ffmpeg on PATH here refuses any argument naming VAAPI before the real ffmpeg sees it,
// so the probe fails the way it fails on a host with no render node, and no device is ever
// opened whatever this machine carries.
func TestHWReport_UnavailableEncoderWritesNoReport(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		needTool(t, tool)
	}
	realFFmpeg := needTool(t, "ffmpeg")
	needTool(t, "ffprobe")
	root := repoRoot(t)
	bin := buildHoldfast(t, root)
	p := newPlanted(t)
	fakes := fakeTools(t, p)
	writeExec(t, filepath.Join(fakes, "ffmpeg"), "#!/bin/sh\n"+
		"for a in \"$@\"; do case \"$a\" in *vaapi*) echo 'No VA display found (stand-in: no render node)' >&2; exit 1;; esac; done\n"+
		"PATH='"+os.Getenv("PATH")+"' exec '"+realFFmpeg+"' \"$@\"\n")
	out := filepath.Join(t.TempDir(), "vaapi-report.json")

	_, stderr, code := runScript(t, root, scriptEnv(p, fakes), "--encoder", "vaapi", "--holdfast", bin, "--out", out)
	if code == 0 {
		t.Fatalf("hw-report.sh exited 0 for an encoder with no device:\n%s", stderr)
	}
	if !strings.Contains(stderr, "encoder 'vaapi' is unavailable") {
		t.Errorf("the refusal does not say the encoder is unavailable:\n%s", stderr)
	}
	if _, err := os.Lstat(out); err == nil {
		t.Errorf("a report was written for an unavailable encoder")
	}
	assertNoWorkDirLeft(t, p.home)
}

// TestHWReport_RefusesWithoutJqAndNamesIt: a required host tool that is missing is named up
// front, before anything is built or run.
func TestHWReport_RefusesWithoutJqAndNamesIt(t *testing.T) {
	bash := needTool(t, "bash")
	root := repoRoot(t)
	p := newPlanted(t)
	only := t.TempDir()
	for _, tool := range []string{"dirname", "hostname"} {
		src := needTool(t, tool)
		if err := os.Symlink(src, filepath.Join(only, tool)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(bash, filepath.Join(only, "bash")); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + only, "HOME=" + p.home, "USER=" + p.user, "LOGNAME=" + p.user}
	_, stderr, code := runScript(t, root, env, "--encoder", "cpu")
	if code == 0 || !strings.Contains(stderr, "required tool 'jq' is not on PATH") {
		t.Errorf("without jq the script exited %d, want a refusal naming jq:\n%s", code, stderr)
	}
}

// TestHWReport_HWDecodeAddsTheH264ClipAndRecordsEachDecode: --hw-decode runs the configuration
// with hw_decode: hardware, adds an 8-bit H.264 clip (a source every vendor's hardware decodes),
// records each clip's declared decode path and names the report <encoder>-hw-decode-<date>.json
// by default. On the software encoder every job decodes in software - there is no hardware in
// it to decode on - so every clip says software, all three are replaced, and the report carries
// none of the planted identities. No device is opened.
func TestHWReport_HWDecodeAddsTheH264ClipAndRecordsEachDecode(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "ffmpeg", "ffprobe"} {
		needTool(t, tool)
	}
	root := repoRoot(t)
	bin := buildHoldfast(t, root)
	p := newPlanted(t)
	fakes := fakeTools(t, p)
	out := filepath.Join(t.TempDir(), "cpu-hw-decode-report.json")

	_, stderr, code := runScript(t, root, scriptEnv(p, fakes), "--encoder", "cpu", "--hw-decode", "--pixel-format", "yuv420p",
		"--holdfast", bin, "--out", out)
	if code != 0 {
		t.Fatalf("hw-report.sh --hw-decode exited %d:\n%s", code, stderr)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("no report was written: %v\n%s", err, stderr)
	}
	var r report
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, raw)
	}
	if r.HWDecode != "hardware" || r.PixelFormat != "yuv420p" {
		t.Errorf("hw_decode %q pixel_format %q, want hardware and yuv420p", r.HWDecode, r.PixelFormat)
	}
	if len(r.Clips) != 3 || r.Clips[2].Clip != "h264" || r.Clips[2].Source.Codec != "h264" || r.Clips[2].Source.PixFmt != "yuv420p" {
		t.Fatalf("clips = %+v, want sdr8, hdr10 and an 8-bit H.264 clip", r.Clips)
	}
	for _, c := range r.Clips {
		if c.Decode != "software" {
			t.Errorf("%s: decode %q, want software (a software encoder decodes in software)", c.Clip, c.Decode)
		}
		if c.Outcome.Status != "done" || c.Outcome.EncoderRan != "cpu" {
			t.Errorf("%s: outcome %q (%s) by %q, want done by cpu", c.Clip, c.Outcome.Status, c.Outcome.Reason, c.Outcome.EncoderRan)
		}
		// pixel_format yuv420p reached the configuration: every output is 8-bit 4:2:0.
		if c.Output == nil || c.Output.PixFmt != "yuv420p" {
			t.Errorf("%s: output %+v, want yuv420p (the configured pixel_format)", c.Clip, c.Output)
		}
	}
	assertAbsent(t, raw, forbidden(t, p, bin, filepath.Dir(bin), fakes, root))
	assertNoWorkDirLeft(t, p.home)

	// The default name says what was run.
	if !strings.Contains(scriptText(t, root), `$ENCODER${HW_DECODE:+-hw-decode}${PIXEL_FORMAT:+-$PIXEL_FORMAT}-$stamp.json`) {
		t.Error("the default report name no longer marks a hardware-decode report")
	}
}

// scriptText is the script itself, for the assertions about its defaults.
func scriptText(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "scripts", "hw-report.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
