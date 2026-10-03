package ui

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/NSchatz/holdfast/internal/sourceoffer"
)

const (
	testSource = "https://git.example.org/fork/holdfast"
	// A value Validate accepts (the scheme prefix is all it tests) and that would be
	// markup if it reached a page unescaped.
	hostileSource = `https://example.invalid/a?b="><img src=x onerror=1>&c='`
)

func testOffer(url string) sourceoffer.Offer {
	return sourceoffer.Offer{SourceURL: url, License: sourceoffer.License, Build: "holdfast v9.9.9 (commit abc1234, built 2026-01-02T03:04:05Z)"}
}

func page(body string) string {
	return "<!doctype html><html><head>" +
		`<script type="module" crossorigin src="/assets/index-AAAA.js"></script>` +
		`<link rel="stylesheet" crossorigin href="/assets/index-BBBB.css">` +
		"</head><body><div id=\"app\"></div>" + body + "</body></html>"
}

func builtTree() fstest.MapFS {
	return fstest.MapFS{
		IndexName:               {Data: []byte(page(OfferSlot))},
		"assets/index-AAAA.js":  {Data: []byte("console.log(1)")},
		"assets/index-BBBB.css": {Data: []byte("body{}")},
		"assets/img/logo.svg":   {Data: []byte("<svg/>")},
		".gitkeep":              {Data: []byte("placeholder")},
	}
}

// The page a built tree serves carries the offer where the slot was: the build, the
// licence, the literal label and the source URL in effect, shown and linked.
func TestLoad_WritesTheOfferIntoThePage(t *testing.T) {
	site, err := Load(builtTree(), testOffer(testSource))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := string(site.Page())
	if strings.Contains(got, OfferSlot) {
		t.Errorf("the empty slot is still in the page: %s", got)
	}
	for _, want := range []string{
		"holdfast v9.9.9 (commit abc1234, built 2026-01-02T03:04:05Z)",
		sourceoffer.License,
		sourceoffer.Label + `: <a href="` + testSource + `" rel="noopener noreferrer">` + testSource + "</a>",
		`<footer id="source-offer">`,
		`<div id="app"></div>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the page lacks %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, `id="source-offer"`); n != 1 {
		t.Errorf("the page carries %d source-offer elements, want 1", n)
	}
}

// A source URL is escaped where it is rendered and never rejected for being unclean
// (sourceoffer.Validate), so the page must carry a hostile one as text and as an
// attribute value, never as markup.
func TestLoad_AHostileSourceURLIsTextNeverMarkup(t *testing.T) {
	site, err := Load(builtTree(), testOffer(hostileSource))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := string(site.Page())
	for _, raw := range []string{"<img", `b="`, "onerror=1>"} {
		if strings.Contains(got, raw) {
			t.Errorf("the page carries %q unescaped:\n%s", raw, got)
		}
	}
	if !strings.Contains(got, "&lt;img src=x onerror=1&gt;") || !strings.Contains(got, "b=&#34;") {
		t.Errorf("the page does not carry the URL in its escaped form:\n%s", got)
	}
	// The build identity is build input too.
	offer := testOffer(testSource)
	offer.Build = "holdfast <script>alert(1)</script>"
	site, err = Load(builtTree(), offer)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Contains(string(site.Page()), "<script>alert(1)</script>") {
		t.Errorf("the build identity reached the page as markup:\n%s", site.Page())
	}
}

