package main

// The bounded run at the COMMAND surface (S0100): `holdfast run --file <path>` and
// `holdfast run --limit N`.
//
// What these cases grade is the door, never the pipeline behind it. A bounded run is a
// SMALLER run and not a lighter one, so every case here is about one of three things: the
// refusals the two flags add and the code they leave by, the startup sequence a bounded
// run still pays for in full, and the help text a caller discovers the surface from.

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
)

// boundedLayout builds a library root, a state directory that does NOT exist yet, and a
// config naming both. The state directory's absence is load-bearing: a refusal that
// creates nothing is asserted by looking at the directory rather than by reading a log
// line (AC-2, AC-3).
func boundedLayout(t *testing.T, extra string) (cfgPath, lib, state string) {
	t.Helper()
	dir := t.TempDir()
	lib = filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	state = filepath.Join(dir, "state")
	cfgPath = filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + state + "\n" + extra
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, lib, state
}

// touch writes a placeholder file. Every case that uses one refuses BEFORE anything is
// probed or encoded, so the bytes never matter - only that a path exists and carries the
// name it carries.
func touch(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not really a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunFile_RefusesAPathOutsideTheRoots grades AC-2: a `--file` target outside every
// configured library root refuses the WHOLE run before anything is probed, encoded or
// mutated, names the path and the rule that rejected it on stderr, and leaves by a code
// distinct from both the environment/startup code and the invocation-error code.
func TestRunFile_RefusesAPathOutsideTheRoots(t *testing.T) {
	cfgPath, _, state := boundedLayout(t, "")
	outside := touch(t, filepath.Join(filepath.Dir(state), "elsewhere", "film.mkv"))

	var out, errOut bytes.Buffer
	code := dispatch([]string{"run", "--config", cfgPath, "--file", outside}, &out, &errOut)
	if code != exitRefused {
		t.Fatalf("run --file outside the roots exited %d, want %d (stderr: %s)", code, exitRefused, errOut.String())
	}
	// Distinct from BOTH of the codes this command already spends, which is the whole
	// point of the criterion: a caller must be able to tell a policy refusal from a typo
	// and from a broken environment.
	if exitRefused == exitError || exitRefused == exitUsage {
		t.Fatalf("the refusal code %d collides with exitError=%d / exitUsage=%d", exitRefused, exitError, exitUsage)
	}
	if !strings.Contains(errOut.String(), outside) {
		t.Errorf("the refusal did not name the rejected path:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), engine.RuleOutsideRoots) {
		t.Errorf("the refusal did not name the rule that rejected it (%s):\n%s", engine.RuleOutsideRoots, errOut.String())
	}
	if _, err := os.Stat(state); err == nil {
		t.Error("the refused run created the state directory")
	}
}

// TestRunFile_StillRunsTheStartupChecks grades AC-8: a bounded run performs the same
// start-or-refuse startup sequence an unbounded one does and refuses on the same
// conditions with the same code. Two of that sequence's steps are exercised here because
// they are the two a `--file` run could plausibly have been narrowed past: the filesystem
// classification decision (which AC-9 explicitly scopes) and the required-binary check.
func TestRunFile_StillRunsTheStartupChecks(t *testing.T) {
	t.Run("the filesystem classification still refuses", func(t *testing.T) {
		cfgPath, state := nasLayout(t, "")
		target := touch(t, filepath.Join(filepath.Dir(filepath.Dir(state)), "media", "film.mkv"))

		var out, errOut bytes.Buffer
		if code := dispatch([]string{"run", "--config", cfgPath, "--file", target}, &out, &errOut); code != exitError {
			t.Fatalf("a bounded run over a state directory on network storage exited %d, want %d (stderr: %s)",
				code, exitError, errOut.String())
		}
		if _, err := os.Stat(state); err == nil {
			t.Error("the refused bounded run created the state directory")
		}
		if !strings.Contains(errOut.String(), state) {
			t.Errorf("the refusal did not name the path:\n%s", errOut.String())
		}
	})

	t.Run("the required-binary check still refuses", func(t *testing.T) {
		cfgPath, lib, state := boundedLayout(t, "")
		target := touch(t, filepath.Join(lib, "film.mkv"))
		t.Setenv("HOLDFAST_FFMPEG", "holdfast-no-such-encoder-binary")

		var out, errOut bytes.Buffer
		if code := dispatch([]string{"run", "--config", cfgPath, "--file", target}, &out, &errOut); code != exitError {
			t.Fatalf("a bounded run with the encoder binary missing exited %d, want %d (stderr: %s)",
				code, exitError, errOut.String())
		}
		if !strings.Contains(errOut.String(), "holdfast-no-such-encoder-binary") {
			t.Errorf("the refusal did not name the binary it could not find:\n%s", errOut.String())
		}
		if _, err := os.Stat(filepath.Join(state, "jobs.db")); err == nil {
			t.Error("the refused bounded run opened the job store")
		}
	})
}

// TestRunFile_RefusesAPathItCannotActOn grades AC-3: a `--file` target that does not
// exist, is not a regular file, or cannot be read by this process refuses the run with the
// SAME code AC-2 uses and a reason naming the path, creates nothing in or under the state
// directory, and leaves the named path byte-for-byte as it was.
func TestRunFile_RefusesAPathItCannotActOn(t *testing.T) {
	t.Run("nothing is there", func(t *testing.T) {
		cfgPath, lib, state := boundedLayout(t, "")
		missing := filepath.Join(lib, "never-existed.mkv")

		var out, errOut bytes.Buffer
		if code := dispatch([]string{"run", "--config", cfgPath, "--file", missing}, &out, &errOut); code != exitRefused {
			t.Fatalf("run --file over a path that is not there exited %d, want %d (stderr: %s)",
				code, exitRefused, errOut.String())
		}
		if !strings.Contains(errOut.String(), missing) {
			t.Errorf("the refusal did not name the path:\n%s", errOut.String())
		}
		mustNotExist(t, state)
	})

	t.Run("it is a directory", func(t *testing.T) {
		cfgPath, lib, state := boundedLayout(t, "")
		dir := filepath.Join(lib, "season.mkv")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}

		var out, errOut bytes.Buffer
		if code := dispatch([]string{"run", "--config", cfgPath, "--file", dir}, &out, &errOut); code != exitRefused {
			t.Fatalf("run --file over a directory exited %d, want %d (stderr: %s)", code, exitRefused, errOut.String())
		}
		if !strings.Contains(errOut.String(), dir) {
			t.Errorf("the refusal did not name the path:\n%s", errOut.String())
		}
		mustNotExist(t, state)
	})

	t.Run("this process cannot read it", func(t *testing.T) {
		cfgPath, lib, state := boundedLayout(t, "")
		target := touch(t, filepath.Join(lib, "locked.mkv"))
		before := readFile(t, target)
		if err := os.Chmod(target, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(target, 0o600) })
		if _, err := os.ReadFile(target); err == nil {
			t.Skip("this process can read a mode-000 file (running as root); the refusal cannot be exercised here")
		}

		var out, errOut bytes.Buffer
		if code := dispatch([]string{"run", "--config", cfgPath, "--file", target}, &out, &errOut); code != exitRefused {
			t.Fatalf("run --file over an unreadable file exited %d, want %d (stderr: %s)",
				code, exitRefused, errOut.String())
		}
		if !strings.Contains(errOut.String(), target) {
			t.Errorf("the refusal did not name the path:\n%s", errOut.String())
		}
		mustNotExist(t, state)
		// Byte-for-byte what it was: the refusal read nothing into it and wrote nothing.
		if err := os.Chmod(target, 0o600); err != nil {
			t.Fatal(err)
		}
		if after := readFile(t, target); !bytes.Equal(before, after) {
			t.Error("the refused run changed the bytes at the named path")
		}
	})
}

