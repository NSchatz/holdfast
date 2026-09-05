// Command govulncheck-gate turns a govulncheck JSON report into a pass/fail verdict,
// subtracting only the advisories recorded in .govulncheck-suppressions.yaml.
//
// It reads the report on STDIN and the suppression file from the working directory.
// There is no flag that selects a report, filters an advisory, or downgrades a verdict:
// the recorded-suppression file is the only surface that can make a reported advisory
// non-fatal, and it is honoured only under the rules enforced in this file.
//
// The verdict, in one place:
//
//   - an advisory govulncheck reports and no record covers      -> fail, naming the id
//   - a record whose advisory the report no longer names        -> fail, naming the id
//   - a record that is malformed in any way                     -> fail, naming the entry
//   - a record covering an advisory that HAS a fixed version    -> fail; that one is
//     remediated at source, by bumping the module or raising the Go toolchain
//   - no suppression file, or an empty one                      -> "no suppressions",
//     which is not an error and is not a way to pass
//   - a report that is empty, truncated or not govulncheck's    -> fail; a tool that did
//     not run is not a tool that found nothing
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

// suppressionFile is the ONLY surface that can make a reported advisory non-fatal.
const suppressionFile = ".govulncheck-suppressions.yaml"

// recordKeys are the four keys an entry must carry - no more, no fewer.
var recordKeys = []string{"id", "reason", "reachability", "recorded"}

var (
	idPattern       = regexp.MustCompile(`^GO-[0-9]{4}-[0-9]+$`)
	recordedPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		for _, line := range strings.Split(strings.TrimRight(err.Error(), "\n"), "\n") {
			fmt.Fprintf(os.Stderr, "::error::%s\n", line)
		}
		os.Exit(1)
	}
}

func run(stdin io.Reader, stdout io.Writer) error {
	report, err := parseReport(stdin)
	if err != nil {
		return err
	}
	records, err := loadRecords(suppressionFile)
	if err != nil {
		return err
	}
	return decide(report, records, stdout)
}

// --- the govulncheck report ---------------------------------------------------------

// frame is one step of a finding's trace, outermost first. govulncheck fills in as much
// of it as it could establish: a module-only frame means "this vulnerable module is in
// the graph", a package frame means "you import it", and a function frame means "your
// code reaches it".
type frame struct {
	Module   string `json:"module"`
	Version  string `json:"version"`
	Package  string `json:"package"`
	Function string `json:"function"`
	Receiver string `json:"receiver"`
}

type finding struct {
	OSV          string  `json:"osv"`
	FixedVersion string  `json:"fixed_version"`
	Trace        []frame `json:"trace"`
}

type osvEntry struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
}

// message is one object of govulncheck's JSON stream. Exactly one field is set per
// object; the ones this gate does not read are still declared so an unrecognised stream
// is distinguishable from an empty one.
type message struct {
	Config   json.RawMessage `json:"config"`
	Progress json.RawMessage `json:"progress"`
	SBOM     json.RawMessage `json:"SBOM"`
	OSV      *osvEntry       `json:"osv"`
	Finding  *finding        `json:"finding"`
}

// advisory is everything the report says about one OSV id, collapsed.
type advisory struct {
	id string
	// summary is govulncheck's own one-line description, when the stream carried it.
	summary string
	// fixedVersion is non-empty when govulncheck named a version that fixes this. Its
	// presence is what R4 turns on: a fix that exists is taken, never recorded.
	fixedVersion string
	// where is the most specific location govulncheck established, rendered for a human.
	where string
	// rank is how specific `where` is: 1 module, 2 package, 3 symbol.
	rank int
}

type report struct {
	advisories map[string]*advisory
}

func parseReport(r io.Reader) (*report, error) {
	dec := json.NewDecoder(r)
	rep := &report{advisories: map[string]*advisory{}}
	summaries := map[string]string{}
	sawConfig := false
	objects := 0

	for {
		var m message
		switch err := dec.Decode(&m); {
		case errors.Is(err, io.EOF):
			// Fall through to the completeness checks below.
		case err != nil:
			return nil, fmt.Errorf("govulncheck's report could not be parsed after %d object(s): %v\n"+
				"an unreadable report is not a clean one - refusing to report green", objects, err)
		default:
			objects++
			if len(m.Config) > 0 {
				sawConfig = true
			}
			if m.OSV != nil && m.OSV.ID != "" {
				summaries[m.OSV.ID] = m.OSV.Summary
			}
			if m.Finding != nil {
				rep.add(m.Finding)
			}
			continue
		}
		break
	}

	// A report with no objects at all is the shape a crashed, killed or misinvoked
	// govulncheck leaves behind, and reading it as "found nothing" is the exact failure
	// this gate exists to prevent.
	if objects == 0 {
		return nil, errors.New("govulncheck produced an empty report - a tool that did not run is not a tool that found nothing")
	}
	if !sawConfig {
		return nil, errors.New("the report carries no govulncheck `config` object - it is truncated or not a govulncheck report at all")
	}

	for id, a := range rep.advisories {
		a.summary = summaries[id]
	}
	return rep, nil
}