// Every asset is served under the type its extension names, by the name the page uses,
// and a file outside the assets directory is not an asset.
func TestLoad_AssetsCarryTheirOwnMediaType(t *testing.T) {
	site, err := Load(builtTree(), testOffer(testSource))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]string{
		"index-AAAA.js":  "text/javascript; charset=utf-8",
		"index-BBBB.css": "text/css; charset=utf-8",
		"img/logo.svg":   "image/svg+xml",
	}
	names := site.AssetNames()
	if !reflect.DeepEqual(names, []string{"img/logo.svg", "index-AAAA.js", "index-BBBB.css"}) {
		t.Fatalf("AssetNames = %v", names)
	}
	for name, ct := range want {
		a, ok := site.Asset(name)
		if !ok || a.ContentType != ct {
			t.Errorf("Asset(%q) = %q, %v; want %q", name, a.ContentType, ok, ct)
		}
	}
	if a, _ := site.Asset("index-AAAA.js"); string(a.Body) != "console.log(1)" {
		t.Errorf("the script's bytes are %q", a.Body)
	}
	for _, not := range []string{IndexName, ".gitkeep", "../index.html", "assets/index-AAAA.js", "", "img"} {
		if _, ok := site.Asset(not); ok {
			t.Errorf("Asset(%q) answered; only a file under %s/ is an asset", not, AssetsDir)
		}
	}
}

// The placeholder alone is the unbuilt state, and it is told apart from every fault.
func TestLoad_ATreeWithNoPageIsNotBuilt(t *testing.T) {
	for name, tree := range map[string]fstest.MapFS{
		"the placeholder alone": {".gitkeep": {Data: []byte("x")}},
		"nothing at all":        {},
		"assets and no page":    {"assets/a.js": {Data: []byte("x")}},
	} {
		if _, err := Load(tree, testOffer(testSource)); !errors.Is(err, ErrNotBuilt) {
			t.Errorf("%s: Load = %v, want ErrNotBuilt", name, err)
		}
	}
}

