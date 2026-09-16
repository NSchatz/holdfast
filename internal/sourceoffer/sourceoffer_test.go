package sourceoffer

import (
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/version"
)

const (
	forkValue    = "https://example.invalid/fork"
	hostileValue = `https://example.invalid/a?b="><img src=x onerror=1>`
)

// AC2: a build that supplies no source-URL value carries the upstream URL, and the
// built-in default IS that URL. There is no sentinel and no "was it set" flag: a
// built binary carries exactly one value.
func TestDefault_IsTheUpstreamURL(t *testing.T) {
	if URL != Upstream {
		t.Fatalf("the built-in source URL is %q, want the upstream URL %q", URL, Upstream)
	}
	o, err := Resolve()
	if err != nil {
		t.Fatalf("the default build refuses its own default value: %v", err)
	}
	if o.SourceURL != Upstream {
		t.Errorf("the default offer names %q, want %q", o.SourceURL, Upstream)
	}
}

// AC6's accept test, both halves, and the boundary F10 names. The predicate is the
// SCHEME PREFIX and nothing more: an absolute http/https URL is accepted however
// unclean it is under RFC 3986, and everything else is refused.
func TestValidate_AcceptsTheSchemePrefixAndNothingMore(t *testing.T) {
	accept := []string{
		Upstream,
		forkValue,
		"http://example.invalid",
		"https://a",
		"http://example.invalid/path?q=1#frag",
		// F10's vectors: significant in HTML, or simply not RFC 3986 clean. Each is
		// escaped at render time and must never be a reason to refuse.
		hostileValue,
		`https://example.invalid/a b`,
		`https://example.invalid/"quoted"`,
		"https://example.invalid/<x>&y",
		"https://exa mple.invalid/",
	}
	for _, v := range accept {
		if err := Validate(v); err != nil {
			t.Errorf("Validate(%q) refused an absolute http/https URL: %v", v, err)
		}
	}

	reject := []string{
		"", "   ", "\t\n", "not-a-url", "javascript:alert(1)", "//example.com",
		"ftp://example.invalid", "https://", "http://", "https:///path", "http:///",
		" https://example.invalid", "HTTPS://example.invalid",
	}
	for _, v := range reject {
		if err := Validate(v); err == nil {
			t.Errorf("Validate(%q) accepted a value nobody could follow to a source tree", v)
		}
	}
}

// The refusal message NAMES the rejected value (AC6) and never offers upstream as a
// fall back, which would tell a fork's users that upstream is the source of a binary
// it is not.
func TestValidate_RefusalNamesTheValueAndNeverFallsBack(t *testing.T) {
	for _, bad := range []string{"", "   ", "not-a-url", "javascript:alert(1)", "//example.com"} {
		err := Validate(bad)
		if err == nil {
			t.Fatalf("Validate(%q) accepted", bad)
		}
		if !strings.Contains(err.Error(), bad) {
			t.Errorf("the refusal for %q does not name it: %v", bad, err)
		}
		if strings.Contains(err.Error(), Upstream) {
			t.Errorf("the refusal for %q names upstream as a fall back: %v", bad, err)
		}
	}
}

// Resolve is THE accept test, and it reads the build-time value rather than an
// argument: a caller cannot accidentally validate one value and serve another.
func TestResolve_RefusesTheBuildTimeValueItAlsoServes(t *testing.T) {
	old := URL
	t.Cleanup(func() { URL = old })

	URL = "javascript:alert(1)"
	if _, err := Resolve(); err == nil {
		t.Fatal("Resolve accepted a rejected build-time value")
	} else if !strings.Contains(err.Error(), "javascript:alert(1)") {
		t.Errorf("Resolve's refusal does not name the value: %v", err)
	}

	URL = forkValue
	o, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve refused %q: %v", forkValue, err)
	}
	if o.SourceURL != forkValue {
		t.Errorf("Resolve returned %q, want %q", o.SourceURL, forkValue)
	}
}

// AC4 and AC5 by construction: the identity in the offer IS version.String(), the
// same text the `version` subcommand prints, for any values a build stamps in and for
// the unstamped defaults. A re-formatted copy would be an agreement between two
// renderers; this is an identity.
func TestOffer_BuildIdentityIsTheVersionBanner(t *testing.T) {
	if got := Current().Build; got != version.String() {
		t.Fatalf("the offer's identity is %q, want the version banner %q", got, version.String())
	}
	// The unstamped defaults (AC5): what a plain `go build` produces, which is what
	// this test binary is.
	for _, want := range []string{"0.0.0-dev", "unknown"} {
		if !strings.Contains(Current().Build, want) {
			t.Errorf("the unstamped identity %q does not carry %q", Current().Build, want)
		}
	}

	oldV, oldC, oldD := version.Version, version.Commit, version.Date
	version.Version, version.Commit, version.Date = "v4.5.6", "feedface", "2026-02-03T04:05:06Z"
	t.Cleanup(func() { version.Version, version.Commit, version.Date = oldV, oldC, oldD })
	if got := Current().Build; got != version.String() {
		t.Errorf("a stamped identity is %q, want %q", got, version.String())
	}
	if !strings.Contains(Current().Build, "v4.5.6") || !strings.Contains(Current().Build, "feedface") {
		t.Errorf("a stamped identity lost the stamp: %q", Current().Build)
	}
}

// --- the fixed rendering (Definitions) ---------------------------------------

// The plain-text rendering (AC9), which is now the ONLY rendering: the same three facts,
// the value verbatim with no escaping, the same literal label immediately before it.
func TestText_CarriesTheSameThreeFactsVerbatim(t *testing.T) {
	for _, v := range []string{Upstream, forkValue, hostileValue} {
		body := Offer{SourceURL: v, License: License, Build: version.String()}.Text()
		if !strings.Contains(body, Label+": "+v) {
			t.Errorf("%q: the text body does not carry the label immediately before the value: %q", v, body)
		}
		if !strings.Contains(body, License) {
			t.Errorf("%q: the text body does not name the licence: %q", v, body)
		}
		if !strings.Contains(body, version.String()) {
			t.Errorf("%q: the text body does not carry the build identity: %q", v, body)
		}
		// No markup and no escaping: this body carries neither.
		for _, banned := range []string{"<a ", "&amp;", "&#34;", "&lt;", "&gt;"} {
			if strings.Contains(body, banned) {
				t.Errorf("%q: the text body carries %q: %q", v, banned, body)
			}
		}
	}
	// AC3 on this branch: a fork build's body names its own tree and never upstream.
	body := Offer{SourceURL: forkValue, License: License, Build: version.String()}.Text()
	if strings.Contains(body, Upstream) {
		t.Errorf("a fork build's text body names upstream: %q", body)
	}
}
