// Command api-schema is the build-side half of the self-describing HTTP surface
// (http-surface H4 and H5, S0128). Three verbs, one document:
//
//	generate   print the surface document this build serves
//	baseline   write it to the committed baseline, then READ BACK what was written
//	diff       compare this build's surface against that baseline and classify every
//	           difference, refusing a breaking one no record names
//
// It is a script rather than a `holdfast` subcommand on purpose: the gate reaches the
// generator through Go tooling, and nothing here is a verb an operator of the daemon runs.
// scripts/compose-image-ref is the precedent - one program, one job, read whole.
//
// The document itself comes from internal/server and from nowhere else, so the bytes this
// program compares are the bytes the running server serves. A second generator here would
// be exactly the hand-maintained parallel document H4 exists to remove.
//
// EXIT STATUS is the interface. Zero means the comparison RAN and found nothing breaking
// that no record names. Non-zero means either a breaking difference or a comparison that
// could not run - a missing, empty or unparseable baseline is the second kind and is never
// reported as "unchanged", because a comparison that did not happen has not passed.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"github.com/NSchatz/holdfast/internal/corpus"
	"github.com/NSchatz/holdfast/internal/server"
)

// baselineFile is the committed document describing the LAST RELEASED version's surface.
// It is not a mirror of HEAD: additive drift between releases is the normal state, and
// nothing here requires the two to be equal.
const baselineFile = "docs/api-schema.json"

// recordFile is the ONLY surface that can make a breaking difference non-fatal. It is a
// record of a decision and not an ignore list: an entry names the EXACT difference and the
// version that will carry it, and an entry matching no difference in the tree fails the
// gate rather than sitting there as a blanket switch.
//
// .govulncheck-suppressions.yaml is the precedent, down to the file's shape.
const recordFile = ".api-schema-breaks.yaml"

