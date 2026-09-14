// Package docscheck is a MECHANICAL check over the repository's own shipped
// documentation. It rides `make check` (via the ordinary test step), so documentation
// that has lost a statement the code's safety story depends on fails the gate that
// everything else fails.
//
// # Why a check at all
//
// The source-mutation guard has a residual window, and the window is different on local
// storage than it is on a network mount. That difference is not a footnote: it is the
// difference between "holdfast will catch a mid-encode rewrite" and "holdfast may not",
// and an operator can only weigh it if it is written down. Documentation obligations
// that nothing enforces are documentation obligations that quietly lapse - so this one
// is enforced by the same target that runs the fixture suite.
//
// # What it checks, and what it deliberately does not
//
// It checks PRESENCE, tokens, and two things about where a file IS. It does not judge
// whether the prose is true, or well written, or complete - no mechanical check can, and
// one that pretended to would either fail good documentation or pass bad. Precisely:
//
//   - Nine fixed anchors must exist: residual-window-local, residual-window-network,
//     reverse-proxy-posture, swap-metadata, non-goal-library-manager, differentiator-gate,
//     swap-invariant, vmaf-pooling and null-is-not-zero. They are FIXED here rather than
//     chosen per-run, because a check free to pick its own anchor is a check that can be
//     made to pass by moving the goalposts.
//   - A statement is PRESENT only when its anchor exists AND at least one non-blank
//     line follows it, before the next anchor or heading, that is not itself a heading
//     or an anchor. An anchor with nothing under it is not a statement.
//   - The network statement must ALSO carry the case-insensitive token "attribute cach"
//     (matching "attribute cache" and "attribute caching"). That is the whole of
//     "attributes the window to the client's attribute caching", mechanically: the
//     network window belongs to the client's cache and a network statement that never
//     says so is not the statement that was owed.
//   - The reverse-proxy statement must carry one token for EACH of the three clauses it
//     owes (see ReverseProxyClauses), and they must be carried by ONE statement: a
//     document saying one clause here and another clause somewhere else has not said
//     what a deploying operator has to read in one place.
//   - The swap-metadata statement owes four clauses under the same one-statement rule
//     (see SwapMetadataClauses).
//   - The library-manager non-goal owes eight clauses under that rule, six of them naming
//     one excluded capability each (see NonGoalLibraryManagerClauses).
//   - The differentiator statement owes nine clauses under that rule (see
//     DifferentiatorClauses) AND is the one anchor checked for what it must NOT say: a
//     uniqueness assertion inside it fails the gate, with two written-out allowances for
//     the strings this repository already carries that contain a guarded phrase while
//     disowning it. See "Why the differentiator anchor exists" below.
//   - The three anchors in AgreeingRules owe their clauses at EVERY occurrence rather
//     than at one: see "Existence, and agreement" below.
//   - A repository-relative link in CLAUDE.md or README.md must resolve to a path in the
//     tree, and a document under docs/design/ must be linked from CLAUDE.md. Those two
//     read the TREE as well as the text, so they take a root and live under CheckRepo
//     rather than Check.
//
// # Existence, and agreement
//
// The six anchors outside AgreeingRules are satisfied by ANY occurrence: the obligation is
// that the statement exists in what the repository ships, so a second document carrying a
// shorter restatement is not an error, and the rule reports a problem only when NO
// occurrence satisfies it. (The differentiator anchor's CLAUSES follow that rule; its
// widening guard does not, and checkNoWidening says why.)
//
// That is the right rule for an obligation about the corpus and the wrong one for an
// argument the documents restate. Under it a second copy may quietly drop a clause while
// the strong copy keeps the gate green, and a reader who lands on the weak copy is missing
// the clause with nothing to tell them so. So for the three anchors in AgreeingRules every
// occurrence must carry every clause, and a copy that does not is named along with the
// clause it does not carry. Faithful repetition still passes - the rule is about what each
// occurrence SAYS, never about how many there are - and which document carries an anchor is
// still not this package's business.
//
// # Why the swap-metadata anchor exists
//
// docs/docker.md has always said what holdfast NEEDS from the filesystem: a `user:` that
// owns the media, write access to the library directories, a single filesystem per
// directory. It never said what a swap CHANGES about the file it publishes - and the swap
// carries the source's mode, carries its ownership only where the process is privileged to,
// carries its modification time unless preserve_mtime is false, and carries no ACL or
// xattr at all. Each of those decides whether a deployment's permissions and its library
// ordering survive a pass, none of them is discoverable from the tool's own output, and
// the last one is the difference between "my ACLs came back" and "my ACLs are gone".
//
// # Why the non-goal-library-manager anchor exists
//
// holdfast runs inside a library somebody else's tool manages, and the two are easy to
// confuse: both walk the same directories and both write to them. The difference is what
// can be PROVED. Every gate here is one judgement made by comparing two video files, and a
// rename, a move, a folder layout, a metadata fetch and a duplicate deletion are all
// filesystem mutations that judgement cannot reach - they are right or wrong for reasons no
// decoder can see. Shipping them would mean shipping mutations with nothing to gate them,
// in the same binary whose whole claim is that it gates what it does. So the boundary is
// ratified rather than merely observed, each excluded capability is named on its own so a
// reader learns which of their problems this tool does not solve, and the statement says
// what to use for that work instead.
//
// # Why the differentiator anchor exists
//
// The README asks a reader to choose this tool over four others, and the whole of what it
// offers them to choose on is one claim: the verify gate is on by default, it is layered,
// it fails closed, and an output that could not be measured is rejected rather than assumed
// good. That claim is narrow, it is true, and it is the kind of sentence that does not stay
// either. It drifts upward one edit at a time - a floor dropped from the list because the
// sentence ran long, a "the only tool that" added because it reads better - and the version
// that survives the drift is the one a reader doing five minutes of research can disprove.
//
// Nothing else in this repository catches that. The fixture suite proves the gate behaves;
// no test has ever read what the README CLAIMS the gate does. So the claim is anchored, its
// clauses are enumerated, and it is the one statement here checked in both directions: it
// reds when a clause goes missing, and it reds when a uniqueness assertion appears. The
// second direction is the one this anchor was actually added for.
//
// # Why the reverse-proxy anchor exists
//
// The read endpoints and the dashboard carry no authentication of their own. On the
// shipped defaults they are protected by the loopback bind, so the day a reverse proxy
// can reach the container that bind protects nothing and the proxy is the only barrier
// left. Three facts decide whether that deployment is safe or quietly open, and none of
// them is discoverable from the API's own responses: the read surface is
// unauthenticated, the mutating endpoints are off until a control token is configured,
// and the page's own requests are root-relative so it must be served at the host root.
// An operator can only weigh those if they are written down, which is the same argument
// the residual-window statements are here for.
//
// # The corpus
//
// Corpus() walks the repository and returns EVERY Markdown file in it. That set is
// recorded here, and it is what the repository ships as documentation, for a reason
// that can be rechecked rather than taken on trust:
//
//   - The repository distributes two things. The container image carries the binary,
//     the bundled ffmpeg and NOTICE - no prose at all - so every document a reader can
//     actually consult is a file in the source tree.
//   - In the source tree, the documents are the Markdown files and nothing else. LICENSE
//     and NOTICE are legal texts, not documentation of behaviour; config.example.yaml
//     and docker-compose.yml are examples that carry comments but state no contract;
//     the Go doc comments are compiled-in developer text, not something a deploying
//     operator reads.
//   - Walking for *.md therefore returns the whole set rather than a selection, and it
//     keeps doing so when a document is ADDED - which is the property a fixed list of
//     filenames would lose the first time somebody wrote docs/nfs.md and put the
//     statement there instead.
//
// Which file carries an anchor is not fixed and is not this package's business - the
// obligation is about the TEXT the repository ships, so a statement anywhere in the corpus
// satisfies it and a statement outside the corpus satisfies nothing however it is
// anchored. (Today the two residual-window statements are in docs/filesystem.md, beside
// the rest of what the storage a library sits on costs, the reverse-proxy posture and
// swap-metadata statements are in docs/docker.md beside the rest of the control surface,
// and the three agreeing statements are one per document under docs/design/; that is a
// choice about where the prose reads best, not a narrowing of the set this package
// checks.)
//
// For an anchor in AgreeingRules "anywhere in the corpus" is still where it may be
// written, and it is no longer the whole rule: a SECOND occurrence is checked too, so
// carrying an anchor in more than one file is an error when those occurrences say
// different things. The two rules under docs/design/ are the only place this package
// takes an interest in a path at all, and neither says which document an anchor belongs
// in.
package docscheck

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The two FIXED anchors. They are the same identifiers the store records as a job's
// residual-window class label, so a reader holding a job's record can grep straight to
// the statement that explains it.
const (
	AnchorLocal   = "residual-window-local"
	AnchorNetwork = "residual-window-network"
)

