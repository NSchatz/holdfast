package startup

import (
	"fmt"
	"os"
	"strings"
)

// The shipped documentation's statement about STREAM SELECTION, checked mechanically over
// the shipped text by a test the aggregate check target runs - like the cost statement, the
// local set, the scratch statement and the path filters beside it, and for the same reason:
// a documentation obligation nothing enforces is one that quietly lapses.
//
// These four keys decide which of a source's streams survive, on a tool that DELETES the
// source once the replacement passes its gates. Every part checked below is one an operator
// who misreads it loses something by, and the losses are not symmetrical:
//
//   - a language list believed to be additive, or believed to apply to a type it does not,
//     drops tracks that are not coming back;
//   - the untagged rule is what keeps a main audio track with no language tag; an operator
//     who believes it is dropped will write a list that keeps a track they already had;
//   - the never-a-silent-file fallback means a list CAN be silently not applied, and a
//     reader who does not know that cannot explain why one file kept every track;
//   - remux-only declines the perceptual gate, which is the single biggest thing to know
//     about the mode and the thing an operator must be told is paid for by the identity
//     check;
//   - and audio transcoding is a NON-GOAL, so a reader must not go looking for a downmix
//     key that does not exist and will not be added by this feature.
const (
	// StreamSelectionDocFile is where the statement lives today. It is named so a failure
	// points somewhere, and the check is not narrowed to it: the statement is satisfied by
	// the anchor wherever in the shipped corpus it appears.
	StreamSelectionDocFile = "docs/profiles.md"

	// AnchorStreamSelection introduces the statement. The anchor is a FIXED constant,
	// because a check free to pick its own anchor is a check that can be made to pass by
	// moving the goalposts.
	AnchorStreamSelection = "stream-selection"
)

// streamSelectionParts are the things the statement must SAY. Each is checked by phrases a
// heading, an anchor or a promise to document it later cannot satisfy.
var streamSelectionParts = []struct {
	Name  string
	Needs []string
}{
	{
		Name: "which four keys these are, and what each of them defaults to",
		Needs: []string{
			"audio_languages", "subtitle_languages", "keep_commentary", "remux_only",
			"both language lists default to empty", "keep_commentary defaults to true",
			"remux_only defaults to false",
		},
	},
	{
		Name:  "that a stream with no language tag, an empty one, or the undefined code und is KEPT whatever a list says",
		Needs: []string{"no language tag", "und", "is kept whatever"},
	},
	{
		Name:  "that a selection which would leave the file with no audio is not applied to audio at all, and that the row says so",
		Needs: []string{"no audio at all", "every audio stream is carried", "recorded on that file's row"},
	},
	{
		Name:  "that remux_only skips the VMAF gate, and only after every carried video stream is established identical to the source's",
		Needs: []string{"skips the vmaf gate", "identical to the source", "rejected and the source is kept"},
	},
	{
		Name:  "that audio TRANSCODING is a non-goal: these keys select and copy, and none of them re-encodes, downmixes or adds a track",
		Needs: []string{"non-goal", "selection and copy", "downmix"},
	},
}

// CheckStreamSelectionStatement reports whether one shipped document states what the
// stream-selection keys do, in every one of their parts.
func CheckStreamSelectionStatement(doc string) error {
	body, ok := anchoredSection(doc, AnchorStreamSelection)
	if !ok {
		return fmt.Errorf("%s: the anchor %q is missing", StreamSelectionDocFile, AnchorStreamSelection)
	}
	if strings.TrimSpace(stripAnchors(body)) == "" {
		return fmt.Errorf("%s: the section under %q is empty; an anchor is not a statement",
			StreamSelectionDocFile, AnchorStreamSelection)
	}
	flat := normalise(body)
	for _, part := range streamSelectionParts {
		for _, need := range part.Needs {
			if !strings.Contains(flat, normalise(need)) {
				return fmt.Errorf("%s: the stream-selection statement does not say %s (missing %q)",
					StreamSelectionDocFile, part.Name, need)
			}
		}
	}
	return nil
}

// CheckStreamSelectionInCorpus reports whether the SHIPPED CORPUS carries the statement:
// some document introduces it under the fixed anchor, and that document says every part.
//
// It is a corpus walk rather than a read of one named file, and that is the half a
// file-scoped check cannot do: a statement can be satisfied by moving to a different
// document, and it can be lost by deleting the document it was in. Both are the same
// question - does the corpus this build ships carry the anchor - and only the walk answers
// it.
func CheckStreamSelectionInCorpus(files []string) error {
	var carrying []string
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		if _, ok := anchoredSection(string(b), AnchorStreamSelection); ok {
			carrying = append(carrying, path)
		}
	}
	if len(carrying) == 0 {
		return fmt.Errorf("no shipped document carries the anchor %q: the stream-selection keys decide "+
			"which of a source's streams survive on a tool that deletes the source, and nothing in the "+
			"corpus introduces the statement that says what they do (it lived in %s)",
			AnchorStreamSelection, StreamSelectionDocFile)
	}
	for _, path := range carrying {
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		if err := CheckStreamSelectionStatement(string(b)); err != nil {
			return fmt.Errorf("%s carries the anchor and %w", path, err)
		}
	}
	return nil
}
