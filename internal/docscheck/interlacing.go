package docscheck

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/NSchatz/holdfast/internal/corpus"
)

// The shipped documentation's statement about INTERLACING, checked mechanically over the
// corpus by tests the aggregate check target runs - a documentation obligation nothing
// enforces is one that quietly lapses.
//
// holdfast used to say, flatly, that interlaced sources are skipped and not converted. That
// is no longer true: a library root may ask for a deinterlace, and a replacement made under
// that key is NOT the same content as the source. The whole of this file exists because
// three different things then have to hold in the documents a stranger reads before they
// point this tool at a library they cannot re-acquire:
//
//   - the posture is stated ONCE, under a fixed anchor, carrying every clause it owes;
//   - every guard token the engine can record has a row an operator can look it up in,
//     read from the ENGINE's own vocabulary rather than from a list somebody remembered
//     to extend;
//   - and the retired claim is GONE, which is an absence and therefore the one thing a
//     presence check can never establish.

const (
	// InterlacingDocFile is where the statement lives today. It is named so a failure
	// points somewhere, and the check is not narrowed to it: the statement is satisfied by
	// the anchor wherever in the shipped corpus it appears.
	InterlacingDocFile = "README.md"

	// AnchorInterlacing introduces the posture statement, and AnchorSkipGuards introduces
	// the guard table. Both are FIXED constants, because a check free to pick its own
	// anchor per run is a check that can be made to pass by moving the goalposts.
	AnchorInterlacing = "interlacing-posture"
	AnchorSkipGuards  = "skip-guards"
)

// InterlacingClauses is the whole obligation the posture statement carries. Each token is
// the shortest string that carries its clause and could not plausibly be written by
// accident while meaning something else.
//
// The four are not decoration and none of them implies another. "Deinterlaced on request"
// without "off by default" reads as a behaviour change to every existing install; either
// without "frame-rate-preserving" leaves a reader expecting the field-doubling a
// deinterlacer usually offers; and all three without the telecine clause invite an operator
// to point it at the 3:2 pulldown content it refuses, and to read the refusal as a defect.
var InterlacingClauses = []Clause{
	{
		Token:  "deinterlaced on request",
		Clause: "an interlaced source is deinterlaced only where a library root asks for it",
	},
	{
		Token:  "off by default",
		Clause: "the key is off by default, so an interlaced source is skipped unless it is set",
	},
	{
		Token: "frame-rate-preserving",
		Clause: "the deinterlace is frame-rate-preserving only - a mode that emits one frame " +
			"per field is refused, because it changes the frame count two parity gates are graded on",
	},
	{
		Token: "telecined sources are skipped",
		Clause: "telecined sources are skipped under their own guard whatever the key says, " +
			"because deinterlacing a pulldown pattern is the wrong operation and no perceptual " +
			"metric flags the judder it leaves",
	},
}

// RetiredInterlacingNonGoal is the claim this repository used to make and no longer may:
// that interlaced sources are skipped and not converted.
//
// It is expressed as the tokens that must not appear in ONE SENTENCE together, rather than
// as a literal sentence, because the claim survived in two spellings ("interlaced sources
// are skipped, not converted" and a list naming interlaced among others) and would survive
// a third the moment a paragraph was re-wrapped. What is retired is the CLAIM, so the check
// is written against the claim.
//
// The same sentence about exotic-chroma or multi-video-stream sources is untouched and must
// stay: those really are skipped and not converted. Only the sentence that says it about
// INTERLACED content is retired.
var RetiredInterlacingNonGoal = RetiredClaim{
	Tokens: []string{"interlaced", "skipped, not converted"},
	Claim:  `interlaced sources are "skipped, not converted"`,
	Why: "a library root can ask for a deinterlace now, and a replacement made under that key " +
		"is NOT the same content as the source. A document that still says interlaced sources are " +
		"never converted tells a reader the opposite of what this build does",
}

// RetiredClaim is a statement the documentation must NO LONGER make: the tokens that
// identify it, what it claimed, and why it is wrong now.
type RetiredClaim struct {
	// Tokens all appear in one sentence of the retired claim, case-insensitively.
	Tokens []string
	// Claim is what the sentence said, for the failure message.
	Claim string
	// Why is what makes it wrong now, so a reader learns the reason rather than the string.
	Why string
}

