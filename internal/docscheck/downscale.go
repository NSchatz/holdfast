package docscheck

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The shipped documentation's statement about RESOLUTION DOWNSCALING, checked mechanically
// over the corpus by tests the aggregate check target runs - a documentation obligation
// nothing enforces is one that quietly lapses.
//
// holdfast used to say, flatly, that it does no resolution downscaling. That is no longer
// true: a library root, or a band inside one, may set `max_height`, and a replacement made
// under that key is a SMALLER PICTURE than the source it replaces. The whole of this file
// exists because two things then have to hold in the documents a stranger reads before they
// point this tool at a library they cannot re-acquire.
//
// # One statement, and it has to match the build
//
// The posture is stated ONCE, under a fixed anchor, carrying every clause it owes - and one
// of those clauses is DERIVED FROM THE SHIPPED DEFAULT rather than written here. A check
// holding its own copy of "off by default" would go on passing on the day the default moved,
// which is the one answer it must never give. The caller reads the default off the build (see
// config.MaxHeightDefault) and hands it in, exactly as CheckGuardTable is handed the engine's
// own skip vocabulary.
//
// # Nowhere else, and that is an ABSENCE
//
// A presence check cannot see a contradiction standing in another document. The old flat
// claim survived in three spellings across three files, and a fourth would have survived a
// re-wrap - so the second check here counts the DOCUMENTS that make a downscaling claim at
// all and refuses more than one. It reads PROSE: a backticked span is an identifier - a
// configuration key, an API field, a guard token - and an identifier is documented wherever
// it is used, so identifiers are removed before the search rather than read as claims.

const (
	// DownscaleDocFile is where the statement lives today. It is named so a failure points
	// somewhere, and the check is not narrowed to it: the statement is satisfied by the
	// anchor wherever in the shipped corpus it appears.
	DownscaleDocFile = "README.md"

	// AnchorDownscale introduces the posture statement. The anchor is a FIXED constant,
	// because a check free to pick its own anchor per run is a check that can be made to
	// pass by moving the goalposts.
	AnchorDownscale = "downscaling-posture"
)

// DownscaleClauses is the obligation the posture statement carries whatever the shipped
// default is. Each token is the shortest string that carries its clause and could not
// plausibly be written by accident while meaning something else.
//
// The three are not decoration and none of them implies another. Naming the key without
// saying what is LOST reads as a quality setting rather than as a one-way trade; saying the
// content changes without the acknowledgement leaves a reader unprepared for the files this
// build then refuses to touch; and either without the scoring direction leaves the obvious
// worry - that a gate comparing two different-sized files is comparing nothing - unanswered,
// on the one page where somebody decides whether to trust this tool with their library.
var DownscaleClauses = []Clause{
	{
		Token:  "max_height",
		Clause: "the statement names the key that turns downscaling on",
	},
	{
		Token: "no longer the same content as the source",
		Clause: "the same-content claim is stated as CONDITIONAL on the key being off - a " +
			"replacement made under it is a smaller picture, and the swap deletes the original",
	},
	{
		Token: "downscale_acknowledged",
		Clause: "the second opt-in is named: with the undo window closed the swap is final, and " +
			"a file that would be scaled is skipped until the operator affirms that separately",
	},
	{
		Token: "scored at the source's resolution",
		Clause: "the perceptual gate scales the OUTPUT back up and scores at the source's " +
			"resolution, rather than grading the encode against a source degraded to meet it",
	},
}

// DownscaleDefaultClause is the clause the statement owes about the SHIPPED DEFAULT,
// derived from the default this build actually loads rather than written down here.
//
// That derivation is the whole of AC-9's second half. With no ceiling shipped the statement
// must say the key is off by default; ship one and the token becomes the height itself, the
// README stops carrying it, and the check reds - which is exactly what "the single statement
// stopped matching the shipped default" means. A constant here could not do that.
func DownscaleDefaultClause(shippedDefault int) Clause {
	if shippedDefault <= 0 {
		return Clause{
			Token: "off by default",
			Clause: "the key is OFF by default, so a replacement carries the source's own " +
				"resolution unless it is set",
		}
	}
	h := strconv.Itoa(shippedDefault)
	return Clause{
		Token: "scaled to " + h + " by default",
		Clause: "this build ships a default ceiling of " + h + " pixels, so the statement has to " +
			"say that every taller source is scaled by default rather than that the key is off",
	}
}

