// Command check-design-record grades this repository's committed design record and the
// dashboard's identity sources against interface-craft C1 and C2. Part of `make check`.
//
// C1 asks for a record naming the display face, the text face, the accent, the radius
// signature and the shadow signature with one sentence each, and C2 names the defaults an
// unspecified interface converges on and says a repository may have one by naming it in
// the record with its reason. Neither clause can be held by prose: a prohibition a
// machine holds does not decay with session depth, where one the model holds falls to 33%
// by turn 16. So this is the machine.
//
// What it asserts, and the acceptance criterion each answers:
//
//	AC-1   all five identity values are declared, each with a reason sentence
//	AC-2   each names the token that carries it and repeats that token's declared value
//	AC-3   the blocklist scan is green on this tree, and no hit comes from the blocklist's
//	       own declaration or from the record's exceptions - neither file is scanned
//	AC-4   the files actually read are printed, so a reader sees what was measured
//	AC-5   an empty identity-source set is a FAILURE, never a green over nothing
//	AC-6   an unexcepted blocklist entry names the entry, the file and the line
//	AC-7   an entry the record excepts with its reason is allowed
//	AC-8   an exception with no reason sentence grants nothing
//	AC-9   a missing value, a missing token, an undeclared token or a disagreeing value
//	       each says which value is at fault and which of those it was
//	AC-10  an absent, unreadable or unparseable record says which, and never passes
//
// It needs no browser and no node, which is why it can live in `check:` at all: every
// question here is about what a file DECLARES. What the page SHOWS is C3 to C7 and is
// graded by the engine, in the dashboard job. `make check-design-record-selftest` defeats
// every failure mode above and every blocklist entry in turn, against a mutated COPY of
// the tree. A guard nobody tries to defeat is a guard nobody knows works.
//
// WHAT IT DOES NOT DECIDE, said out loud rather than implied. It reads SOURCE TEXT, which
// is what C2 prescribes in those words, so it decides what a file says and never what the
// cascade resolved, what the engine substituted for a face it could not find, or what a
// reader saw. It matches names, so a face reached through a CSS escape sequence, a
// runtime-assembled class or a stylesheet fetched at load time is outside it - the page
// fetches nothing at load time and is generated from committed sources, which is what
// makes that bound acceptable here rather than merely convenient. And it never reads the
// design record's prose: a reason has to BE there and be a sentence, and whether it is a
// GOOD reason is a question for the human reading the diff.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	recordPath    = "docs/design-record.md"
	tokenFilePath = "internal/webui/src/tokens.css"
	blocklistFile = "scripts/check-design-record/blocklist.go"
)

// identitySources are the committed stylesheet and template sources C2 asks for, plus the
// generated document `go:embed` actually ships. The generated one is READ ONLY: `make
// webui-gen` is its single writer and `make webui-stale` inside `check` refuses a stale
// copy, so scanning it here adds a reader and no writer.
var identitySources = []string{
	"internal/webui/src/tokens.css",
	"internal/webui/src/dashboard.css",
	"internal/webui/src/index.html.tmpl",
	"internal/webui/index.html",
}

func main() {
	root := flag.String("root", ".", "repository root whose design record is checked")
	flag.Parse()

	g := &gate{root: *root, out: os.Stdout}
	g.run()
	if g.failed {
		fmt.Fprintln(os.Stderr, "::error::the design record does not hold (see above)")
		os.Exit(1)
	}
	fmt.Fprintln(g.out, "design record ok")
}

type gate struct {
	root   string
	out    io.Writer
	failed bool
}

func (g *gate) path(rel string) string { return filepath.Join(g.root, filepath.FromSlash(rel)) }

func (g *gate) note(format string, a ...any) {
	fmt.Fprintf(g.out, "  ok: %s\n", fmt.Sprintf(format, a...))
}

// bad records a failure and keeps going, so one run reports every independent problem and
// a fix is one edit rather than one edit per re-run.
func (g *gate) bad(format string, a ...any) {
	g.failed = true
	msg := strings.TrimRight(fmt.Sprintf(format, a...), "\n")
	lines := strings.Split(msg, "\n")
	fmt.Fprintf(os.Stderr, "::error::%s\n", lines[0])
	for _, l := range lines[1:] {
		fmt.Fprintf(os.Stderr, "       %s\n", l)
	}
}

func positionOf(file string, line int) string { return fmt.Sprintf("%s:%d", file, line) }

