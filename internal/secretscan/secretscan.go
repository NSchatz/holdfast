// Package secretscan is the repository's secret scanner (secrets K4): it reads every
// TRACKED file and refuses the tree when one carries an issued credential or is named
// like a credential store. It runs on the pre-commit path and inside `make check`, so a
// hit blocks a commit and a pull request.
//
// # What it catches, and what it deliberately does not
//
// High-signal VENDOR PREFIXES and forbidden FILENAMES only. There is no entropy
// heuristic, and that is a stated trade rather than an omission: this catches an ISSUED
// token, whose shape is fixed by the vendor that issued it, and it does NOT catch a
// password typed into a config file, a bare base64 blob or a hand-made shared secret.
// Nothing may read a clean run as coverage of those. Families() and ForbiddenNames are
// the whole claim.
//
// # Scope
//
// Tracked files only. An untracked file is not on its way into a commit, and .gitignore
// is the control for those. The pre-commit path reads the INDEX rather than the worktree,
// because `git commit` ships the index: a credential staged and then deleted from the
// worktree is what would actually land.
//
// # Why the patterns are composed at runtime
//
// This scanner reads every tracked file, which includes its own source and its own
// graders. A family whose pattern is a plain literal - the PyPI upload token is the only
// one - would therefore flag the file that defines it, and the only fixes are an
// allowlist that grows until the scanner stops scanning, or composing the literal so the
// source never contains it. The repository already settled that question in
// scripts/check-pins-selftest.sh; this follows it. Every other family's pattern contains
// a character class where the credential has payload, so it cannot match itself.
package secretscan

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// PrefixLen is how much of a matched credential a finding may print. A finding has to be
// specific enough to find the line and never enough to use: the report is read by humans,
// pasted into issues and printed into CI logs that outlive the rotation.
const PrefixLen = 8

// maxFileBytes bounds one file. A tracked file larger than this is reported as a file the
// scan could not grade rather than skipped, because a skip is a silent hole.
const maxFileBytes = 32 << 20

// Family is one credential family: a name an operator can act on, and the pattern that
// recognises an issued credential of that kind.
type Family struct {
	Name string
	Re   *regexp.Regexp
}

// Families is the ruleset, in match order. The order matters in one place: sk-ant- is
// also a valid prefix for the looser OpenAI pattern, so Anthropic is listed first and a
// line is reported under the FIRST family that claims it.
//
// It is a function rather than a package variable so that each call gets its own slice and
// no caller can mutate the ruleset out from under another.
func Families() []Family {
	// The PyPI token's pattern is the one literal in the set, composed so this file does
	// not contain the string it searches for. See the package comment.
	pypi := "pypi-" + "AgEIcHlwaS5vcmc"
	return []Family{
		{"AWS access key id", regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`)},
		{"GitHub fine-grained token", regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`)},
		{"GitHub token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36}`)},
		{"Slack webhook", regexp.MustCompile(`hooks\.slack\.com/services/T[A-Za-z0-9_/]{8,}`)},
		{"Slack token", regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`)},
		{"Google API key", regexp.MustCompile(`AIza[A-Za-z0-9_-]{35}`)},
		{"Stripe secret key", regexp.MustCompile(`[sr]k_live_[A-Za-z0-9]{16,}`)},
		{"Anthropic API key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{24,}`)},
		{"OpenAI API key", regexp.MustCompile(`sk-(?:proj-)?[A-Za-z0-9_-]{32,}`)},
		{"PEM private key block", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)},
		{"PyPI upload token", regexp.MustCompile(pypi + `[A-Za-z0-9_\-]{8,}`)},
		// Beyond the transcribed ruleset, and deliberately. It is what makes the ONE name
		// exemption below safe: the only reason a registry config is a forbidden name is
		// that it carries an auth directive, so a rule that finds the directive itself
		// refuses the actual failure whether or not the name rule got to it first.
		{"npm registry auth directive", regexp.MustCompile(`(?i)_auth(?:Token)?[[:space:]]*=[[:space:]]*[^[:space:]]`)},
	}
}

// NameExemption is one tracked PATH the forbidden-NAME rule does not bind, and why.
//
// It exists for a collision the repository cannot resolve any other way, and it is
// built to refuse growth rather than to accommodate it - the same discipline, and for the
// same reason, as internal/commentdensity.Exemptions. What it does NOT do is exempt the
// file from anything else: every credential family above, including the npm auth
// directive, still applies to an exempted path in full. It forgives a NAME, never content.
type NameExemption struct {
	Path   string
	Reason string
}

// MaxNameExemptions caps the escape hatch. Narrowing the ruleset to fit a file is never
// the answer, and an allowlist that may grow is an allowlist that grows until the scanner
// stops scanning. The cap is unchanged by the register being empty: it bounds what a
// future entry may cost, which is the question it was added to answer.
const MaxNameExemptions = 2