// mustNotExist fails when a refused run left anything at path. The state directory is the
// one every AC-2/AC-3 case checks, because a refusal that had already opened the job store
// would be a refusal that created a database on storage nobody had approved.
func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Errorf("the refused run created %s", path)
	}
}

// TestRunLimit_RefusesAValueThatIsNotACount grades AC-6: a `--limit` that is not an
// integer of at least 1 is refused with the SAME invocation-error code a missing --config
// returns, naming the flag and the value that was rejected.
func TestRunLimit_RefusesAValueThatIsNotACount(t *testing.T) {
	cfgPath, _, state := boundedLayout(t, "")

	// The code a missing --config returns, read from the command rather than asserted as
	// a literal: the criterion is that the two agree.
	var missingOut, missingErr bytes.Buffer
	missingConfig := dispatch([]string{"run"}, &missingOut, &missingErr)
	if missingConfig != exitUsage {
		t.Fatalf("run with no --config exited %d, want %d", missingConfig, exitUsage)
	}

	for _, value := range []string{"0", "-1", "abc", "2.5", " "} {
		t.Run("--limit "+strconv.Quote(value), func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := dispatch([]string{"run", "--config", cfgPath, "--limit", value}, &out, &errOut)
			if code != missingConfig {
				t.Fatalf("run --limit %q exited %d, want %d (stderr: %s)", value, code, missingConfig, errOut.String())
			}
			if !strings.Contains(errOut.String(), "--limit") {
				t.Errorf("the refusal did not name the flag:\n%s", errOut.String())
			}
			if strings.TrimSpace(value) != "" && !strings.Contains(errOut.String(), value) {
				t.Errorf("the refusal did not name the rejected value %q:\n%s", value, errOut.String())
			}
			mustNotExist(t, state)
		})
	}
}

