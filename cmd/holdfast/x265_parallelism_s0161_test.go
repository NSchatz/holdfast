package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/engine"
)

// The S0161 records are graded on what a REAL `holdfast run` child process writes to its
// own standard error, because that is the only place an operator reads them. The cgroup
// hierarchy it reads is a fixture handed to it through HOLDFAST_CGROUP_ROOT, and the
// ffmpeg it runs is the pinned one behind a wrapper that writes down every argv it is
// given, so the record and the encode it describes are graded from the same run.

// s0161Msg is the startup record's message, which carries no value of its own.
const s0161Msg = "libx265 parallelism for this run"

// s0161Record is one parsed slog TEXT line of the child's capture.
type s0161Record struct {
	level, msg string
	attrs      map[string]string
	raw        string
}

// s0161Run is one `holdfast run` of a single-file library under the cgroup hierarchy
// built from files, with extra appended to the configuration. It returns what the child
// logged, the argv of the encode of that file, and its exit code.
func s0161Run(t *testing.T, files map[string]string, extra string) (recs []s0161Record, argv []string, code int) {
	t.Helper()
	real, err := exec.LookPath(envOr("HOLDFAST_FFMPEG", "ffmpeg"))
	if err != nil {
		t.Fatalf("::error:: ffmpeg required for the parallelism records: %v", err)
	}
	probeBin, err := exec.LookPath(envOr("HOLDFAST_FFPROBE", "ffprobe"))
	if err != nil {
		t.Fatalf("::error:: ffprobe required for the parallelism records: %v", err)
	}
	dir := t.TempDir()
	cgroup := filepath.Join(dir, "cgroup")
	lib := filepath.Join(dir, "media")
	for _, d := range []string{cgroup, lib} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range files {
		p := filepath.Join(cgroup, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(lib, "movie.mkv")
	if out, err := exec.Command(real, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10", "-c:v", "libx264", "-preset", "ultrafast",
		"-b:v", "8M", "-pix_fmt", "yuv420p", "--", src).CombinedOutput(); err != nil {
		t.Fatalf("building the library fixture: %v\n%s", err, out)
	}

	argvLog := filepath.Join(dir, "argv.log")
	wrapper := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\n{ for a in \"$@\"; do printf '%s\\n' \"$a\"; done; printf -- '---\\n'; } >> \"" +
		argvLog + "\"\nexec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + filepath.Join(dir, "state") +
		"\nvmaf_enable: false\nmin_bitrate_kbps: 0\npreset: ultrafast\n" + extra
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "HOLDFAST_") {
			env = append(env, kv)
		}
	}
	env = append(env, subprocessEnv+"=1", "HOLDFAST_FFMPEG="+wrapper, "HOLDFAST_FFPROBE="+probeBin,
		engine.CgroupRootEnv+"="+cgroup)
	var stderr bytes.Buffer
	cmd := exec.Command(os.Args[0], "run", "--config", cfgPath)
	cmd.Env = env
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	code = 0
	if runErr != nil {
		ee, ok := runErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("the child could not be run at all: %v", runErr)
		}
		code = ee.ExitCode()
	}

	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		r, ok := hpParseLine(line)
		if !ok {
			t.Fatalf("the child wrote a line that is not a levelled record: %s", line)
		}
		recs = append(recs, s0161Record{level: r.level, msg: r.msg, attrs: s0161Attrs(line), raw: line})
	}

	logged, _ := os.ReadFile(argvLog)
	for _, inv := range strings.Split(string(logged), "---\n") {
		args := strings.Split(strings.TrimSuffix(inv, "\n"), "\n")
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-i" && args[i+1] == src {
				argv = args
			}
		}
	}
	if code == 0 && argv == nil {
		t.Fatalf("the run exited 0 without encoding %s:\n%s", src, stderr.String())
	}
	return recs, argv, code
}

