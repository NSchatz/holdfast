// Package sourceoffer carries the Corresponding Source offer that every response a
// remote user can get from the root path of `holdfast serve` must display.
//
// AGPL-3.0 section 13 obliges whoever runs a MODIFIED holdfast over a network to
// "prominently offer all users interacting with it remotely ... an opportunity to
// receive the Corresponding Source". That binds a MODIFIER, not the author of the
// unmodified work - so this is the affordance a fork needs, not a debt being paid
// off. The URL is therefore a BUILD-TIME value with the upstream tree as its
// default, and a fork points it at its own tree by supplying one build argument
// instead of patching embedded HTML:
//
//	make build SOURCE_URL=https://git.example.org/me/holdfast
//	docker buildx build --build-arg SOURCE_URL=https://git.example.org/me/holdfast .
//
// Deliberately NOT a config key or an environment variable. The offer names the tree
// the BINARY came from, and a runtime setting would let whoever runs a binary rename
// its source after the fact - it would also be a new public configuration surface
// that the release phase would then have to freeze.
//
// A build that supplies an unusable value is a REFUSAL, never a quiet fall back to
// upstream: a fork whose override is malformed and silently fell back would tell its
// users that upstream is the source of a binary it is not, which is a worse failure
// than not serving. cmd/holdfast performs that refusal once, ahead of every listener
// it can create; see Resolve.
package sourceoffer

import (
	"fmt"
	"html"
	"strings"

	"github.com/NSchatz/holdfast/internal/version"
)

// Upstream is the Corresponding Source URL of the unmodified work, and the source
// URL in effect for any build that supplies none. It is the built-in default, and
// the copies of it in the Makefile and the Dockerfile are checked against this
// constant by a test that runs inside `make check` - so the three cannot drift.
const Upstream = "https://github.com/NSchatz/holdfast"

// License is the SPDX identifier holdfast is distributed under. It is shown as TEXT
// in the offer and is never a link: the offer owes a route to the SOURCE, and the
// licence text itself already ships in the image at /usr/share/doc/holdfast/.
const License = "AGPL-3.0-only"

// Label is the literal text that introduces the link, in the offer, immediately
// before it. The rendering is FIXED (see Offer.HTML and Offer.Text) so the served
// bytes are determined for every build.
const Label = "Corresponding Source"

// URL is the source URL IN EFFECT for this binary: Upstream unless the build that
// produced it supplied a different one via
//
//	-ldflags -X github.com/NSchatz/holdfast/internal/sourceoffer.URL=<url>
//
// A built binary carries exactly ONE such value and cannot tell "no value was
// supplied" from "a value equal to the default was supplied" - nothing here needs
// that distinction, and nothing should be built that does.
var URL = Upstream

const howToSet = "set it with `make build SOURCE_URL=...` or `docker build --build-arg SOURCE_URL=...`"

// Validate reports whether raw is usable as the source URL in effect.
//
// The accept test is the SCHEME PREFIX and nothing more: http:// or https://
// followed by at least one character that is not '/'. It deliberately does NOT turn
// on the value being clean under RFC 3986. Characters that are significant in HTML,
// spaces, and anything else an encoder would percent-encode are escaped at render
// time and are never by themselves a reason to refuse - a round-trip check such as
// url.Parse followed by u.String() == raw would reject exactly the values the
// escaping exists to carry. Do not tighten this.
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

// Offer is the resolved source offer: the Corresponding Source URL, the licence name
// and the build identity, TOGETHER. Every root-serving branch renders this same
// value, so the dashboard and the API-only page cannot disagree about what tree the
// running binary came from.
type Offer struct {
	// SourceURL is the source URL in effect, unescaped.
	SourceURL string
	// License is the SPDX identifier, shown as text.
	License string
	// Build is the build identity, byte-for-byte what the `version` subcommand of
	// this same binary prints. Wiring it to version.String rather than to a
	// re-formatted copy is what makes "the same build stamp the version command
	// reports" true by construction instead of by agreement.
	Build string
}

// Current returns the offer for this binary WITHOUT validating it. Callers that are
// about to serve it must use Resolve instead; this exists for the render path, which
// runs only after a refusal site has already accepted the value.
func Current() Offer {
	return Offer{SourceURL: URL, License: License, Build: version.String()}
}

// Resolve validates the source URL this binary was built with and returns the offer
// every root-serving branch renders. A non-nil error is a REFUSAL to serve the root
// path at all, and its message names the rejected value.
//
// This is the single accept test in the program. cmd/holdfast calls it at startup,
// ahead of every listener; the root-serving branches call it at router/handler
// construction, so an in-process construction with a rejected value refuses rather
// than serving an offer that names one.
func Resolve() (Offer, error) {
	if err := Validate(URL); err != nil {
		return Offer{}, err
	}
	return Current(), nil
}

// HTML renders the offer as the dashboard fragment.
//
// The rendering is FIXED: the link's displayed text is the source URL in effect and
// nothing else, and the literal text "Corresponding Source" sits immediately before
// the link with only punctuation and whitespace between. Every value that reaches the
// document is escaped, so the URL can appear ONLY as the link's target and its
// displayed text - it can introduce no element, no attribute and no script, which is
// what lets the page keep its tight Content-Security-Policy and its
// no-HTML-string-sink render idiom exactly as they are.
func (o Offer) HTML() string {
	u := html.EscapeString(o.SourceURL)
	var b strings.Builder
	b.WriteString(`<p class="source-offer">This is `)
	b.WriteString(html.EscapeString(o.Build))
	b.WriteString(`, free software you may redistribute and modify under `)
	b.WriteString(html.EscapeString(o.License))
	b.WriteString(`. `)
	b.WriteString(Label)
	b.WriteString(`: <a class="source-offer-link" href="`)
	b.WriteString(u)
	b.WriteString(`">`)
	b.WriteString(u)
	b.WriteString(`</a></p>`)
	return b.String()
}

// Text renders the offer for the plain-text API-only root page, which carries no
// markup: the same three facts, with the source URL in effect verbatim and the same
// literal label immediately before it.
func (o Offer) Text() string {
	return o.Build + ", free software you may redistribute and modify under " + o.License +
		".\n" + Label + ": " + o.SourceURL + "\n"
}