// AnchorReverseProxy introduces the reverse-proxy posture statement: what changes about
// this daemon's authorization the moment it is reached over something other than
// loopback.
const AnchorReverseProxy = "reverse-proxy-posture"

// ReverseProxyRootPathToken carries the ROOT-PATH constraint, and it is the one token
// here whose absence is a deployment that BREAKS rather than one that is merely
// undocumented: the dashboard requests its own API and its own assets with
// root-relative paths, so a router that strips or rewrites a path prefix serves the
// document and 404s everything under it. It is called out as its own exported constant
// because that is the clause a proxy configuration gets wrong.
const ReverseProxyRootPathToken = "root-relative"

// AnchorSwapMetadata introduces the swap-metadata statement: what a swap CHANGES about
// the file it publishes, beside the existing statement of what holdfast NEEDS from the
// filesystem.
const AnchorSwapMetadata = "swap-metadata"

// Clause is one of the things an anchored statement must say, and the case-insensitive
// token that carries it. The token is what is CHECKED; the clause is what a failure
// message says was missing, so a reader learns what to write rather than which string to
// paste.
//
// Also carries the REST of the tokens one clause owes, for a clause that is compound in
// the criterion that defines it - "the encode goes to a same-directory temp and the swap
// is the only filesystem mutation, an atomic same-filesystem rename" is one obligation
// with three halves, and a single token for it would grade whichever half was picked and
// let the others be dropped in silence. Every token in Token plus Also must be carried,
// by the same statement, for the clause to count as said. Empty for a clause one token
// carries whole, which is every clause of the four rules that predate it.
type Clause struct {
	Token  string
	Clause string
	Also   []string
}

// ReverseProxyClause is Clause under the name it had when the reverse-proxy rule was the
// only rule of this shape. It is an alias rather than a second type so that the two rules
// share one implementation and one set of failure messages.
type ReverseProxyClause = Clause

