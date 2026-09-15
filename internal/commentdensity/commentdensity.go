// Package commentdensity measures how much of each Go file is comment prose and
// refuses a file that is over the ceiling. It rides `make check` as
// `make comment-density`.
//
// The count is by TOKENS: the Go parser decides what a comment is, so `//` inside
// a string literal is code and a raw string full of comment-looking bytes is code.
// A pattern matcher gets that backwards, and the file it scores as prose is the
// file somebody then guts.
//
// A line carrying both code and a trailing comment counts as CODE. Go's trailing
// comment idiom is ordinary code with a note on it, not a narrated line.
package commentdensity

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The thresholds, in whole percentage points, and the only place they are written.
// They were derived once from this repository's own pre-trim P90; the arithmetic and
// the measurement it rests on are in docs/comment-density.md. WarnPct is spelled out
// rather than computed so the suite has something to check.
const (
	CeilingPct = 50
	WarnPct    = 40
)

// MinLines is the floor under which a file is not measured: below it a single doc
// comment swings the ratio further than any judgement about the file could.
const MinLines = 30

// MaxExemptions caps the escape hatch. Widening the ceiling to fit a file is never
// the answer, and neither is a list long enough to hold whatever fails next.
const MaxExemptions = 3

// Exemption is one file the ceiling does not bind, and why. Each entry is itself
// checked: one naming a vanished path, or a file now under the ceiling, fails.
type Exemption struct {
	Path   string
	Reason string
}

// Exemptions is the whole list. Empty is the healthy state.
var Exemptions = []Exemption{
	{
		Path:   "internal/store/store.go",
		Reason: "declarations only: the comments ARE the ledger's contract, and none of them restates the code",
	},
}

// Counts is a prose and code line tally.
type Counts struct {
	Prose int
	Code  int
}

// Total is every line the ratio is taken over. Blank lines outside a comment
// belong to neither tally.
func (c Counts) Total() int { return c.Prose + c.Code }

// Ratio is the share of measured lines that are prose, 0 for an empty tally.
func (c Counts) Ratio() float64 {
	if c.Total() == 0 {
		return 0
	}
	return float64(c.Prose) / float64(c.Total())
}

// Over reports whether the tally breaks the ceiling. The comparison is integer
// arithmetic so a file exactly on the ceiling is never decided by float rounding.
func (c Counts) Over() bool { return c.Prose*100 > CeilingPct*c.Total() }

// InBand reports whether the tally is at or above the warn band without breaking
// the ceiling.
func (c Counts) InBand() bool { return !c.Over() && c.Prose*100 >= WarnPct*c.Total() }

// Measurement is one file's tally plus the eligibility fact that needs a parse.
type Measurement struct {
	Counts
	Generated bool
}

// File is one measured file, its path relative to the scanned root.
type File struct {
	Path string
	Counts
}

// Report is the whole scan: the eligible files ranked prose-heaviest first, and the
// aggregate tally over them.
type Report struct {
	Root  string
	Files []File
	Counts
}

// Max is the prose-heaviest eligible file.
func (r Report) Max() (File, bool) {
	if len(r.Files) == 0 {
		return File{}, false
	}
	return r.Files[0], true
}