// TestRunBoth_AcceptsTheCombinationAndAppliesTheFileBound grades AC-7: `--file` and
// `--limit` together are a well-formed invocation, and the single-file bound is what
// applies - at most one file reaches a terminal outcome.
//
// The library holds two files a run would really transcode, so "the other one was not
// touched" is a fact about the library rather than about a log line.
func TestRunBoth_AcceptsTheCombinationAndAppliesTheFileBound(t *testing.T) {
	ffmpeg := envOr("HOLDFAST_FFMPEG", "ffmpeg")
	if _, err := exec.LookPath(ffmpeg); err != nil {
		t.Fatalf("::error:: ffmpeg is required here: a bounded run that never ran proves nothing: %v", err)
	}
	cfgPath, lib, _ := boundedLayout(t, "min_bitrate_kbps: 0\nvmaf_enable: false\n")
	target := h264Fixture(t, ffmpeg, filepath.Join(lib, "a_target.mkv"))
	other := h264Fixture(t, ffmpeg, filepath.Join(lib, "b_other.mkv"))

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"run", "--config", cfgPath, "--file", target, "--limit", "5"}, &out, &errOut); code != exitOK {
		t.Fatalf("run --file with --limit exited %d, want %d (stderr: %s)", code, exitOK, errOut.String())
	}
	if codec := videoCodec(t, target); codec == "h264" {
		t.Errorf("the named file was not carried to an outcome: %s is still %s", target, codec)
	}
	if codec := videoCodec(t, other); codec != "h264" {
		t.Errorf("a second file was processed under a --file bound: %s is now %s", other, codec)
	}
}

// h264Fixture writes a real, transcodable h264 source.
func h264Fixture(t *testing.T, ffmpeg, path string) string {
	t.Helper()
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M", "-pix_fmt", "yuv420p", "--", path).CombinedOutput()
	if err != nil {
		t.Fatalf("build fixture %s: %v\n%s", path, err, out)
	}
	return path
}

