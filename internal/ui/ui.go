// Package ui carries the web UI the binary embeds and the one way it is turned into
// something a server may send.
//
// The UI is built by Vite from web/ into web/dist and copied into dist/ here, beside a
// committed placeholder (dist/.gitkeep). The placeholder is what lets `go build` and
// `go vet` pass on a tree where nobody has built the UI: `//go:embed all:dist` refuses a
// directory with no embeddable file, and without `all:` a dot-file does not count
// (verified with Go 1.25.14, 2026-09-29). A binary built from such a tree embeds the
// placeholder alone, and Load answers ErrNotBuilt for it.
//
// A Site is the page and its assets, read ONCE: the page with the AGPL section 13
// Corresponding Source offer already written into it, each asset with the media type it
// is sent as. A page the offer cannot be written into is refused whole, because a page a
// remote user interacts with owes that offer (docs/design/web-ui.md#embed).
package ui

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/NSchatz/holdfast/internal/sourceoffer"
)

//go:embed all:dist
var dist embed.FS

const (
	// IndexName is the page, at the root of the built tree.
	IndexName = "index.html"
	// AssetsDir is the one directory of the built tree that is served beside the page.
	// Vite writes every script, stylesheet and other asset there with a content hash in
	// its name (web/vite.config.ts).
	AssetsDir = "assets"
	// OfferSlot is the empty element web/index.html carries for the offer. It is replaced
	// byte for byte, so it is matched byte for byte: web/src/page.test.ts holds the page
	// to carrying it exactly once.
	OfferSlot = `<footer id="source-offer"></footer>`
)

// ErrNotBuilt reports a tree that holds no page: the placeholder alone, which is what a
// binary built without `make ui-build` embeds. It is a state and not a fault.
var ErrNotBuilt = errors.New("no web UI was built into this binary")

// Embedded is the tree this binary embeds: the Vite build where one was copied in before
// the binary was built, the placeholder alone otherwise.
func Embedded() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// fs.Sub fails only on an invalid path, and "dist" is a constant.
		panic("internal/ui: " + err.Error())
	}
	return sub
}

// Asset is one file served beside the page.
type Asset struct {
	Body        []byte
	ContentType string
}

// Site is a built UI, ready to serve.
type Site struct {
	page   []byte
	assets map[string]Asset
}

// Page is the HTML page, carrying the Corresponding Source offer.
func (s *Site) Page() []byte { return s.page }

// Asset returns the asset named name, a slash-separated path inside AssetsDir. Only a
// file Load read is ever returned: there is no path this resolves at request time.
func (s *Site) Asset(name string) (Asset, bool) {
	a, ok := s.assets[name]
	return a, ok
}

// AssetNames lists the assets, sorted.
func (s *Site) AssetNames() []string {
	names := make([]string, 0, len(s.assets))
	for n := range s.assets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// mediaTypes is the CLOSED list of what an asset may be sent as, by extension. A file of
// any other kind refuses the whole UI: the alternative is to send bytes under a type
// guessed from their content, and the page runs under `X-Content-Type-Options: nosniff`.
var mediaTypes = map[string]string{
	".js":    "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".webp":  "image/webp",
	".woff2": "font/woff2",
	".json":  "application/json",
	".txt":   "text/plain; charset=utf-8",
}

// Load reads a built UI out of fsys and writes offer into its page.
//
// It returns ErrNotBuilt for a tree with no page, and a descriptive error for a tree
// that has one and cannot be served as declared: a page that does not carry OfferSlot
// exactly once, a page that references no asset, an asset of a kind mediaTypes does not
// name, or anything under AssetsDir that is not a regular file. The caller serves no part
// of a UI Load refused.
func Load(fsys fs.FS, offer sourceoffer.Offer) (*Site, error) {
	raw, err := fs.ReadFile(fsys, IndexName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotBuilt
	}
	if err != nil {
		return nil, fmt.Errorf("reading the web UI's %s: %w", IndexName, err)
	}
	if n := bytes.Count(raw, []byte(OfferSlot)); n != 1 {
		return nil, fmt.Errorf("the web UI's %s carries the Corresponding Source slot %s %d times, not once: "+
			"a page with no offer in it is not served", IndexName, OfferSlot, n)
	}
	page := bytes.Replace(raw, []byte(OfferSlot), []byte(offer.HTML()), 1)

	assets := map[string]Asset{}
	walkErr := fs.WalkDir(fsys, AssetsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		ct, ok := mediaTypes[strings.ToLower(path.Ext(p))]
		if !ok {
			return fmt.Errorf("%s is of a kind this server sends under no media type", p)
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		assets[strings.TrimPrefix(p, AssetsDir+"/")] = Asset{Body: body, ContentType: ct}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("reading the web UI's %s directory: %w", AssetsDir, walkErr)
	}
	// A page is nothing without its script. A tree with a page and no assets is half a
	// copy, and serving it would put a blank screen where the plain-text page was.
	if len(assets) == 0 {
		return nil, fmt.Errorf("the web UI has a page and no file under %s/: the build was not copied whole", AssetsDir)
	}
	referenced := false
	for name := range assets {
		if bytes.Contains(raw, []byte("/"+AssetsDir+"/"+name)) {
			referenced = true
			break
		}
	}
	if !referenced {
		return nil, fmt.Errorf("the web UI's %s references no file under %s/: the page and the assets are not one build",
			IndexName, AssetsDir)
	}
	return &Site{page: page, assets: assets}, nil
}
