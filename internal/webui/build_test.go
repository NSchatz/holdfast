package webui

import (
	"bytes"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/webui/gen"
)

// The BUILD of the embedded dashboard (WEBUI-10).
//
// index.html is generated from the modules under internal/webui/src and COMMITTED, so the
// binary keeps embedding exactly one self-contained file. These are the properties that
// makes that arrangement safe rather than merely convenient:
//
//   - the committed file is what the sources generate (B2), so nobody reads a page whose
//     behaviour its source no longer describes;
//   - the generator is Go and stdlib only (B3), so `make build` and the container image
//     gain no stage and no tool, and need no network;
//   - a source that is missing, unreadable or unparseable stops the generator with the
//     file named and the committed document untouched (B4), never a half page;
//   - nothing third-party enters the page or the test tooling (B18), which is what keeps
//     the fourth inherited criterion decidable for whoever adopts a library later.

const srcDir = "src"

func generated(t *testing.T) []byte {
	t.Helper()
	doc, err := gen.Document(os.DirFS(srcDir))
	if err != nil {
		t.Fatalf("generating the dashboard from %s: %v", srcDir, err)
	}
	return doc
}

// B2. The committed, go:embed-ed document IS what the sources generate. This runs inside
// `go test`, so `make check` catches a stale artifact by name; `make webui-stale` (also in
// `check`) is the same comparison from the command line.
func TestBuild_TheCommittedDocumentIsNotStale(t *testing.T) {
	doc := generated(t)
	if bytes.Equal(indexHTML, doc) {
		return
	}
	i := 0
	for i < len(indexHTML) && i < len(doc) && indexHTML[i] == doc[i] {
		i++
	}
	t.Fatalf("stale artifact internal/webui/index.html: it is not what internal/webui/src generates "+
		"(%d bytes committed, %d generated; first difference at byte %d).\n"+
		"Regenerate with `make webui-gen` and commit internal/webui/index.html.",
		len(indexHTML), len(doc), i)
}

// And the generator is deterministic: the same sources produce the same bytes, so the
// stale check above can never be satisfied by luck.
func TestBuild_TheGeneratorIsDeterministic(t *testing.T) {
	a, b := generated(t), generated(t)
	if !bytes.Equal(a, b) {
		t.Fatal("two runs of the generator over the same sources produced different documents")
	}
}

// The generated document really is ONE self-contained file: every module's text is in it,
// there is exactly one script element and one style element, and it names no sub-resource
// to fetch. (What the BROWSER does with it is B1, graded in dashboard_rendered_test.go.)
func TestBuild_TheGeneratedDocumentIsOneSelfContainedFile(t *testing.T) {
	s := string(indexHTML)
	if n := strings.Count(s, "<script"); n != 1 {
		t.Errorf("the document carries %d script elements, want exactly the one inline script", n)
	}
	if n := strings.Count(s, "<style"); n != 1 {
		t.Errorf("the document carries %d style elements, want exactly the one inline stylesheet", n)
	}
	for _, banned := range []string{"<script src", "<link rel", "@import", "http://", "https://", "//cdn"} {
		if strings.Contains(s, banned) {
			t.Errorf("the generated document names %q, which would be a fetch from outside the binary", banned)
		}
	}
	mods, err := gen.Modules(os.DirFS(srcDir))
	if err != nil {
		t.Fatalf("reading the module manifest: %v", err)
	}
	if len(mods) < 2 {
		t.Fatalf("the manifest names %d modules; the split is the point of this phase", len(mods))
	}
	for _, m := range mods {
		body, err := fs.ReadFile(os.DirFS(srcDir), m)
		if err != nil {
			t.Fatalf("reading %s: %v", m, err)
		}
		// A distinctive line from each module must be in the served document, so a module
		// silently dropped from the build is a failure rather than a smaller page.
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if len(line) > 30 && !strings.HasPrefix(line, "//") {
				if !strings.Contains(s, line) {
					t.Errorf("the module %s is not in the generated document (missing %q)", m, line)
				}
				break
			}
		}
	}
	// The document says it is generated, so nobody edits it by hand and loses the change.
	if !strings.Contains(s, "GENERATED FILE - do not edit by hand") {
		t.Error("the generated document does not say it is generated")
	}
}