func videoCodec(t *testing.T, path string) string {
	t.Helper()
	ffprobe := envOr("HOLDFAST_FFPROBE", "ffprobe")
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name", "-of", "default=nw=1:nk=1", "--", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

// TestRunFile_ScopesTheClassificationWithoutMovingTheTargetsDecision grades AC-9: a
// `--file` run may reach the start-or-refuse decision for its target without classifying
// roots it will not touch, the decision FOR THAT PATH is identical either way, and the
// roots it did not classify are recorded at info.
func TestRunFile_ScopesTheClassificationWithoutMovingTheTargetsDecision(t *testing.T) {
	// Two configured roots: one real, one that does not exist. A whole-library
	// classification refuses the run over the second (a missing root is row 2); a run
	// acting under the first never reaches a file under it.
	layout := func(t *testing.T) (cfg *config.Config, lib, absent string) {
		t.Helper()
		dir := t.TempDir()
		lib = filepath.Join(dir, "media")
		absent = filepath.Join(dir, "unmounted")
		if err := os.MkdirAll(lib, 0o755); err != nil {
			t.Fatal(err)
		}
		cfgPath := filepath.Join(dir, "config.yaml")
		body := "library_roots:\n  - " + lib + "\n  - " + absent + "\nstate_dir: " + filepath.Join(dir, "state") + "\n"
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, err := config.Load(cfgPath)
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		return loaded, lib, absent
	}

	t.Run("a whole-library classification refuses over the other root", func(t *testing.T) {
		cfg, _, absent := layout(t)
		res, code := startupCheck(cfg, discardLog(), io.Discard, classifyScope{})
		if code != exitError || res.Start {
			t.Fatalf("the whole-library check started with %s missing (code %d); this case has nothing to narrow", absent, code)
		}
	})

	t.Run("a scoped classification starts and names what it did not classify", func(t *testing.T) {
		cfg, lib, absent := layout(t)
		target := touch(t, filepath.Join(lib, "film.mkv"))
		log, buf := jsonLog()
		res, code := startupCheck(cfg, log, io.Discard, classifyScope{Root: lib, File: target})
		if code != exitOK || !res.Start {
			t.Fatalf("a scoped check refused the run (code %d, row %d): the target's own root is fine", code, res.Row)
		}
		if len(res.Unclassified) != 1 || res.Unclassified[0] != absent {
			t.Fatalf("the check reports %v as unclassified, want exactly [%s]", res.Unclassified, absent)
		}
		// Recorded as FIELDS rather than as a sentence, at info, naming the target, the
		// root that was classified and every root that was not.
		found := false
		for _, rec := range decodeLines(t, buf) {
			if rec["classification"] != "scoped-to-target" {
				continue
			}
			found = true
			if rec["level"] != "INFO" {
				t.Errorf("the scoping record is at %v, want INFO", rec["level"])
			}
			if rec["file"] != target {
				t.Errorf("the scoping record names file %v, want %s", rec["file"], target)
			}
			if rec["classified_library_root"] != lib {
				t.Errorf("the scoping record names root %v, want %s", rec["classified_library_root"], lib)
			}
			if got := fmt.Sprint(rec["library_roots_not_classified"]); !strings.Contains(got, absent) {
				t.Errorf("the scoping record does not name the root it did not classify: %v", got)
			}
		}
		if !found {
			t.Errorf("no record says the classification was scoped:\n%s", buf.String())
		}
	})

	t.Run("the target's own root decides identically", func(t *testing.T) {
		// The narrowing must not reach the target: a condition under ITS root refuses a
		// scoped run exactly as it refuses a whole-library one.
		cfgPath, _ := nasLayout(t, "")
		cfg, err := config.Load(cfgPath)
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		lib := cfg.LibraryRoots[0]
		target := touch(t, filepath.Join(lib, "film.mkv"))

		whole, wholeCode := startupCheck(cfg, discardLog(), io.Discard, classifyScope{})
		scoped, scopedCode := startupCheck(cfg, discardLog(), io.Discard, classifyScope{Root: lib, File: target})
		if wholeCode != scopedCode || whole.Start != scoped.Start || whole.Row != scoped.Row {
			t.Fatalf("scoping moved the decision for the target's own root: whole (code %d, start %v, row %d) "+
				"against scoped (code %d, start %v, row %d)", wholeCode, whole.Start, whole.Row,
				scopedCode, scoped.Start, scoped.Row)
		}
		if len(scoped.Unclassified) != 0 {
			t.Errorf("a single-root configuration reported %v as unclassified", scoped.Unclassified)
		}
	})
}

// TestRunHelp_ListsTheBoundsTheExampleAndEveryExitCode grades AC-12: `run --help` lists
// both bounded flags with a description each, at least one runnable bounded example, and
// every code in the declared table with its meaning - and the table is ONE declared set in
// the source that the help text and this check both read.
func TestRunHelp_ListsTheBoundsTheExampleAndEveryExitCode(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"run", "--help"}, &out, &errOut); code != exitOK {
		t.Fatalf("run --help exited %d, want %d", code, exitOK)
	}
	// Requested output, on stdout, where a caller discovering the surface can parse it
	// (cli L2, L5).
	help := out.String()
	if help == "" {
		t.Fatalf("run --help wrote nothing to stdout (stderr: %s)", errOut.String())
	}
	for _, want := range []string{"-file", "-limit"} {
		if !strings.Contains(help, want) {
			t.Errorf("the help does not list %s:\n%s", want, help)
		}
	}
	// A runnable bounded example: a line that invokes this command with a bound on it.
	example := false
	for _, line := range strings.Split(help, "\n") {
		if strings.Contains(line, "holdfast run --config") &&
			(strings.Contains(line, "--file ") || strings.Contains(line, "--limit ")) {
			example = true
		}
	}
	if !example {
		t.Errorf("the help carries no runnable bounded example:\n%s", help)
	}
	// EVERY code in the declared table, with its meaning. The expectation is the table
	// itself, so a code added there and not rendered fails here.
	for _, c := range runExitCodes {
		if !strings.Contains(help, strconv.Itoa(c.Code)) || !strings.Contains(help, c.Meaning) {
			t.Errorf("the help does not carry exit code %d with its meaning:\n%s", c.Code, help)
		}
	}
}