// P90 is the ratio of the eligible file at nearest rank ceil(0.90 * N), the files
// sorted by ratio ascending.
func (r Report) P90() float64 {
	n := len(r.Files)
	if n == 0 {
		return 0
	}
	asc := make([]float64, 0, n)
	for i := n - 1; i >= 0; i-- {
		asc = append(asc, r.Files[i].Ratio())
	}
	rank := int(math.Ceil(0.90 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	return asc[rank-1]
}

// Measure parses one Go source and tallies its lines.
//
// Comment spans come from the parsed comment groups; code lines come from a second
// pass with the scanner in its no-comment mode, which is what makes a multi-line raw
// string count as the code it is. A line claimed by both is code.
func Measure(name string, src []byte) (Measurement, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return Measurement{}, err
	}

	lines := strings.Split(string(src), "\n")
	comment := make([]bool, len(lines)+2)
	code := make([]bool, len(lines)+2)

	for _, group := range file.Comments {
		for _, c := range group.List {
			mark(comment, fset.Position(c.Pos()).Line, fset.Position(c.End()).Line)
		}
	}
	markCode(code, name, src)

	m := Measurement{Generated: generated(file)}
	for i, text := range lines {
		n := i + 1
		blank := strings.TrimSpace(text) == ""
		switch {
		case code[n]:
			if !blank {
				m.Code++
			}
		case comment[n]:
			m.Prose++
		case !blank:
			m.Code++
		}
	}
	return m, nil
}

func mark(lines []bool, from, to int) {
	for n := from; n <= to && n < len(lines); n++ {
		if n >= 0 {
			lines[n] = true
		}
	}
}

// markCode marks every line any non-comment token occupies, including the inner
// lines of a token that spans several - a raw string literal being the one that
// matters here.
func markCode(code []bool, name string, src []byte) {
	fset := token.NewFileSet()
	f := fset.AddFile(name, fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(f, src, nil, 0)
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		start := f.Line(pos)
		end := start
		// An automatically inserted semicolon carries "\n" as its literal and would
		// otherwise claim the following line.
		if tok != token.SEMICOLON {
			end += strings.Count(lit, "\n")
		}
		mark(code, start, end)
	}
}

// generated reports whether the file carries the go generate convention's marker
// ahead of its package clause. Matched on the parsed comments, so the same text
// inside a string literal is what it is: a string.
func generated(file *ast.File) bool {
	const prefix = "// Code generated "
	const suffix = " DO NOT EDIT."
	for _, group := range file.Comments {
		if group.Pos() > file.Package {
			return false
		}
		for _, c := range group.List {
			for _, line := range strings.Split(c.Text, "\n") {
				line = strings.TrimRight(line, "\r")
				if strings.HasPrefix(line, prefix) && strings.HasSuffix(line, suffix) {
					return true
				}
			}
		}
	}
	return false
}

// Scan walks root and measures every eligible Go file: not under a testdata
// directory, not generated, at least MinLines long.
//
// A file it cannot read or cannot parse is a problem, never a skip and never a zero:
// an unmeasured file that scores nothing is how this gate would rot into a permanent
// green. So is a walk that found nothing at all.
func Scan(root string) (Report, error) {
	rep := Report{Root: root}
	var problems []error

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", rel(root, path), err))
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(d.Name()) != ".go" {
			return nil
		}
		name := rel(root, path)
		src, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: cannot be read, so it was not measured: %w", name, err))
			return nil
		}
		m, err := Measure(name, src)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: cannot be parsed, so it was not measured: %w", name, err))
			return nil
		}
		if m.Generated || m.Total() < MinLines {
			return nil
		}
		rep.Files = append(rep.Files, File{Path: name, Counts: m.Counts})
		rep.Prose += m.Prose
		rep.Code += m.Code
		return nil
	})
	if walkErr != nil {
		problems = append(problems, fmt.Errorf("walking %s: %w", root, walkErr))
	}

	sort.Slice(rep.Files, func(i, j int) bool {
		if rep.Files[i].Ratio() != rep.Files[j].Ratio() {
			return rep.Files[i].Ratio() > rep.Files[j].Ratio()
		}
		return rep.Files[i].Path < rep.Files[j].Path
	})
	if len(rep.Files) == 0 {
		problems = append(problems, fmt.Errorf(
			"no eligible Go file under %s - a scan that measured nothing has not passed", root))
	}
	return rep, errors.Join(problems...)
}

func rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}

