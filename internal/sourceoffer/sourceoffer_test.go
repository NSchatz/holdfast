package sourceoffer

import (
	"html"
	"strings"
	"testing"
	"unicode"

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

// linkProblems reports every way frag departs from the rendering Definitions fixes:
//
//	Corresponding Source: <a href="VALUE">VALUE</a>
//
// It is a function returning findings rather than a pile of t.Errorf calls so the
// mutation test below can prove it BITES: a grader that cannot fail is not evidence.
func linkProblems(frag, want string) []string {
	var out []string
	esc := html.EscapeString(want)

	if n := strings.Count(frag, "<a "); n != 1 {
		out = append(out, "the offer carries "+itoa(n)+" links, want exactly 1")
		return out
	}
	open := strings.Index(frag, "<a ")
	closeTag := strings.Index(frag, "</a>")
	if closeTag < open {
		out = append(out, "the link is not closed")
		return out
	}
	gt := strings.Index(frag[open:], ">")
	if gt < 0 {
		out = append(out, "the link's opening tag is unterminated")
		return out
	}
	openTag := frag[open : open+gt+1]
	shown := frag[open+gt+1 : closeTag]

	if !strings.Contains(openTag, `href="`+esc+`"`) {
		out = append(out, "the link target is not the source URL in effect: "+openTag)
	}
	if shown != esc {
		out = append(out, "the link's displayed text is "+shown+", want the source URL in effect and nothing else")
	}
	if html.UnescapeString(shown) != want {
		out = append(out, "the displayed text does not decode back to the value character for character")
	}

	// The literal label sits immediately before the link, with only whitespace and
	// punctuation between the end of that text and the link's opening tag.
	i := strings.LastIndex(frag[:open], Label)
	if i < 0 {
		out = append(out, "the literal text "+Label+" does not occur before the link")
		return out
	}
	for _, r := range frag[i+len(Label) : open] {
		if !unicode.IsSpace(r) && !unicode.IsPunct(r) {
			out = append(out, "the character "+string(r)+" sits between the label and the link")
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// AC1, AC3, AC11, AC12 on the rendered fragment, over every vector the spec names.
func TestHTML_RendersTheFixedShapeForEveryVector(t *testing.T) {
	for _, v := range []string{Upstream, forkValue, hostileValue, "https://example.invalid/a b"} {
		frag := Offer{SourceURL: v, License: License, Build: version.String()}.HTML()
		if probs := linkProblems(frag, v); probs != nil {
			t.Errorf("%q: %v\nfragment: %s", v, probs, frag)
		}
		// AC12: the licence is TEXT and never a link.
		if !strings.Contains(frag, License) {
			t.Errorf("%q: the fragment does not name the licence", v)
		}
		if strings.Contains(frag, `href="`+License) {
			t.Errorf("%q: the licence is a link target", v)
		}
		lic := strings.Index(frag, License)
		if a, z := strings.Index(frag, "<a "), strings.Index(frag, "</a>"); lic > a && lic < z {
			t.Errorf("%q: the licence is inside the link", v)
		}
		// AC1: the identity is there too, so the fragment IS the whole offer.
		if !strings.Contains(frag, html.EscapeString(version.String())) {
			t.Errorf("%q: the fragment does not carry the build identity", v)
		}
		// AC11: the value introduced no element, no attribute and no script. The
		// fragment is exactly two elements deep whatever the value carries, and the
		// link's opening tag is exactly the two attributes this package writes -
		// so `"><img src=x onerror=1>` inside the value cannot have closed the
		// attribute, opened an element or added a handler.
		if got := strings.Count(frag, "<"); got != 4 { // <p, <a, </a, </p
			t.Errorf("%q: the fragment has %d tag openings, want 4: %s", v, got, frag)
		}
		wantOpen := `<a class="source-offer-link" href="` + html.EscapeString(v) + `">`
		if !strings.Contains(frag, wantOpen) {
			t.Errorf("%q: the link's opening tag is not exactly %s: %s", v, wantOpen, frag)
		}
		// `<img` and `<script` are markup wherever they appear; the escaped forms
		// (`&lt;img`) are not, and no substring test can tell an ATTRIBUTE from the
		// same characters rendered as text - which is why "no element or attribute
		// was introduced" is decided in the browser, by
		// internal/webui.TestRendered_*, over the real DOM.
		for _, banned := range []string{"<img", "<script"} {
			if strings.Contains(frag, banned) {
				t.Errorf("%q: the value introduced %q: %s", v, banned, frag)
			}
		}
	}
}

// The grader above must BITE. Each mutation breaks exactly one clause of the fixed
// rendering, and linkProblems has to report it - otherwise every assertion built on
// it is decoration.
func TestLinkProblems_FailsAgainstEveryRenderingMutation(t *testing.T) {
	const v = forkValue
	good := Offer{SourceURL: v, License: License, Build: "b"}.HTML()
	if probs := linkProblems(good, v); probs != nil {
		t.Fatalf("the conforming rendering was reported as broken: %v", probs)
	}
	for name, mutant := range map[string]string{
		"label text is the link text": `<p>Corresponding Source: <a href="` + v + `">` + Label + `</a></p>`,
		"label is missing":            `<p>Source: <a href="` + v + `">` + v + `</a></p>`,
		"words between label and link": `<p>` + Label + ` is available at <a href="` + v + `">` +
			v + `</a></p>`,
		"target is upstream":       `<p>` + Label + `: <a href="` + Upstream + `">` + v + `</a></p>`,
		"displayed text truncated": `<p>` + Label + `: <a href="` + v + `">example.invalid</a></p>`,
		"label follows the link":   `<p><a href="` + v + `">` + v + `</a> ` + Label + `</p>`,
		"a second link": `<p>` + Label + `: <a href="` + v + `">` + v + `</a> <a href="` + v +
			`">mirror</a></p>`,
		"no link at all": `<p>` + Label + `: ` + v + `</p>`,
	} {
		if probs := linkProblems(mutant, v); probs == nil {
			t.Errorf("the grader passed a mutation that breaks the rendering (%s): %s", name, mutant)
		}
	}

	// Escaping mutations, on a value that HAS characters to escape: an unescaped
	// value (which would break out of the attribute) and a double-escaped one (which
	// would no longer decode back to the value character for character).
	const h = hostileValue
	esc := html.EscapeString(h)
	if probs := linkProblems(`<p>`+Label+`: <a href="`+esc+`">`+esc+`</a></p>`, h); probs != nil {
		t.Fatalf("the conforming escaped rendering was reported as broken: %v", probs)
	}
	for name, mutant := range map[string]string{
		"value is not escaped":    `<p>` + Label + `: <a href="` + h + `">` + h + `</a></p>`,
		"value is double-escaped": `<p>` + Label + `: <a href="` + html.EscapeString(esc) + `">` + html.EscapeString(esc) + `</a></p>`,
	} {
		if probs := linkProblems(mutant, h); probs == nil {
			t.Errorf("the grader passed a mutation that breaks the escaping (%s): %s", name, mutant)
		}
	}
}

// The plain-text rendering (AC9): the same three facts, the value verbatim with no
// escaping, the same literal label immediately before it.
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