// Corpus returns every Markdown file this repository ships.
//
// It is here so a check over the whole corpus has one way to get its file set, and so an
// ABSENCE check in particular cannot be narrowed by accident: a retired claim that survives
// in a document nobody thought to list is exactly the failure such a check exists to catch,
// and a caller that passed its own list would decide the answer by deciding the list.
func Corpus() ([]string, error) {
	root, err := corpus.RepoRoot(".")
	if err != nil {
		return nil, fmt.Errorf("docscheck: locate the repository root: %w", err)
	}
	files, err := corpus.Markdown(root)
	if err != nil {
		return nil, fmt.Errorf("docscheck: list the shipped Markdown: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("docscheck: the repository ships no Markdown - a check with no corpus " +
			"passes everything")
	}
	return files, nil
}

// CheckInterlacing applies the anchored-statement rule to the interlacing posture: some
// shipped document introduces it under the fixed anchor, that occurrence has text, and ONE
// occurrence carries a token for every clause it owes.
//
// It is CheckDynamicHDR's rule over a different anchor and a different clause set, and it
// reports the same four distinct failures for the same reason: an anchor nobody wrote, an
// anchor with nothing under it, a statement missing a clause, and clauses that exist but in
// two documents rather than one are four different repairs.
func CheckInterlacing(files []string) error {
	if len(files) == 0 {
		return fmt.Errorf("docscheck: no documents to search - a check with no corpus passes everything")
	}
	sts, err := statements(files, AnchorInterlacing)
	if err != nil {
		return err
	}
	if len(sts) == 0 {
		return fmt.Errorf("no shipped document carries the anchor %q, so the interlacing statement is "+
			"MISSING: this build can deinterlace a source and delete the original, and nothing the "+
			"repository ships says when it does that, that it is off by default, that it preserves the "+
			"frame rate, or that telecined content is skipped (it belongs in %s)",
			AnchorInterlacing, InterlacingDocFile)
	}

	var present []Statement
	for _, s := range sts {
		if s.Present() {
			present = append(present, s)
		}
	}
	if len(present) == 0 {
		return fmt.Errorf("%s: the anchor %q is present but no line of text follows it before the next "+
			"anchor or heading - an anchor with nothing under it is not a statement, so the interlacing "+
			"statement is MISSING", sts[0].File, AnchorInterlacing)
	}

	for _, s := range present {
		if len(missingClauses(s, InterlacingClauses)) == 0 {
			return nil
		}
	}

	if len(present) > 1 && carriedBetweenThem(present, InterlacingClauses) {
		var where []string
		for _, s := range present {
			var carries []string
			for _, c := range InterlacingClauses {
				if carriesClause(s, c) {
					carries = append(carries, fmt.Sprintf("%q", c.Token))
				}
			}
			where = append(where, fmt.Sprintf("%s carries %s", s.File, strings.Join(carries, " and ")))
		}
		return fmt.Errorf("the interlacing statement is SPLIT across %d documents and no single statement "+
			"carries all %d clauses (%s): a reader deciding whether to turn the key on has to find all of "+
			"them in one place, so the clauses belong in one statement under %q",
			len(present), len(InterlacingClauses), strings.Join(where, "; "), AnchorInterlacing)
	}

	best := bestStatement(present, InterlacingClauses)
	var says []string
	for _, c := range missingClauses(best, InterlacingClauses) {
		says = append(says, fmt.Sprintf("never says %q, so the statement that %s is MISSING", c.Token, c.Clause))
	}
	return fmt.Errorf("%s comes closest of the %d document(s) carrying %q, and it %s",
		best.File, len(present), AnchorInterlacing, strings.Join(says, "; it "))
}

// tableRowRe matches a Markdown table row whose first cell is a backticked token, which is
// how every guard row in the shipped table names its guard.
var tableRowRe = regexp.MustCompile("^\\s*\\|\\s*`([^`]+)`\\s*\\|")