// NameExemptions is the whole list, and EMPTY is the healthy state. It held one entry
// while this repository carried a tracked .npmrc beside a package.json - check-pins.sh
// section 8 requires that decision surface wherever a node manifest exists - and holds
// none now that there is no node manifest to decide about. The register stays, and stays
// validated, because section 8 is a tripwire rather than a ban: the moment a manifest
// comes back so does its .npmrc, and an exemption granted then must be checked the same
// way this one was.
var NameExemptions = []NameExemption{}

// ValidateNameExemptions returns one problem per entry that has outlived its reason, so a
// stale or useless exemption fails the scan instead of quietly widening it. paths is the
// enumeration the scan is about to grade.
func ValidateNameExemptions(paths []string, list []NameExemption) []string {
	var problems []string
	if len(list) > MaxNameExemptions {
		problems = append(problems, fmt.Sprintf("the name-exemption list holds %d entries, more than "+
			"the %d allowed - narrowing the ruleset to fit a file is never the answer",
			len(list), MaxNameExemptions))
	}
	present := make(map[string]bool, len(paths))
	for _, p := range paths {
		present[p] = true
	}
	seen := make(map[string]bool, len(list))
	for _, e := range list {
		switch {
		case strings.TrimSpace(e.Reason) == "":
			problems = append(problems, fmt.Sprintf("exemption %q carries no reason - an exemption "+
				"nobody can check is an exemption nobody can retire", e.Path))
		case seen[e.Path]:
			problems = append(problems, fmt.Sprintf("exemption %q is listed twice", e.Path))
		case !present[e.Path]:
			problems = append(problems, fmt.Sprintf("exemption %q names a path this scan does not "+
				"enumerate, so the exemption grants nothing - delete the entry", e.Path))
		default:
			if _, forbidden := forbiddenName(e.Path); !forbidden {
				problems = append(problems, fmt.Sprintf("exemption %q names a path whose filename is "+
					"not forbidden, so the exemption grants nothing - delete the entry", e.Path))
			}
		}
		seen[e.Path] = true
	}
	return problems
}

// ForbiddenNames are the exact base names a tracked file may not have, whatever it
// contains. Each one is a file whose CONVENTION is to hold a credential, so its presence
// in a commit is the finding and reading it would add nothing.
var ForbiddenNames = []string{"id_rsa", "id_ed25519", ".npmrc", ".pypirc"}

// AllowedEnvNames are the dotenv names that are documentation rather than configuration.
// They are the ONLY exceptions, and they are listed here rather than derived from a suffix
// rule so that ".env.example.real" is still refused.
var AllowedEnvNames = []string{".env.example", ".env.sample", ".env.template"}

// Kind says whether a finding is about a file's CONTENT or about its NAME. A reader needs
// the difference: one is removed by editing a line, the other by deleting a file.
type Kind string

const (
	KindContent Kind = "content"
	KindName    Kind = "name"
)

// Finding is one reason the tree is refused.
type Finding struct {
	Kind   Kind
	Path   string
	Line   int // 1-based; 0 for a name finding, which is about no particular line
	Family string
	// Prefix is at most PrefixLen bytes of the matched text and is ALWAYS shorter than
	// the match. Empty for a name finding.
	Prefix string
	// MatchLen is how long the whole match was, so a reader can tell a truncated prefix
	// from a short match without the rest of it being printed.
	MatchLen int
}

// String is the one rendering of a finding, used by the command and asserted by the
// graders, so a message cannot drift between what a human reads and what is tested.
func (f Finding) String() string {
	if f.Kind == KindName {
		return fmt.Sprintf("%s: forbidden filename (%s) - this name is a credential store by "+
			"convention, so it is refused whatever the file contains", f.Path, f.Family)
	}
	return fmt.Sprintf("%s:%d: %s - matched %d bytes starting %q (truncated: the whole match is "+
		"never printed)", f.Path, f.Line, f.Family, f.MatchLen, f.Prefix)
}

// truncate returns at most PrefixLen bytes of match, and never all of it.
func truncate(match string) string {
	n := PrefixLen
	if n >= len(match) {
		n = len(match) - 1
	}
	if n < 0 {
		n = 0
	}
	return match[:n]
}

// ScanContent reports the credential families a file's bytes carry, at most one finding
// per line: the first family that claims a line is the one reported, because the outcome
// (the tree is refused, naming that line) does not depend on enumerating the rest.
func ScanContent(path string, b []byte, families []Family) []Finding {
	var out []Finding
	for i, line := range bytes.Split(b, []byte("\n")) {
		for _, fam := range families {
			if m := fam.Re.Find(line); m != nil {
				out = append(out, Finding{
					Kind: KindContent, Path: path, Line: i + 1, Family: fam.Name,
					Prefix: truncate(string(m)), MatchLen: len(m),
				})
				break
			}
		}
	}
	return out
}