// s0161Attrs reads every key=value pair of a slog TEXT line, unquoting quoted values.
func s0161Attrs(line string) map[string]string {
	out := map[string]string{}
	rest := line
	for {
		rest = strings.TrimLeft(rest, " ")
		key, after, ok := strings.Cut(rest, "=")
		if !ok || key == "" {
			return out
		}
		if strings.HasPrefix(after, `"`) {
			q, err := strconv.QuotedPrefix(after)
			if err != nil {
				return out
			}
			v, _ := strconv.Unquote(q)
			out[key] = v
			rest = after[len(q):]
			continue
		}
		v, tail, _ := strings.Cut(after, " ")
		out[key] = v
		rest = tail
	}
}

// s0161Component returns the records the parallelism derivation emitted at level.
func s0161Component(recs []s0161Record, level string) []s0161Record {
	var out []s0161Record
	for _, r := range recs {
		if r.attrs["component"] == "encoder.parallelism" && strings.EqualFold(r.level, level) {
			out = append(out, r)
		}
	}
	return out
}

func s0161X265Params(argv []string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-x265-params" {
			return argv[i+1]
		}
	}
	return ""
}

// s0161Startup is the run's one startup record, failing unless there is exactly one.
func s0161Startup(t *testing.T, recs []s0161Record) s0161Record {
	t.Helper()
	var found []s0161Record
	for _, r := range s0161Component(recs, "INFO") {
		if r.msg == s0161Msg {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		var all []string
		for _, r := range recs {
			all = append(all, r.raw)
		}
		t.Fatalf("the run emitted %d %q info records, want exactly 1:\n%s", len(found), s0161Msg,
			strings.Join(all, "\n"))
	}
	return found[0]
}

func s0161Want(t *testing.T, r s0161Record, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if r.attrs[k] != v {
			t.Errorf("record attribute %s = %q, want %q\n%s", k, r.attrs[k], v, r.raw)
		}
	}
}

// TestS0161_AC7_TheRunStatesItsParallelismOnceAsAttributes grades AC-7 for each of the
// three sources a run can have: exactly one info record, its figures and source carried as
// attributes, its message the same constant in every case so no value is interpolated into
// it, and the encode the run then performs built from the figures it states.
func TestS0161_AC7_TheRunStatesItsParallelismOnceAsAttributes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		files  map[string]string
		want   map[string]string
		params string
	}{
		{"a cgroup quota", map[string]string{"cpu.max": "300000 100000\n"}, map[string]string{
			"x265_parallelism_source": "cgroup", "x265_cpus": "3", "x265_pools": "3", "x265_frame_threads": "1",
		}, "log-level=error:pools=3:frame-threads=1"},
		{"a cgroup with no limit", map[string]string{"cpu.max": "max 100000\n"}, map[string]string{
			"x265_parallelism_source": "no-quota-found", "x265_cpus": "auto", "x265_pools": "auto",
			"x265_frame_threads": "auto",
		}, "log-level=error"},
		{"no interface at all", map[string]string{}, map[string]string{
			"x265_parallelism_source": "no-quota-found", "x265_cpus": "auto", "x265_pools": "auto",
			"x265_frame_threads": "auto",
		}, "log-level=error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs, argv, code := s0161Run(t, tc.files, "")
			if code != 0 {
				t.Fatalf("the run exited %d", code)
			}
			s0161Want(t, s0161Startup(t, recs), tc.want)
			if got := s0161X265Params(argv); got != tc.params {
				t.Errorf("the run stated %v and encoded with -x265-params %q, want %q", tc.want, got, tc.params)
			}
			if w := s0161Component(recs, "WARN"); len(w) != 0 {
				t.Errorf("a cgroup with nothing broken in it produced a parallelism warn: %s", w[0].raw)
			}
		})
	}
}

