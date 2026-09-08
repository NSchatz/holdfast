package webui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/sourceoffer"
)

// The REMOVAL RECORD and the documentation links (S0052 AC7, AC16, AC19).
//
// These graders run inside `make check`, which must stay green on a machine with no
// browser, so none of them asks what the page LOOKS like - that is what the rendered
// graders beside them are for. What they decide is a property of the repository's own
// committed files: that no claim this item took off the surface was dropped rather than
// moved, that every documentation link the page renders resolves to a document committed
// here, and that the packages deciding whether a transcode is faithful were left alone.

// --- reading the record ---------------------------------------------------------

// removalRecord is the committed map from a sentence this item took off the rendered
// page to the heading in this repository's own documentation it now lives under.
//
// `baseline` is what makes the record CHECKABLE in both directions. Without it the
// record could only ever say "here are some sentences I moved", and a sentence deleted
// and never written down would be invisible to any gate. With it the check is
// exhaustive: every sentence the page used to render is either still in the document it
// generates, or is named here with somewhere it went.
//
// `reworded` is the second of the two ways a sentence can leave the page, and it is a
// different fact from the first: the claim is still ON the surface, in fewer words. It
// carries the replacement as well as the heading, so the gate can require that the
// replacement is in the served document - which is what stops "reworded" being a place to
// file a deletion.
type removalRecord struct {
	baseline []string
	moved    map[string]string // sentence -> path#anchor
	order    []string          // the moved sentences, in the order the record names them
	reworded map[string]rewording
	rewrites []string // the reworded sentences, in the order the record names them
}

// rewording is one sentence the page still says, with what it says now and the heading
// that documents the claim.
type rewording struct {
	now    string
	target string
}

// normalisedSentence is the form every comparison in this file is made under: one line,
// single spaces, and the typographic characters a page and a document routinely disagree
// about folded to their plain equivalents. Comparing raw text would make the record fail
// on a line wrap, which teaches a reader to loosen the check rather than fix the claim.
func normalisedSentence(s string) string {
	r := strings.NewReplacer(
		"—", "-", "–", "-", "’", "'", "“", `"`, "”", `"`,
		" ", " ", "…", "...",
	)
	return strings.Join(strings.Fields(r.Replace(s)), " ")
}

func parseRemovalRecord(t *testing.T) removalRecord {
	t.Helper()
	rec := removalRecord{moved: map[string]string{}, reworded: map[string]rewording{}}
	section := ""
	for i, line := range strings.Split(proseFile(t, "removal-record.txt"), "\n") {
		raw := strings.TrimSpace(line)
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
			section = raw
			continue
		}
		switch section {
		case "[baseline]":
			rec.baseline = append(rec.baseline, raw)
		case "[moved]":
			sentence, target, ok := strings.Cut(raw, "==>")
			if !ok {
				t.Fatalf("removal-record.txt line %d is in [moved] but names no destination: %q", i+1, raw)
			}
			s := strings.TrimSpace(sentence)
			rec.moved[normalisedSentence(s)] = strings.TrimSpace(target)
			rec.order = append(rec.order, s)
		case "[reworded]":
			parts := strings.Split(raw, "==>")
			if len(parts) != 3 {
				t.Fatalf("removal-record.txt line %d is in [reworded] but is not <old> ==> <new> ==> <path>#<anchor>: %q", i+1, raw)
			}
			s := strings.TrimSpace(parts[0])
			rec.reworded[normalisedSentence(s)] = rewording{
				now:    strings.TrimSpace(parts[1]),
				target: strings.TrimSpace(parts[2]),
			}
			rec.rewrites = append(rec.rewrites, s)
		default:
			t.Fatalf("removal-record.txt line %d is outside any section: %q", i+1, raw)
		}
	}
	return rec
}

// removedSentences is the list AC4's grader reads: every sentence the record says is no
// longer on the visible surface, whichever way it left.
func removedSentences(t *testing.T) []string {
	t.Helper()
	rec := parseRemovalRecord(t)
	return append(append([]string(nil), rec.order...), rec.rewrites...)
}