func (g *gate) run() {
	// The record first, and fatally: with no record there is no list of five values to
	// check and no list of exceptions to check hits against, and a check that carried on
	// would report a green over a document nobody can read (AC-10).
	rec, err := loadRecord(g.path(recordPath))
	if err != nil {
		g.bad("%v", err)
		return
	}

	sources, unreadable := g.readSources()
	g.printWhatWasRead(rec, sources)
	for _, problem := range unreadable {
		g.bad("%s", problem)
	}
	if len(sources) == 0 {
		g.bad("NO IDENTITY SOURCE COULD BE READ, so there is nothing to scan and this check has decided nothing.\nA blocklist scan that passes because it read no file is the failure mode this exit exists to close: it would report green on a repository with no dashboard at all.\nThe sources it looked for: %s", strings.Join(identitySources, ", "))
	}

	tokens := tokensOf(sources)
	if tokens == nil {
		g.bad("the token file %s was not among the sources that could be read, so no identity value can be held to the value it names", tokenFilePath)
	}
	g.checkIdentity(rec, tokens)
	g.checkBlocklist(rec, sources)
}

// --- what was read (AC-4) --------------------------------------------------------------

func (g *gate) printWhatWasRead(rec *record, sources []*source) {
	fmt.Fprintf(g.out, "check-design-record: read %d file(s), of which %d identity source(s)\n", len(sources)+1, len(sources))
	fmt.Fprintf(g.out, "  design record:   %s (%d value(s), %d exception(s))\n", recordPath, len(rec.values), len(rec.exceptions))
	for _, s := range sources {
		role := "identity source"
		if s.path == tokenFilePath {
			role = "identity source + token file"
		}
		fmt.Fprintf(g.out, "  %s: %s (%d bytes, %d declaration(s))\n", role, s.path, s.size, len(s.decls))
	}
	fmt.Fprintf(g.out, "  not scanned:     %s declares the blocklist and %s names its exceptions. Neither is an identity source and neither is opened for scanning, so no hit below can arise from either.\n", blocklistFile, recordPath)
}

func (g *gate) readSources() ([]*source, []string) {
	var out []*source
	var problems []string
	for _, rel := range identitySources {
		raw, err := os.ReadFile(g.path(rel))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			problems = append(problems, fmt.Sprintf("the identity source %s IS ABSENT, so the blocklist was not scanned over it", rel))
			continue
		case err != nil:
			problems = append(problems, fmt.Sprintf("the identity source %s CANNOT BE READ (%v), so the blocklist was not scanned over it", rel, err))
			continue
		}
		if len(raw) == 0 {
			problems = append(problems, fmt.Sprintf("the identity source %s IS EMPTY (0 bytes). A file with nothing in it satisfies every entry of the blocklist, which is not the same as passing it", rel))
			continue
		}
		s := readSource(rel, raw)
		out = append(out, &s)
	}
	return out, problems
}

// --- the five identity values (AC-1, AC-2, AC-9) ----------------------------------------

// tokenDecl is every declaration of one custom property in the token file.
type tokenDecl struct {
	name  string
	decls []decl
}

// def is the DEFAULT declaration: the one not inside an at-rule. S6 hand-authors a value
// per theme, so `--accent` is declared twice; the record repeats the default, which is
// what `:root` alone declares and what an engine reporting no preference renders.
func (t tokenDecl) def() (decl, bool) {
	for _, d := range t.decls {
		if !d.underAtRule() {
			return d, true
		}
	}
	return decl{}, false
}

func (t tokenDecl) others() []string {
	var out []string
	for _, d := range t.decls {
		if d.underAtRule() {
			out = append(out, fmt.Sprintf("%s under `%s` = %s", d.where(), strings.Join(d.context, " "), d.value))
		}
	}
	return out
}

func tokensOf(sources []*source) map[string]*tokenDecl {
	for _, s := range sources {
		if s.path != tokenFilePath {
			continue
		}
		out := map[string]*tokenDecl{}
		for _, d := range s.decls {
			if !strings.HasPrefix(d.prop, "--") {
				continue
			}
			t, ok := out[d.prop]
			if !ok {
				t = &tokenDecl{name: d.prop}
				out[d.prop] = t
			}
			t.decls = append(t.decls, d)
		}
		return out
	}
	return nil
}