// ReverseProxyClauses is the whole obligation. Each token is the shortest string that
// carries its clause and could not plausibly be written by accident while meaning
// something else, and two of the three are identifiers this repository already treats
// as fixed (`server_auth_token` is a config key; `root-relative` is how the page's own
// requests are described everywhere else).
var ReverseProxyClauses = []Clause{
	{
		Token: "the only barrier",
		Clause: "that the dashboard and the read API are unauthenticated, " +
			"so a reverse proxy in front of them is the only barrier",
	},
	{
		Token: "server_auth_token",
		Clause: "that the mutating endpoints stay disabled until a control token " +
			"(server_auth_token / HOLDFAST_SERVER_AUTH_TOKEN) is configured",
	},
	{
		Token: ReverseProxyRootPathToken,
		Clause: "that holdfast must be served at the host root, because the page requests " +
			"its own API and assets with root-relative paths",
	},
}

// SwapMetadataClauses is the whole of what the swap-metadata statement owes. Each token is
// the shortest string that carries its clause and could not plausibly be written by
// accident while meaning something else, and the mtime clause is carried by an identifier
// this repository already treats as fixed (`preserve_mtime` is a config key).
//
// The last one is the clause a reader is most likely to need and least likely to guess:
// nothing in holdfast's output says that a POSIX ACL or an SELinux label on the source did
// not survive the swap, and an operator relying on one has no other way to find out.
// The Clause texts here deliberately do NOT open with "that", the way the reverse-proxy
// table's do: the shared failure message already supplies one ("so the statement that %s
// is MISSING"), and a clause that supplies a second reads as a stutter in the one place an
// operator meets it.
var SwapMetadataClauses = []Clause{
	{
		Token:  "carries the source's mode",
		Clause: "the replacement carries the source's mode whatever umask holdfast runs under",
	},
	{
		Token: "only where holdfast is privileged",
		Clause: "ownership is carried only where holdfast is privileged to carry it, " +
			"and is otherwise the holdfast uid",
	},
	{
		Token: "preserve_mtime",
		Clause: "the modification time is carried from the source unless preserve_mtime " +
			"is false",
	},
	{
		Token:  "acls and xattrs are not carried",
		Clause: "ACLs and xattrs are not carried onto the replacement",
	},
}

// AnchorNonGoalLibraryManager introduces the library-manager non-goal: the capabilities
// holdfast excludes permanently, the reason the exclusion is structural rather than a
// scope preference, and what an operator should reach for instead.
const AnchorNonGoalLibraryManager = "non-goal-library-manager"

// NonGoalLibraryManagerClauses is the whole of what the library-manager non-goal owes, and
// it is the longest table here because the obligation is to name each excluded capability
// SEPARATELY. "holdfast is not a library manager" names the category, and a reader deciding
// whether this tool solves their problem cannot tell from a category which of their
// problems it leaves alone - so each capability is its own clause and a statement that
// drops one is a statement that quietly re-opens it.
//
// The last two clauses are the ones that make the boundary durable rather than decorative.
// The REASON clause carries why the line is where it is: a filesystem mutation of this kind
// cannot be judged by comparing two video files, which is the only judgement this project
// makes, so the verify gate everything else rests on has nothing to say about it. A reason
// survives a future contributor's enthusiasm where a preference does not. The INSTEAD
// clause carries the other half: a boundary that names what to use for the excluded work
// reads as a recommendation, and one that does not reads as a refusal.
var NonGoalLibraryManagerClauses = []Clause{
	{
		Token:  "renaming to a scheme",
		Clause: "no file is renamed to a naming scheme",
	},
	{
		Token:  "moving between folders",
		Clause: "no file is moved between folders",
	},
	{
		Token:  "folder organisation",
		Clause: "no folder layout is organised",
	},
	{
		Token:  "metadata fetch",
		Clause: "no metadata is fetched for anything in the library",
	},
	{
		Token:  "duplicate detection",
		Clause: "duplicates are not detected",
	},
	{
		Token: "no deletion",
		Also:  []string{"whose verified replacement passed"},
		Clause: "nothing is deleted but a source whose verified replacement passed, which is the " +
			"one deletion the swap already owns",
	},
	{
		Token: "filesystem mutation",
		Also:  []string{"verify gate", "comparing two video files"},
		Clause: "every excluded capability is a filesystem mutation whose correctness cannot be " +
			"established by comparing two video files, so the verify gate this project is built " +
			"on has nothing to say about it",
	},
	{
		Token: "instead",
		Also:  []string{"plex"},
		Clause: "what an operator should reach for instead, named rather than gestured at, so the " +
			"boundary reads as a recommendation and not as a refusal",
	},
}

// AnchorDifferentiator introduces the one claim holdfast's competitive framing rests on:
// what its verify gate does that a reader is being asked to choose it for. It is anchored
// for a reason the other anchors are not - not because the statement could be DELETED, but
// because it could be WIDENED. A competitive claim drifts upward one edit at a time, and
// the version that survives the drift is the one nothing can defend.
const AnchorDifferentiator = "differentiator-gate"