// --- the documentation the record points into ------------------------------------

func repoPath(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

// slugify is GitHub's own heading-anchor rule, which is the one a reader following a
// link from the page will actually be resolved by: case folded, spaces to hyphens, and
// everything that is not a letter, a digit, a hyphen or an underscore dropped.
var slugDrop = regexp.MustCompile(`[^a-z0-9\-_ ]+`)

func slugify(heading string) string {
	s := strings.ToLower(strings.TrimSpace(heading))
	s = slugDrop.ReplaceAllString(s, "")
	return strings.ReplaceAll(s, " ", "-")
}

var headingLine = regexp.MustCompile(`^(#{1,6})\s+(.*\S)\s*$`)

// docHeading is one heading of a committed document and the body text beneath it, up to
// the next heading at the same level or above.
type docHeading struct {
	level int
	text  string
	slug  string
	body  string
}

func readDocHeadings(path string) ([]docHeading, error) {
	b, err := os.ReadFile(repoPath(filepath.FromSlash(path)))
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	var out []docHeading
	fenced := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
		}
		if fenced {
			continue
		}
		m := headingLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		h := docHeading{level: len(m[1]), text: m[2], slug: slugify(m[2])}
		var body []string
		infence := false
		for _, next := range lines[i+1:] {
			if strings.HasPrefix(strings.TrimSpace(next), "```") {
				infence = !infence
			}
			if !infence {
				if n := headingLine.FindStringSubmatch(next); n != nil && len(n[1]) <= h.level {
					break
				}
			}
			body = append(body, next)
		}
		h.body = strings.Join(body, "\n")
		out = append(out, h)
	}
	return out, nil
}

// docTreeSegment is the path segment the handler puts between the source URL the binary
// was built with and the repository-relative document path (webui.go's docLinkHTML). It
// is restated here rather than exported, and the test below reads the hrefs the REAL
// handler produced, so a change to the link shape reds this file instead of passing
// silently against a form nothing renders.
const docTreeSegment = "/blob/main/"

// splitDocRef takes a documentation href as the page renders it and returns the
// repository-relative path and the anchor.
func splitDocRef(raw string) (path, anchor string) {
	i := strings.Index(raw, docTreeSegment)
	if i < 0 {
		return "", ""
	}
	path, anchor, _ = strings.Cut(raw[i+len(docTreeSegment):], "#")
	return path, anchor
}

// isDocumentationLink reports whether an href names a document committed in this
// repository, in either of the two forms above.
func isDocumentationLink(raw string) bool {
	path, _ := splitDocRef(raw)
	return strings.HasPrefix(path, "docs/") && strings.HasSuffix(path, ".md")
}

// documentationTargetProblem is AC16 and the second half of AC7: the link resolves to a
// document committed here, the anchor names a heading in it, and that heading has body
// text beneath it.
func documentationTargetProblem(path, anchor string) error {
	if path == "" {
		return fmt.Errorf("the link names no document in this repository")
	}
	headings, err := readDocHeadings(path)
	if err != nil {
		return fmt.Errorf("the link names %s, which is not a document committed in this repository: %w", path, err)
	}
	if anchor == "" {
		return nil
	}
	for _, h := range headings {
		if h.slug != anchor {
			continue
		}
		if strings.TrimSpace(h.body) == "" {
			return fmt.Errorf("%s#%s is a heading with no body text beneath it", path, anchor)
		}
		return nil
	}
	return fmt.Errorf("%s carries no heading whose anchor is #%s", path, anchor)
}

// --- AC7: every claim moved, none dropped ----------------------------------------

// renderedPage is the document the REAL handler produces for this repository's own source
// URL: the committed page with the AGPL offer and the per-region documentation links
// resolved into it. A sentence that is not in these bytes cannot be rendered by this page,
// whatever a rule does to it - which is the one direction of "is it still on the surface"
// a browserless gate can settle, and it is the safe direction: it can only ever say a
// sentence is GONE.
func renderedPage() []byte { return render(offerFor(sourceoffer.Upstream)) }