// B3. The build uses the GO TOOLCHAIN ALONE. Asserted three ways, because all three have
// to hold for the container image to gain no stage and no tool: the generator's own import
// set is stdlib and cannot reach the network; the repository's build target invokes no
// JavaScript tooling and does not depend on the generator at all (the artifact is
// committed); and the Dockerfile gains neither.
func TestBuild_UsesTheGoToolchainAloneAndNeedsNoNetwork(t *testing.T) {
	// 1. The generator's imports, read with the Go parser rather than guessed at.
	const modulePath = "github.com/NSchatz/holdfast/"
	imports := map[string]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir("gen", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
			return err
		}
		f, perr := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		for _, im := range f.Imports {
			path, uerr := strconv.Unquote(im.Path.Value)
			if uerr != nil {
				return uerr
			}
			imports[path] = p
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading the generator's imports: %v", err)
	}
	if len(imports) == 0 {
		t.Fatal("found no imports in internal/webui/gen: the walk is not looking at the generator")
	}
	for path, file := range imports {
		switch {
		case strings.HasPrefix(path, modulePath):
			if !strings.HasPrefix(path, modulePath+"internal/webui/gen") {
				t.Errorf("%s imports %q; the generator is a leaf of this repository", file, path)
			}
		case strings.Contains(firstSegment(path), "."):
			t.Errorf("%s imports the third-party module %q; the generator must be stdlib only", file, path)
		case path == "net" || path == "net/http" || path == "net/url" || path == "os/exec":
			t.Errorf("%s imports %q; the generator must need no network and must invoke no other tool", file, path)
		}
	}

	// 2. The repository's build target.
	mk := readRepoFile(t, "Makefile")
	build := makeRecipe(t, mk, "build")
	if build == "" {
		t.Fatal("the Makefile has no build target to check")
	}
	for _, tool := range jsToolchain {
		if strings.Contains(build, tool) {
			t.Errorf("the build target invokes the JavaScript toolchain (%q): %q", tool, build)
		}
	}
	if !strings.Contains(build, "go build") {
		t.Errorf("the build target is not a go build: %q", build)
	}
	if regexp.MustCompile(`(?m)^build:.*\bwebui-gen\b`).MatchString(mk) {
		t.Error("the build target depends on webui-gen; the embedded document is COMMITTED, " +
			"so a build must not have to generate it (and an image build must not have to run it)")
	}

	// 3. The image.
	docker := readRepoFile(t, "Dockerfile")
	for _, tool := range jsToolchain {
		if regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(tool) + `\b`).MatchString(docker) {
			t.Errorf("the Dockerfile names the JavaScript toolchain (%q); the image must gain no stage and no tool", tool)
		}
	}
}

func firstSegment(p string) string {
	s, _, _ := strings.Cut(p, "/")
	return s
}

var jsToolchain = []string{
	"node", "npm", "npx", "pnpm", "yarn", "bun", "deno",
	"esbuild", "webpack", "rollup", "vite", "tsc", "babel", "terser", "uglify",
}

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// makeRecipe returns the recipe lines of one Makefile target.
func makeRecipe(t *testing.T, mk, target string) string {
	t.Helper()
	lines := strings.Split(mk, "\n")
	var out []string
	in := false
	for _, l := range lines {
		if strings.HasPrefix(l, target+":") {
			in = true
			continue
		}
		if in {
			if strings.HasPrefix(l, "\t") {
				out = append(out, strings.TrimPrefix(l, "\t"))
				continue
			}
			if strings.TrimSpace(l) == "" {
				continue
			}
			break
		}
	}
	return strings.Join(out, "\n")
}