// DifferentiatorClauses is the whole of what the differentiator statement owes. It is a
// claim about holdfast and about nothing else: no clause here mentions another tool, and no
// competitor shipping a feature can turn this red. That is deliberate. A gate that froze a
// competitor's feature set would red on somebody else's release notes, which is a build
// nobody can fix.
//
// Five of the nine tokens are identifiers this repository already treats as fixed
// (`min_vmaf`, `vmaf_min_pool`, `vmaf_min_chroma`), or phrases the gate's own prose uses
// everywhere else. The three VMAF floors are three clauses rather than one because that is
// how a claim like this is quietly narrowed: a statement that names the mean and the worst
// frame and forgets chroma reads as complete, and chroma is the floor that catches the
// damage the luma-only model is structurally blind to.
var DifferentiatorClauses = []Clause{
	{
		Token:  "default-on",
		Clause: "the gate is on by DEFAULT, not a mode somebody has to find and enable",
	},
	{
		Token:  "layered",
		Also:   []string{"every layer runs"},
		Clause: "the gate is layered and every layer runs, rather than the first one that answers",
	},
	{
		Token:  "fails closed",
		Clause: "the gate FAILS CLOSED",
	},
	{
		Token:  "structural parity",
		Clause: "the structural-parity layer is one of them",
	},
	{
		Token:  "full decode-integrity",
		Clause: "the full decode-integrity layer is one of them",
	},
	{
		Token:  "min_vmaf",
		Clause: "the VMAF mean floor (min_vmaf) is one of them",
	},
	{
		Token:  "vmaf_min_pool",
		Clause: "the VMAF worst-frame floor (vmaf_min_pool) is one of them",
	},
	{
		Token: "vmaf_min_chroma",
		Clause: "the VMAF chroma floor (vmaf_min_chroma) is one of them, which is the floor a " +
			"statement naming only the mean and the worst frame drops while still reading as complete",
	},
	{
		Token:  "cannot be measured is rejected",
		Also:   []string{"assumed good"},
		Clause: "an output that cannot be MEASURED is rejected rather than assumed good",
	},
}

// DifferentiatorUniqueness is the widening guard, and it is the half of this rule that has
// no precedent in this package: every other rule fails on what a statement STOPS saying,
// and this one also fails on what it STARTS saying.
//
// The phrases are the ones an honest narrow claim turns into over a few edits. They are
// matched case-insensitively inside the anchored statement and nowhere else - the guard is
// about the claim this repository makes for itself under its own anchor, not about every
// sentence in the corpus that happens to contain the word "only".
var DifferentiatorUniqueness = []string{
	"the only tool",
	"the only transcoder",
	"the only one that",
	"no other tool",
	"uniquely",
	"the only project",
	"the only self-hosted",
	"nothing else does",
}

// DifferentiatorUniquenessAllowed are the strings that CONTAIN a guarded phrase without
// asserting it, and the reason this guard can be shipped at all.
//
// Both of them are the repository disowning the claim rather than making it - the section
// heading says holdfast is NOT the only tool that verifies, and the closing disclaimer says
// the narrow claim is truer than the wide one. A guard that failed on those two would be
// satisfiable by deleting the sentences that make the claim honest, which is the exact
// outcome it exists to prevent. So they are allowances, written out in full: a paraphrase
// that keeps the guarded phrase and drops the disavowal is not covered, and reds.
var DifferentiatorUniquenessAllowed = []string{
	"We are not the only tool that verifies before it replaces",
	`narrower and truer than "the only one that checks"`,
}

// The three anchors under the AGREEMENT rule. Each introduces an argument the repository
// states in one place and points at from everywhere else, and each is checked at EVERY
// occurrence rather than at the best one - see AgreeingRules.
const (
	AnchorSwapInvariant = "swap-invariant"
	AnchorVmafPooling   = "vmaf-pooling"
	AnchorNullIsNotZero = "null-is-not-zero"
)

// SwapInvariantClauses is the whole of what the swap-invariant statement owes: the
// promise itself, the shape of the only mutation that keeps it, what a refusal leaves
// behind, and the half of the promise that a crash rather than a gate decides.
//
// The durability clause is the one a reader is least likely to supply for themselves. An
// atomic rename is atomic for a concurrent READER and says nothing about a power loss an
// instant later, so a statement that stops at "atomic" has described a weaker property
// than the one holdfast implements, and the kept-source failure mode - the single place
// where holdfast declines to finish a swap it has already proved - would go unwritten.
var SwapInvariantClauses = []Clause{
	{
		Token:  "no source is mutated until a replacement has passed every gate",
		Clause: "no source is mutated until a replacement has passed every gate",
	},
	{
		Token: "same-directory temp",
		Also:  []string{"the only filesystem mutation", "atomic same-filesystem rename"},
		Clause: "the encode goes to a same-directory temp and the swap is the only filesystem " +
			"mutation there is, an atomic same-filesystem rename",
	},
	{
		Token:  "leaves the source byte-for-byte intact",
		Also:   []string{"discards the temp"},
		Clause: "any gate failure discards the temp and leaves the source byte-for-byte intact",
	},
	{
		Token: "durable rather than merely atomic",
		Also:  []string{"fsyncs the parent directory", "the source is kept"},
		Clause: "the rename is made durable rather than merely atomic, the parent directory " +
			"being fsynced after it and the source KEPT when that fsync fails",
	},
}