// CheckGuardTable reports the tokens that have NO row in the guard table the corpus ships
// under the fixed anchor.
//
// tokens is the ENGINE's own vocabulary, read at runtime by the caller and passed in - this
// package must not carry a copy. A grader with the token list inlined passes on the day a
// third token ships undocumented, which is precisely the failure it exists to catch: a skip
// token reaches an operator through a stored row, an API payload and a metrics label, and a
// token nobody can look up is a verdict with no explanation attached.
func CheckGuardTable(tokens, files []string) error {
	if len(files) == 0 {
		return fmt.Errorf("docscheck: no documents to search - a check with no corpus passes everything")
	}
	if len(tokens) == 0 {
		return fmt.Errorf("docscheck: no tokens to look for - a guard table check with an empty " +
			"vocabulary passes every table, including an empty one")
	}
	documented, file, err := guardTableTokens(files)
	if err != nil {
		return err
	}
	if file == "" {
		return fmt.Errorf("no shipped document carries the anchor %q, so there is no guard table: a skip "+
			"token reaches an operator on a stored row and a metrics label, and one nobody can look up is "+
			"a verdict with no explanation (it belongs in %s)", AnchorSkipGuards, InterlacingDocFile)
	}
	if len(documented) == 0 {
		return fmt.Errorf("%s: the anchor %q introduces no table row naming a guard - an anchor with no "+
			"table under it documents nothing", file, AnchorSkipGuards)
	}

	var missing []string
	for _, tok := range tokens {
		if !documented[tok] {
			missing = append(missing, tok)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return fmt.Errorf("%s: the guard table has no row for %v. Every token in the engine's closed skip "+
			"vocabulary reaches an operator through a stored row, the API payload and a /metrics label, so "+
			"a token with no row is a verdict they cannot look up. The table documents %d of %d",
			file, missing, len(tokens)-len(missing), len(tokens))
	}
	return nil
}

// guardTableTokens reads the first cell of every table row under the guard-table anchor,
// and the file that anchor was found in ("" when no document carries it).
//
// The table ends where the statement rules in this package end: at the next anchor or the
// next heading. A row outside it is not the guard table, which is what keeps this from
// passing on a backticked token in some unrelated table elsewhere in the document.
func guardTableTokens(files []string) (map[string]bool, string, error) {
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, "", fmt.Errorf("docscheck: read %s: %w", f, err)
		}
		lines := strings.Split(string(b), "\n")
		start := -1
		for i, line := range lines {
			if m := anchorRe.FindStringSubmatch(line); len(m) == 2 && m[1] == AnchorSkipGuards {
				start = i + 1
				break
			}
		}
		if start < 0 {
			continue
		}
		found := map[string]bool{}
		for _, line := range lines[start:] {
			if m := anchorRe.FindStringSubmatch(line); len(m) == 2 {
				break
			}
			if headingRe.MatchString(line) {
				break
			}
			if m := tableRowRe.FindStringSubmatch(line); len(m) == 2 {
				found[strings.TrimSpace(m[1])] = true
			}
		}
		return found, f, nil
	}
	return nil, "", nil
}

// sentenceSplit breaks normalised text into sentences. It is deliberately crude - a full
// stop, a question mark or an exclamation mark ends a sentence - because the question it
// serves is "do these tokens still appear together in one claim", and a sentence is the
// smallest unit that answers it without being defeated by a line wrap.
var sentenceSplit = regexp.MustCompile(`[.!?]+`)

// CheckRetired reports whether a retired claim survives anywhere in the corpus.
//
// It is an ABSENCE check, which is the whole reason it exists: the presence check above
// cannot see a contradiction standing next to the statement it passes on, and the rename
// guard in scripts/check-pins.sh matches IDENTIFIERS and says in as many words that prose
// is not matched. Nothing else in this repository grades a sentence for being gone.
//
// The match is over NORMALISED text - lower-cased, whitespace collapsed, Markdown emphasis
// and code marks removed - and it is scoped to a SENTENCE, so the tokens have to still be
// making one claim together rather than merely appearing in one document.
func CheckRetired(r RetiredClaim, files []string) error {
	if len(files) == 0 {
		return fmt.Errorf("docscheck: no documents to search - a check with no corpus passes everything")
	}
	if len(r.Tokens) == 0 {
		return fmt.Errorf("docscheck: a retired claim with no tokens matches nothing, so it would " +
			"report every document clean")
	}
	var found []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("docscheck: read %s: %w", f, err)
		}
		for _, sentence := range sentenceSplit.Split(plain(string(b)), -1) {
			if carriesAll(sentence, r.Tokens) {
				found = append(found, fmt.Sprintf("%s: %q", f, strings.TrimSpace(sentence)))
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return fmt.Errorf("the retired claim that %s survives in %d place(s):\n  %s\nIt is retired because %s",
		r.Claim, len(found), strings.Join(found, "\n  "), r.Why)
}

// carriesAll reports whether one normalised sentence carries every token.
func carriesAll(sentence string, tokens []string) bool {
	for _, tok := range tokens {
		if !strings.Contains(sentence, normalize(tok)) {
			return false
		}
	}
	return true
}

// plain normalises a document for a claim search: it is normalize plus the removal of the
// Markdown marks an author may put ANYWHERE inside a phrase. `**interlaced** sources are
// **skipped, not converted**` says exactly what the unmarked sentence says, and a check
// that could be defeated by bolding half of it would be a check about formatting.
func plain(s string) string {
	return normalize(strings.NewReplacer("*", "", "_", "", "`", "", "[", "", "]", "").Replace(s))
}
