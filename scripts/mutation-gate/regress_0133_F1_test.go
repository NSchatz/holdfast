package main

// Impl-gate finding F1 of S0133, documented as a test rather than as an opinion.
//
// AC-4: "WHEN the unscoped mutation run executes THE SYSTEM SHALL run on a declared
// schedule against the default branch and outside the pull-request gate, invoke the
// runner with no diff scope so that the whole mutation domain is mutated, take its floor
// from the same committed configuration the pull-request run takes it from, and publish
// its machine-readable report as a retrievable artifact of that run."
//
// Two packages inside the mutation domain declared by .gremlins.yaml - internal/probe and
// internal/encoder - fail loud when the real ffmpeg and ffprobe are not on PATH, by
// design ("a skip here would be a false green"). The workflow that runs the unscoped
// mutation run installs neither, unlike .github/workflows/ci.yml, which installs the
// pinned build through scripts/install-ffmpeg.sh. So on the scheduled run the gate's own
// baseline check stops it (requireGreenSuite, exit 9 "THE DOMAIN'S OWN SUITE IS NOT
// GREEN") before the runner is reached: nothing is mutated, no score is measured, no
// report is written, and the artifact AC-4 asks for is empty.
//
// This test FAILS while that stands. It passes when the domain and the job it runs in
// agree again - either the mutation job installs the pinned ffmpeg the way ci.yml does, or
// the two packages leave the domain and docs/mutation-testing.md records the exclusion
// with its reason.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/scripts/mutation"
)

// regress0133Root walks up to the module root, so the test can read the committed
// configuration and the workflow it is about.
func regress0133Root(t *testing.T) string {
	t.Helper()
	d, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			t.Fatalf("no go.mod above the test's working directory")
		}
		d = parent
	}
}

// regress0133NoFFmpeg makes this process look like the mutation job: every PATH entry
// carrying an ffmpeg or ffprobe binary is dropped, and the two override variables the
// suites read are pointed at nothing. The Go toolchain and everything else are untouched,
// and the change is undone when the test ends.
func regress0133NoFFmpeg(t *testing.T) {
	t.Helper()
	var kept []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		carries := false
		for _, bin := range []string{"ffmpeg", "ffprobe"} {
			if _, err := os.Stat(filepath.Join(dir, bin)); err == nil {
				carries = true
			}
		}
		if !carries {
			kept = append(kept, dir)
		}
	}
	t.Setenv("PATH", strings.Join(kept, string(os.PathListSeparator)))
	t.Setenv("HOLDFAST_FFMPEG", filepath.Join(os.TempDir(), "holdfast-regress-0133-absent-ffmpeg"))
	t.Setenv("HOLDFAST_FFPROBE", filepath.Join(os.TempDir(), "holdfast-regress-0133-absent-ffprobe"))
}

func TestRegress0133F1_UnscopedRunCannotMeasureTheDomainItIsWiredFor(t *testing.T) {
	root := regress0133Root(t)

	cfg, err := mutation.ReadConfig(root)
	if err != nil {
		t.Fatalf("read %s: %v", mutation.ConfigName, err)
	}
	var excludes []*regexp.Regexp
	for _, p := range cfg.ExcludeFiles() {
		re, err := regexp.Compile(p)
		if err != nil {
			t.Fatalf("exclude pattern %q: %v", p, err)
		}
		excludes = append(excludes, re)
	}

	// 1. The two ffmpeg-dependent packages are inside the domain.
	var inDomain []string
	for _, pkg := range []string{"internal/probe/", "internal/encoder/"} {
		excluded := false
		for _, re := range excludes {
			if re.MatchString(pkg) {
				excluded = true
			}
		}
		if !excluded {
			inDomain = append(inDomain, strings.TrimSuffix(pkg, "/"))
		}
	}
	if len(inDomain) == 0 {
		t.Skipf("both ffmpeg-dependent packages have left the mutation domain in %s - the finding no longer applies", mutation.ConfigName)
	}

	// 2. The job the unscoped run happens in installs no ffmpeg.
	wfPath := filepath.Join(root, ".github", "workflows", "mutation.yml")
	wf, err := os.ReadFile(wfPath)
	if err != nil {
		t.Fatalf("read %s: %v", wfPath, err)
	}
	if strings.Contains(string(wf), "install-ffmpeg") {
		t.Skip(".github/workflows/mutation.yml now installs ffmpeg - the finding no longer applies")
	}

	// 3. So the gate's own baseline check over those domain packages refuses to measure,
	//    which is the answer the scheduled run gets instead of a score.
	regress0133NoFFmpeg(t)
	var out, errOut strings.Builder
	code := requireGreenSuite(root, inDomain, &out, &errOut)
	if code == exitOK {
		t.Logf("the domain's suite is green with no ffmpeg reachable, so the domain does not need one after all")
		return
	}
	if code != exitBaseline {
		t.Fatalf("the baseline check returned %d, expected %d (exitBaseline):\n%s", code, exitBaseline, errOut.String())
	}

	t.Fatalf(`the mutation domain cannot be measured in the job the unscoped run is wired into.
  in the domain and needing the real binaries: %s
  .github/workflows/mutation.yml installs no ffmpeg (ci.yml installs the pinned build through scripts/install-ffmpeg.sh)
  requireGreenSuite over those packages, with no ffmpeg or ffprobe reachable, returned %d:
%s
  That is the scheduled run's whole result: no mutant, no score, no report to publish.`,
		strings.Join(inDomain, " "), code, regress0133Indent(errOut.String()))
}

func regress0133Indent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		b.WriteString("    ")
		b.WriteString(strings.TrimRight(line, " "))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