// VmafPoolingClauses is the whole of what the vmaf-pooling statement owes. Three of its
// four clauses are carried by identifiers this repository already treats as fixed
// (min_vmaf, vmaf_min_pool and vmaf_min_chroma are config keys), which is what makes the
// "all three floors, all on by default" clause checkable at all: a statement that names
// two floors and forgets the third reads as complete.
var VmafPoolingClauses = []Clause{
	{
		Token: "bounds the worst frame",
		Also:  []string{"an average hides local damage"},
		Clause: "the gate bounds the worst frame and not only the average, because an average " +
			"hides local damage",
	},
	{
		Token: "min_vmaf",
		Also:  []string{"vmaf_min_pool", "vmaf_min_chroma", "all on by default"},
		Clause: "the mean floor (min_vmaf), the worst-frame pool floor (vmaf_min_pool) and the " +
			"chroma floor (vmaf_min_chroma) are all on by default and all named",
	},
	{
		Token: "luma-only vmaf model cannot see colour",
		Also:  []string{"the chroma floor is separate"},
		Clause: "the luma-only VMAF model cannot see colour at all, which is why the chroma " +
			"floor is separate",
	},
	{
		Token:  "cannot be measured is rejected",
		Also:   []string{"assumed good"},
		Clause: "an output that cannot be MEASURED is rejected rather than assumed good",
	},
}

// NullIsNotZeroClauses is the whole of what the null-is-not-zero statement owes. The
// middle clause is the reason the rule exists rather than a restatement of it: a zero is
// not a cautious answer but a confident wrong one, and nothing in a response tells a
// client that this zero was never read.
var NullIsNotZeroClauses = []Clause{
	{
		Token:  "explicit null",
		Also:   []string{"never as 0"},
		Clause: "an unreadable figure is reported as an explicit null and never as 0",
	},
	{
		Token:  "a zero would claim the ledger is empty",
		Clause: "a zero would claim the ledger is empty beside rows the caller can already see",
	},
	{
		Token:  "the rows still ship",
		Also:   []string{"never costs an operator the records"},
		Clause: "the rows still ship, so one unreadable figure never costs an operator the records",
	},
}

// Rule is one anchored obligation: the anchor, the name a failure message calls the
// statement, and the clauses it owes.
type Rule struct {
	Anchor  string
	What    string
	Clauses []Clause
}

// AgreeingRules are the anchors held to the AGREEMENT rule, and the list is the whole of
// what that rule applies to. The other five anchors are deliberately NOT here: they keep
// the any-occurrence-satisfies rule they were written under, so this file adds a check
// and removes none. See Check for what the difference buys.
var AgreeingRules = []Rule{
	{AnchorSwapInvariant, "swap-invariant", SwapInvariantClauses},
	{AnchorVmafPooling, "VMAF pooling", VmafPoolingClauses},
	{AnchorNullIsNotZero, "null-is-not-zero", NullIsNotZeroClauses},
}

// NetworkToken is the case-insensitive substring the NETWORK statement must carry. It
// is the shortest string that matches both "attribute cache" and "attribute caching"
// without matching anything else that could be written by accident.
const NetworkToken = "attribute cach"

// anchorRe matches an HTML anchor of the form <a id="..."></a> or <a name="...">, which
// is how a Markdown document that has to be readable on GitHub declares one.
var anchorRe = regexp.MustCompile(`(?i)<a\s+(?:id|name)\s*=\s*"([^"]+)"`)

// headingRe matches an ATX Markdown heading.
var headingRe = regexp.MustCompile(`^\s{0,3}#{1,6}\s`)

// Corpus returns every Markdown file under root, sorted, skipping VCS and vendor
// directories. See the package doc for why this set IS what the repository ships.
func Corpus(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(d.Name()), ".md") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// RepoRoot walks up from dir until it finds the directory holding go.mod - the
// repository root, and therefore the root of the corpus. It is here rather than in the
// test because the corpus is defined RELATIVE TO THE REPOSITORY, and a check that
// guessed its own root from a working directory would be checking a different set of
// files depending on who invoked it.
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
			return "", fmt.Errorf("docscheck: no go.mod above %s - cannot locate the repository root", dir)
		}
		d = parent
	}
}

// Statement is what one anchor introduced, gathered from one file.
type Statement struct {
	Anchor string
	File   string
	// Text is every line after the anchor, up to the next anchor or heading, that is
	// itself neither a heading nor an anchor.
	Text string
}

// Present reports whether the anchor introduced any real text at all.
func (s Statement) Present() bool { return strings.TrimSpace(s.Text) != "" }