// TestRunExitCodes_TheTableIsTheOnlySourceOfTruth is the other half of AC-12, and it is
// the half that keeps the help honest as the command changes: every exit code `run` can
// return has a row in the declared table.
//
// It reads the RUN PATH out of the source: cmdRun and every function in this package it
// can reach, down to each integer literal returned as an exit code. A `return 4` added
// anywhere on that path, without a row in runExitCodes, fails here rather than shipping as
// a number the help text never mentions.
//
// A code can reach the command without being a literal, though, and that is the second
// assertion below: `const exitSomething = 4` returned BY NAME is not an integer literal
// anywhere on the path, so the walk cannot see it. Every exit* constant this package
// declares must therefore appear in the table itself, which is the same property from the
// other end - one declared set, no code outside it.
func TestRunExitCodes_TheTableIsTheOnlySourceOfTruth(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse the command package: %v", err)
	}
	pkg, ok := pkgs["main"]
	if !ok {
		t.Fatal("no package main here; this check cannot read the run path")
	}
	funcs := map[string]*ast.FuncDecl{}
	for _, f := range pkg.Files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				funcs[fd.Name.Name] = fd
			}
		}
	}
	if _, ok := funcs["cmdRun"]; !ok {
		t.Fatal("cmdRun not found; this check cannot read the run path")
	}

	declared := map[int]bool{}
	for _, c := range runExitCodes {
		declared[c.Code] = true
	}

	seen := map[string]bool{}
	var walk func(name string)
	walk = func(name string) {
		fd, ok := funcs[name]
		if !ok || seen[name] {
			return
		}
		seen[name] = true
		if at, isCode := exitCodeResult(fd); isCode {
			for _, lit := range returnedIntLiterals(fd, at) {
				if !declared[lit] {
					t.Errorf("%s returns exit code %d, which has no row in runExitCodes - "+
						"`run --help` would never mention it", name, lit)
				}
			}
		}
		ast.Inspect(fd, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok {
					walk(id.Name)
				}
			}
			return true
		})
	}
	walk("cmdRun")

	// The anti-vacuity arm: the walk really reaches the functions that return `run`'s
	// codes. A traversal that reached nothing would agree with any table at all.
	for _, want := range []string{"loadConfig", "buildEngine", "startupCheck", "resolveBound"} {
		if !seen[want] {
			t.Errorf("the run-path walk never reached %s, so it grades less than it claims to", want)
		}
	}

	// Every exit* constant the package declares has a row. This is the leg the walk above
	// cannot cover: a code spelled as a named constant and returned by name is no integer
	// literal, so it would otherwise reach the command with nothing in the table and
	// nothing in the help.
	named := exitConstants(pkg.Files)
	if !named["exitRefused"] {
		t.Fatal("no exit* constants were found, so this leg grades nothing")
	}
	inTable := identsIn(pkg.Files, "runExitCodes")
	if len(inTable) == 0 {
		t.Fatal("runExitCodes was not found, so this leg grades nothing")
	}
	for name := range named {
		if !inTable[name] {
			t.Errorf("%s is declared as an exit code and has no row in runExitCodes - "+
				"returning it by name would reach a caller that `run --help` never told about it", name)
		}
	}
}

