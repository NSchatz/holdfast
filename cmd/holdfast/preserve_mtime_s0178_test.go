package main

// S0178, the statement half: `validate` and startup state the effective preserve_mtime
// choice. The default stays true by the owner's decision; these tests grade only that
// the choice is said, once, on the stream and at the level every other notice is.

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
)

// s0178Config writes a minimal configuration over an EMPTY library, so nothing here can
// reach an encode, and returns its path.
func s0178Config(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + filepath.Join(dir, "state") + "\n" + extra
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// s0178Cases is the table both tests walk: where the value comes from, what it is, and
// the phrase that states that value's consequence (compared case-insensitively).
var s0178Cases = []struct {
	name, file, env string
	value           string
	say, unsay      string
	source          string
}{
	{name: "default", value: "true", say: "modification time alone", unsay: "recently added", source: "the default: neither"},
	{name: "file true", file: "preserve_mtime: true\n", value: "true", say: "modification time alone", unsay: "recently added", source: "set explicitly"},
	{name: "file false", file: "preserve_mtime: false\n", value: "false", say: "recently added", unsay: "modification time alone", source: "set explicitly"},
	{name: "env false", env: "false", value: "false", say: "recently added", unsay: "modification time alone", source: "set explicitly"},
	{name: "env true over file false", file: "preserve_mtime: false\n", env: "true", value: "true", say: "modification time alone", unsay: "recently added", source: "set explicitly"},
}

func s0178Check(t *testing.T, line, value, say, unsay, source string) {
	t.Helper()
	low := strings.ToLower(line)
	if !strings.Contains(line, "preserve_mtime is "+value+" ") {
		t.Errorf("the statement does not carry the effective value %s:\n%s", value, line)
	}
	if !strings.Contains(low, say) {
		t.Errorf("the statement never says %q:\n%s", say, line)
	}
	if strings.Contains(low, unsay) {
		t.Errorf("the statement for %s carries the other setting's consequence (%q):\n%s", value, unsay, line)
	}
	if !strings.Contains(line, source) {
		t.Errorf("the statement does not say %q:\n%s", source, line)
	}
	if !strings.Contains(low, "default") {
		t.Errorf("the statement does not mention the default:\n%s", line)
	}
}

// TestValidate_S0178_AC5_PrintsExactlyOnePreserveMtimeNote: an accepted configuration
// gets exactly one `note:` line on STDOUT naming the key and its effective value.
func TestValidate_S0178_AC5_PrintsExactlyOnePreserveMtimeNote(t *testing.T) {
	for _, tc := range s0178Cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("HOLDFAST_PRESERVE_MTIME", tc.env)
			}
			var out, errOut bytes.Buffer
			if code := dispatch([]string{"validate", "--config", s0178Config(t, tc.file)}, &out, &errOut); code != 0 {
				t.Fatalf("validate exited %d (stderr: %s)", code, errOut.String())
			}
			var notes []string
			for _, line := range strings.Split(out.String(), "\n") {
				if strings.Contains(line, "preserve_mtime") {
					notes = append(notes, line)
				}
			}
			if len(notes) != 1 {
				t.Fatalf("validate printed %d stdout line(s) naming preserve_mtime, want exactly 1:\n%s", len(notes), out.String())
			}
			if !strings.HasPrefix(notes[0], "note: ") {
				t.Errorf("the statement is not a `note:` line (a notice is not a warning):\n%s", notes[0])
			}
			s0178Check(t, notes[0], tc.value, tc.say, tc.unsay, tc.source)
			if strings.Contains(errOut.String(), "preserve_mtime") {
				t.Errorf("the statement went to stderr; it is requested output and belongs on stdout:\n%s", errOut.String())
			}
		})
	}
}

