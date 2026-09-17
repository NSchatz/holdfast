package docscheck

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// The shipped documentation's statement about DYNAMIC HDR, checked mechanically over the
// corpus by a test the aggregate check target runs - a documentation obligation nothing
// enforces is one that quietly lapses.
//
// holdfast skips Dolby Vision and HDR10+ sources, and the skip is right: a generic
// libx265 re-encode strips an RPU or SMPTE2094-40 dynamic metadata, and that loss is
// invisible until somebody watches the file. What the shipped text kept getting wrong is
// the FRAMING. Read as a permanent boundary, the skip tells an operator with a library of
// 4K Dolby Vision remuxes that this tool will never be for them; read as what it is - a
// deferral with a named cost - it tells them why, and what would have to be true first.
//
// So the statement owes three things and this checks all three, because any two of them
// without the third is a different claim:
//
//   - deferred rather than permanent, or the reader takes it as settled;
//   - an external RPU toolchain beside the bundled ffmpeg, or the reader is not told
//     what lifting it would take;
//   - and the verification gap, or the reader is left thinking the toolchain is the
//     whole of it. The gate compares pixels, an RPU is not pixels, and a tool that
//     deletes the source once a replacement passes cannot ship a step it did not check.
//
// The check is PRESENCE and a token per clause, like the multi-clause statements this
// package has carried before. Whether the prose is well written is not a question any
// mechanical check can answer, and one that pretended to would either fail good
// documentation or pass bad.
const (
	// DynamicHDRDocFile is where the statement lives today. It is named so a failure
	// points somewhere, and the check is not narrowed to it: the statement is satisfied
	// by the anchor wherever in the shipped corpus it appears.
	DynamicHDRDocFile = "README.md"

	// AnchorDynamicHDR introduces the statement. The anchor is a FIXED constant, because
	// a check free to pick its own anchor per run is a check that can be made to pass by
	// moving the goalposts.
	AnchorDynamicHDR = "dynamic-hdr-deferred"
)

// Clause is one of the things an anchored statement must say, and the case-insensitive
// token that carries it. The token is what is CHECKED; the clause is what a failure
// message says was missing, so a reader learns what to write rather than which string to
// paste.
type Clause struct {
	Token  string
	Clause string
}

// DynamicHDRClauses is the whole obligation. Each token is the shortest string that
// carries its clause and could not plausibly be written by accident while meaning
// something else - a common English word would let the check pass on prose that never
// says the thing, which is the failure mode a documentation check has.
var DynamicHDRClauses = []Clause{
	{
		Token:  "deferred, not permanent",
		Clause: "the Dolby Vision and HDR10+ skip is DEFERRED rather than permanent",
	},
	{
		Token: "external RPU toolchain",
		Clause: "lifting the skip needs an external RPU toolchain beside the bundled " +
			"ffmpeg, to extract the dynamic metadata and reinject it",
	},
	{
		Token: "an RPU is not pixels",
		Clause: "it stays deferred because the gate compares pixels, an RPU is not " +
			"pixels, and holdfast will not delete a source on the strength of a step it " +
			"did not check",
	},
}

// anchorRe matches an HTML anchor of the form <a id="..."></a> or <a name="...">, which is
// how a Markdown document that has to render on GitHub declares one.
var anchorRe = regexp.MustCompile(`(?i)<a\s+(?:id|name)\s*=\s*"([^"]+)"`)

// headingRe matches an ATX Markdown heading.
var headingRe = regexp.MustCompile(`^\s{0,3}#{1,6}\s`)

// Statement is what one anchor introduced, gathered from one file.
type Statement struct {
	Anchor string
	File   string
	// Text is every line after the anchor, up to the next anchor or heading, that is
	// itself neither a heading nor an anchor.
	Text string
}

// Present reports whether the anchor introduced any real text at all.
func (s Statement) Present() bool { return strings.TrimSpace(s.Text) != "" }

