package sourceoffer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The STANDING coverage for the container build path (LICENSE-3 / AC13), and it runs
// with NO container runtime, inside `make check`, on every machine.
//
// A docker step is deliberately NOT added to `make check`: that target is this repo's
// gate and must stay runnable by a human with no docker, and the image build pulls
// three base images and a 100 MB ffmpeg tarball. So the gate asserts the WIRING
// instead - that the container build definition declares the build argument, defaults
// it to the same upstream URL the Go package does, and threads it into the SAME
// -ldflags invocation that carries the version stamp. That is a proxy and is accepted
// as one: it cannot prove an image serves the value, only that the wiring which makes
// it do so has not silently drifted. The end-to-end docker-built observation is a
// manual one-off, recorded by the implementer.
//
// The same assertions are made against the Makefile, because a fork must be able to
// point the offer at its own tree on BOTH paths that build the binary today.

func repoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// ldflagsValue returns the text of the first -ldflags=" ... " value in s,
// continuation backslashes and all.
func ldflagsValue(t *testing.T, s, what string) string {
	t.Helper()
	const marker = `-ldflags="`
	i := strings.Index(s, marker)
	if i < 0 {
		t.Fatalf("%s has no -ldflags=\" invocation", what)
	}
	rest := s[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("%s has an unterminated -ldflags value", what)
	}
	return rest[:j]
}

// The -X assignments that must ride ONE ldflags invocation. The source URL travels
// with the version stamp because the offer is the link PLUS the build identity: split
// them across two builds and the page could name one tree while the binary came from
// another. Quoting around an assignment is allowed (the source URL is single-quoted so
// a value carrying a space survives go's shell-like splitting of the -ldflags value),
// so the assignment is matched, not the exact `-X ` spelling - with the count of -X
// flags pinned separately.
var wantX = []string{
	"github.com/NSchatz/holdfast/internal/version.Version=",
	"github.com/NSchatz/holdfast/internal/version.Commit=",
	"github.com/NSchatz/holdfast/internal/version.Date=",
	"github.com/NSchatz/holdfast/internal/sourceoffer.URL=",
}

// ldflagsProblems reports every way ld departs from "one invocation carrying exactly
// the four -X assignments above". It returns findings rather than calling t.Errorf so
// the mutation test at the bottom of this file can prove it BITES: a check that
// cannot fail is not evidence.
func ldflagsProblems(ld string) []string {
	var out []string
	for _, x := range wantX {
		if !strings.Contains(ld, x) {
			out = append(out, "the -ldflags invocation is missing -X "+x)
		}
	}
	if got := strings.Count(ld, "-X "); got != len(wantX) {
		out = append(out, "the -ldflags invocation carries the wrong number of -X flags: "+ld)
	}
	return out
}

func assertOneLdflagsInvocation(t *testing.T, ld, what string) {
	t.Helper()
	for _, p := range ldflagsProblems(ld) {
		t.Errorf("%s: %s", what, p)
	}
}

func TestBuildWiring_DockerfileThreadsTheSourceURLIntoTheVersionLdflags(t *testing.T) {
	df := repoFile(t, "Dockerfile")

	// 1. The build argument is declared, and its default is the SAME upstream URL the
	//    Go package carries - so an image built with the argument unset offers
	//    upstream (AC13's second limb), and this copy of the URL cannot drift from the
	//    built-in one.
	wantARG := "ARG SOURCE_URL=" + Upstream
	argAt := strings.Index(df, wantARG)
	if argAt < 0 {
		t.Fatalf("Dockerfile does not declare %q - a fork could not point the image's source offer at its own tree", wantARG)
	}

	// 2. It is declared in the stage that builds the binary, ahead of the build.
	ldAt := strings.Index(df, `-ldflags="`)
	if ldAt < 0 {
		t.Fatal("Dockerfile has no -ldflags invocation")
	}
	if argAt > ldAt {
		t.Error("ARG SOURCE_URL is declared after the go build that consumes it, so it would expand to nothing")
	}

	// 3. It rides the same -ldflags invocation as the version stamp, and it is the
	//    build ARG that feeds it (not a hard-coded copy of the URL).
	ld := ldflagsValue(t, df, "Dockerfile")
	assertOneLdflagsInvocation(t, ld, "Dockerfile")
	if !strings.Contains(ld, "internal/sourceoffer.URL=${SOURCE_URL}") {
		t.Errorf("the Dockerfile stamps the source URL from something other than the SOURCE_URL build arg: %q", ld)
	}
}