// ForbiddenName reports whether a path's base name is one a tracked file may not have,
// honouring the bounded exemption register. The register is checked LAST, so a path only
// escapes by being named in it and never by the shape of its name.
func ForbiddenName(path string, exemptions []NameExemption) (Finding, bool) {
	f, bad := forbiddenName(path)
	if !bad {
		return Finding{}, false
	}
	for _, e := range exemptions {
		if e.Path == path {
			return Finding{}, false
		}
	}
	return f, true
}

// forbiddenName is the RULE, with no exemption applied: it is what the register validates
// its own entries against, so an entry for a name that was never forbidden cannot sit
// there granting nothing.
func forbiddenName(path string) (Finding, bool) {
	base := filepath.Base(path)
	for _, allowed := range AllowedEnvNames {
		if base == allowed {
			return Finding{}, false
		}
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return Finding{Kind: KindName, Path: path, Family: "dotenv file"}, true
	}
	for _, name := range ForbiddenNames {
		if base == name {
			return Finding{Kind: KindName, Path: path, Family: name}, true
		}
	}
	return Finding{}, false
}

// Source is where a scan gets its files. Two exist: the tracked worktree, and the index a
// commit would ship.
type Source interface {
	// Paths lists every file in scope, repository-relative.
	Paths() ([]string, error)
	// Read returns one path's bytes.
	Read(path string) ([]byte, error)
	// Describe names the scope for the report, so "clean" says what was actually read.
	Describe() string
	// RequiresFiles reports whether an EMPTY enumeration is a failure. It is, for the
	// whole tree: comparing nothing against a ruleset passes every file it never saw. It
	// is not, for a staged set, where an empty commit is legitimate.
	RequiresFiles() bool
}

// Scan runs the whole ruleset over a source. The error return is "this scan could not
// run", which is a different outcome from "this scan found something" and is reported
// under its own exit code.
func Scan(src Source, families []Family) ([]Finding, error) {
	return ScanWith(src, families, NameExemptions)
}

// ScanWith is Scan against an explicit exemption register, which is how the graders drive
// a register the committed one does not contain.
func ScanWith(src Source, families []Family, exemptions []NameExemption) ([]Finding, error) {
	paths, err := src.Paths()
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 && src.RequiresFiles() {
		return nil, fmt.Errorf("%s listed no files. An empty enumeration passes every file it never "+
			"saw, so this is reported as a scan that could not run rather than as a clean tree", src.Describe())
	}
	// A register entry that has outlived its reason is a scan that CANNOT RUN, never a
	// scan that quietly grants more than it was asked to. Skipped for a staged scan, whose
	// enumeration is a handful of files and would report every entry as absent.
	if src.RequiresFiles() {
		if problems := ValidateNameExemptions(paths, exemptions); len(problems) > 0 {
			return nil, fmt.Errorf("the forbidden-name exemption register is not trustworthy: %s",
				strings.Join(problems, "; "))
		}
	}
	var out []Finding
	for _, p := range paths {
		if f, bad := ForbiddenName(p, exemptions); bad {
			out = append(out, f)
		}
		b, err := src.Read(p)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		if len(b) > maxFileBytes {
			return nil, fmt.Errorf("%s is %d bytes, over the %d-byte limit this scan can grade. "+
				"A file it cannot read is a file it cannot clear", p, len(b), maxFileBytes)
		}
		out = append(out, ScanContent(p, b, families)...)
	}
	return out, nil
}

// Git is a Source backed by a real repository.
type Git struct {
	Root string
	// Staged selects the index a commit would ship rather than the worktree.
	Staged bool
}

func (g Git) Describe() string {
	if g.Staged {
		return "the staged changes in " + g.Root
	}
	return "the tracked files of " + g.Root
}

func (g Git) RequiresFiles() bool { return !g.Staged }

func (g Git) Paths() ([]string, error) {
	args := []string{"ls-files", "-z"}
	if g.Staged {
		// ACMR: added, copied, modified, renamed. A DELETION carries no content to scan,
		// and including it would make every `git rm` a scan that could not read its file.
		args = []string{"diff", "--cached", "-z", "--name-only", "--diff-filter=ACMR"}
	}
	out, err := g.git(args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

func (g Git) Read(path string) ([]byte, error) {
	if g.Staged {
		return g.git("show", ":"+path)
	}
	b, err := os.ReadFile(filepath.Join(g.Root, path))
	if err == nil {
		return b, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	// Tracked but absent from the worktree: a staged deletion, mid-`git rm`. The bytes
	// that still exist are the index's, and grading those is right - they are what a
	// commit from here would carry.
	return g.git("show", ":"+path)
}

// git runs one git command inside the repository and fails LOUDLY. git's own exit status
// is propagated as an error rather than swallowed: `fatal: detected dubious ownership`
// (128) is routine in containerised CI, and a swallowed status turns a guard into a no-op
// that reports success.
func (g Git) git(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.Root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s failed in %s: %w: %s",
			strings.Join(args, " "), g.Root, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