// findStatement scans one file for the text an anchor introduces.
//
// The stop conditions are the criterion word for word: text runs to the NEXT ANCHOR or
// the next HEADING, and a line that is itself a heading or an anchor does not count as
// the statement's text. Two anchors back to back therefore introduce nothing, which is
// exactly the failure this has to catch.
func findStatement(path, anchor string) (Statement, error) {
	f, err := os.Open(path)
	if err != nil {
		return Statement{}, err
	}
	defer func() { _ = f.Close() }()

	st := Statement{Anchor: anchor, File: path}
	var b strings.Builder
	found := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		anchors := anchorRe.FindStringSubmatch(line)
		isHeading := headingRe.MatchString(line)

		if !found {
			if len(anchors) == 2 && anchors[1] == anchor {
				found = true
			}
			continue
		}
		if len(anchors) == 2 || isHeading {
			break // the next anchor or heading ends the statement
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if err := sc.Err(); err != nil {
		return Statement{}, err
	}
	if !found {
		return Statement{Anchor: anchor, File: ""}, nil
	}
	st.Text = b.String()
	return st, nil
}

// Check applies the anchor rules to a corpus and returns one problem per line, empty when
// the documentation satisfies them. Two rules, and which one an anchor is held to is
// fixed by AgreeingRules rather than chosen per run.
//
// For the six anchors outside AgreeingRules an anchor appearing in more than one file is not an
// error: the check wants the statement to EXIST in what the repository ships, so ANY
// occurrence that satisfies the rule satisfies the criterion, and the problem is only
// reported when NONE does. The message then names the strongest near-miss, so a failure
// points at a file rather than saying "nowhere".
//
// For the three anchors in AgreeingRules EVERY occurrence must carry every clause. That
// is the difference between enforcing that an argument is written down somewhere and
// enforcing that the documents AGREE about it: under the any-occurrence rule a second
// copy may quietly drop a clause, and a reader who lands on that copy is missing the
// clause with nothing to tell them so.
func Check(files []string) ([]string, error) {
	var problems []string

	local, err := statements(files, AnchorLocal)
	if err != nil {
		return nil, err
	}
	switch {
	case len(local) == 0:
		problems = append(problems, fmt.Sprintf(
			"no shipped document carries the anchor %q - the local residual-window statement is missing", AnchorLocal))
	case !anyPresent(local):
		problems = append(problems, fmt.Sprintf(
			"%s: the anchor %q is present but nothing follows it - an anchor with no text is not a statement",
			local[0].File, AnchorLocal))
	}

	network, err := statements(files, AnchorNetwork)
	if err != nil {
		return nil, err
	}
	switch {
	case len(network) == 0:
		problems = append(problems, fmt.Sprintf(
			"no shipped document carries the anchor %q - the network residual-window statement is missing", AnchorNetwork))
	case !anyPresent(network):
		problems = append(problems, fmt.Sprintf(
			"%s: the anchor %q is present but nothing follows it - an anchor with no text is not a statement",
			network[0].File, AnchorNetwork))
	case !anyCarriesToken(network):
		problems = append(problems, fmt.Sprintf(
			"%s: the network residual-window statement never says %q - the network window is the CLIENT's attribute cache, "+
				"and a statement that does not attribute it there is not the statement that was owed",
			firstPresent(network).File, NetworkToken))
	}

	proxy, err := statements(files, AnchorReverseProxy)
	if err != nil {
		return nil, err
	}
	problems = append(problems, checkClauses(proxy, AnchorReverseProxy, "reverse-proxy posture", ReverseProxyClauses)...)

	metadata, err := statements(files, AnchorSwapMetadata)
	if err != nil {
		return nil, err
	}
	problems = append(problems, checkClauses(metadata, AnchorSwapMetadata, "swap-metadata", SwapMetadataClauses)...)

	nonGoal, err := statements(files, AnchorNonGoalLibraryManager)
	if err != nil {
		return nil, err
	}
	problems = append(problems, checkClauses(
		nonGoal, AnchorNonGoalLibraryManager, "library-manager non-goal", NonGoalLibraryManagerClauses)...)

	differentiator, err := statements(files, AnchorDifferentiator)
	if err != nil {
		return nil, err
	}
	problems = append(problems, checkClauses(
		differentiator, AnchorDifferentiator, "differentiator", DifferentiatorClauses)...)
	problems = append(problems, checkNoWidening(differentiator, "differentiator")...)

	for _, r := range AgreeingRules {
		sts, err := statements(files, r.Anchor)
		if err != nil {
			return nil, err
		}
		problems = append(problems, checkClausesEveryOccurrence(sts, r.Anchor, r.What, r.Clauses)...)
	}

	return problems, nil
}

// CheckRepo applies every rule this package owns to a repository rooted at root: the
// anchor rules of Check, plus the two that need to resolve a PATH and so cannot be
// decided from the corpus text alone.
//
// It takes the root explicitly for the same reason Corpus does. A link is
// repository-relative, and a check that guessed its own root would resolve the same link
// against a different tree depending on who invoked it - which is the failure mode where
// a gate reports green over links it never resolved.
func CheckRepo(root string, files []string) ([]string, error) {
	problems, err := Check(files)
	if err != nil {
		return nil, err
	}
	dead, err := CheckLinks(root, files)
	if err != nil {
		return nil, err
	}
	orphans, err := CheckDesignDocs(root, files)
	if err != nil {
		return nil, err
	}
	return append(append(problems, dead...), orphans...), nil
}

// checkClauses applies the multi-clause rule to one anchor: the statement must exist, must
// have text, and ONE occurrence of it must carry a token for every clause it owes. `what`
// names the statement in a failure message.
//
// Every failure it can report says MISSING, because that is what each of them IS: an
// anchor nobody wrote, an anchor with nothing under it, and a statement that never says
// one of the things - a reader who needs that clause finds nothing in all three cases, and
// a message that called the third "incomplete" would suggest the clause is there in weaker
// words.
func checkClauses(sts []Statement, anchor, what string, clauses []Clause) []string {
	switch {
	case len(sts) == 0:
		return []string{noSuchAnchor(anchor, what)}
	case !anyPresent(sts):
		return []string{bareAnchor(sts[0].File, anchor, what)}
	}

	// ONE statement must carry every clause. Report against the strongest near-miss, so a
	// failure names a file to edit rather than saying "nowhere".
	best := bestClauseStatement(sts, clauses)
	var problems []string
	for _, c := range clauses {
		if !carries(normalize(best.Text), c) {
			problems = append(problems, fmt.Sprintf(
				"%s: the %s statement never says %q, so the statement that %s is MISSING",
				best.File, what, missingToken(normalize(best.Text), c), c.Clause))
		}
	}
	return problems
}

// checkClausesEveryOccurrence is the AGREEMENT rule: the statement must exist, and EVERY
// occurrence of it must carry every clause it owes. It is the same obligation
// checkClauses applies, asked of each occurrence rather than of the best one, so a second
// copy that drops a clause is a failure naming that copy rather than a near-miss the
// stronger copy hides.
//
// A duplicate that merely repeats the statement faithfully passes: the rule is about what
// each occurrence SAYS, never about how many there are. Which document carries an anchor
// is still not this package's business.
func checkClausesEveryOccurrence(sts []Statement, anchor, what string, clauses []Clause) []string {
	if len(sts) == 0 {
		return []string{noSuchAnchor(anchor, what)}
	}
	var problems []string
	for _, s := range sts {
		if !s.Present() {
			problems = append(problems, bareAnchor(s.File, anchor, what))
			continue
		}
		text := normalize(s.Text)
		for _, c := range clauses {
			if carries(text, c) {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"%s: this occurrence of the %s statement never says %q, so the statement that %s is "+
					"MISSING from it - every occurrence of %q carries every clause it owes or the "+
					"shipped documents disagree with each other",
				s.File, what, missingToken(text, c), c.Clause, anchor))
		}
	}
	return problems
}