// B4. A source that is missing, unreadable or unparseable stops the generator with the
// offending file NAMED, and leaves the committed document exactly as it found it.
func TestBuild_ABrokenSourceStopsTheGeneratorAndLeavesTheDocumentUntouched(t *testing.T) {
	const sentinel = "<!-- the committed document, untouched -->\n"

	cases := []struct {
		name   string
		break_ func(t *testing.T, dir string)
		names  string // the file the message must name
	}{
		{"a module the manifest names is missing", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "js", "40-cells.js")); err != nil {
				t.Fatal(err)
			}
		}, "js/40-cells.js"},
		{"a module is unreadable", func(t *testing.T, dir string) {
			if err := os.Chmod(filepath.Join(dir, "js", "50-rows.js"), 0o000); err != nil {
				t.Fatal(err)
			}
		}, "js/50-rows.js"},
		{"a module has an unterminated string", func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "js", "70-render.js"), "function x() { return \"oops;\n}\n")
		}, "js/70-render.js"},
		{"a module has an unterminated block comment", func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "js", "70-render.js"), "/* never closed\nfunction x() {}\n")
		}, "js/70-render.js"},
		{"a module has unbalanced braces", func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "js", "70-render.js"), "function x() {\n  if (true) {\n}\n")
		}, "js/70-render.js"},
		{"a module would close the script element early", func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "js", "70-render.js"), "const s = \"</script>\";\n")
		}, "js/70-render.js"},
		{"the manifest names something that is not a module", func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "js", "modules.txt"), "10-constants.js\n../../etc/passwd\n")
		}, "js/modules.txt"},
		{"the manifest is missing", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "js", "modules.txt")); err != nil {
				t.Fatal(err)
			}
		}, "js/modules.txt"},
		{"the page shell is missing", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "index.html.tmpl")); err != nil {
				t.Fatal(err)
			}
		}, "index.html.tmpl"},
		{"the page shell lost its script marker", func(t *testing.T, dir string) {
			b := readFile(t, filepath.Join(dir, "index.html.tmpl"))
			writeFile(t, filepath.Join(dir, "index.html.tmpl"), strings.Replace(b, "//holdfast:inline-js", "", 1))
		}, "index.html.tmpl"},
		{"the page shell lost its stylesheet marker", func(t *testing.T, dir string) {
			b := readFile(t, filepath.Join(dir, "index.html.tmpl"))
			writeFile(t, filepath.Join(dir, "index.html.tmpl"), strings.Replace(b, "/*holdfast:inline-css*/", "", 1))
		}, "index.html.tmpl"},
		{"the stylesheet is missing", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "dashboard.css")); err != nil {
				t.Fatal(err)
			}
		}, "dashboard.css"},
		{"the stylesheet would close the style element early", func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "dashboard.css"), "body { color: red; } </style>\n")
		}, "dashboard.css"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := copyTree(t, srcDir)
			out := filepath.Join(t.TempDir(), "index.html")
			writeFile(t, out, sentinel)
			c.break_(t, dir)

			var stdout, stderr bytes.Buffer
			// The REAL command body, not a stand-in for it.
			code := gen.Main([]string{"-src", dir, "-out", out}, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("the generator succeeded on a broken source tree (%s)\nstdout: %s", c.name, stdout.String())
			}
			if !strings.Contains(stderr.String(), c.names) {
				t.Errorf("the failure does not name the offending file %q:\n%s", c.names, stderr.String())
			}
			if got := readFile(t, out); got != sentinel {
				t.Errorf("the generator wrote to the committed document on failure: %q", got)
			}
			// And in -check mode too: a broken source tree is a failure, not a silent pass.
			stderr.Reset()
			if code := gen.Main([]string{"-src", dir, "-out", out, "-check"}, &stdout, &stderr); code == 0 {
				t.Errorf("the stale check passed on a broken source tree (%s)", c.name)
			}
		})
	}
}