// TestStartup_S0178_AC5_LogsThePreserveMtimeChoice: the same statement goes through the
// structured logger at startup, once, at WARN - the level every other Notices() line is
// logged at, so it survives `log_level: warn`.
//
// logConfigWarnings is the emission point `run` and `serve` both go through, so the record
// is read there for every case; the last subtest then runs the real `run` command in a
// child process over an empty library and reads the record off its stderr.
func TestStartup_S0178_AC5_LogsThePreserveMtimeChoice(t *testing.T) {
	for _, tc := range s0178Cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("HOLDFAST_PRESERVE_MTIME", tc.env)
			}
			cfg, err := config.Load(s0178Config(t, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			logConfigWarnings(cfg, slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			var about []map[string]any
			for _, rec := range logRecords(t, &buf) {
				if msg, _ := rec["msg"].(string); strings.Contains(msg, "preserve_mtime") {
					about = append(about, rec)
				}
			}
			if len(about) != 1 {
				t.Fatalf("startup logged %d record(s) naming preserve_mtime, want exactly 1: %v", len(about), about)
			}
			if about[0]["level"] != "WARN" {
				t.Errorf("the statement is logged at %v, want WARN, the level every other notice is logged at", about[0]["level"])
			}
			msg, _ := about[0]["msg"].(string)
			s0178Check(t, msg, tc.value, tc.say, tc.unsay, tc.source)

			// And it is the SAME statement `validate` prints.
			var out, errOut bytes.Buffer
			if code := dispatch([]string{"validate", "--config", s0178Config(t, tc.file)}, &out, &errOut); code != 0 {
				t.Fatalf("validate exited %d (stderr: %s)", code, errOut.String())
			}
			if !strings.Contains(out.String(), "note: "+msg+"\n") {
				t.Errorf("startup and validate state the choice in different words.\nstartup: %s\nvalidate:\n%s", msg, out.String())
			}
		})
	}

	t.Run("the real run command, at log_level warn", func(t *testing.T) {
		code, out := cliProcess(t, "run", "--config", s0178Config(t, "log_level: warn\n"))
		if code != 0 {
			t.Fatalf("run over an empty library exited %d:\n%s", code, out)
		}
		var about []string
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "preserve_mtime") {
				about = append(about, line)
			}
		}
		if len(about) != 1 {
			t.Fatalf("run logged %d line(s) naming preserve_mtime, want exactly 1:\n%s", len(about), out)
		}
		if !strings.Contains(about[0], "level=WARN") {
			t.Errorf("the statement is not a WARN record:\n%s", about[0])
		}
		s0178Check(t, about[0], "true", "modification time alone", "recently added", "the default: neither")
	})
}

// TestCommands_S0178_AC6_ANonBooleanPreserveMtimeRefusesBeforeAnyWork: validate, run and
// serve each exit non-zero naming the key, from the file and from the environment, and
// none of them reaches the store.
func TestCommands_S0178_AC6_ANonBooleanPreserveMtimeRefusesBeforeAnyWork(t *testing.T) {
	for _, src := range []struct{ name, file, env string }{
		{name: "file", file: "preserve_mtime: banana\n"},
		{name: "file number", file: "preserve_mtime: 3\n"},
		{name: "env", env: "banana"},
	} {
		for _, cmd := range []string{"validate", "run", "serve"} {
			t.Run(src.name+"/"+cmd, func(t *testing.T) {
				if src.env != "" {
					t.Setenv("HOLDFAST_PRESERVE_MTIME", src.env)
				}
				cfgPath := s0178Config(t, src.file)
				var out, errOut bytes.Buffer
				if code := dispatch([]string{cmd, "--config", cfgPath}, &out, &errOut); code == 0 {
					t.Fatalf("%s exited 0 on a non-boolean preserve_mtime\nstdout: %s", cmd, out.String())
				}
				if !strings.Contains(errOut.String(), "preserve_mtime") {
					t.Errorf("%s's refusal does not name preserve_mtime: %s", cmd, errOut.String())
				}
				if strings.Contains(out.String(), "config OK") {
					t.Errorf("%s reported the configuration as accepted:\n%s", cmd, out.String())
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(cfgPath), "state", "jobs.db")); err == nil {
					t.Errorf("%s opened the job store before refusing", cmd)
				}
			})
		}
	}
}
