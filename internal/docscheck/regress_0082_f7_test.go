// REFUTER ARTIFACT for S0082, finding F7. The defect it caught is fixed - the token is in
// engine.SkipGuards - and this is what keeps the next one from repeating it.
//
// It lives in internal/docscheck rather than internal/engine on purpose: it reads SOURCE
// instead of linking the package, so it still runs on a tree whose internal/engine does
// not compile. That is the tree the finding was gathered on, where a new guard's call site
// and a rewritten because() had merged cleanly into something that did not build, and the
// one place a vocabulary check has to keep working is the tree nobody can compile.

package docscheck

// A terminal skip token that is NOT in engine.SkipGuards is an exclusion no operator can
// lift. S0080 made that precise: every terminal row records the configuration keys its
// decision actually READ, and a scan re-opens a row whose recorded values no longer match
// the configuration in force. A guard that read no configuration records nothing read -
// "a verdict no key can move" (docs/requeue.md) - so for those guards `requeue --guard
// <token>` is the ONLY lever there is. engine.SkipGuards is the closed vocabulary that
// verb accepts, and its own comment says what may be left out of it: the two MUTABLE
// guards, "cleared and re-derived on every pass already, so there is nothing for a
// requeue to re-open".
//
// S0082's `multi-video-stream` is neither. It reads no configuration key (a source's
// video-stream shape is a property of the file), so no configuration change re-opens it,
// and it is not mutable, so the exemption does not apply. Absent from SkipGuards it is a
// permanent exclusion with no lever at all.
//
// The vocabulary is read out of the DECLARATIONS, the same instrument S0082's own
// TestSkipVocabularyIsDocumented uses, so no hand-kept list can drift behind it.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// mutableGuards are the only tokens engine.SkipGuards is documented as omitting: the ones
// cleared and re-derived on every pass, where a requeue would have nothing to re-open.
// Named here as the EXEMPTION so that adding a token to this list is a visible decision
// and not a silent one.
var mutableGuards = map[string]bool{
	"SkipHardlinked":          true,
	"SkipUndoRetentionFailed": true,
}

func TestRegress0082F7_EveryTerminalSkipTokenIsRequeueable(t *testing.T) {
	root, err := RepoRoot(".")
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	dir := filepath.Join(root, "internal", "engine")

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}

	var declared []string // every Skip* constant NAME the package declares
	var guards []string   // the element names of var SkipGuards
	guardsFound := false
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					// The Skip* constants. Collected by NAME, not by value: on the
					// current pin SkipRestoredOriginal is an identifier rather than a
					// string literal, and a collector keyed on literals drops it.
					if gd.Tok == token.CONST {
						for _, name := range vs.Names {
							if strings.HasPrefix(name.Name, "Skip") {
								declared = append(declared, name.Name)
							}
						}
						continue
					}
					// var SkipGuards = []string{...}
					if gd.Tok != token.VAR {
						continue
					}
					for i, name := range vs.Names {
						if name.Name != "SkipGuards" || i >= len(vs.Values) {
							continue
						}
						lit, ok := vs.Values[i].(*ast.CompositeLit)
						if !ok {
							continue
						}
						guardsFound = true
						for _, el := range lit.Elts {
							if id, ok := el.(*ast.Ident); ok {
								guards = append(guards, id.Name)
							}
						}
					}
				}
			}
		}
	}

	if !guardsFound {
		t.Skipf("this tree declares no engine.SkipGuards, so the requeue vocabulary does "+
			"not exist in it and there is nothing to register against (parsed %d Skip* "+
			"constant(s) in %s)", len(declared), dir)
	}

	// Anti-vacuity, both halves. A parse that found nothing would satisfy the loop below.
	if len(declared) < 10 {
		t.Fatalf("only %d Skip* constant(s) parsed out of %s (%v) - the vocabulary is not "+
			"being read, so nothing below is being checked", len(declared), dir, declared)
	}
	if len(guards) < 8 {
		t.Fatalf("only %d entr(ies) parsed out of SkipGuards (%v) - the requeue vocabulary "+
			"is not being read, so nothing below is being checked", len(guards), guards)
	}

	inGuards := map[string]bool{}
	for _, g := range guards {
		inGuards[g] = true
	}
	var orphans []string
	for _, name := range declared {
		if mutableGuards[name] || inGuards[name] {
			continue
		}
		orphans = append(orphans, name)
	}
	sort.Strings(orphans)

	if len(orphans) > 0 {
		t.Errorf("%v %s declared in internal/engine but absent from engine.SkipGuards, and "+
			"not one of the mutable guards SkipGuards is documented as omitting (%v).\n"+
			"A terminal guard that reads no configuration key records nothing read, so no "+
			"configuration change re-opens its rows; `requeue --guard <token>` is the only "+
			"lever, and it answers ErrUnknownGuard for a token that is not in this list. "+
			"The exclusion is therefore permanent and silent: nothing in `make check` fails "+
			"today with the token missing.\n"+
			"SkipGuards has: %v",
			orphans, plural(len(orphans)), keys(mutableGuards), guards)
	}
}

func plural(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