func generatedDocument() string { return normalisedSentence(string(renderedPage())) }

// recordProblems is AC7. It is a function returning findings rather than a body of
// t.Errorf calls so the mutation proof below can show it BITES.
func recordProblems(rec removalRecord, document string) []string {
	var out []string
	if len(rec.baseline) == 0 {
		return []string{"the removal record names no baseline page copy, so nothing could be checked"}
	}
	seen := map[string]bool{}
	for _, s := range rec.baseline {
		n := normalisedSentence(s)
		if seen[n] {
			out = append(out, fmt.Sprintf("the baseline names the same sentence twice: %q", s))
		}
		seen[n] = true
		if strings.Contains(document, n) {
			continue // still in the document the binary serves, so it was not removed
		}
		_, moved := rec.moved[n]
		_, reworded := rec.reworded[n]
		if !moved && !reworded {
			out = append(out, fmt.Sprintf("the sentence %q is no longer in the generated document and the removal record does not say where it went; a claim was dropped, not moved", s))
		}
		if moved && reworded {
			out = append(out, fmt.Sprintf("the removal record files %q as BOTH moved off the surface and reworded on it; it is one or the other", s))
		}
	}

	// destinationProblems is the half both sections share: the record names a heading in
	// this repository's own documentation, and that heading exists with body text.
	destinationProblems := func(s, target string) []string {
		path, anchor, _ := strings.Cut(target, "#")
		if !strings.HasPrefix(path, "docs/") || anchor == "" {
			return []string{fmt.Sprintf("the removal record sends %q to %q, which is not a heading in this repository's own documentation", s, target)}
		}
		if err := documentationTargetProblem(path, anchor); err != nil {
			return []string{fmt.Sprintf("the removal record sends %q to %q: %v", s, target, err)}
		}
		return nil
	}

	for _, s := range rec.order {
		n := normalisedSentence(s)
		if !seen[n] {
			out = append(out, fmt.Sprintf("the removal record moves %q, which is not one of the sentences the baseline says the page rendered", s))
		}
		if strings.Contains(document, n) {
			out = append(out, fmt.Sprintf("the removal record says %q was taken off the surface, but the generated document still carries it", s))
		}
		target := rec.moved[n]
		if probs := destinationProblems(s, target); probs != nil {
			out = append(out, probs...)
			continue
		}
		path, anchor, _ := strings.Cut(target, "#")
		headings, err := readDocHeadings(path)
		if err != nil {
			out = append(out, fmt.Sprintf("reading %s: %v", path, err))
			continue
		}
		found := false
		for _, h := range headings {
			if h.slug == anchor && strings.Contains(normalisedSentence(h.body), n) {
				found = true
			}
		}
		if !found {
			out = append(out, fmt.Sprintf("the claim %q is not under %s; the record maps it to a heading that does not carry it, so the claim was dropped rather than moved", s, target))
		}
	}

	// A rewording is a claim the page STILL makes. The old words must be gone and the new
	// words must be there: a "rewording" whose replacement is not in the document is a
	// deletion filed under the wrong heading, which is exactly what this section could
	// otherwise become.
	for _, s := range rec.rewrites {
		n := normalisedSentence(s)
		if !seen[n] {
			out = append(out, fmt.Sprintf("the removal record rewords %q, which is not one of the sentences the baseline says the page rendered", s))
		}
		if strings.Contains(document, n) {
			out = append(out, fmt.Sprintf("the removal record says %q was reworded, but the generated document still carries the old words", s))
		}
		r := rec.reworded[n]
		if strings.TrimSpace(r.now) == "" {
			out = append(out, fmt.Sprintf("the removal record rewords %q into nothing at all; that is a deletion, and it belongs in [moved]", s))
			continue
		}
		if !strings.Contains(document, normalisedSentence(r.now)) {
			out = append(out, fmt.Sprintf("the removal record says %q now reads %q, but the generated document carries no such words; the claim was dropped, not reworded", s, r.now))
		}
		out = append(out, destinationProblems(s, r.target)...)
	}
	return out
}