// exitConstants is every constant in the package whose name declares it an exit code.
func exitConstants(files map[string]*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range vs.Names {
					if strings.HasPrefix(name.Name, "exit") {
						out[name.Name] = true
					}
				}
			}
		}
	}
	return out
}

// identsIn is every identifier named in the value of the package-level variable v - here
// the table itself, so what it declares is read from the declaration rather than from a
// second list a test would have to maintain.
func identsIn(files map[string]*ast.File, v string) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if name.Name != v || i >= len(vs.Values) {
						continue
					}
					ast.Inspect(vs.Values[i], func(n ast.Node) bool {
						if id, ok := n.(*ast.Ident); ok {
							out[id.Name] = true
						}
						return true
					})
				}
			}
		}
	}
	return out
}

// exitCodeResult reports the result position carrying an exit code - the LAST result,
// where it is an int - and whether there is one.
func exitCodeResult(fd *ast.FuncDecl) (int, bool) {
	if fd.Type.Results == nil || len(fd.Type.Results.List) == 0 {
		return 0, false
	}
	at := 0
	for _, field := range fd.Type.Results.List {
		n := len(field.Names)
		if n == 0 {
			n = 1
		}
		at += n
	}
	last := fd.Type.Results.List[len(fd.Type.Results.List)-1]
	id, ok := last.Type.(*ast.Ident)
	if !ok || id.Name != "int" {
		return 0, false
	}
	return at - 1, true
}

// returnedIntLiterals collects every integer LITERAL returned in the exit-code position.
// A named constant is not one: those are covered by the constants leg of the check above,
// which holds every exit* constant to a row in the table rather than assuming it has one.
func returnedIntLiterals(fd *ast.FuncDecl, at int) []int {
	var out []int
	ast.Inspect(fd, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || at >= len(ret.Results) {
			return true
		}
		lit, ok := ret.Results[at].(*ast.BasicLit)
		if !ok || lit.Kind != token.INT {
			return true
		}
		if v, err := strconv.Atoi(lit.Value); err == nil {
			out = append(out, v)
		}
		return true
	})
	return out
}
