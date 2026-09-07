package webui

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode"

	"github.com/NSchatz/holdfast/internal/sourceoffer"
	"github.com/NSchatz/holdfast/internal/version"
)

// pinnedCSP is the Content-Security-Policy the dashboard sent at pin 38fb8b3,
// spelled out here rather than read from the package so this test pins the BYTES and
// not whatever the code currently says. The source offer must need no relaxation of
// it (AC10).
const pinnedCSP = "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; require-trusted-types-for 'script'"

const (
	forkValue    = "https://example.invalid/fork"
	hostileValue = `https://example.invalid/a?b="><img src=x onerror=1>`
)

func offerFor(url string) sourceoffer.Offer {
	return sourceoffer.Offer{SourceURL: url, License: sourceoffer.License, Build: version.String()}
}

// fetchRoot serves GET / with NO credentials of any kind and returns the body.
func fetchRoot(t *testing.T, h http.Handler) (*httptest.ResponseRecorder, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body, _ := io.ReadAll(rec.Body)
	return rec, string(body)
}

// offerRegion returns THE SOURCE OFFER: the smallest element of the served document
// containing the Corresponding Source link, the licence name AND the build identity.
// Every assertion below scoped to "the offer" reads this region, not the narrower
// element around the link alone (binding advisory F11).
func offerRegion(t *testing.T, doc string) string {
	t.Helper()
	const open = `<p class="source-offer">`
	i := strings.Index(doc, open)
	if i < 0 {
		t.Fatal("the served document carries no source offer")
	}
	rest := doc[i:]
	j := strings.Index(rest, "</p>")
	if j < 0 {
		t.Fatal("the source offer element is not closed")
	}
	region := rest[:j+len("</p>")]

	// It really is the SMALLEST such element: the only element nested inside it is
	// the link, and the link carries neither the licence name nor the identity, so
	// no smaller element contains all three.
	if strings.Count(region, "<a ") != 1 {
		t.Fatalf("the offer must carry exactly one element (the link): %s", region)
	}
	link := region[strings.Index(region, "<a "):strings.Index(region, "</a>")]
	if strings.Contains(link, sourceoffer.License) {
		t.Fatalf("the licence name is inside the link, so the link would be the smaller region: %s", region)
	}
	if strings.Contains(link, version.String()) {
		t.Fatalf("the identity is inside the link, so the link would be the smaller region: %s", region)
	}
	return region
}

// --- the visible-without-interaction predicate -------------------------------

// hidingSelectors returns the selector of every rule in the document's OWN embedded
// stylesheet that sets `display: none` or `visibility: hidden`. This is a byte-level
// scan of the served rule text, deliberately: Definitions puts a CSS parser and an
// element tree out of scope and says this byte-level check is the whole of what the
// clause obliges. The BROWSER-side confirmation is TestRendered_* in this package,
// which reads the computed style after the real cascade.
func hidingSelectors(doc string) ([]string, error) {
	i, j := strings.Index(doc, "<style>"), strings.Index(doc, "</style>")
	if i < 0 || j < 0 || j < i {
		return nil, fmt.Errorf("the served document has no embedded stylesheet")
	}
	css := doc[i+len("<style>") : j]
	var out []string
	for _, chunk := range strings.Split(css, "}") {
		k := strings.Index(chunk, "{")
		if k < 0 {
			continue
		}
		sel, body := strings.TrimSpace(chunk[:k]), chunk[k+1:]
		flat := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, body)
		if strings.Contains(flat, "display:none") || strings.Contains(flat, "visibility:hidden") {
			out = append(out, sel)
		}
	}
	return out, nil
}

// openTagOf returns the opening tag of the first element in doc starting with prefix,
// or "" when there is none.
func openTagOf(doc, prefix string) string {
	i := strings.Index(doc, prefix)
	if i < 0 {
		return ""
	}
	j := strings.Index(doc[i:], ">")
	if j < 0 {
		return ""
	}
	return doc[i : i+j+1]
}

// offerCarries names every class, id and element type the offer or one of its
// ancestors carries. A hiding rule in the embedded stylesheet may name none of them.
var offerCarries = []string{"source-offer", "footer", "body", "html", "*"}