func main() {
	if err := run(os.Args[1:]); err != nil {
		for _, line := range strings.Split(strings.TrimRight(err.Error(), "\n"), "\n") {
			fmt.Fprintf(os.Stderr, "::error::%s\n", line)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("api-schema", flag.ContinueOnError)
	root := flags.String("root", "", "repository root (default: the one holding go.mod above the working directory)")
	flags.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: api-schema <generate|baseline|diff> [-root DIR]

  generate   print the surface document this build serves
  baseline   write `+baselineFile+` and read back what was written
  diff       compare this build's surface against `+baselineFile+`
`)
	}
	if len(args) == 0 {
		flags.Usage()
		return errors.New("api-schema: name a verb: generate, baseline or diff")
	}
	verb := args[0]
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	dir, err := repoRoot(*root)
	if err != nil {
		return err
	}

	switch verb {
	case "generate":
		b, err := server.ReferenceSurfaceJSON()
		if err != nil {
			return fmt.Errorf("generating the surface: %w", err)
		}
		_, err = os.Stdout.Write(b)
		return err
	case "baseline":
		return writeBaseline(dir)
	case "diff":
		return diff(dir)
	default:
		flags.Usage()
		return fmt.Errorf("api-schema: %q is not a verb", verb)
	}
}

func repoRoot(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return corpus.RepoRoot(wd)
}

// --- baseline ------------------------------------------------------------------

// writeBaseline writes the baseline and then READS BACK what it wrote, so the command's own
// exit status carries its claim (AC-13). A baseline that does not parse as the document the
// diff gate consumes, or that records no holdfast version, is never left behind for a later
// run to compare against: this exits non-zero and says which.
func writeBaseline(dir string) error {
	doc, err := server.ReferenceSurface()
	if err != nil {
		return fmt.Errorf("generating the surface: %w", err)
	}
	b, err := server.MarshalDocument(doc)
	if err != nil {
		return fmt.Errorf("rendering the surface: %w", err)
	}
	path := filepath.Join(dir, baselineFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", baselineFile, err)
	}

	// The read-back. Nothing above is trusted: the file on disk is what a later gate run
	// will read, so the file on disk is what is checked.
	readBack, why := loadBaseline(dir)
	if why != nil {
		return fmt.Errorf("%s was written and does not read back as a surface document: %w\n"+
			"the file is NOT usable as a baseline", baselineFile, why)
	}
	if readBack.HoldfastVersion == "" {
		return fmt.Errorf("%s records no holdfast version, so nothing says which version's surface it "+
			"describes; the file is NOT usable as a baseline", baselineFile)
	}
	if readBack.Schema != server.SchemaFormat {
		return fmt.Errorf("%s names format %q, not %q", baselineFile, readBack.Schema, server.SchemaFormat)
	}
	if len(readBack.Endpoints) == 0 {
		return fmt.Errorf("%s lists no endpoint at all", baselineFile)
	}
	fmt.Printf("%s: %d endpoints, holdfast %s\n", baselineFile, len(readBack.Endpoints), readBack.HoldfastVersion)
	fmt.Printf("It describes the surface of the version named above. Commit it with the release that " +
		"carries that version - see docs/release.md.\n")
	return nil
}

// loadBaseline reads the committed baseline, distinguishing the three ways it can fail to
// be one (AC-9). Each is an error naming which it saw; none of them is "unchanged".
func loadBaseline(dir string) (server.Document, error) {
	path := filepath.Join(dir, baselineFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return server.Document{}, fmt.Errorf("the baseline %s is MISSING. A comparison that could not "+
				"run has not passed - regenerate it with `make api-schema-baseline`", baselineFile)
		}
		return server.Document{}, fmt.Errorf("the baseline %s could not be READ: %w", baselineFile, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return server.Document{}, fmt.Errorf("the baseline %s is EMPTY. A comparison that could not run "+
			"has not passed - regenerate it with `make api-schema-baseline`", baselineFile)
	}
	doc, err := server.ParseDocument(raw)
	if err != nil {
		return server.Document{}, fmt.Errorf("the baseline %s is NOT PARSEABLE as a surface document: %w",
			baselineFile, err)
	}
	return doc, nil
}

// --- diff ----------------------------------------------------------------------

func diff(dir string) error {
	current, err := server.ReferenceSurface()
	if err != nil {
		return fmt.Errorf("generating this build's surface: %w", err)
	}
	base, err := loadBaseline(dir)
	if err != nil {
		return err
	}

	diffs := compare(base, current)
	records, err := loadRecords(dir)
	if err != nil {
		return err
	}

	var breaking, additive []difference
	for _, d := range diffs {
		if d.breaking {
			breaking = append(breaking, d)
		} else {
			additive = append(additive, d)
		}
	}

	// A record that matches no breaking difference in THIS tree fails, whether or not
	// anything else is wrong. That is what stops the file surviving as a blanket switch
	// once the break it recorded has shipped.
	byID := map[string]bool{}
	for _, d := range breaking {
		byID[d.id] = true
	}
	var stale []string
	covered := map[string]record{}
	for _, r := range records {
		if !byID[r.Difference] {
			stale = append(stale, r.Difference)
			continue
		}
		covered[r.Difference] = r
	}

	var unrecorded []difference
	for _, d := range breaking {
		if _, ok := covered[d.id]; !ok {
			unrecorded = append(unrecorded, d)
		}
	}

	var problems []string
	if len(unrecorded) > 0 {
		// EVERY breaking difference, never the first: a gate that stopped at one would
		// have to be run once per break to find them all.
		lines := []string{fmt.Sprintf(
			"this build BREAKS the surface %s records, in %d way(s), and %s records none of them:",
			baselineFile, len(unrecorded), recordFile)}
		for _, d := range unrecorded {
			lines = append(lines, "  "+d.id)
			lines = append(lines, "      "+d.detail)
		}
		lines = append(lines, "",
			"A breaking change is a version change (http-surface H5). Take the change back, or record",
			"each difference above in "+recordFile+" naming the version that will carry it.")
		problems = append(problems, strings.Join(lines, "\n"))
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		lines := []string{fmt.Sprintf(
			"%s records %d difference(s) this tree does not have. A record is a decision about a "+
				"difference that EXISTS; one that outlives its difference is a standing exemption, so "+
				"delete it:", recordFile, len(stale))}
		for _, s := range stale {
			lines = append(lines, "  "+s)
		}
		problems = append(problems, strings.Join(lines, "\n"))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n\n"))
	}

	fmt.Printf("api-schema: this build's surface against %s (holdfast %s)\n", baselineFile, base.HoldfastVersion)
	if len(additive) == 0 && len(breaking) == 0 {
		fmt.Println("  ok: no difference")
		return nil
	}
	for _, d := range additive {
		fmt.Printf("  additive: %s\n", d.id)
	}
	for _, d := range breaking {
		r := covered[d.id]
		fmt.Printf("  breaking, RECORDED for %s: %s\n", r.Version, d.id)
	}
	fmt.Printf("  ok: %d addition(s), %d recorded break(s), 0 unrecorded break(s)\n", len(additive), len(breaking))
	return nil
}

// difference is one way the current surface differs from the baseline. id is stable and
// greppable, and it is the exact string a record names.
type difference struct {
	id       string
	detail   string
	breaking bool
}

// compare classifies every difference between two surfaces.
//
// BREAKING is exactly http-surface H5's list and nothing wider: an endpoint or method
// removed, a declared response field removed, a field's declaration narrowed, a status code
// removed, or the response recorded for a status code changed. Everything else - every
// addition - is additive and passes.
//
// The document's own format version and the holdfast version are NOT surface and are not
// compared: a version bump alone would otherwise read as a change on every release.
func compare(base, current server.Document) []difference {
	var out []difference
	cur := map[string]server.Endpoint{}
	for _, ep := range current.Endpoints {
		cur[ep.Method+" "+ep.Path] = ep
	}
	seen := map[string]bool{}

	for _, was := range base.Endpoints {
		key := was.Method + " " + was.Path
		seen[key] = true
		is, ok := cur[key]
		if !ok {
			out = append(out, difference{
				id:       "endpoint-removed:" + key,
				detail:   fmt.Sprintf("%s was served and is not served now", key),
				breaking: true,
			})
			continue
		}
		out = append(out, compareResponses(key, was, is)...)
	}
	for _, is := range current.Endpoints {
		key := is.Method + " " + is.Path
		if !seen[key] {
			out = append(out, difference{
				id:     "endpoint-added:" + key,
				detail: fmt.Sprintf("%s is served now and was not", key),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func compareResponses(key string, was, is server.Endpoint) []difference {
	var out []difference
	cur := map[int]server.Response{}
	for _, r := range is.Responses {
		cur[r.Status] = r
	}
	seen := map[int]bool{}
	for _, wasResp := range was.Responses {
		seen[wasResp.Status] = true
		isResp, ok := cur[wasResp.Status]
		if !ok {
			out = append(out, difference{
				id:       fmt.Sprintf("status-removed:%s:%d", key, wasResp.Status),
				detail:   fmt.Sprintf("%s answered %d and does not now", key, wasResp.Status),
				breaking: true,
			})
			continue
		}
		if wasResp.MediaType != isResp.MediaType {
			out = append(out, difference{
				id: fmt.Sprintf("response-changed:%s:%d", key, wasResp.Status),
				detail: fmt.Sprintf("%s %d was %q and is %q", key, wasResp.Status,
					wasResp.MediaType, isResp.MediaType),
				breaking: true,
			})
			continue
		}
		if wasResp.Body.Kind != isResp.Body.Kind {
			out = append(out, difference{
				id: fmt.Sprintf("response-changed:%s:%d", key, wasResp.Status),
				detail: fmt.Sprintf("%s %d carried a %s body and carries a %s body", key, wasResp.Status,
					wasResp.Body.Kind, isResp.Body.Kind),
				breaking: true,
			})
			continue
		}
		out = append(out, compareShape(fmt.Sprintf("%s:%d", key, wasResp.Status), "", wasResp.Body, isResp.Body)...)
	}
	for _, isResp := range is.Responses {
		if !seen[isResp.Status] {
			out = append(out, difference{
				id:     fmt.Sprintf("status-added:%s:%d", key, isResp.Status),
				detail: fmt.Sprintf("%s answers %d now and did not", key, isResp.Status),
			})
		}
	}
	return out
}

// compareShape walks two declarations of the same body in step.
func compareShape(where, at string, was, is server.Shape) []difference {
	var out []difference
	switch was.Kind {
	case "object":
		cur := map[string]server.Field{}
		for _, f := range is.Fields {
			cur[f.Name] = f
		}
		seen := map[string]bool{}
		for _, wf := range was.Fields {
			seen[wf.Name] = true
			isf, ok := cur[wf.Name]
			if !ok {
				out = append(out, difference{
					id:       fmt.Sprintf("field-removed:%s:%s", where, join(at, wf.Name)),
					detail:   fmt.Sprintf("%s declared %s and does not now", where, join(at, wf.Name)),
					breaking: true,
				})
				continue
			}
			if narrowed(wf, isf) {
				out = append(out, difference{
					id: fmt.Sprintf("field-narrowed:%s:%s", where, join(at, wf.Name)),
					detail: fmt.Sprintf("%s %s was %s and is %s", where, join(at, wf.Name),
						describe(wf), describe(isf)),
					breaking: true,
				})
				continue
			}
			out = append(out, compareShape(where, join(at, wf.Name), wf.Type, isf.Type)...)
		}
		for _, f := range is.Fields {
			if !seen[f.Name] {
				out = append(out, difference{
					id:     fmt.Sprintf("field-added:%s:%s", where, join(at, f.Name)),
					detail: fmt.Sprintf("%s declares %s now and did not", where, join(at, f.Name)),
				})
			}
		}
	case "array", "map":
		if was.Elem != nil && is.Elem != nil {
			out = append(out, compareShape(where, at+"[]", *was.Elem, *is.Elem)...)
		}
	}
	return out
}

// narrowed reports whether a field's DECLARATION got stricter or otherwise moved: its kind,
// its nullability or whether it is required. All three are the field's type as a caller
// reads it, and every one of them is H5's "a field's type narrowed".
func narrowed(was, is server.Field) bool {
	return was.Required != is.Required ||
		was.Type.Kind != is.Type.Kind ||
		was.Type.Nullable != is.Type.Nullable
}

func describe(f server.Field) string {
	s := f.Type.Kind
	if f.Type.Nullable {
		s += " or null"
	}
	if f.Required {
		return s + ", always present"
	}
	return s + ", optional"
}

func join(at, name string) string {
	if at == "" {
		return name
	}
	return at + "." + name
}

// --- the record ------------------------------------------------------------------

// record is one deliberate break: the EXACT difference, the version that will carry it, why
// it is being taken and the date it was written down.
type record struct {
	Difference string `yaml:"difference"`
	Version    string `yaml:"version"`
	Reason     string `yaml:"reason"`
	Recorded   string `yaml:"recorded"`
}

var recordKeys = []string{"difference", "version", "reason", "recorded"}

// loadRecords reads the record file. Absent or empty is "no records", which is not an error
// and is not a way to pass anything - the gate still fails on every break nothing names.
// A malformed entry is an error, so the file cannot fail open.
func loadRecords(dir string) ([]record, error) {
	raw, err := os.ReadFile(filepath.Join(dir, recordFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s could not be read: %w", recordFile, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var entries []map[string]any
	if err := yaml.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("%s is not a sequence of records: %w", recordFile, err)
	}
	out := make([]record, 0, len(entries))
	seen := map[string]bool{}
	for i, e := range entries {
		var r record
		for _, k := range recordKeys {
			v, ok := e[k]
			if !ok {
				return nil, fmt.Errorf("%s entry %d carries no %q. An entry names exactly %s",
					recordFile, i+1, k, strings.Join(recordKeys, ", "))
			}
			s, ok := v.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, fmt.Errorf("%s entry %d has an empty or non-text %q", recordFile, i+1, k)
			}
			switch k {
			case "difference":
				r.Difference = s
			case "version":
				r.Version = s
			case "reason":
				r.Reason = s
			case "recorded":
				r.Recorded = s
			}
		}
		for k := range e {
			if !slicesContains(recordKeys, k) {
				return nil, fmt.Errorf("%s entry %d carries unknown key %q. An entry names exactly %s",
					recordFile, i+1, k, strings.Join(recordKeys, ", "))
			}
		}
		if !strings.HasPrefix(r.Version, "v") {
			return nil, fmt.Errorf("%s entry %d names version %q, which is not a version tag (vMAJOR.MINOR.PATCH)",
				recordFile, i+1, r.Version)
		}
		if seen[r.Difference] {
			return nil, fmt.Errorf("%s names the difference %q twice", recordFile, r.Difference)
		}
		seen[r.Difference] = true
		out = append(out, r)
	}
	return out, nil
}

func slicesContains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