func (rep *report) add(f *finding) {
	if f.OSV == "" {
		return
	}
	a := rep.advisories[f.OSV]
	if a == nil {
		a = &advisory{id: f.OSV}
		rep.advisories[f.OSV] = a
	}
	// govulncheck emits a finding per level; any one of them naming a fix means a fix
	// exists, so this never un-sets it.
	if a.fixedVersion == "" {
		a.fixedVersion = f.FixedVersion
	}
	if where, rank := describe(f); rank > a.rank {
		a.where, a.rank = where, rank
	}
}

// describe renders a finding's most specific frame, and how specific it is.
func describe(f *finding) (string, int) {
	best, bestRank := "", 0
	for _, fr := range f.Trace {
		var (
			desc string
			rank int
		)
		switch {
		case fr.Function != "":
			sym := fr.Function
			if fr.Receiver != "" {
				sym = fr.Receiver + "." + fr.Function
			}
			desc, rank = fmt.Sprintf("%s.%s (called)", fr.Package, sym), 3
		case fr.Package != "":
			desc, rank = fr.Package+" (imported)", 2
		case fr.Module != "":
			desc, rank = fr.Module+"@"+fr.Version+" (in the module graph)", 1
		default:
			continue
		}
		if rank > bestRank {
			best, bestRank = desc, rank
		}
	}
	return best, bestRank
}

func (rep *report) ids() []string {
	out := make([]string, 0, len(rep.advisories))
	for id := range rep.advisories {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// --- the recorded suppressions -------------------------------------------------------

type record struct {
	id           string
	reason       string
	reachability string
	recorded     string
	line         int
}

// loadRecords reads the suppression file. An absent file and an empty one both mean
// "no suppressions" and are not errors; anything present and not exactly conformant is.
func loadRecords(path string) ([]record, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s could not be read: %v", path, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s is not valid YAML: %v", path, err)
	}
	// A file that is empty or holds only comments decodes to a document with no content.
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	// An explicit `null` (`~`, or a lone `---`) is the empty sequence written another way.
	if root.Tag == "!!null" {
		return nil, nil
	}
	if root.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s must be a YAML sequence of entries, found %s at line %d",
			path, kindName(root.Kind), root.Line)
	}

	var records []record
	seen := map[string]int{}
	for i, item := range root.Content {
		rec, err := parseRecord(item, i+1)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", path, err)
		}
		if prev, dup := seen[rec.id]; dup {
			return nil, fmt.Errorf("%s: entry #%d (line %d) repeats %s, already recorded by entry #%d - one entry per advisory",
				path, i+1, rec.line, rec.id, prev)
		}
		seen[rec.id] = i + 1
		records = append(records, rec)
	}
	return records, nil
}