// checkNoWidening is the guard against a claim growing. Every other rule in this package
// asks whether a statement still SAYS enough; this one asks whether it has started saying
// too much, and it is applied to EVERY occurrence rather than to the best one.
//
// Every occurrence, because the alternative is a hole with a shape: under the
// any-occurrence rule a second document could carry the anchor, assert the wide claim, and
// hide behind the narrow copy that keeps the clause check green. A widening guard with a
// second copy as its exit is not a guard.
//
// The allowances are removed from the text BEFORE the phrases are looked for, so a
// statement that disowns the claim in the repository's own words passes and one that
// paraphrases the disavowal away does not. Removal, rather than "the phrase appears in an
// allowed sentence", because removal is decidable from the text alone: no sentence
// splitter, no judgement about what a clause is attached to.
func checkNoWidening(sts []Statement, what string) []string {
	var problems []string
	for _, s := range sts {
		if !s.Present() {
			continue
		}
		text := normalize(s.Text)
		for _, allowed := range DifferentiatorUniquenessAllowed {
			text = strings.ReplaceAll(text, normalize(allowed), " ")
		}
		for _, phrase := range DifferentiatorUniqueness {
			if !strings.Contains(text, normalize(phrase)) {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"%s: the %s statement asserts %q, which is a uniqueness claim this project cannot "+
					"defend - the claim it can defend is about what holdfast's own gate does, and a "+
					"claim about every other tool in the field is one nobody here has checked. Say the "+
					"narrow thing, or add the disavowal to DifferentiatorUniquenessAllowed and say why",
				s.File, what, phrase))
		}
	}
	return problems
}

// noSuchAnchor and bareAnchor are the two ways a statement can be absent rather than
// incomplete, worded identically for both rules: a reader who needs the clause finds
// nothing in either case, and a gate that described the same absence two ways would read
// as two different problems.
func noSuchAnchor(anchor, what string) string {
	return fmt.Sprintf("no shipped document carries the anchor %q - the %s statement is MISSING", anchor, what)
}

func bareAnchor(file, anchor, what string) string {
	return fmt.Sprintf(
		"%s: the anchor %q is present but nothing follows it - an anchor with no text is not a statement, "+
			"so the %s statement is MISSING",
		file, anchor, what)
}

// carries reports whether normalised text carries every token one clause owes.
func carries(text string, c Clause) bool {
	return missingToken(text, c) == ""
}

// missingToken is the first token of a clause the text does not carry, "" when it carries
// them all. It is what a failure message names, so a compound clause points at the half
// that is actually absent rather than at the whole obligation.
func missingToken(text string, c Clause) string {
	for _, t := range append([]string{c.Token}, c.Also...) {
		if !strings.Contains(text, t) {
			return t
		}
	}
	return ""
}

