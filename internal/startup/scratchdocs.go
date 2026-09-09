package startup

import (
	"fmt"
	"os"
	"strings"

	"github.com/NSchatz/holdfast/internal/docscheck"
)

// The shipped documentation's statement about the configurable working location,
// checked mechanically over the shipped text by a test the aggregate check target
// runs - like the cost statement and the local set beside it, and for the same
// reason: a documentation obligation nothing enforces is one that quietly lapses.
//
// This one is checked HARDER than presence, because the failure mode is not a
// missing statement, it is a FALSE one. The reason an operator reaches for a
// scratch location is almost always "the life of a hard drive is shortened by lots
// of IO on it", and that reason does not survive the arithmetic: the source is read
// once and the output written once whether or not there is a scratch directory, so
// the bytes the source's drive handles do not change. What DOES change is real and
// is worth having - the four benefits below - and what it costs is real too: the
// scratch device pays a full write-plus-read cycle it would not otherwise pay,
// which on an SSD is finite write endurance spent at video-file sizes.
//
// So the check has two halves. The shipped text must SAY the four benefits and the
// cost, and NO document in the repository may CLAIM the thing that is not true. The
// second half is why this cannot be a presence check: a document could carry a
// perfect benefits section and, three paragraphs later, tell a reader this setting
// spares their array's drives.
const (
	// ScratchDocFile is where the statement lives today. It is named so a failure
	// points somewhere, and the check is not narrowed to it: ScratchStatement is
	// satisfied by the anchor wherever in the corpus it appears, exactly as
	// internal/docscheck's obligations are.
	ScratchDocFile = "docs/scratch.md"

	// AnchorScratchBenefit introduces the statement. It is a fixed anchor for the
	// reason docscheck's are fixed: a check free to pick its own anchor is a check
	// that can be made to pass by moving the goalposts.
	AnchorScratchBenefit = "scratch-benefit"

	// ScratchClaimAllow is the marker a line must carry to quote a forbidden claim
	// in order to PROHIBIT it. The repository already uses this shape for the
	// rename guard, and for the same reason: an exemption that is a marker is
	// greppable, while an exemption that is a file-level exclusion hides the next
	// real leak.
	ScratchClaimAllow = "scratch-claim-allow"
)

// scratchBenefitParts are the things the statement must SAY. Each is checked by a
// phrase that a heading, an anchor or a promise to document it later cannot
// satisfy.
var scratchBenefitParts = []struct {
	Name  string
	Needs []string
}{
	{
		Name:  "that a spinning disk is spared the seek thrash of interleaving the read and the write",
		Needs: []string{"seek thrash", "spinning disk"},
	},
	{
		Name:  "that a parity array is spared the read-modify-write churn of a whole encode's worth of dribbling writes",
		Needs: []string{"parity", "read-modify-write", "unraid", "snapraid", "raidz"},
	},
	{
		Name:  "that a failed or aborted transcode never reaches the array at all",
		Needs: []string{"failed or aborted", "off the array"},
	},
	{
		Name:  "that a network share sees one streamed copy rather than an encoder's write pattern",
		Needs: []string{"nfs", "smb", "write pattern"},
	},
	{
		Name:  "that the scratch device pays a full write-plus-read cycle, spending SSD write endurance at video-file sizes",
		Needs: []string{"write-plus-read cycle", "write endurance", "video-file sizes"},
	},
}

// forbiddenScratchClaims are the claims no shipped document may make, in normalised
// form. They are the OVERSTATEMENT this feature attracts, and each is false: a
// scratch location does not change how many bytes the source's drive handles, so it
// cannot reduce them and cannot lengthen that drive's life.
//
// They are matched as phrases rather than as a sentiment, which bounds what this
// can catch and is stated rather than glossed: a document could still say the
// untrue thing in words nobody listed. What the list does catch is every
// respelling of the claim anyone has actually reached for, and the shipped text is
// written so that the true statement needs no allowance from it.
var forbiddenScratchClaims = []string{
	"reduces the bytes written to the source",
	"reduce the bytes written to the source",
	"reduces the number of bytes written to the source",
	"fewer bytes written to the source",
	"fewer bytes are written to the source",
	"less data written to the source",
	"reduces wear on the source",
	"reduce wear on the source",
	"less wear on the source",
	"saves wear on the source",
	"reduces io on the source",
	"reduces i/o on the source",
	"extends the life of the source",
	"extend the life of the source",
	"lengthens the life of the source",
	"lengthen the life of the source",
	"prolongs the life of the source",
	"prolong the life of the source",
	"lengthens the source drive",
	"extends the source drive",
}

// CheckScratchStatement reports whether one shipped document states what the
// scratch location buys and what it costs, in all five of its parts.
func CheckScratchStatement(doc string) error {
	body, ok := anchoredSection(doc, AnchorScratchBenefit)
	if !ok {
		return fmt.Errorf("%s: the anchor %q is missing", ScratchDocFile, AnchorScratchBenefit)
	}
	if strings.TrimSpace(stripAnchors(body)) == "" {
		return fmt.Errorf("%s: the section under %q is empty; an anchor is not a statement", ScratchDocFile, AnchorScratchBenefit)
	}
	flat := normalise(body)
	for _, part := range scratchBenefitParts {
		for _, need := range part.Needs {
			if !strings.Contains(flat, normalise(need)) {
				return fmt.Errorf("%s: the scratch statement does not say %s (missing %q)", ScratchDocFile, part.Name, need)
			}
		}
	}
	return nil
}

// CheckNoSourceDriveClaim reports every line in the corpus that CLAIMS a scratch
// location reduces the bytes written to the source drive or lengthens that drive's
// life. A line carrying ScratchClaimAllow is exempt, which is how the shipped text
// is able to quote a claim in order to refuse it.
//
// It reports EVERY offending line rather than the first, so a document that drifted
// in three places is fixed in one pass.
func CheckNoSourceDriveClaim(files []string) ([]string, error) {
	var found []string
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, ScratchClaimAllow) {
				continue
			}
			flat := normalise(line)
			for _, claim := range forbiddenScratchClaims {
				if strings.Contains(flat, claim) {
					found = append(found, fmt.Sprintf("%s:%d: %q - a scratch location does not change how many bytes "+
						"the source's drive handles (it is read once and written once either way); mark the line with %q if it is quoting the claim in order to refuse it",
						path, i+1, strings.TrimSpace(line), ScratchClaimAllow))
					break
				}
			}
		}
	}
	return found, nil
}

// ScratchCorpus is the set CheckNoSourceDriveClaim is run over: every Markdown file
// in the repository, which is what the repository ships as documentation. It is a
// WALK and not a list, so a claim moved into a document that did not exist when
// this was written is still caught.
func ScratchCorpus(dir string) ([]string, error) {
	root, err := docscheck.RepoRoot(dir)
	if err != nil {
		return nil, err
	}
	return docscheck.Corpus(root)
}

// anchoredSection returns the text under an HTML anchor, up to the next anchor or
// the next heading. The scratch statement is keyed to an ANCHOR rather than to a
// heading because a heading is prose an editor rewords, and the identifier in a
// refusal message has to keep pointing at the same paragraph.
func anchoredSection(doc, anchor string) (string, bool) {
	lines := strings.Split(doc, "\n")
	start := -1
	for i, l := range lines {
		if strings.Contains(l, `id="`+anchor+`"`) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", false
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "## ") || strings.HasPrefix(t, "# ") || strings.Contains(t, `<a id="`) {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n"), true
}