func parseRecord(node *yaml.Node, n int) (record, error) {
	if node.Kind != yaml.MappingNode {
		return record{}, fmt.Errorf("entry #%d (line %d) is %s, not a mapping of the four required keys (%s)",
			n, node.Line, kindName(node.Kind), strings.Join(recordKeys, ", "))
	}
	values := map[string]string{}
	var order []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			return record{}, fmt.Errorf("entry #%d (line %d) has a non-scalar key", n, k.Line)
		}
		if _, dup := values[k.Value]; dup {
			return record{}, fmt.Errorf("entry #%d (line %d) repeats the key %q", n, k.Line, k.Value)
		}
		if v.Kind != yaml.ScalarNode || v.Tag == "!!null" {
			return record{}, fmt.Errorf("entry #%d: key %q at line %d must be a non-empty string", n, k.Value, v.Line)
		}
		values[k.Value] = v.Value
		order = append(order, k.Value)
	}

	// Exactly the four keys: a missing one is a record that does not say what it must,
	// and an unknown one is a record whose author believed it said something it did not.
	for _, want := range recordKeys {
		if _, ok := values[want]; !ok {
			return record{}, fmt.Errorf("entry #%d (line %d) is missing the required key %q - the four are: %s",
				n, node.Line, want, strings.Join(recordKeys, ", "))
		}
	}
	for _, got := range order {
		if !known(got) {
			return record{}, fmt.Errorf("entry #%d (line %d) carries the unknown key %q - the four are: %s",
				n, node.Line, got, strings.Join(recordKeys, ", "))
		}
	}
	for _, k := range recordKeys {
		if strings.TrimSpace(values[k]) == "" {
			return record{}, fmt.Errorf("entry #%d (line %d) has an empty %q", n, node.Line, k)
		}
	}

	rec := record{
		id:           values["id"],
		reason:       values["reason"],
		reachability: values["reachability"],
		recorded:     values["recorded"],
		line:         node.Line,
	}
	if !idPattern.MatchString(rec.id) {
		return record{}, fmt.Errorf("entry #%d (line %d) has id %q, which is not an OSV identifier (GO-YYYY-NNNN)",
			n, node.Line, rec.id)
	}
	if !recordedPattern.MatchString(rec.recorded) {
		return record{}, fmt.Errorf("entry #%d (%s, line %d) has recorded %q, which is not an ISO date (YYYY-MM-DD)",
			n, rec.id, node.Line, rec.recorded)
	}
	if _, err := time.Parse("2006-01-02", rec.recorded); err != nil {
		return record{}, fmt.Errorf("entry #%d (%s, line %d) has recorded %q, which is not a real date",
			n, rec.id, node.Line, rec.recorded)
	}
	return rec, nil
}

func known(key string) bool {
	for _, k := range recordKeys {
		if k == key {
			return true
		}
	}
	return false
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.SequenceNode:
		return "a sequence"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.ScalarNode:
		return "a scalar"
	case yaml.AliasNode:
		return "an alias"
	default:
		return "empty"
	}
}

// --- the verdict ---------------------------------------------------------------------

func decide(rep *report, records []record, out io.Writer) error {
	byID := map[string]record{}
	for _, r := range records {
		byID[r.id] = r
	}

	var problems []string
	honoured := make([]string, 0, len(records))

	for _, id := range rep.ids() {
		a := rep.advisories[id]
		rec, recorded := byID[id]
		switch {
		case !recorded:
			problems = append(problems, fmt.Sprintf(
				"%s is reported by govulncheck and is not recorded in %s\n"+
					"    %s\n"+
					"    found at:  %s\n"+
					"    %s\n"+
					"    details:   https://pkg.go.dev/vuln/%s",
				id, suppressionFile, summaryOr(a), whereOr(a), fixLine(a), id))
		case a.fixedVersion != "":
			// R4. A record is for an advisory with nowhere to go; this one has somewhere.
			problems = append(problems, fmt.Sprintf(
				"%s is recorded in %s (line %d) but a FIX EXISTS: %s\n"+
					"    a suppression may not stand in for remediation - bump the module, or raise the Go toolchain, and delete the entry",
				id, suppressionFile, rec.line, a.fixedVersion))
		default:
			honoured = append(honoured, id)
		}
	}

	for _, r := range records {
		if _, still := rep.advisories[r.id]; !still {
			// R1. A suppression may not outlive the advisory it was written for.
			problems = append(problems, fmt.Sprintf(
				"%s is recorded in %s (line %d) but govulncheck no longer reports it - the entry is STALE and must be deleted",
				r.id, suppressionFile, r.line))
		}
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n") + "\n" +
			fmt.Sprintf("govulncheck gate: %d problem(s) - the gate is RED", len(problems)))
	}

	switch {
	case len(rep.advisories) == 0:
		fmt.Fprintln(out, "govulncheck gate: no vulnerabilities reported")
	default:
		fmt.Fprintf(out, "govulncheck gate: %d advisory(ies) reported, all recorded in %s with no fix available:\n",
			len(honoured), suppressionFile)
		for _, id := range honoured {
			fmt.Fprintf(out, "  %s  %s\n", id, summaryOr(rep.advisories[id]))
		}
	}
	return nil
}

func summaryOr(a *advisory) string {
	if a.summary == "" {
		return "(govulncheck reported no summary for this advisory)"
	}
	return a.summary
}

func whereOr(a *advisory) string {
	if a.where == "" {
		return "(govulncheck reported no location for this advisory)"
	}
	return a.where
}

func fixLine(a *advisory) string {
	if a.fixedVersion == "" {
		return "no fix:    govulncheck names no fixed version - if that is still true, record it in " + suppressionFile
	}
	return "fixed in:  " + a.fixedVersion + " - take the fix; do not record it"
}