// downscaleObligation is the whole clause set for a given shipped default.
func downscaleObligation(shippedDefault int) []Clause {
	return append(append([]Clause(nil), DownscaleClauses...), DownscaleDefaultClause(shippedDefault))
}

// CheckDownscale applies the anchored-statement rule to the downscaling posture: EXACTLY ONE
// shipped document introduces it under the fixed anchor, that occurrence has text, and it
// carries a token for every clause it owes - including the one derived from shippedDefault.
//
// It differs from CheckInterlacing and CheckDynamicHDR in one way that matters: those are
// satisfied by ANY occurrence carrying every clause, and this refuses a SECOND occurrence
// outright. The downscaling posture is read by another document in this repository's own
// planning and by every operator deciding whether to turn the key on, so two copies are two
// things to keep true and one of them will stop being true silently.
func CheckDownscale(files []string, shippedDefault int) error {
	if len(files) == 0 {
		return fmt.Errorf("docscheck: no documents to search - a check with no corpus passes everything")
	}
	clauses := downscaleObligation(shippedDefault)
	sts, err := statements(files, AnchorDownscale)
	if err != nil {
		return err
	}
	if len(sts) == 0 {
		return fmt.Errorf("no shipped document carries the anchor %q, so the downscaling statement is "+
			"MISSING: this build can scale a replacement down to fewer pixels than its source and then "+
			"delete that source, and nothing the repository ships says when it does that, what the "+
			"default is, what is lost, or how the perceptual gate measures it (it belongs in %s)",
			AnchorDownscale, DownscaleDocFile)
	}

	var present []Statement
	for _, s := range sts {
		if s.Present() {
			present = append(present, s)
		}
	}
	if len(present) == 0 {
		return fmt.Errorf("%s: the anchor %q is present but no line of text follows it before the next "+
			"anchor or heading - an anchor with nothing under it is not a statement, so the downscaling "+
			"statement is MISSING", sts[0].File, AnchorDownscale)
	}
	if len(present) > 1 {
		var where []string
		for _, s := range present {
			where = append(where, s.File)
		}
		return fmt.Errorf("%d shipped documents carry the anchor %q (%s): the downscaling posture is "+
			"stated in exactly ONE document, because two copies are two things to keep true and one of "+
			"them stops being true silently. Keep the statement in %s and link to it from the other",
			len(present), AnchorDownscale, strings.Join(where, ", "), DownscaleDocFile)
	}

	s := present[0]
	missing := missingClauses(s, clauses)
	if len(missing) == 0 {
		return nil
	}
	var says []string
	for _, c := range missing {
		says = append(says, fmt.Sprintf("never says %q, so the statement that %s is MISSING", c.Token, c.Clause))
	}
	return fmt.Errorf("%s carries %q, and it %s", s.File, AnchorDownscale, strings.Join(says, "; it "))
}

// DownscaleClaimTokens identify a sentence that makes a claim ABOUT DOWNSCALING, in any
// direction: that this build does none, that it does some, that it is off by default. They
// are the words a claim is made in rather than the spellings one claim happened to take -
// the old flat non-goal survived in three spellings across three files, and enumerating
// those would have missed the fourth.
//
// A sentence matches when it carries ANY of them. That is deliberately wider than the
// all-tokens rule a RetiredClaim uses: this check is not looking for one sentence, it is
// counting how many documents talk about the subject at all.
var DownscaleClaimTokens = []string{"downscal", "resolution ceiling"}

// codeSpan matches a Markdown code span, whose contents are an IDENTIFIER rather than prose.
// linkTarget matches the destination half of an inline link, which is a path or an anchor
// name - also an identifier, and one that necessarily NAMES the statement it points at. A
// document linking to the posture is deferring to it, which is the opposite of restating it.
var (
	codeSpan   = regexp.MustCompile("`[^`]*`")
	linkTarget = regexp.MustCompile(`\]\([^)]*\)`)
)

