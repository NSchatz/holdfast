package engine

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/NSchatz/holdfast/internal/corpus"
)

// mutableSkipGuards are the only tokens SkipGuards is documented as omitting: the ones
// cleared and re-derived on every pass, where a requeue would have nothing to re-open.
// They are named here as the EXEMPTION, so that adding a token to this list is a visible
// decision rather than a silent one.
var mutableSkipGuards = map[string]bool{
	"SkipHardlinked":          true,
	"SkipUndoRetentionFailed": true,
	// The operator's own withholding. It is cleared and re-derived on every pass from the
	// record they created, so a requeue would have nothing to re-open: the lever is
	// REMOVING the withholding, which the same surface that recorded it offers beside it.
	// A token in SkipGuards would offer a second lever that cannot move the row the first
	// one is holding.
	"SkipOperatorExcluded": true,
}

// TestSkipGuards_EveryTerminalSkipTokenIsRequeueable grades [AC-9] of S0150: every
// terminal Skip* token this package declares is in SkipGuards, apart from the mutable
// guards above. Its subject is this package's own vocabulary, so it is asserted in the
// package whose declarations it reads.
//
// A terminal row records the configuration keys its decision actually READ, and a scan
// re-opens a row whose recorded values no longer match the configuration in force. A
// guard that reads no configuration key records nothing read - a verdict no key can move
// (docs/requeue.md) - so for those guards `requeue --guard <token>` is the only lever
// there is, and Requeue answers ErrUnknownGuard for a token outside SkipGuards. A
// terminal token missing from that list is therefore a permanent and silent exclusion:
// no other assertion reds on it, and an operator has nothing to reach for.
//
// The vocabulary is read out of the DECLARATIONS, through the instrument
// TestSkipVocabularyIsDocumented uses, so no hand-kept list can drift behind it: the only
// way to add a token is to add a constant, and a constant added without an entry here
// reds this test. Membership is then checked against the RUNNING SkipGuards rather than
// against a parse of it, so what is graded is the list the verb actually accepts.
func TestSkipGuards_EveryTerminalSkipTokenIsRequeueable(t *testing.T) {
	root, err := corpus.RepoRoot(".")
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	declared := skipVocabulary(t, filepath.Join(root, "internal", "engine"))

	// Anti-vacuity, both halves: a parse that found nothing, or a vocabulary the verb
	// accepts nothing out of, satisfies the loop below without checking anything.
	if len(declared) < 10 {
		t.Fatalf("only %d Skip* constant(s) were parsed out of the engine package (%v) - the "+
			"vocabulary is not being read, so nothing below is being checked",
			len(declared), sortedNames(declared))
	}
	if len(SkipGuards) < 8 {
		t.Fatalf("SkipGuards holds %d entr(ies) (%v) - the requeue vocabulary is not being "+
			"read, so nothing below is being checked", len(SkipGuards), SkipGuards)
	}

	accepted := map[string]bool{}
	for _, tok := range SkipGuards {
		accepted[tok] = true
	}
	var orphans []string
	for name, tok := range declared {
		if mutableSkipGuards[name] || accepted[tok] {
			continue
		}
		orphans = append(orphans, name+" ("+tok+")")
	}
	sort.Strings(orphans)

	if len(orphans) > 0 {
		t.Errorf("%v declared in internal/engine but absent from SkipGuards, and not one of "+
			"the mutable guards SkipGuards is documented as omitting (%v).\n"+
			"A terminal guard that reads no configuration key records nothing read, so no "+
			"configuration change re-opens its rows; `requeue --guard <token>` is the only "+
			"lever and it answers ErrUnknownGuard for a token that is not in this list. The "+
			"exclusion is therefore permanent and silent.\nSkipGuards has: %v",
			orphans, sortedNames(mutableSkipGuards), SkipGuards)
	}
}

// sortedNames is the keys of a map in a stable order, so a failure names the same set the
// same way twice.
func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