// ValidateExemptions returns one problem per entry that has outlived its reason, so
// the escape hatch ratchets shut instead of accumulating.
func ValidateExemptions(rep Report, list []Exemption) []string {
	var problems []string
	if len(list) > MaxExemptions {
		problems = append(problems, fmt.Sprintf(
			"the exemption list holds %d entries, more than the %d allowed: %s",
			len(list), MaxExemptions, strings.Join(paths(list), ", ")))
	}
	measured := map[string]File{}
	for _, f := range rep.Files {
		measured[f.Path] = f
	}
	for _, e := range list {
		f, ok := measured[e.Path]
		if !ok {
			if _, err := os.Stat(filepath.Join(rep.Root, filepath.FromSlash(e.Path))); err != nil {
				problems = append(problems, fmt.Sprintf(
					"exemption %q names a path that does not exist - delete the entry", e.Path))
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"exemption %q names a file the scan does not measure, so the exemption grants nothing - delete the entry",
				e.Path))
			continue
		}
		if !f.Over() {
			problems = append(problems, fmt.Sprintf(
				"exemption %q is at %.1f%%, at or under the %d%% ceiling - delete the entry",
				e.Path, f.Ratio()*100, CeilingPct))
		}
	}
	return problems
}

func paths(list []Exemption) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.Path)
	}
	return out
}

// Run scans root, prints the ranked table and the summary, and returns an error
// naming every non-exempt file over the ceiling. A banded file is printed as a
// warning and is not an error.
func Run(root string, out io.Writer, list []Exemption) error {
	rep, scanErr := Scan(root)
	for _, f := range rep.Files {
		fmt.Fprintf(out, "%6.1f%%  %-56s prose %4d  code %4d\n", f.Ratio()*100, f.Path, f.Prose, f.Code)
	}
	fmt.Fprintf(out, "comment-density: %d eligible file(s), aggregate %.1f%% prose, ceiling %d%%, warn %d%%\n",
		len(rep.Files), rep.Ratio()*100, CeilingPct, WarnPct)
	if top, ok := rep.Max(); ok {
		fmt.Fprintf(out, "totals: prose %d, code %d\n", rep.Prose, rep.Code)
		fmt.Fprintf(out, "max: %s at %.1f%%\n", top.Path, top.Ratio()*100)
		// Two decimals: this is the number a ceiling is derived from, and rounding it
		// to the tenth hides which multiple of five it rounds up to.
		fmt.Fprintf(out, "p90: %.2f%%\n", rep.P90()*100)
	}
	if scanErr != nil {
		fmt.Fprintf(out, "FAIL: %v\n", scanErr)
		return scanErr
	}

	exempt := map[string]string{}
	for _, e := range list {
		exempt[e.Path] = e.Reason
	}
	var over []string
	for _, f := range rep.Files {
		switch {
		case f.Over():
			if reason, ok := exempt[f.Path]; ok {
				fmt.Fprintf(out, "exempt: %s at %.1f%% is over the %d%% ceiling - %s\n",
					f.Path, f.Ratio()*100, CeilingPct, reason)
				continue
			}
			line := fmt.Sprintf("%s at %.1f%% is over the %d%% ceiling", f.Path, f.Ratio()*100, CeilingPct)
			over = append(over, line)
			fmt.Fprintf(out, "FAIL: %s\n", line)
		case f.InBand():
			fmt.Fprintf(out, "warn: %s at %.1f%% is at or above the %d%% warn band\n",
				f.Path, f.Ratio()*100, WarnPct)
		}
	}
	problems := ValidateExemptions(rep, list)
	for _, p := range problems {
		fmt.Fprintf(out, "FAIL: %s\n", p)
	}
	if len(over) == 0 && len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("comment-density: %s\nSay it once and delete the restatement; keep what records an invariant, a rationale or a directive",
		strings.Join(append(over, problems...), "\n"))
}

// RepoRoot walks up from dir to the directory holding go.mod. The scan is defined
// relative to the repository, so a gate that took the working directory for the root
// would measure a different set depending on who invoked it.
func RepoRoot(dir string) (string, error) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("commentdensity: no go.mod above %s - cannot locate the repository root", dir)
		}
		d = parent
	}
}