// CheckDownscaleStatedOnce reports whether more than one shipped document makes a claim about
// downscaling, and whether the one that does is the one carrying the anchored statement.
//
// It is an ABSENCE check over everything else in the corpus, which is the whole reason it
// exists: CheckDownscale cannot see a contradiction standing in another document, and the
// rename guard in scripts/check-pins.sh matches IDENTIFIERS and says in as many words that
// prose is not matched.
//
// IDENTIFIERS ARE NOT CLAIMS. `downscaled` and `downscale_scaler` are API fields, and
// `downscale-unacknowledged` is a guard token; each is documented wherever it is used, and a
// check that read a field name as a restatement of the posture would force the reference
// documentation to describe the build in words that avoid the build's own vocabulary. So
// every code span is REMOVED before the search rather than unwrapped into the prose.
func CheckDownscaleStatedOnce(files []string) error {
	if len(files) == 0 {
		return fmt.Errorf("docscheck: no documents to search - a check with no corpus passes everything")
	}
	claiming := map[string][]string{}
	var order []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("docscheck: read %s: %w", f, err)
		}
		for _, sentence := range sentenceSplit.Split(prose(string(b)), -1) {
			if !carriesAny(sentence, DownscaleClaimTokens) {
				continue
			}
			if _, seen := claiming[f]; !seen {
				order = append(order, f)
			}
			claiming[f] = append(claiming[f], strings.TrimSpace(sentence))
		}
	}
	if len(order) == 0 {
		return fmt.Errorf("no shipped document says anything about downscaling at all, so the posture "+
			"is UNSTATED: this build can scale a replacement down to fewer pixels than its source and "+
			"then delete that source (it belongs in %s, under %q)", DownscaleDocFile, AnchorDownscale)
	}
	if len(order) > 1 {
		sort.Strings(order)
		var where []string
		for _, f := range order {
			where = append(where, fmt.Sprintf("%s: %q", f, first(claiming[f])))
		}
		return fmt.Errorf("the downscaling posture is stated in %d documents:\n  %s\nIt belongs in "+
			"exactly ONE, under the anchor %q, because two statements are two things to keep true and "+
			"one of them stops being true silently - which is precisely what happened to the flat "+
			"non-goal this key replaced. Keep it in %s and have the others link to it; a document that "+
			"needs to NAME a key or a field may do so in a code span, which is an identifier and not a "+
			"claim", len(order), strings.Join(where, "\n  "), AnchorDownscale, DownscaleDocFile)
	}
	sts, err := statements(files, AnchorDownscale)
	if err != nil {
		return err
	}
	for _, s := range sts {
		if s.File == order[0] {
			return nil
		}
	}
	return fmt.Errorf("%s is the one document making a downscaling claim (%q), but it does not carry "+
		"the anchor %q - so the statement a reader is sent to and the statement that exists are two "+
		"different things", order[0], first(claiming[order[0]]), AnchorDownscale)
}

// RetiredFlatSameContent is the claim this repository used to make and no longer may: that a
// replacement is the same content as its source, full stop.
//
// It is expressed as tokens that must not appear in ONE SENTENCE together, rather than as a
// literal sentence, for the reason RetiredInterlacingNonGoal is: what is retired is the
// CLAIM, and a claim survives a re-wrap.
//
// The QUALIFIED form is untouched and must stay: "the replacement is no longer the same
// content as the source" is what the deinterlacing and downscaling statements both say about
// the keys that make it true, and it is the opposite claim.
var RetiredFlatSameContent = RetiredClaim{
	Tokens: []string{"codec-only", "same-content"},
	Claim:  `holdfast performs "codec-only, same-content re-encoding"`,
	Why: "a library root can set max_height now, and a replacement made under that key is a " +
		"SMALLER PICTURE than the source it replaces. A document that still makes the same-content " +
		"claim unconditionally tells a reader the opposite of what this build can do, beside the " +
		"very key that contradicts it",
}

// carriesAny reports whether one normalised sentence carries ANY of these tokens.
func carriesAny(sentence string, tokens []string) bool {
	for _, tok := range tokens {
		if strings.Contains(sentence, normalize(tok)) {
			return true
		}
	}
	return false
}

// prose is normalize with Markdown CODE SPANS and LINK TARGETS removed and the remaining
// emphasis marks stripped: what is left is what the document ASSERTS, with the identifiers it
// merely names taken out. See CheckDownscaleStatedOnce for why that distinction is the check.
func prose(s string) string {
	return plain(linkTarget.ReplaceAllString(codeSpan.ReplaceAllString(s, " "), "] "))
}

// first returns the first of a document's matching sentences, for a message that has to point
// at a line rather than at a file.
func first(sentences []string) string {
	if len(sentences) == 0 {
		return ""
	}
	return sentences[0]
}