// TestS0161_AC8_AConfiguredFigureWinsOverTheCgroup grades AC-8: x265_cpus on a host whose
// cgroup reports a different figure is the figure the encode is built from, and the
// record names the configuration as its source.
func TestS0161_AC8_AConfiguredFigureWinsOverTheCgroup(t *testing.T) {
	recs, argv, code := s0161Run(t, map[string]string{"cpu.max": "300000 100000\n"}, "x265_cpus: 2\n")
	if code != 0 {
		t.Fatalf("the run exited %d", code)
	}
	s0161Want(t, s0161Startup(t, recs), map[string]string{
		"x265_parallelism_source": "configuration", "x265_cpus": "2", "x265_pools": "2", "x265_frame_threads": "1",
	})
	if got, want := s0161X265Params(argv), "log-level=error:pools=2:frame-threads=1"; got != want {
		t.Errorf("-x265-params = %q, want the configured %q rather than the cgroup's 3", got, want)
	}
}

// TestS0161_AC6_ABrokenInterfaceIsOneWarnAndTheRunContinues grades AC-6: a bandwidth file
// that exists and cannot be read, or does not parse, is ONE warn naming the file, what was
// read there and that the run continues without a derived figure; the run completes with
// the encoder's own default parallelism; and nothing is recorded at error.
func TestS0161_AC6_ABrokenInterfaceIsOneWarnAndTheRunContinues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		read  string
	}{
		{"malformed", map[string]string{"cpu.max": "garbage 1 2 3\n"}, "garbage 1 2 3"},
		{"unreadable", map[string]string{"cpu.max/x": "a directory where the file should be"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs, argv, code := s0161Run(t, tc.files, "")
			if code != 0 {
				t.Fatalf("the run exited %d: a broken cgroup interface must not fail the run", code)
			}
			warns := s0161Component(recs, "WARN")
			if len(warns) != 1 {
				t.Fatalf("%d parallelism warn records, want exactly 1", len(warns))
			}
			w := warns[0]
			if !strings.HasSuffix(w.attrs["interface"], filepath.Join("cgroup", "cpu.max")) {
				t.Errorf("the warn names interface %q, want the cpu.max it tried\n%s", w.attrs["interface"], w.raw)
			}
			if w.attrs["read"] != tc.read {
				t.Errorf("the warn says it read %q there, want %q\n%s", w.attrs["read"], tc.read, w.raw)
			}
			if !strings.Contains(w.attrs["next_action"], "continue without a derived figure") {
				t.Errorf("the warn does not say the run continues without a derived figure\n%s", w.raw)
			}
			if w.attrs["err"] == "" {
				t.Errorf("the warn carries no error\n%s", w.raw)
			}
			for _, r := range recs {
				if strings.EqualFold(r.level, "ERROR") {
					t.Errorf("the run recorded at error: %s", r.raw)
				}
			}
			s0161Want(t, s0161Startup(t, recs), map[string]string{"x265_parallelism_source": "no-quota-found"})
			if got := s0161X265Params(argv); got != "log-level=error" {
				t.Errorf("-x265-params = %q, want the encoder's own default %q", got, "log-level=error")
			}
		})
	}
}

// TestS0161_AC9_ValidateExitsNonZeroOnAnOutOfRangeX265CPUs grades AC-9's command half.
func TestS0161_AC9_ValidateExitsNonZeroOnAnOutOfRangeX265CPUs(t *testing.T) {
	for _, bad := range []string{"-1", "1025", "2.5"} {
		var out, errOut bytes.Buffer
		code := dispatch([]string{"validate", "--config", emptyLibraryConfig(t, "x265_cpus: "+bad+"\n")}, &out, &errOut)
		if code == 0 {
			t.Errorf("validate exited 0 on x265_cpus: %s", bad)
		}
		if !strings.Contains(errOut.String(), "x265_cpus") {
			t.Errorf("validate's refusal of x265_cpus: %s does not name the key: %s", bad, errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"validate", "--config", emptyLibraryConfig(t, "x265_cpus: 4\n")}, &out, &errOut); code != 0 {
		t.Errorf("validate exited %d on x265_cpus: 4: %s", code, errOut.String())
	}
}