// The generator's stale check must also FAIL on a document that drifted, naming it.
func TestBuild_TheStaleCheckNamesTheArtifactItRefuses(t *testing.T) {
	out := filepath.Join(t.TempDir(), "index.html")
	var stdout, stderr bytes.Buffer

	// Current: it passes.
	writeFile(t, out, string(generated(t)))
	if code := gen.Main([]string{"-src", srcDir, "-out", out, "-check"}, &stdout, &stderr); code != 0 {
		t.Fatalf("the stale check refused a current document (exit %d): %s", code, stderr.String())
	}
	// Drifted by ONE byte: it refuses, and says which file.
	stderr.Reset()
	writeFile(t, out, string(generated(t))+" ")
	if code := gen.Main([]string{"-src", srcDir, "-out", out, "-check"}, &stdout, &stderr); code == 0 {
		t.Fatal("the stale check passed a document that differs from what the sources generate")
	}
	if !strings.Contains(stderr.String(), "stale artifact") || !strings.Contains(stderr.String(), out) {
		t.Errorf("the stale check does not name the artifact it refused:\n%s", stderr.String())
	}
	// Absent entirely: also a refusal.
	stderr.Reset()
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	if code := gen.Main([]string{"-src", srcDir, "-out", out, "-check"}, &stdout, &stderr); code == 0 {
		t.Fatal("the stale check passed with no document at all")
	}

	// And writing works, atomically, from the real command.
	stderr.Reset()
	if code := gen.Main([]string{"-src", srcDir, "-out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("the generator failed to write (exit %d): %s", code, stderr.String())
	}
	if got := readFile(t, out); got != string(generated(t)) {
		t.Error("the generator wrote something other than what it generates")
	}
}

// B18. Nothing third-party enters the served page or the test tooling. This is the
// invariant that keeps the fourth inherited criterion (A4) decidable: whoever adopts a
// rendering library later has to change this test to do it, in front of a reviewer, rather
// than have a lockfile appear.
func TestBuild_NoThirdPartyJavaScriptEntersThePageOrTheTooling(t *testing.T) {
	root := filepath.Join("..", "..")
	forbidden := []string{
		"package.json", "package-lock.json", "npm-shrinkwrap.json",
		"pnpm-lock.yaml", "yarn.lock", "bun.lockb", "bun.lock",
		"deno.json", "deno.jsonc", "deno.lock", ".npmrc", ".nvmrc",
		"node_modules", "vendor.js", "webpack.config.js", "rollup.config.js", "vite.config.js",
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner of the tree is not this test's business
		}
		name := d.Name()
		if d.IsDir() && (name == ".git" || name == "dist" || name == "out") {
			return filepath.SkipDir
		}
		for _, f := range forbidden {
			if name == f {
				rel, _ := filepath.Rel(root, p)
				t.Errorf("%s exists: the dashboard and its tooling must introduce no registry package, "+
					"no lockfile and no third-party JavaScript runtime", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}

	// Every module and every test file resolves only relative paths and node's own
	// builtins. A bare specifier is a registry package by definition.
	bareImport := regexp.MustCompile(`(?:require\(|from\s+|import\s*\()\s*["']([^"']+)["']`)
	for _, f := range jsSources(t) {
		body := readFile(t, f)
		for _, m := range bareImport.FindAllStringSubmatch(body, -1) {
			spec := m[1]
			if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") || strings.HasPrefix(spec, "node:") {
				continue
			}
			t.Errorf("%s resolves the bare specifier %q; only relative paths and node: builtins are allowed", f, spec)
		}
	}

	// And the go module gained no dependency for any of this.
	gomod := readRepoFile(t, "go.mod")
	for _, tool := range []string{"esbuild", "goja", "otto", "quickjs", "v8go", "rod", "chromedp", "playwright"} {
		if strings.Contains(gomod, tool) {
			t.Errorf("go.mod requires %q; the generator is stdlib only and the graders drive the browser this container already ships", tool)
		}
	}
}

// jsSources is every JavaScript file this package owns: the page's modules and the unit
// suite that exercises them.
func jsSources(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".js") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", srcDir, err)
	}
	if len(out) == 0 {
		t.Fatalf("no JavaScript sources found under %s", srcDir)
	}
	return out
}

// --- helpers -------------------------------------------------------------------

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("reading %s: %v", p, err)
	}
	return string(b)
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatalf("writing %s: %v", p, err)
	}
}

// copyTree copies the source tree into a temp directory the test may break.
func copyTree(t *testing.T, dir string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatalf("copying %s: %v", dir, err)
	}
	// t.TempDir's own cleanup cannot remove a file this test chmods to 0000, so put the
	// permissions back on the way out.
	t.Cleanup(func() {
		_ = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				_ = os.Chmod(p, 0o644)
			}
			return nil
		})
	})
	return dst
}