// bestClauseStatement returns the present statement carrying the most of these clauses.
// A statement carrying all of them makes the check pass; when none does, this is the
// one closest to being the statement that was owed.
func bestClauseStatement(sts []Statement, clauses []Clause) Statement {
	best := firstPresent(sts)
	bestScore := -1
	for _, s := range sts {
		if !s.Present() {
			continue
		}
		score := 0
		for _, c := range clauses {
			if carries(normalize(s.Text), c) {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = s, score
		}
	}
	return best
}

// statements returns every occurrence of an anchor across the corpus, in corpus order.
func statements(files []string, anchor string) ([]Statement, error) {
	var out []Statement
	for _, f := range files {
		st, err := findStatement(f, anchor)
		if err != nil {
			return nil, err
		}
		if st.File == "" {
			continue
		}
		out = append(out, st)
	}
	return out, nil
}

func anyPresent(sts []Statement) bool {
	for _, s := range sts {
		if s.Present() {
			return true
		}
	}
	return false
}

func anyCarriesToken(sts []Statement) bool {
	for _, s := range sts {
		if s.Present() && strings.Contains(normalize(s.Text), NetworkToken) {
			return true
		}
	}
	return false
}

// normalize lower-cases a statement and collapses every run of whitespace to one space
// before the token is looked for. That is not a loosening, it is what "the text carries
// this token" MEANS in Markdown: a single newline inside a paragraph is a space, so
// where the author's line wrap happens to fall would otherwise decide whether the
// documentation passes - and re-flowing a paragraph is not a change to what it says.
// (Observed, not theorised: the shipped paragraph wrapped between "attribute" and
// "cache" and the un-normalised check failed it.)
func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func firstPresent(sts []Statement) Statement {
	for _, s := range sts {
		if s.Present() {
			return s
		}
	}
	return sts[0]
}

// --- the two rules that resolve a PATH ----------------------------------------
//
// Everything above decides its answer from the TEXT of the corpus. These two decide it
// from the text AND the tree, which is why they take a root and why they are not part of
// Check: a link and an orphan are both claims about where a file IS.

// LinkedDocs are the documents whose repository-relative links are resolved. They are the
// two a reader arrives at without being sent - the front door and the agent's brief - so
// a dead link in one of them is a reader who never reaches the document that was written
// for them. Every other document is reached THROUGH these.
var LinkedDocs = []string{"CLAUDE.md", "README.md"}

// IndexDoc is the document a design document has to be reachable from.
const IndexDoc = "CLAUDE.md"

// DesignDir holds one document per argument. A document here that nothing links to is the
// specific failure a consolidation can cause: the argument was moved out of the file that
// used to carry it, into a file no reader is ever sent to.
const DesignDir = "docs/design"

// linkRe matches an inline Markdown link or image and captures its target. Reference-style
// links are not matched because this repository writes none; a link shape nothing in the
// corpus uses would be a rule with no fixture behind it.
var linkRe = regexp.MustCompile(`!?\[[^\]]*\]\(([^)]+)\)`)

// repoLinks returns every REPOSITORY-RELATIVE link target in a file, with any title and
// any #fragment stripped.
//
// Absolute http:, https: and mailto: targets are dropped rather than checked. `make
// check` runs on every pull request and must not depend on a third party being up: a gate
// that reds because somebody else had a bad afternoon is a gate the author cannot fix, and
// the same argument keeps the ffmpeg liveness probe out of it.
//
// The fragment goes before resolution because a fragment names a place INSIDE a document,
// never a file: docs/design/swap.md#swap-invariant is a link to docs/design/swap.md, and
// resolving the whole string would report every anchored cross-reference in the repository
// as dead.
func repoLinks(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range linkRe.FindAllStringSubmatch(string(body), -1) {
		t := strings.TrimSpace(m[1])
		if i := strings.IndexAny(t, " \t"); i >= 0 {
			t = t[:i] // [text](path "title")
		}
		t = strings.Trim(t, "<>")
		low := strings.ToLower(t)
		if strings.HasPrefix(low, "http:") || strings.HasPrefix(low, "https:") || strings.HasPrefix(low, "mailto:") {
			continue
		}
		if i := strings.Index(t, "#"); i >= 0 {
			t = t[:i]
		}
		if t = strings.TrimSpace(t); t == "" {
			continue // a bare #fragment names no file
		}
		out = append(out, t)
	}
	return out, nil
}

// resolve turns one link target into a path on disk. A target is relative to the document
// that carries it, which is what a Markdown reader does with it; a leading slash is read
// from the repository root.
func resolve(root, file, target string) string {
	if strings.HasPrefix(target, "/") {
		return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(target, "/")))
	}
	return filepath.Join(filepath.Dir(file), filepath.FromSlash(target))
}

// rel is a path as a document would write it: relative to the repository root, with
// forward slashes whatever the host uses.
func rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}

// CheckLinks reports every repository-relative link in LinkedDocs whose target is not in
// the tree.
func CheckLinks(root string, files []string) ([]string, error) {
	linked := map[string]bool{}
	for _, d := range LinkedDocs {
		linked[d] = true
	}
	var problems []string
	for _, f := range files {
		name := rel(root, f)
		if !linked[name] {
			continue
		}
		targets, err := repoLinks(f)
		if err != nil {
			return nil, err
		}
		for _, t := range targets {
			if _, err := os.Stat(resolve(root, f, t)); err != nil {
				problems = append(problems, fmt.Sprintf(
					"%s: the link to %q names a path that is not in the tree - a reader following it gets nothing",
					name, t))
			}
		}
	}
	return problems, nil
}

// CheckDesignDocs reports every document under DesignDir that no line in IndexDoc links
// to. An IndexDoc that does not exist links to nothing, so every design document is
// orphaned by it, which is the honest answer rather than a vacuous pass.
func CheckDesignDocs(root string, files []string) ([]string, error) {
	index := filepath.Join(root, IndexDoc)
	targets, err := repoLinks(index)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	linked := map[string]bool{}
	for _, t := range targets {
		linked[rel(root, resolve(root, index, t))] = true
	}

	var problems []string
	for _, f := range files {
		name := rel(root, f)
		if !strings.HasPrefix(name, DesignDir+"/") || linked[name] {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"%s: no line in %s links to it - a rationale document nothing points at is one a reader never reaches, "+
				"which is exactly how moving an argument out of the file that used to carry it loses it",
			name, IndexDoc))
	}
	return problems, nil
}
