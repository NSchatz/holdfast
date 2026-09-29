package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/logging"
)

// offsetTime matches a structured line's leading time field in the form S0176 requires:
// RFC 3339 at millisecond precision ending in a numeric offset, never `Z`.
var offsetTime = regexp.MustCompile(`^time=\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}([+-]\d{2}:\d{2}) `)

// cliProcessEnv is cliProcess with the child's environment adjusted and its two streams
// kept apart. A value of nil for a key unsets it; any other value sets it, the empty string
// included. The zone is read once when a Go process starts, so a claim about the clock a
// command logs under has to be made about a process started under that zone.
func cliProcessEnv(t *testing.T, env map[string]*string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	for _, kv := range os.Environ() {
		if _, set := env[strings.SplitN(kv, "=", 2)[0]]; !set {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, subprocessEnv+"=1")
	for k, v := range env {
		if v != nil {
			cmd.Env = append(cmd.Env, k+"="+*v)
		}
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errorsAs(err, &ee) {
			t.Fatalf("running %v: %v\n%s", args, err, errOut.String())
		}
		code = ee.ExitCode()
	}
	return code, out.String(), errOut.String()
}

func ptr(s string) *string { return &s }

// timedLines returns every line of a stream that is a structured record, with the offset
// its time field ends in. A record whose time field is not in the required form is a test
// failure naming the line: it is the defect, not noise.
func timedLines(t *testing.T, stream string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "time=") {
			continue
		}
		m := offsetTime.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("a structured line's time field carries no numeric offset:\n%s", line)
			continue
		}
		out[line] = m[1]
	}
	return out
}

// emptyLibrary is a configuration over a present, empty library root: every command can
// start over it, and nothing is probed, encoded or swapped.
func emptyLibrary(t *testing.T) (cfgPath string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + filepath.Join(dir, "state") + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// TestS0176ACH4_RestoreLogsItsLineWithANumericOffsetUnderUTC grades AC-H4: `holdfast
// restore <path>`, started as its own process under UTC, writes "undo window: restored
// the original" to stderr with a time field ending in `+00:00`.
func TestS0176ACH4_RestoreLogsItsLineWithANumericOffsetUnderUTC(t *testing.T) {
	cfgPath, _, _, src := undoLibrary(t, 24, "")
	before := sha256File(t, src)
	if code, out := cliProcess(t, "run", "--config", cfgPath); code != 0 {
		t.Fatalf("the run exited %d:\n%s", code, out)
	}
	if sha256File(t, src) == before {
		t.Fatal("the run did not swap, so there is nothing to restore")
	}

	code, _, stderr := cliProcessEnv(t, map[string]*string{"TZ": ptr("UTC")}, "restore", "--config", cfgPath, src)
	if code != 0 {
		t.Fatalf("restore exited %d:\n%s", code, stderr)
	}
	found := false
	for line, offset := range timedLines(t, stderr) {
		if !strings.Contains(line, `msg="undo window: restored the original"`) {
			continue
		}
		found = true
		if offset != "+00:00" {
			t.Errorf("under UTC the restore line's offset is %q, want +00:00:\n%s", offset, line)
		}
	}
	if !found {
		t.Fatalf("restore wrote no \"undo window: restored the original\" line to stderr:\n%s", stderr)
	}
}

// TestS0176ACH5_TheDefaultLoggerKeepsTheClockOnceACommandSetItUp grades AC-H5: once a
// subcommand has built its logger, a component handed no logger - an undo window is the
// one this proves it with - logs through the process-wide default in the same form, never
// in the standard log package's layout, which carries no offset.
func TestS0176ACH5_TheDefaultLoggerKeepsTheClockOnceACommandSetItUp(t *testing.T) {
	cfgPath := emptyLibrary(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		read <- string(b)
	}()
	saved := os.Stderr
	os.Stderr = w
	code, _, errOut := cli(t, "restore", "--config", cfgPath)
	engine.NewUndoWindow(config.Config{}, nil, nil).Log.Info("S0176 AC-H5: an undo window handed no logger")
	os.Stderr = saved
	_ = w.Close()
	stderr := <-read
	_ = r.Close()
	// Put the default back over the real stderr, so nothing later writes into the closed pipe.
	logging.New("info")

	if code != 0 {
		t.Fatalf("restore (listing) exited %d:\n%s", code, errOut)
	}
	found := false
	for line := range timedLines(t, stderr) {
		if strings.Contains(line, "S0176 AC-H5: an undo window handed no logger") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the default logger did not write the probe as a structured line with a numeric "+
			"offset; stderr was:\n%s", stderr)
	}
}

// TestS0176ACH6_AnUnknownOrEmptyZoneStillStartsAndStatesItsOffset grades AC-H6: a process
// started with `TZ` naming a zone the database does not hold, or with `TZ` set empty, still
// starts and logs, and every time field carries a numeric offset (the fallback zone's).
// Go falls back to UTC in both cases: `$TZ="" means use UTC`, and a zone it cannot load
// reaches "Fall back to UTC" (src/time/zoneinfo_unix.go, initLocal, go1.25.14; read
// 2026-09-29 from the pinned toolchain's own source).
func TestS0176ACH6_AnUnknownOrEmptyZoneStillStartsAndStatesItsOffset(t *testing.T) {
	for name, tz := range map[string]string{"unknown zone": "Not/AZone", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			cfgPath := emptyLibrary(t)
			code, _, stderr := cliProcessEnv(t, map[string]*string{"TZ": ptr(tz)}, "run", "--config", cfgPath)
			if code != 0 {
				t.Fatalf("run with TZ=%q exited %d:\n%s", tz, code, stderr)
			}
			lines := timedLines(t, stderr)
			if len(lines) == 0 {
				t.Fatalf("run with TZ=%q logged no structured line, so nothing was proved:\n%s", tz, stderr)
			}
			for line, offset := range lines {
				if offset != "+00:00" {
					t.Errorf("with TZ=%q a time field's offset is %q, want the fallback zone's +00:00:\n%s", tz, offset, line)
				}
			}
		})
	}
}