// CheckDynamicHDR applies the whole rule to a corpus: some shipped document introduces the
// statement under the fixed anchor, that occurrence has text, and ONE occurrence carries a
// token for every clause it owes.
//
// An anchor appearing in more than one file is not an error: the check wants the statement
// to EXIST in what the repository ships, so ANY occurrence satisfying the rule satisfies
// it, and a problem is reported only when NONE does. The message then names the strongest
// near-miss, so a failure points at a file to edit rather than saying "nowhere".
//
// Each way of failing is reported as its OWN named failure, because they are different
// repairs: an anchor nobody wrote, an anchor with nothing under it, a statement missing a
// clause, and three clauses that exist but in two documents rather than one.
func CheckDynamicHDR(files []string) error {
	if len(files) == 0 {
		return fmt.Errorf("docscheck: no documents to search - a check with no corpus passes everything")
	}
	sts, err := statements(files, AnchorDynamicHDR)
	if err != nil {
		return err
	}
	if len(sts) == 0 {
		return fmt.Errorf("no shipped document carries the anchor %q, so the dynamic-HDR statement is MISSING: "+
			"Dolby Vision and HDR10+ sources are skipped by a tool that deletes a source once a replacement "+
			"passes, and nothing the repository ships now says that skip is deferred, what lifting it needs, or "+
			"what keeps it deferred (it lived in %s)", AnchorDynamicHDR, DynamicHDRDocFile)
	}

	var present []Statement
	for _, s := range sts {
		if s.Present() {
			present = append(present, s)
		}
	}
	if len(present) == 0 {
		return fmt.Errorf("%s: the anchor %q is present but no line of text follows it before the next anchor or "+
			"heading - an anchor with nothing under it is not a statement, so the dynamic-HDR statement is MISSING",
			sts[0].File, AnchorDynamicHDR)
	}

	for _, s := range present {
		if len(missingClauses(s, DynamicHDRClauses)) == 0 {
			return nil
		}
	}

	// Nothing carries all three. Before naming a near-miss, say so when the clauses exist
	// but are SPLIT: that is a different defect with a different repair, and a reader
	// weighing the skip has to find all three in one place rather than assemble them.
	if len(present) > 1 && carriedBetweenThem(present, DynamicHDRClauses) {
		var where []string
		for _, s := range present {
			var carries []string
			for _, c := range DynamicHDRClauses {
				if carriesClause(s, c) {
					carries = append(carries, fmt.Sprintf("%q", c.Token))
				}
			}
			where = append(where, fmt.Sprintf("%s carries %s", s.File, strings.Join(carries, " and ")))
		}
		return fmt.Errorf("the dynamic-HDR statement is SPLIT across %d documents and no single statement carries "+
			"all %d clauses (%s): a reader weighing the skip has to find all of them in one place, so the clauses "+
			"belong in one statement under %q",
			len(present), len(DynamicHDRClauses), strings.Join(where, "; "), AnchorDynamicHDR)
	}

	best := bestStatement(present, DynamicHDRClauses)
	var says []string
	for _, c := range missingClauses(best, DynamicHDRClauses) {
		says = append(says, fmt.Sprintf("never says %q, so the statement that %s is MISSING", c.Token, c.Clause))
	}
	return fmt.Errorf("%s comes closest of the %d document(s) carrying %q, and it %s",
		best.File, len(present), AnchorDynamicHDR, strings.Join(says, "; it "))
}

// carriesClause reports whether a statement carries one clause's token, case-insensitively
// and over whitespace-collapsed text.
func carriesClause(s Statement, c Clause) bool {
	return strings.Contains(normalize(s.Text), normalize(c.Token))
}

// missingClauses returns the clauses this statement does not carry, in table order.
func missingClauses(s Statement, clauses []Clause) []Clause {
	var out []Clause
	for _, c := range clauses {
		if !carriesClause(s, c) {
			out = append(out, c)
		}
	}
	return out
}

// carriedBetweenThem reports whether every clause is carried by SOME statement while none
// carries them all - the clauses are written down, just not together.
func carriedBetweenThem(sts []Statement, clauses []Clause) bool {
	for _, c := range clauses {
		found := false
		for _, s := range sts {
			if carriesClause(s, c) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// bestStatement returns the statement carrying the most of these clauses. One carrying all
// of them makes the check pass; when none does, this is the one closest to being the
// statement that was owed, and therefore the file a failure should name.
func bestStatement(sts []Statement, clauses []Clause) Statement {
	best, bestScore := sts[0], -1
	for _, s := range sts {
		score := len(clauses) - len(missingClauses(s, clauses))
		if score > bestScore {
			best, bestScore = s, score
		}
	}
	return best
}

// statements returns every occurrence of an anchor across the corpus, in corpus order.
func statements(files []string, anchor string) ([]Statement, error) {
	var out []Statement
	for _, f := range files {
		st, err := findStatement(f, anchor)
		if err != nil {
			return nil, err
		}
		if st.File == "" {
			continue
		}
		out = append(out, st)
	}
	return out, nil
}

// findStatement scans one file for the text an anchor introduces.
//
// The stop conditions are the rule word for word: the text runs to the NEXT ANCHOR or the
// next HEADING, and a line that is itself a heading or an anchor is not the statement's
// text. Two anchors back to back therefore introduce nothing, which is exactly one of the
// failures this has to catch.
func findStatement(path, anchor string) (Statement, error) {
	f, err := os.Open(path)
	if err != nil {
		return Statement{}, fmt.Errorf("docscheck: read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	st := Statement{Anchor: anchor, File: path}
	var b strings.Builder
	found := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		anchors := anchorRe.FindStringSubmatch(line)
		isHeading := headingRe.MatchString(line)

		if !found {
			if len(anchors) == 2 && anchors[1] == anchor {
				found = true
			}
			continue
		}
		if len(anchors) == 2 || isHeading {
			break // the next anchor or heading ends the statement
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if err := sc.Err(); err != nil {
		return Statement{}, fmt.Errorf("docscheck: read %s: %w", path, err)
	}
	if !found {
		return Statement{Anchor: anchor, File: ""}, nil
	}
	st.Text = b.String()
	return st, nil
}

// normalize lower-cases a statement and collapses every run of whitespace to one space
// before a token is looked for. That is not a loosening, it is what "the text carries this
// token" MEANS in Markdown: a single newline inside a paragraph is a space, so where the
// author's line wrap happens to fall would otherwise decide whether the documentation
// passes - and re-flowing a paragraph is not a change to what it says.
func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
