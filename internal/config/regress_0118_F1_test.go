package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped example configuration against the build it is an example OF.
//
// config.example.yaml is a document an operator reads and copies before the first file
// goes, and it is the file the resolution ceiling is written IN. It still carries the flat
// downscaling non-goal this key retired, in the very block where a rule's `max_height`
// belongs, and it still enumerates the rule knobs as three when the build admits four.
//
// This is the failure the anchored-statement rule exists to prevent: two copies of one
// posture, one of which stopped being true silently. The Markdown check cannot see it -
// docscheck.Corpus reads Markdown only - so the second copy is the one left standing.

// TestRegress0118F1_TheShippedExampleDoesNotRestateTheRetiredDownscalingNonGoal grades
// [AC-9] over the shipped example configuration, and [AC-8]'s second half beside it.
//
// [AC-9]: "THE downscaling non-goal SHALL be stated in exactly one document in this
// repository, and the mechanical documentation check SHALL fail if a second document
// restates it or if the single statement stops matching the shipped default."
//
// [AC-8]: "no unqualified same-content claim SHALL remain beside the key that can
// contradict it." The sentence below sits inside the `rules` block, which is one of the
// three places an operator writes `max_height`.
//
// MUTATION: delete the retired sentence from config.example.yaml and the first arm greens;
// name every knob RuleKnobs() admits in the example's own enumeration and the second does.
func TestRegress0118F1_TheShippedExampleDoesNotRestateTheRetiredDownscalingNonGoal(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("reading the shipped example: %v", err)
	}
	example := string(b)

	// The retired claim, quoted as it stands. It is a statement of the downscaling
	// non-goal in a second document, and it is now FALSE of this build: max_height is a
	// key this very file's `rules` block accepts.
	const retired = "nothing here downscales anything"
	if strings.Contains(example, retired) {
		t.Errorf("config.example.yaml still says %q. That is the downscaling non-goal stated in a "+
			"SECOND document, and it contradicts the build: a root or a rule may set max_height, and "+
			"a replacement made under it is a smaller picture than the source the swap then deletes. "+
			"The posture is anchored in README.md under %q and belongs nowhere else",
			retired, "downscaling-posture")
	}

	// And the enumeration of what a rule may override, derived from the build rather than
	// written down here: an example that names three of four knobs tells an operator the
	// ceiling cannot be written per band, which is the whole reason it is rule-overridable.
	for _, knob := range RuleKnobs() {
		if !strings.Contains(example, knob) {
			t.Errorf("config.example.yaml never names the rule knob %q, so the example describes a "+
				"rule surface this build no longer has: RuleKnobs() = %v", knob, RuleKnobs())
		}
	}
	if strings.Contains(example, "exactly three knobs") {
		t.Errorf("config.example.yaml says a rule may override %q, but this build admits %d: %v",
			"exactly three knobs", len(RuleKnobs()), RuleKnobs())
	}
}