func TestProse_EveryClaimTakenOffThePageIsInTheDocumentationInstead(t *testing.T) {
	rec := parseRemovalRecord(t)
	if len(rec.order) == 0 {
		t.Fatal("the removal record moves nothing at all; this item removed prose, so an empty record is a record that was not written")
	}
	for _, f := range recordProblems(rec, generatedDocument()) {
		t.Error(f)
	}
}

// The record check must BITE. Each mutation is a way the record can be wrong, and the
// grader has to report every one of them - a check nobody tries to defeat is a check
// nobody knows works (AC17).
func TestProse_TheRemovalRecordCheckFailsAgainstEveryWayItCanBeWrong(t *testing.T) {
	base := parseRemovalRecord(t)
	doc := generatedDocument()
	if probs := recordProblems(base, doc); probs != nil {
		t.Fatalf("the committed record is already failing, so the mutations below prove nothing: %v", probs)
	}

	clone := func() removalRecord {
		c := removalRecord{
			baseline: append([]string(nil), base.baseline...),
			moved:    map[string]string{},
			order:    append([]string(nil), base.order...),
			reworded: map[string]rewording{},
			rewrites: append([]string(nil), base.rewrites...),
		}
		for k, v := range base.moved {
			c.moved[k] = v
		}
		for k, v := range base.reworded {
			c.reworded[k] = v
		}
		return c
	}
	first := base.order[0]
	if len(base.rewrites) == 0 {
		t.Fatal("the removal record rewords nothing at all; the rewording mutations below would prove nothing")
	}
	firstRewrite := base.rewrites[0]

	for name, mutate := range map[string]func(removalRecord) removalRecord{
		"a removed sentence is not named at all": func(c removalRecord) removalRecord {
			delete(c.moved, normalisedSentence(first))
			out := c.order[:0]
			for _, s := range c.order {
				if s != first {
					out = append(out, s)
				}
			}
			c.order = out
			return c
		},
		"a removed sentence is sent to a document that is not committed here": func(c removalRecord) removalRecord {
			c.moved[normalisedSentence(first)] = "docs/no-such-document.md#right-now"
			return c
		},
		"a removed sentence is sent to a heading that does not exist": func(c removalRecord) removalRecord {
			c.moved[normalisedSentence(first)] = DocPath + "#a-heading-nobody-wrote"
			return c
		},
		"a removed sentence is sent to a heading that does not carry it": func(c removalRecord) removalRecord {
			c.moved[normalisedSentence(first)] = DocPath + "#themes-type-and-motion"
			return c
		},
		"a removed sentence is sent somewhere outside the documentation": func(c removalRecord) removalRecord {
			c.moved[normalisedSentence(first)] = "somewhere else"
			return c
		},
		"the record claims to have moved a sentence the page still renders": func(c removalRecord) removalRecord {
			still := "Loading" // a word the shipped document does still carry
			c.baseline = append(c.baseline, still)
			c.order = append(c.order, still)
			c.moved[normalisedSentence(still)] = DocPath + "#the-three-states-every-view-shows"
			return c
		},
		"the baseline is empty": func(c removalRecord) removalRecord {
			c.baseline = nil
			return c
		},
		"a reworded sentence is not named at all": func(c removalRecord) removalRecord {
			delete(c.reworded, normalisedSentence(firstRewrite))
			out := c.rewrites[:0]
			for _, s := range c.rewrites {
				if s != firstRewrite {
					out = append(out, s)
				}
			}
			c.rewrites = out
			return c
		},
		"a rewording names words the page does not render": func(c removalRecord) removalRecord {
			r := c.reworded[normalisedSentence(firstRewrite)]
			r.now = "words nothing on this page has ever said"
			c.reworded[normalisedSentence(firstRewrite)] = r
			return c
		},
		"a rewording replaces a sentence with nothing": func(c removalRecord) removalRecord {
			r := c.reworded[normalisedSentence(firstRewrite)]
			r.now = ""
			c.reworded[normalisedSentence(firstRewrite)] = r
			return c
		},
		"a rewording names a heading that does not exist": func(c removalRecord) removalRecord {
			r := c.reworded[normalisedSentence(firstRewrite)]
			r.target = DocPath + "#a-heading-nobody-wrote"
			c.reworded[normalisedSentence(firstRewrite)] = r
			return c
		},
		"a sentence is filed as both moved and reworded": func(c removalRecord) removalRecord {
			c.moved[normalisedSentence(firstRewrite)] = DocPath + "#the-three-states-every-view-shows"
			c.order = append(c.order, firstRewrite)
			return c
		},
	} {
		if probs := recordProblems(mutate(clone()), doc); probs == nil {
			t.Errorf("the removal-record check accepted a record that is wrong (%s)", name)
		}
	}
}