// visibilityProblems is the predicate in full, exactly as Definitions states it: the
// offer is present in the bytes of the root response as served; it is not inside a
// <details> element; neither it nor any ancestor carries a `hidden` attribute; and no
// rule in the document's own embedded stylesheet that sets `display: none` or
// `visibility: hidden` names a class, id or element type the offer or one of its
// ancestors carries.
//
// It returns findings rather than calling t.Errorf so the mutation test below can
// prove it BITES.
func visibilityProblems(doc, region string) []string {
	var out []string
	if !strings.Contains(doc, region) {
		return []string{"the offer is not present in the served bytes"}
	}
	if strings.Contains(doc, "<details") {
		out = append(out, "the served document contains a <details> element; the offer must not be behind one")
	}
	for _, prefix := range []string{"<html", "<body", "<footer"} {
		tag := openTagOf(doc, prefix)
		if tag == "" {
			out = append(out, "the served document has no "+prefix+" element")
			continue
		}
		out = append(out, hiddenAttrProblems(tag)...)
	}
	out = append(out, hiddenAttrProblems(openTagOf(region, "<p "))...)

	sels, err := hidingSelectors(doc)
	if err != nil {
		return append(out, err.Error())
	}
	for _, sel := range sels {
		for _, token := range offerCarries {
			if strings.Contains(sel, token) {
				out = append(out, "the embedded stylesheet rule "+sel+" hides "+token+
					", which the offer or an ancestor carries")
			}
		}
	}
	return out
}

func hiddenAttrProblems(tag string) []string {
	for _, form := range []string{" hidden>", " hidden ", " hidden="} {
		if strings.Contains(tag, form) {
			return []string{"the offer or an ancestor carries a hidden attribute: " + tag}
		}
	}
	return nil
}

// The predicate must BITE. Each mutation is a way the offer could be in the bytes and
// still be unreachable without interaction; the grader has to report every one.
func TestVisibilityPredicate_FailsAgainstEveryHidingMutation(t *testing.T) {
	_, doc := fetchRoot(t, HandlerFor(offerFor(sourceoffer.Upstream)))
	region := offerRegion(t, doc)
	if probs := visibilityProblems(doc, region); probs != nil {
		t.Fatalf("the shipped document was reported as hiding the offer: %v", probs)
	}

	for name, mutate := range map[string]func(string) string{
		"the offer's own class is display:none": func(d string) string {
			return strings.Replace(d, ".source-offer { margin:12px 0 0; }",
				".source-offer { display:none; }", 1)
		},
		"an ancestor element type is display:none": func(d string) string {
			return strings.Replace(d, "footer { color:var(--muted);",
				"footer { display: none; color:var(--muted);", 1)
		},
		"an ancestor element type is visibility:hidden": func(d string) string {
			return strings.Replace(d, "footer { color:var(--muted);",
				"footer { visibility:hidden; color:var(--muted);", 1)
		},
		"the offer carries a hidden attribute": func(d string) string {
			return strings.Replace(d, `<p class="source-offer">`, `<p class="source-offer" hidden>`, 1)
		},
		"an ancestor carries a hidden attribute": func(d string) string {
			return strings.Replace(d, "<footer>", "<footer hidden>", 1)
		},
		"the offer is behind a details element": func(d string) string {
			return strings.Replace(d, "<footer>", "<footer><details><summary>legal</summary>", 1)
		},
		"the offer is not in the bytes at all": func(d string) string {
			return strings.Replace(d, region, "", 1)
		},
	} {
		mutant := mutate(doc)
		if mutant == doc {
			t.Fatalf("the mutation %q did not change the document - the assertion below would be vacuous", name)
		}
		if probs := visibilityProblems(mutant, region); probs == nil {
			t.Errorf("the visibility grader passed a mutation that hides the offer (%s)", name)
		}
	}
}

// --- the offer, on the default build -----------------------------------------

// The marker is the one seam between the embedded document and the rendered offer.
// Exactly one, and none left in the served bytes - a marker that went missing would
// silently drop the offer from every response.
func TestSourceOffer_MarkerIsSubstitutedExactlyOnce(t *testing.T) {
	if got := strings.Count(string(indexHTML), offerMarker); got != 1 {
		t.Fatalf("index.html carries the offer marker %d times, want exactly 1", got)
	}
	_, doc := fetchRoot(t, HandlerFor(offerFor(sourceoffer.Upstream)))
	if strings.Contains(doc, offerMarker) {
		t.Error("the offer marker survived into the served document")
	}
}

