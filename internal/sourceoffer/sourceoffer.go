// Package sourceoffer carries the Corresponding Source offer that every response a
// remote user can get from the root path of `holdfast serve` must display.
//
// AGPL-3.0 section 13 obliges whoever runs a MODIFIED holdfast over a network to
// "prominently offer all users interacting with it remotely ... an opportunity to receive
// the Corresponding Source". That binds a MODIFIER, not the author of the unmodified work,
// so this is the affordance a fork needs. The URL is a BUILD-TIME value defaulting to the
// upstream tree, and a fork points it at its own with one build argument rather than by
// patching embedded HTML:
//
//	make build SOURCE_URL=https://git.example.org/me/holdfast   (or --build-arg, for docker)
//
// Deliberately NOT a config key or an environment variable: the offer names the tree the
// BINARY came from, and a runtime setting would let whoever runs a binary rename its
// source after the fact.
//
// A build that supplies an unusable value is a REFUSAL, never a quiet fall back to
// upstream, because a fork whose malformed override fell back would tell its users that
// upstream is the source of a binary it is not. See Resolve.
package sourceoffer

import (
	"fmt"
	"html"
	"net/url"
	"strings"

	"github.com/NSchatz/holdfast/internal/version"
)

// Upstream is the Corresponding Source URL of the unmodified work, and the URL in effect
// for any build that supplies none. The copies in the Makefile and the Dockerfile are
// checked against this constant by a test inside `make check`, so the three cannot drift.
const Upstream = "https://github.com/NSchatz/holdfast"

// License is the SPDX identifier holdfast is distributed under. It is shown as TEXT and
// never a link: the offer owes a route to the SOURCE, and the licence text itself already
// ships in the image at /usr/share/doc/holdfast/.
const License = "AGPL-3.0-only"

// Label is the literal text that introduces the link, immediately before it. The rendering
// is FIXED (Offer.HTML and Offer.Text) so the served bytes are determined for every build.
const Label = "Corresponding Source"

// URL is the source URL IN EFFECT for this binary: Upstream unless the build supplied a
// different one via `-ldflags -X <this package>.URL=<url>`. A built binary carries exactly
// ONE such value and cannot tell "none supplied" from "one equal to the default supplied";
// nothing here needs that distinction.
var URL = Upstream

const howToSet = "set it with `make build SOURCE_URL=...` or `docker build --build-arg SOURCE_URL=...`"

// Validate reports whether raw is usable as the source URL in effect.
//
// The accept test is the SCHEME PREFIX and nothing more: http:// or https:// followed by
// at least one character that is not '/'. It deliberately does NOT turn on the value being
// clean under RFC 3986, because everything an encoder would percent-encode is escaped at
// render time instead, and a round-trip check such as url.Parse then u.String() == raw
// would reject exactly the values the escaping exists to carry. Do not tighten this.
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

// Offer is the resolved source offer: the Corresponding Source URL, the licence name and
// the build identity, TOGETHER. Every root-serving branch renders this same value, so the
// dashboard and the API-only page cannot disagree about what tree the binary came from.
type Offer struct {
	// SourceURL is the source URL in effect, unescaped.
	SourceURL string
	// License is the SPDX identifier, shown as text.
	License string
	// Build is byte-for-byte what the `version` subcommand prints. Wiring it to
	// version.String rather than to a re-formatted copy makes that true by construction
	// instead of by agreement.
	Build string
}

// Current returns the offer for this binary WITHOUT validating it. Anything about to serve
// it must use Resolve; this is for the render path, which runs only after a refusal site
// has accepted the value.
func Current() Offer {
	return Offer{SourceURL: URL, License: License, Build: version.String()}
}

// Resolve validates the source URL this binary was built with and returns the offer every
// root-serving branch renders. A non-nil error is a REFUSAL to serve the root path at all,
// and its message names the rejected value. This is the single accept test in the program:
// cmd/holdfast calls it at startup ahead of every listener, and the root-serving branches
// call it at construction, so an in-process construction with a rejected value refuses.
func Resolve() (Offer, error) {
	if err := Validate(URL); err != nil {
		return Offer{}, err
	}
	return Current(), nil
}

// githubMark is the GitHub mark, DRAWN inline and not fetched: the served
// Content-Security-Policy is `default-src 'none'` and `img-src` falls back to it, so a
// referenced image would be a broken page rather than a heavier one. It carries no URL, no
// font and no data: URI, and is aria-hidden because the link's text is the source URL
// itself, which is the accessible name and the offer both.
const githubMark = `<svg class="gh" viewBox="0 0 24 24" width="16" height="16" ` +
	`aria-hidden="true" focusable="false"><path fill="currentColor" d="M12 .297c-6.63 0-12 5.373-12 12 0 ` +
	`5.303 3.438 9.8 8.205 11.385.6.113.82-.258.82-.577 0-.285-.01-1.04-.015-2.04-3.338.724-4.042-1.61-4.042-1.61C4.422 ` +
	`18.07 3.633 17.7 3.633 17.7c-1.087-.744.084-.729.084-.729 1.205.084 1.838 1.236 1.838 1.236 1.07 1.835 2.809 1.305 ` +
	`3.495.998.108-.776.417-1.305.76-1.605-2.665-.3-5.466-1.332-5.466-5.93 0-1.31.465-2.38 1.235-3.22-.135-.303-.54-1.523.105-3.176 ` +
	`0 0 1.005-.322 3.3 1.23.96-.267 1.98-.399 3-.405 1.02.006 2.04.138 3 .405 2.28-1.552 3.285-1.23 3.285-1.23.645 1.653.24 ` +
	`2.873.12 3.176.765.84 1.23 1.91 1.23 3.22 0 4.61-2.805 5.625-5.475 5.92.42.36.81 1.096.81 2.22 0 1.606-.015 2.896-.015 ` +
	`3.286 0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12"/></svg>`

// isGitHub reports whether a source URL is served by GitHub itself, the only condition
// under which the mark above is drawn: putting GitHub's mark beside a fork's GitLab URL
// would tell a reader something untrue about where its source is.
func isGitHub(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "github.com" || strings.HasSuffix(h, ".github.com")
}

// HTML renders the offer as the dashboard fragment.
//
// The rendering is FIXED: the link's displayed text is the source URL in effect and
// nothing else, with the literal Label immediately before it. Every value that reaches the
// document is escaped, so the URL appears ONLY as the link's target and its displayed text
// and can introduce no element, no attribute and no script. The mark is an ADDITION and
// never a replacement: the displayed URL is what discharges the section 13 offer, so an
// icon-only link would be a smaller footer bought by dropping the obligation it exists for.
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
	if isGitHub(o.SourceURL) {
		b.WriteString(githubMark)
	}
	b.WriteString(`<span class="source-offer-url">`)
	b.WriteString(u)
	b.WriteString(`</span></a></p>`)
	return b.String()
}

// Text renders the offer for the plain-text API-only root page: the same three facts, with
// the source URL verbatim and the same literal label immediately before it.
func (o Offer) Text() string {
	return o.Build + ", free software you may redistribute and modify under " + o.License +
		".\n" + Label + ": " + o.SourceURL + "\n"
}