// --- AC16: every documentation link the page renders resolves ---------------------

var hrefRe = regexp.MustCompile(`href="([^"]*)"`)

// documentLinkProblems is AC16 over the document the handler SERVES, which is the
// document the page renders: a link that is not in these bytes cannot be rendered, and
// one that is must resolve into this repository.
func documentLinkProblems(document string) []string {
	var out []string
	seen := 0
	for _, m := range hrefRe.FindAllStringSubmatch(document, -1) {
		raw := m[1]
		if !strings.Contains(raw, docTreeSegment) {
			continue
		}
		seen++
		path, anchor := splitDocRef(raw)
		if err := documentationTargetProblem(path, anchor); err != nil {
			out = append(out, fmt.Sprintf("the page links to %q: %v", raw, err))
		}
	}
	if seen == 0 {
		out = append(out, "the served document renders no documentation link at all, so this check cannot fail")
	}
	return out
}

func TestProse_EveryDocumentationLinkThePageRendersResolvesHere(t *testing.T) {
	for _, f := range documentLinkProblems(string(renderedPage())) {
		t.Error(f)
	}
	// And it BITES: each of these is a link that would leave a reader nowhere (AC17).
	for name, bad := range map[string]string{
		"a document that is not committed here": `href="https://example.invalid` + docTreeSegment + `docs/nowhere.md#right-now"`,
		"a heading nobody wrote":                `href="https://example.invalid` + docTreeSegment + `docs/dashboard-methodology.md#not-a-heading"`,
		"no link at all":                        `href="/api/events"`,
	} {
		if documentLinkProblems(bad) == nil {
			t.Errorf("the documentation-link check accepted %s", name)
		}
	}
}

// --- AC19: the deciding packages are untouched ------------------------------------

// verdictPackages are the packages that decide whether a transcode is faithful or that
// remove a source file. This item changes the words printed AROUND that verdict and may
// not change the verdict, so the assertion is that the dashboard's own package tree
// imports none of them and that this item's surface is confined to it.
//
// The diff itself is checked by the impl gate; what is pinned HERE, where it can red on
// every future run, is the structural half: nothing under internal/webui can reach the
// engine, the verifier, the VMAF instrument or the store.
var verdictPackages = []string{
	"internal/engine", "internal/vmaf", "internal/probe", "internal/store",
	"internal/encoder", "internal/hdr",
}

func TestProse_TheDashboardCannotReachAnythingThatDecidesASwap(t *testing.T) {
	self := moduleName(t)
	var offenders []string
	err := filepath.WalkDir(".", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, pkg := range verdictPackages {
			if strings.Contains(string(b), `"`+self+"/"+pkg+`"`) {
				offenders = append(offenders, p+" imports "+pkg)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking internal/webui: %v", err)
	}
	for _, o := range offenders {
		t.Errorf("%s: the dashboard may print fewer words, but nothing here may reach a package that decides whether a swap is safe", o)
	}
	// The sweep must be able to see an import at all, or it proves nothing.
	if !strings.Contains(string(mustRead(t, "webui.go")), `"`+self+"/internal/sourceoffer\"") {
		t.Fatal("the import sweep found none of this package's own imports; it is not reading the source")
	}
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return b
}