// AC1, AC2, AC5, AC7, AC10, AC12 on the default build. This test binary is UNSTAMPED
// (`go test` applies none of the Makefile's -ldflags), so the identity asserted here
// is the AC5 default-identity case, made against a build with no stamp at all -
// binding advisory F15, which is why it is NOT made against `make build`, whose
// Makefile derives a real commit from `git rev-parse`.
func TestSourceOffer_DefaultBuildServesTheOfferToAnUncredentialedRequest(t *testing.T) {
	rec, doc := fetchRoot(t, HandlerFor(offerFor(sourceoffer.Upstream)))

	// AC7: no bearer token was sent and none is needed at the root.
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / with no credentials: code %d, want 200", rec.Code)
	}
	region := offerRegion(t, doc)

	// AC1: the offer is visible without interaction.
	if probs := visibilityProblems(doc, region); probs != nil {
		t.Errorf("the offer is not visible without interaction: %v", probs)
	}

	// AC1 + AC2: the link's target AND its displayed text are both the upstream URL,
	// with the literal label immediately before it.
	if probs := offerLinkProblems(region, sourceoffer.Upstream); probs != nil {
		t.Errorf("%v\noffer: %s", probs, region)
	}

	// AC12: the licence is named as text and is not a link.
	if probs := licenceProblems(region); probs != nil {
		t.Errorf("%v\noffer: %s", probs, region)
	}

	// AC5: the identity is the default one the `version` subcommand prints, never
	// blank or absent.
	for _, want := range []string{"0.0.0-dev", "unknown"} {
		if !strings.Contains(region, want) {
			t.Errorf("the offer does not carry the unstamped default identity %q: %s", want, region)
		}
	}
	if !strings.Contains(region, version.String()) {
		t.Errorf("the offer's identity is not what `holdfast version` prints (%q): %s", version.String(), region)
	}

	// AC10: the Content-Security-Policy is byte-identical to the pinned one, and the
	// served document still assigns no string to an HTML sink.
	if got := rec.Header().Get("Content-Security-Policy"); got != pinnedCSP {
		t.Errorf("CSP changed.\n got: %q\nwant: %q", got, pinnedCSP)
	}
	assertNoHTMLStringSinks(t, doc)
}

// AC3: a fork build's offer names the fork's tree, character for character, as both
// the link target and the displayed text - and the upstream URL occurs NOWHERE inside
// the offer, neither as a link target nor as text. The region read is the source
// offer as Definitions bounds it, not the element around the link alone (F11).
func TestSourceOffer_ForkBuildNamesItsOwnTreeAndNeverUpstream(t *testing.T) {
	_, doc := fetchRoot(t, HandlerFor(offerFor(forkValue)))
	region := offerRegion(t, doc)

	if probs := offerLinkProblems(region, forkValue); probs != nil {
		t.Errorf("%v\noffer: %s", probs, region)
	}
	if strings.Contains(region, sourceoffer.Upstream) {
		t.Errorf("the upstream URL occurs inside a fork build's source offer: %s", region)
	}
	if probs := visibilityProblems(doc, region); probs != nil {
		t.Errorf("the fork build's offer is not visible without interaction: %v", probs)
	}
}

// AC11 (and AC6's accept half, and AC10): a value carrying characters that are
// significant in HTML is SERVED, not refused, and appears only as the link's target
// and its displayed text. It introduces no element, no attribute and no script, and
// the CSP and the no-HTML-sink idiom are untouched.
//
// The byte-level half is here; the browser decides whether an ELEMENT or an ATTRIBUTE
// appeared, in TestRendered_HostileValueIntroducesNoElementOrAttribute.
func TestSourceOffer_HTMLSignificantValueIntroducesNoMarkup(t *testing.T) {
	rec, doc := fetchRoot(t, HandlerFor(offerFor(hostileValue)))
	if rec.Code != http.StatusOK {
		t.Fatalf("a value with HTML-significant characters must be served, got code %d", rec.Code)
	}
	region := offerRegion(t, doc)

	if probs := offerLinkProblems(region, hostileValue); probs != nil {
		t.Fatalf("%v\noffer: %s", probs, region)
	}
	// The value carried `"><img src=x onerror=1>`. No img element appears anywhere in
	// the served document, and the tag counts are unchanged bar the offer's own two:
	// nothing the value contains became markup.
	if strings.Contains(doc, "<img") {
		t.Error("the value introduced an img element into the served document")
	}
	if got, want := strings.Count(doc, "<script"), strings.Count(string(indexHTML), "<script"); got != want {
		t.Errorf("the served document has %d script tags, the embedded one has %d", got, want)
	}
	benign := renderedTagCount(t, sourceoffer.Upstream)
	if got := renderedTagCount(t, hostileValue); got != benign {
		t.Errorf("the hostile value changed the served document's tag count: %d, want %d", got, benign)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != pinnedCSP {
		t.Errorf("CSP changed to carry a hostile value.\n got: %q\nwant: %q", got, pinnedCSP)
	}
	assertNoHTMLStringSinks(t, doc)
	if probs := visibilityProblems(doc, region); probs != nil {
		t.Errorf("the hostile-value offer is not visible without interaction: %v", probs)
	}
}

func renderedTagCount(t *testing.T, url string) int {
	t.Helper()
	_, doc := fetchRoot(t, HandlerFor(offerFor(url)))
	return strings.Count(doc, "<")
}

// AC8, dashboard half: the offer is in the STATIC bytes of the root response. It is
// substituted server-side at handler construction and no script in the document
// touches it, so no API endpoint failing, timing out or returning garbage can change
// or remove it (binding advisory F17 - the offer is discharged by construction, and
// this is the assertion that shows it). The end-to-end observation with every open
// read endpoint returning 500 is in internal/server.
func TestSourceOffer_IsStaticBytesAndNotFetchedByThePage(t *testing.T) {
	_, doc := fetchRoot(t, HandlerFor(offerFor(forkValue)))
	region := offerRegion(t, doc)

	if strings.Contains(region, "<script") {
		t.Error("the offer contains a script; it must be static bytes")
	}
	script := doc[strings.Index(doc, "<script"):]
	for _, token := range []string{"source-offer", "Corresponding Source", sourceoffer.License, forkValue} {
		if strings.Contains(script, token) {
			t.Errorf("the page's JavaScript references %q; the offer must not depend on any client-side render", token)
		}
	}
	// Two identically-constructed handlers serve byte-identical offers: the value is
	// fixed by the build, not by anything a request can influence.
	_, again := fetchRoot(t, HandlerFor(offerFor(forkValue)))
	if offerRegion(t, again) != region {
		t.Error("the offer differs between responses")
	}
}

// AC6, dashboard half, in process: a handler constructed while the build-time value
// is unusable REFUSES rather than serving an offer that names a rejected value. In
// the daemon this can never be reached - cmd/holdfast refuses before any listener
// exists - but the same accept test decides both (binding advisories F13 and F16).
func TestSourceOffer_HandlerRefusesEveryRejectedValue(t *testing.T) {
	for _, bad := range []string{"", "   ", "not-a-url", "javascript:alert(1)", "//example.com"} {
		old := sourceoffer.URL
		sourceoffer.URL = bad

		rec, body := fetchRoot(t, Handler())
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%q: code %d, want 503 - a rejected value must not reach a root response", bad, rec.Code)
		}
		if !strings.Contains(body, bad) && bad != "" && strings.TrimSpace(bad) != "" {
			t.Errorf("%q: the refusal does not name the rejected value: %q", bad, body)
		}
		// It is a refusal, not an offer: no link, no licence name, no document.
		for _, forbidden := range []string{"<a ", sourceoffer.License, "<!DOCTYPE html>"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%q: the refusal body carries %q; it must not be an offer: %q", bad, forbidden, body)
			}
		}
		// And it does not silently fall back to the upstream URL, which would tell a
		// fork's users that upstream is the source of a binary it is not.
		if strings.Contains(body, sourceoffer.Upstream) {
			t.Errorf("%q: the refusal fell back to naming upstream: %q", bad, body)
		}
		sourceoffer.URL = old
	}
}