func TestBuildWiring_MakefileThreadsTheSourceURLIntoTheVersionLdflags(t *testing.T) {
	mk := repoFile(t, "Makefile")

	// The default is the same upstream URL the Go package carries.
	if !strings.Contains(mk, "SOURCE_URL ?= "+Upstream) {
		t.Errorf("Makefile does not default SOURCE_URL to the built-in upstream URL %q", Upstream)
	}
	// One LDFLAGS definition carrying all four -X assignments.
	i := strings.Index(mk, "LDFLAGS :=")
	if i < 0 {
		t.Fatal("Makefile has no LDFLAGS definition")
	}
	ld := mk[i:]
	if j := strings.Index(ld, "\n\n"); j >= 0 {
		ld = ld[:j]
	}
	assertOneLdflagsInvocation(t, ld, "Makefile")
	if !strings.Contains(ld, "internal/sourceoffer.URL=$(SOURCE_URL)") {
		t.Errorf("the Makefile stamps the source URL from something other than $(SOURCE_URL): %q", ld)
	}
	// `make build` uses that LDFLAGS, and `make image` forwards the same value to the
	// container build - otherwise `make image SOURCE_URL=...` would silently build an
	// image offering upstream.
	if !strings.Contains(mk, `-ldflags="$(LDFLAGS)"`) {
		t.Error("the build target does not use $(LDFLAGS)")
	}
	if !strings.Contains(mk, "--build-arg SOURCE_URL=$(SOURCE_URL)") {
		t.Error("the image target does not forward SOURCE_URL to the container build")
	}
}

// The wiring proxy must BITE. Each mutation is a way the build path could silently
// stop stamping the source URL while every other test in this repo stayed green,
// because a `go build -X` on a symbol path that does not exist is IGNORED, not an
// error. The assertions above are re-run over mutated file text; each must report.
func TestBuildWiring_AssertionsFailAgainstEveryDriftMutation(t *testing.T) {
	// The grader passes the real thing first, so a mutation reporting is a signal and
	// not a check that reports on everything.
	if probs := ldflagsProblems(ldflagsValue(t, repoFile(t, "Dockerfile"), "Dockerfile")); probs != nil {
		t.Fatalf("the shipped Dockerfile ldflags was reported as broken: %v", probs)
	}
	for name, ld := range map[string]string{
		"the -X is dropped entirely": `-s -w \
      -X ...internal/version.Version=${VERSION} \
      -X ...internal/version.Commit=${COMMIT} \
      -X ...internal/version.Date=${DATE}`,
		"the symbol path is misspelled": `-s -w \
      -X github.com/NSchatz/holdfast/internal/version.Version=${VERSION} \
      -X github.com/NSchatz/holdfast/internal/version.Commit=${COMMIT} \
      -X github.com/NSchatz/holdfast/internal/version.Date=${DATE} \
      -X 'github.com/NSchatz/holdfast/internal/sourceurl.URL=${SOURCE_URL}'`,
		"a fifth -X sneaks in": `-s -w \
      -X github.com/NSchatz/holdfast/internal/version.Version=${VERSION} \
      -X github.com/NSchatz/holdfast/internal/version.Commit=${COMMIT} \
      -X github.com/NSchatz/holdfast/internal/version.Date=${DATE} \
      -X github.com/NSchatz/holdfast/internal/sourceoffer.URL=${SOURCE_URL} \
      -X github.com/NSchatz/holdfast/internal/sourceoffer.License=${LICENSE}`,
	} {
		if probs := ldflagsProblems(ld); probs == nil {
			t.Errorf("the ldflags grader passed a drift mutation (%s): %s", name, ld)
		}
	}
}
