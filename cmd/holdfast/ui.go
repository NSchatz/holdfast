package main

import (
	"errors"
	"io/fs"
	"log/slog"

	"github.com/NSchatz/holdfast/internal/server"
	"github.com/NSchatz/holdfast/internal/sourceoffer"
	"github.com/NSchatz/holdfast/internal/ui"
)

// wireUI hands the server the web UI found in fsys, and says at startup which of three
// things this binary does at the root path, because the three look the same from a
// configuration file:
//
//   - a UI was built in and is served to a request that asks for HTML;
//   - no UI was built in (a plain `go build`), which is a state and not a fault;
//   - a UI was built in and CANNOT be served as declared - its page has no place for the
//     Corresponding Source offer, or its assets are not the ones the page names. That is
//     logged at WARN and no part of it is served: the root stays the plain-text page,
//     which carries the offer, rather than a page that might not.
//
// The offer is the one `serve` already accepted before any listener existed; a value it
// would have refused cannot reach here, and is refused again rather than rendered.
func wireUI(srv *server.Server, fsys fs.FS, log *slog.Logger) {
	offer, err := sourceoffer.Resolve()
	if err != nil {
		log.Warn("the web UI is not served: the source offer it must carry was refused", "err", err)
		return
	}
	site, err := ui.Load(fsys, offer)
	switch {
	case err == nil:
		srv.SetUI(site)
		log.Info("web UI embedded: / serves it to a request that asks for HTML, and the plain-text page otherwise",
			"assets", len(site.AssetNames()))
	case errors.Is(err, ui.ErrNotBuilt):
		log.Info("no web UI in this build: / serves the plain-text page")
	default:
		log.Warn("the embedded web UI is not served, and / serves the plain-text page", "err", err)
	}
}