// --- shared graders ----------------------------------------------------------

// offerLinkProblems reports every way the offer departs from the rendering
// Definitions fixes: the literal text `Corresponding Source` immediately before the
// link with only whitespace and punctuation between, and the source URL in effect as
// BOTH the link's target and its displayed text, decoding back character for
// character. The mutation proof for this grader is in
// internal/sourceoffer.TestLinkProblems_FailsAgainstEveryRenderingMutation, which
// runs it against eleven renderings that each break exactly one clause.
func offerLinkProblems(region, want string) []string {
	var out []string
	esc := html.EscapeString(want)
	wantLink := `<a class="source-offer-link" href="` + esc + `">` + esc + `</a>`
	if !strings.Contains(region, wantLink) {
		out = append(out, "the offer does not carry "+want+" as both the link target and the displayed text")
	}
	if html.UnescapeString(esc) != want {
		out = append(out, "the document's escaping does not decode back to the value character for character")
	}
	const label = "Corresponding Source"
	open := strings.Index(region, "<a ")
	if open < 0 {
		return append(out, "the offer carries no link")
	}
	i := strings.LastIndex(region[:open], label)
	if i < 0 {
		return append(out, "the literal text "+label+" does not occur in the offer before the link")
	}
	for _, r := range region[i+len(label) : open] {
		if !unicode.IsSpace(r) && !unicode.IsPunct(r) {
			out = append(out, "the character "+string(r)+" sits between the label and the link; only whitespace and punctuation may")
		}
	}
	return out
}

// licenceProblems is AC12: the licence is named as TEXT and never by way of a link.
func licenceProblems(region string) []string {
	var out []string
	lic := strings.Index(region, sourceoffer.License)
	if lic < 0 {
		return []string{"the offer does not name the licence " + sourceoffer.License}
	}
	start, end := strings.Index(region, "<a "), strings.Index(region, "</a>")
	if start >= 0 && end > start && lic > start && lic < end {
		out = append(out, "the licence is inside the link")
	}
	if strings.Contains(region, `href="`+sourceoffer.License) {
		out = append(out, "the licence is a link target")
	}
	return out
}

// AC10: the served document still assigns no string to an HTML sink.
func assertNoHTMLStringSinks(t *testing.T, doc string) {
	t.Helper()
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(doc, sink) {
			t.Errorf("the served document uses the HTML-string sink %q", sink)
		}
	}
}