func (g *gate) checkIdentity(rec *record, tokens map[string]*tokenDecl) {
	known := map[string]bool{}
	for _, n := range identityNames {
		known[n] = true
	}
	for name := range rec.values {
		if !known[name] {
			g.bad("the design record declares the identity value %q, which is not one of the five interface-craft C1 names (%s). Either it is a typo or it is a sixth value nothing grades", name, strings.Join(identityNames, ", "))
		}
	}

	for _, name := range identityNames {
		v, ok := rec.values[name]
		if !ok {
			g.bad("the design record declares NO %q. interface-craft C1 requires all five - %s - and this one is MISSING", name, strings.Join(identityNames, ", "))
			continue
		}
		if !isReason(v.why) {
			g.bad("the identity value %q at %s carries NO REASON SENTENCE. C1 asks for one sentence saying why that value, and %q is not one: a value with no reason is the unstated choice the clause exists to stop", name, positionOf(recordPath, v.line), v.why)
		}
		if v.token == "" {
			g.bad("the identity value %q at %s NAMES NO TOKEN. It has to name the token in %s that carries it, or there is nothing holding this record to the surface it describes", name, positionOf(recordPath, v.line), tokenFilePath)
			continue
		}
		if v.value == "" {
			g.bad("the identity value %q at %s DECLARES NO VALUE, so there is nothing to compare with `%s`", name, positionOf(recordPath, v.line), v.token)
			continue
		}
		if tokens == nil {
			continue // already reported: the token file could not be read
		}
		t, ok := tokens[v.token]
		if !ok {
			g.bad("the identity value %q names the token `%s`, which %s DOES NOT DECLARE. A record naming a token that does not exist is held to nothing", name, v.token, tokenFilePath)
			continue
		}
		d, ok := t.def()
		if !ok {
			g.bad("the identity value %q names the token `%s`, which %s declares only inside an at-rule and never by default, so there is no default value for the record to agree with", name, v.token, tokenFilePath)
			continue
		}
		if normalizeSpace(v.value) != normalizeSpace(d.value) {
			g.bad("the identity value %q DISAGREES WITH THE TOKEN IT NAMES.\n  the record says `%s` is: %s\n  %s declares:       %s\nThe token file is the one writer of a value. Fix the record, or change the token deliberately and then fix the record", name, v.token, v.value, d.where(), d.value)
			continue
		}
		extra := ""
		if o := t.others(); len(o) > 0 {
			extra = " (also declared per theme: " + strings.Join(o, "; ") + ")"
		}
		g.note("%s = `%s` %s, agreeing with %s, with a reason%s", name, v.token, v.value, d.where(), extra)
	}
}

// --- the blocklist scan (AC-3, AC-6, AC-7, AC-8) -----------------------------------------

func (g *gate) checkBlocklist(rec *record, sources []*source) {
	catalog := map[string]string{}
	for _, s := range sources {
		for _, d := range s.decls {
			if strings.HasPrefix(d.prop, "--") {
				if _, seen := catalog[d.prop]; !seen {
					catalog[d.prop] = d.value
				}
			}
		}
	}

	known := map[string]bool{}
	for _, e := range blocklist {
		known[e.id] = true
	}
	for _, id := range rec.order {
		if !known[id] {
			g.bad("the design record excepts %q, which is NOT A BLOCKLIST ENTRY. It grants nothing and the entry somebody meant is still refused. The entries are: %s", id, strings.Join(entryIDs(), ", "))
		}
	}

	hits := map[string][]hit{}
	for _, e := range blocklist {
		for _, s := range sources {
			hits[e.id] = append(hits[e.id], e.detect(s, catalog)...)
		}
	}

	var allowed, unused []string
	clean := true
	for _, e := range blocklist {
		found := hits[e.id]
		ex, excepted := rec.exceptions[e.id]
		switch {
		case len(found) == 0:
			if excepted {
				unused = append(unused, e.id)
			}
		case excepted && isReason(ex.why):
			allowed = append(allowed, fmt.Sprintf("%s (%d hit(s))", e.id, len(found)))
		case excepted:
			clean = false
			g.bad("the design record EXCEPTS `%s` WITH NO REASON SENTENCE at %s, so the exception grants nothing and the %d hit(s) below stand. C2's last sentence buys an entry with a reason; %q is not one.\n%s",
				e.id, positionOf(recordPath, ex.line), len(found), ex.why, hitList(found))
		default:
			clean = false
			for _, h := range found {
				g.bad("%s: `%s` IS NOT EXCEPTED by the design record - %s.\ninterface-craft C2 refuses %s. Either take it out of the surface, or name `%s` under `%s` in %s with one sentence saying why this repository has it",
					positionOf(h.file, h.line), h.entry, h.detail, whatOf(h.entry), h.entry, exceptionHeading, recordPath)
			}
		}
	}

	if clean {
		g.note("no unexcepted blocklist entry in %d identity source(s); all %d entries were looked for (%s)",
			len(sources), len(blocklist), strings.Join(entryIDs(), ", "))
	}
	if len(allowed) > 0 {
		sort.Strings(allowed)
		g.note("allowed by the design record, each with its reason: %s", strings.Join(allowed, ", "))
	}
	if len(unused) > 0 {
		sort.Strings(unused)
		fmt.Fprintf(g.out, "  note: the record excepts %s, which this scan did not find. An exception that grants nothing can be deleted\n", strings.Join(unused, ", "))
	}
}

func whatOf(id string) string {
	for _, e := range blocklist {
		if e.id == id {
			return e.what
		}
	}
	return id
}

func hitList(hits []hit) string {
	var lines []string
	for _, h := range hits {
		lines = append(lines, fmt.Sprintf("  %s: %s", positionOf(h.file, h.line), h.detail))
	}
	return strings.Join(lines, "\n")
}