// A tree that has a page and cannot be served as declared is refused whole, by a named
// reason, and is never mistaken for the unbuilt state.
func TestLoad_RefusesAPageItCannotServeAsDeclared(t *testing.T) {
	with := func(change func(fstest.MapFS)) fstest.MapFS {
		tree := builtTree()
		change(tree)
		return tree
	}
	cases := map[string]struct {
		tree fstest.MapFS
		want string
	}{
		"no slot for the offer": {
			with(func(m fstest.MapFS) { m[IndexName] = &fstest.MapFile{Data: []byte(page(""))} }),
			"0 times, not once",
		},
		"the slot twice": {
			with(func(m fstest.MapFS) { m[IndexName] = &fstest.MapFile{Data: []byte(page(OfferSlot + OfferSlot))} }),
			"2 times, not once",
		},
		"a slot that already holds something": {
			with(func(m fstest.MapFS) {
				m[IndexName] = &fstest.MapFile{Data: []byte(page(`<footer id="source-offer">x</footer>`))}
			}),
			"0 times, not once",
		},
		"an asset of a kind with no media type": {
			with(func(m fstest.MapFS) { m["assets/run.wasm"] = &fstest.MapFile{Data: []byte("x")} }),
			"assets/run.wasm is of a kind this server sends under no media type",
		},
		"an asset with no extension": {
			with(func(m fstest.MapFS) { m["assets/LICENSE"] = &fstest.MapFile{Data: []byte("x")} }),
			"assets/LICENSE is of a kind",
		},
		"a page inside the assets directory": {
			with(func(m fstest.MapFS) { m["assets/other.html"] = &fstest.MapFile{Data: []byte("x")} }),
			"assets/other.html is of a kind",
		},
		"something that is not a regular file": {
			with(func(m fstest.MapFS) { m["assets/link.js"] = &fstest.MapFile{Mode: fs.ModeSymlink} }),
			"assets/link.js is not a regular file",
		},
		"a page and no assets": {
			fstest.MapFS{IndexName: {Data: []byte(page(OfferSlot))}},
			"reading the web UI's assets directory",
		},
		"an empty assets directory": {
			fstest.MapFS{IndexName: {Data: []byte(page(OfferSlot))}, "assets": {Mode: fs.ModeDir}},
			"the build was not copied whole",
		},
		"a page naming a stylesheet the build does not hold": {
			with(func(m fstest.MapFS) { delete(m, "assets/index-BBBB.css") }),
			"names /assets/index-BBBB.css, which the build does not hold",
		},
		"assets of another build": {
			fstest.MapFS{IndexName: {Data: []byte(page(OfferSlot))}, "assets/index-ZZZZ.js": {Data: []byte("x")}},
			"names /assets/index-AAAA.js, which the build does not hold",
		},
		"a page that names no asset at all": {
			fstest.MapFS{IndexName: {Data: []byte("<html><body>" + OfferSlot + "</body></html>")}, "assets/a.js": {Data: []byte("x")}},
			"references no file under assets/",
		},
	}
	for name, tc := range cases {
		site, err := Load(tc.tree, testOffer(testSource))
		if err == nil {
			t.Errorf("%s: Load served it (%d assets)", name, len(site.AssetNames()))
			continue
		}
		if errors.Is(err, ErrNotBuilt) {
			t.Errorf("%s: Load called it not built, which hides the fault: %v", name, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Load = %q, want it to say %q", name, err, tc.want)
		}
	}
}

// The tree this binary embeds is one of exactly two things, whichever tree the test
// binary was built from: the committed placeholder alone, or a whole build Load serves.
// A half-copied build in the embed directory fails here, in the gate, before it ships.
func TestEmbedded_IsThePlaceholderOrAWholeBuild(t *testing.T) {
	tree := Embedded()
	if _, err := fs.Stat(tree, ".gitkeep"); err != nil {
		t.Errorf("the embedded tree lost its committed placeholder: %v", err)
	}
	site, err := Load(tree, testOffer(testSource))
	switch {
	case errors.Is(err, ErrNotBuilt):
		t.Log("this binary embeds the placeholder alone: no UI was built into the tree")
	case err != nil:
		t.Errorf("the embedded UI cannot be served as declared: %v", err)
	default:
		if !strings.Contains(string(site.Page()), testSource) {
			t.Errorf("the embedded page does not carry the offer")
		}
		t.Logf("this binary embeds a built UI: %v", site.AssetNames())
	}
}

// `go build` and `go vet` pass on a tree where no UI was ever built, and it is the
// committed placeholder that makes them pass. This package is copied as git tracks it -
// its Go files and dist/.gitkeep, never a built UI - into a module of its own, with the
// two packages it imports; then the placeholder is taken away and the build must fail.
// Without the second half the first proves nothing about the placeholder.
func TestUnbuiltTree_BuildsAndVetsBecauseOfThePlaceholder(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("no go toolchain on PATH to build the unbuilt tree with: %v", err)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	mod := t.TempDir()
	copyFile := func(rel string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		dst := filepath.Join(mod, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	copyFile("go.mod")
	copyFile("go.sum")
	for _, pkg := range []string{"internal/ui", "internal/sourceoffer", "internal/version"} {
		files, err := filepath.Glob(filepath.Join(root, pkg, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no Go files in %s (%v)", pkg, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			copyFile(filepath.Join(pkg, filepath.Base(f)))
		}
	}
	copyFile(filepath.Join("internal", "ui", "dist", ".gitkeep"))

	run := func(args ...string) (string, error) {
		cmd := exec.Command(goBin, args...)
		cmd.Dir = mod
		// The copy is its own module root: nothing above it may claim it.
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("build", "./internal/ui/"); err != nil {
		t.Fatalf("go build fails on a tree with no UI built: %v\n%s", err, out)
	}
	if out, err := run("vet", "./internal/ui/"); err != nil {
		t.Fatalf("go vet fails on a tree with no UI built: %v\n%s", err, out)
	}

	if err := os.Remove(filepath.Join(mod, "internal", "ui", "dist", ".gitkeep")); err != nil {
		t.Fatal(err)
	}
	out, err := run("build", "./internal/ui/")
	if err == nil {
		t.Fatal("go build passed with the placeholder gone: this test no longer proves the placeholder is what an unbuilt tree needs")
	}
	if !strings.Contains(out, "no embeddable files") {
		t.Errorf("go build failed without the placeholder, but not for the embed directory being empty:\n%s", out)
	}
}
