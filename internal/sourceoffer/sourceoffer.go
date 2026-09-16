// Package sourceoffer carries the Corresponding Source offer that every response a
// remote user can get from the root path of `holdfast serve` must display.
//
// AGPL-3.0 section 13 obliges whoever runs a MODIFIED holdfast over a network to
// "prominently offer all users interacting with it remotely ... an opportunity to receive
// the Corresponding Source". That binds a MODIFIER, not the author of the unmodified work,
// so this is the affordance a fork needs, and it is one build argument:
//
//	make build SOURCE_URL=https://git.example.org/me/holdfast   (or --build-arg, for docker)
//
// Deliberately NOT a config key or an environment variable: the offer names the tree the
// BINARY came from, and a runtime setting would let whoever runs a binary rename its
// source after the fact. An unusable value is a REFUSAL; see Resolve for why.
package sourceoffer

import (
	"fmt"
	"strings"

	"github.com/NSchatz/holdfast/internal/version"
)

// Upstream is the Corresponding Source URL of the unmodified work, and the URL in effect
// for any build that supplies none. A test inside `make check` holds the Makefile's and
// the Dockerfile's copies to this constant, so the three cannot drift.
const Upstream = "https://github.com/NSchatz/holdfast"

// License is the SPDX identifier holdfast is distributed under. The offer owes a route to
// the SOURCE, and the licence text itself already ships at /usr/share/doc/holdfast/.
const License = "AGPL-3.0-only"

// Label is the literal text immediately before the URL. The rendering is FIXED
// (Offer.Text), so the served bytes are determined for every build.
const Label = "Corresponding Source"

// URL is the source URL IN EFFECT for this binary: Upstream unless the build supplied a
// different one via `-ldflags -X <this package>.URL=<url>`. A built binary carries exactly
// ONE such value, and nothing here needs to tell a default from an override equal to it.
var URL = Upstream

const howToSet = "set it with `make build SOURCE_URL=...` or `docker build --build-arg SOURCE_URL=...`"

// Validate reports whether raw is usable as the source URL in effect. The accept test is
// the SCHEME PREFIX and nothing more: http:// or https:// followed by at least one
// character that is not '/'. It deliberately does NOT turn on RFC 3986 cleanliness: the
// offer is plain text, so a round-trip check (url.Parse then u.String() == raw) would
// reject values the page carries perfectly well, at BUILD time. Do not tighten this.
func Validate(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("build-time source URL %q is empty or whitespace-only, so this build has no Corresponding Source to offer: %s", raw, howToSet)
	}
	for _, scheme := range []string{"https://", "http://"} {
		rest, ok := strings.CutPrefix(raw, scheme)
		if ok && rest != "" && !strings.HasPrefix(rest, "/") {
			return nil
		}
	}
	return fmt.Errorf("build-time source URL %q is not an absolute http:// or https:// URL, so a user could not reach the Corresponding Source from it: %s", raw, howToSet)
}

// Offer is the resolved source offer: the source URL, the licence name and the build
// identity TOGETHER, so the page and `version` cannot disagree about what tree this is.
type Offer struct {
	SourceURL string
	License   string
	// Build is byte-for-byte what the `version` subcommand prints, wired to version.String
	// rather than to a re-formatted copy, so it holds by construction.
	Build string
}

// Current returns the offer WITHOUT validating it: the render path, reached only after a
// refusal site accepted the value. To serve, use Resolve.
func Current() Offer {
	return Offer{SourceURL: URL, License: License, Build: version.String()}
}

// Resolve returns the offer the root path renders, or a REFUSAL naming the rejected value
// - never a quiet fall back to upstream, which would tell a fork's users that upstream is
// the source of a binary it is not. It is the single accept test: cmd/holdfast calls it at
// startup ahead of every listener, and the root handler again at construction.
func Resolve() (Offer, error) {
	if err := Validate(URL); err != nil {
		return Offer{}, err
	}
	return Current(), nil
}

// Text renders the offer for the plain-text root page, the ONE rendering: the build
// identity, the licence name and the source URL verbatim, the literal Label before it.
func (o Offer) Text() string {
	return o.Build + ", free software you may redistribute and modify under " + o.License +
		".\n" + Label + ": " + o.SourceURL + "\n"
}
