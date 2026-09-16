package config

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Path filters: which paths under a library root this tool may touch.
//
// `exclude_paths` and `include_paths` are optional list-valued keys, both at the top
// level and inside a `library_roots` entry, both defaulting to empty. They decide
// whether a file is OFFERED to the pipeline at all; nothing here decides how a file is
// encoded, which is what keeps them out of the profile (see profileKnobs).
//
// Three rules govern a match, and each is the safe reading of what an operator wrote:
//
//   - WHOLE PATH, never a substring. A substring filter makes `movies/tv` exclude
//     `movies/tv-archive` and nobody finds out. doublestar's Match requires the pattern
//     to match all of the name, which is exactly the property a substring filter lacks.
//   - A DIRECTORY MATCH COVERS WHAT IS UNDER IT. Whole-path matching alone would make
//     `**/Extras` match a directory nobody offers and leave every file inside it
//     eligible, which is the opposite of what the operator meant. So a pattern is
//     weighed against the file's own path AND against every directory path containing
//     it, down to and including the root.
//   - EXCLUDE WINS. A file matched by both lists is excluded, because the fail-safe
//     direction for a tool that deletes sources is to touch fewer files.
//
// ANCHORING. A pattern that does not begin with `/` is weighed against the path
// RELATIVE to the root that contains the file; one that does begin with `/` is weighed
// against the absolute path. That keeps `/media/movies/4k` - the spelling the operator
// has in front of them - meaningful, and it is what makes "this pattern covers nothing"
// decidable without touching the filesystem: an absolute pattern anchored under no root
// it applies to can never match one (see UnreachablePatterns).
const (
	excludePathsKey = "exclude_paths"
	includePathsKey = "include_paths"
)

// filterKeys is THE enumeration of the path-filter keys, in the order they are
// validated, printed and reported. It is CLOSED, and it is deliberately not part of
// profileKnobs: an entry may carry these keys (so a root can say which of its own paths
// this tool may touch) while a filter edit leaves every root's profile digest where it
// was, because a digest says what DECIDED a file and a filter decides whether a file is
// looked at at all.
var filterKeys = []string{excludePathsKey, includePathsKey}

// FilterKeys returns the closed set of path-filter keys a configuration may carry, in
// the order a resolved filter set is printed. It returns a copy: the set is closed, and
// a caller must not be able to open it.
func FilterKeys() []string { return append([]string(nil), filterKeys...) }

// isFilterKey reports whether key names one of the path-filter keys. A library_roots
// entry may carry these beside the profile knobs.
func isFilterKey(key string) bool {
	for _, k := range filterKeys {
		if k == key {
			return true
		}
	}
	return false
}

// PathFilters is one root's RESOLVED filter set: the patterns in force for it, and
// which of the three layers supplied each list.
//
// The layer is recorded per LIST rather than per pattern because a list is replaced
// whole: an entry that names a key overrides the top-level value entirely, empty list
// included, so no resolved list is ever half of one layer and half of another.
type PathFilters struct {
	// Exclude holds the patterns that keep a file out of this run. It wins over
	// Include.
	Exclude []string
	// Include, when non-empty, is the whole of what this run may offer under this
	// root. Empty - the default, and what an absent key resolves to - leaves every
	// file under the root eligible, which is the behaviour of every configuration
	// written before these keys existed.
	Include []string

	// ExcludeLayer and IncludeLayer say which layer supplied each list, so
	// `holdfast validate` can state not only what the inheritance produced but where
	// each piece of it came from.
	ExcludeLayer Layer
	IncludeLayer Layer
}

// Patterns returns the patterns in force for key. An unknown key yields none rather
// than panicking: every caller names a constant from filterKeys.
func (f PathFilters) Patterns(key string) []string {
	if key == excludePathsKey {
		return f.Exclude
	}
	if key == includePathsKey {
		return f.Include
	}
	return nil
}

// LayerOf reports which layer supplied key's resolved list, defaulting to the built-in
// default for a filter set assembled by hand.
func (f PathFilters) LayerOf(key string) Layer {
	var l Layer
	switch key {
	case excludePathsKey:
		l = f.ExcludeLayer
	case includePathsKey:
		l = f.IncludeLayer
	}
	if l == "" {
		return LayerDefault
	}
	return l
}

// InForce reports whether this root has any filter at all. A root with none offers
// exactly the files it offered before these keys existed.
func (f PathFilters) InForce() bool { return len(f.Exclude) > 0 || len(f.Include) > 0 }

// set records one resolved list and the layer that supplied it.
func (f *PathFilters) set(key string, patterns []string, l Layer) {
	switch key {
	case excludePathsKey:
		f.Exclude, f.ExcludeLayer = patterns, l
	case includePathsKey:
		f.Include, f.IncludeLayer = patterns, l
	}
}

// Offers reports whether p - a path beneath this root - may be offered to the pipeline
// under this root's filters. A path that is not beneath this root is nobody's business
// here and is left alone.
//
// It decides from the PATH and nothing else: it opens nothing, stats nothing and lists
// nothing, which is what lets the enumeration apply it to an entry it has already
// listed without changing which directories that listing covered.
func (r Root) Offers(p string) bool {
	return r.Filters.offers(r.Clean, filepath.Clean(p))
}

// offers is the whole decision, over a cleaned root and a cleaned path.
func (f PathFilters) offers(rootClean, p string) bool {
	if !f.InForce() {
		return true
	}
	rel, ok := relUnder(rootClean, p)
	if !ok {
		return true
	}
	abs := path.Join(filepath.ToSlash(rootClean), rel)
	// Exclude first and unconditionally: a file matched by both lists is excluded.
	if matchesAny(f.Exclude, rootClean, rel, abs) {
		return false
	}
	if len(f.Include) == 0 {
		return true
	}
	return matchesAny(f.Include, rootClean, rel, abs)
}

// relUnder returns p's path relative to rootClean, `/`-separated, and whether p really
// is beneath the root. The root ITSELF is not beneath itself: there is no file there to
// offer, and a relative path of "." would be weighed against every pattern.
func relUnder(rootClean, p string) (string, bool) {
	if p == rootClean || !underRoot(rootClean, p) {
		return "", false
	}
	rel := strings.TrimPrefix(p, rootClean)
	rel = strings.TrimPrefix(rel, string(filepath.Separator))
	return filepath.ToSlash(rel), true
}

// matchesAny reports whether any pattern matches, each weighed against the spelling its
// own anchoring selects.
func matchesAny(patterns []string, rootClean, rel, abs string) bool {
	rootSlash := filepath.ToSlash(rootClean)
	for _, pattern := range patterns {
		target, stop := rel, "."
		if strings.HasPrefix(pattern, "/") {
			target, stop = abs, rootSlash
		}
		if matchesSelfOrAncestor(pattern, target, stop) {
			return true
		}
	}
	return false
}

// matchesSelfOrAncestor reports whether pattern matches the whole of target, or the
// whole of any directory path containing it, walking up as far as stop and no further.
// stop is the root in whichever spelling the pattern is weighed in, and it is INCLUDED:
// a pattern naming the root names everything under it, which is what every other
// directory pattern means here.
func matchesSelfOrAncestor(pattern, target, stop string) bool {
	p := target
	for {
		if matchPattern(pattern, p) {
			return true
		}
		if p == stop {
			return false
		}
		parent := path.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}

// matchPattern is the ONE call this build makes to the matcher, so the language the
// documentation states and the language the filters actually speak cannot diverge.
//
// A pattern that does not parse matches NOTHING here rather than erroring: Validate
// refuses such a pattern at start (see validateFilterList), so by the time anything is
// matched the only reachable reading of an error is "refuse to act", which for a tool
// that deletes sources is the direction to fail in.
func matchPattern(pattern, p string) bool {
	ok, err := doublestar.Match(pattern, p)
	return err == nil && ok
}

// validPattern reports whether s is well formed in the language above. It is the check
// that makes a malformed pattern a startup refusal rather than a first-scan surprise.
func validPattern(s string) bool { return doublestar.ValidatePattern(s) }

// validateFilterList refuses every pattern that is not usable, naming the key and the
// pattern. where names the library root when the list came from that root's own entry
// and is empty for the top-level list, because those are two different fixes in two
// different places in an operator's file.
func validateFilterList(where, key string, patterns []string) error {
	for _, pattern := range patterns {
		if strings.TrimSpace(pattern) == "" {
			return fmt.Errorf("%s%s carries an empty pattern: a filter either names paths or is "+
				"absent, and an empty pattern is neither - remove it, or write the paths it was "+
				"meant to name", where, key)
		}
		if !validPattern(pattern) {
			return fmt.Errorf("%s%s pattern %q is not a valid path pattern: the language is "+
				"doublestar globs (* any run of non-separator characters, ** any number of "+
				"directories as its own path component, ? one character, [class] one character of a "+
				"class, {a,b} alternatives), and an unterminated class or brace is refused rather "+
				"than guessed at", where, key, pattern)
		}
	}
	return nil
}

// validateFilters runs the refusal over every list this configuration carries: the top
// level, and every list a root's own entry supplied. A list a root INHERITED is checked
// once, at the top level, rather than once per root that inherited it.
func (c *Config) validateFilters() error {
	for _, key := range filterKeys {
		if err := validateFilterList("", key, c.topLevelPatterns(key)); err != nil {
			return err
		}
	}
	for _, r := range c.RootProfiles() {
		for _, key := range filterKeys {
			if r.Filters.LayerOf(key) != LayerProfile {
				continue
			}
			where := "library root " + r.Clean + ": "
			if err := validateFilterList(where, key, r.Filters.Patterns(key)); err != nil {
				return err
			}
		}
	}
	return nil
}

// topLevelPatterns is the top-level value of one filter key.
func (c *Config) topLevelPatterns(key string) []string {
	if key == excludePathsKey {
		return c.ExcludePaths
	}
	return c.IncludePaths
}

// TopLevelFilters is the filter set a root with no filters of its own resolves to: the
// top-level value of both keys. It is the middle of the three layers, and the whole of
// what a configuration that writes these keys only once has ever meant.
func (c *Config) TopLevelFilters() PathFilters {
	var f PathFilters
	for _, key := range filterKeys {
		patterns := append([]string(nil), c.topLevelPatterns(key)...)
		l := LayerTopLevel
		if len(patterns) == 0 {
			l = LayerDefault
		}
		f.set(key, patterns, l)
	}
	return f
}

// UnreachablePattern is one configured pattern that can match nothing under any library
// root it applies to: it is an absolute pattern anchored outside every one of them.
//
// It is a REPORT and never a refusal. A pattern legitimately guards a directory that
// does not exist yet, or is empty this week, so "matches nothing today" is not an error
// - but a pattern anchored outside the roots can never match whatever appears on disk,
// and a filter an operator believes is protecting a directory and is not is the failure
// mode worth spending a line on.
type UnreachablePattern struct {
	// Key is the configuration key that carried it.
	Key string
	// Pattern is the pattern itself, as configured.
	Pattern string
	// Roots are the library roots it was weighed against, cleaned, in configuration
	// order.
	Roots []string
}

// String renders the report an operator reads, naming the pattern and the roots.
func (u UnreachablePattern) String() string {
	return fmt.Sprintf("%s pattern %q COVERS NOTHING: it is an absolute pattern anchored outside "+
		"every library root it applies to (%s), so no file this run can reach will ever match it. "+
		"The run is not refused - but if this was meant to protect a directory, it is not protecting "+
		"one. A pattern that does not begin with / is weighed against the path relative to its root.",
		u.Key, u.Pattern, strings.Join(u.Roots, ", "))
}

// UnreachablePatterns reports every configured pattern that can match nothing under any
// root it applies to. It reads the CONFIGURATION alone - no directory is listed and no
// path is stat'd - which is what lets `holdfast validate` answer it without walking the
// library.
//
// A pattern is reported only when it is unreachable from EVERY root it applies to: the
// same top-level pattern may be idle under one root and the whole of the protection
// under another, and reporting it would be reporting a working filter.
func (c *Config) UnreachablePatterns() []UnreachablePattern {
	type weighed struct {
		roots     []string
		reachable bool
	}
	seen := map[string]*weighed{}
	var order []struct {
		key, pattern string
	}
	for _, r := range c.RootProfiles() {
		for _, key := range filterKeys {
			for _, pattern := range r.Filters.Patterns(key) {
				id := key + "\x00" + pattern
				w, ok := seen[id]
				if !ok {
					w = &weighed{}
					seen[id] = w
					order = append(order, struct{ key, pattern string }{key, pattern})
				}
				w.roots = append(w.roots, r.Clean)
				if patternCanReach(pattern, r.Clean) {
					w.reachable = true
				}
			}
		}
	}
	var out []UnreachablePattern
	for _, o := range order {
		w := seen[o.key+"\x00"+o.pattern]
		if w.reachable {
			continue
		}
		out = append(out, UnreachablePattern{Key: o.key, Pattern: o.pattern, Roots: w.roots})
	}
	return out
}

// patternCanReach reports whether pattern could match any path under rootClean, decided
// from the two strings alone.
//
// A relative pattern always can: it is weighed against a path relative to whichever
// root contains the file, and every root has paths under it. An absolute pattern can
// only match paths beginning with its own LITERAL PREFIX - everything before the first
// component carrying a wildcard - so it can reach this root only when that prefix and
// the root lie on one branch: the prefix at or beneath the root (`/media/4k` under
// `/media`), or the root at or beneath the prefix (`/media/**` over `/media/tv`).
func patternCanReach(pattern, rootClean string) bool {
	if !strings.HasPrefix(pattern, "/") {
		return true
	}
	prefix := literalPrefix(pattern)
	root := filepath.ToSlash(rootClean)
	return underSlash(root, prefix) || underSlash(prefix, root)
}

// literalPrefix is the longest leading run of whole path components in an absolute
// pattern that carry no wildcard - the deepest directory every path the pattern can
// match must lie under.
//
// A component carrying any metacharacter ends the prefix, escapes included: a pattern
// this cannot read literally must widen the prefix rather than narrow it, because a
// prefix that is too deep would report a working filter as covering nothing.
func literalPrefix(pattern string) string {
	segments := strings.Split(pattern, "/")
	literal := make([]string, 0, len(segments))
	for _, s := range segments[1:] {
		if strings.ContainsAny(s, `*?[]{}\`) {
			break
		}
		literal = append(literal, s)
	}
	return path.Join("/", path.Join(literal...))
}

// underSlash reports whether child is root or lies beneath it, comparing `/`-separated
// paths on PATH BOUNDARIES: /a/b is under /a and /ab is not.
func underSlash(root, child string) bool {
	if child == root {
		return true
	}
	if root == "/" {
		return strings.HasPrefix(child, "/")
	}
	return strings.HasPrefix(child, root+"/")
}

// stringList reads a raw layered value as a list of patterns, the way every other
// list-valued key in this build is read: a YAML list, a list of strings, or the single
// value an environment override carries. Each element is rendered as the text the
// weakly-typed decoder would produce, so the top-level fields on Config and the
// per-root lists resolved here cannot disagree about what the file said.
func stringList(raw any) []string {
	switch v := raw.(type) {
	case nil:
		return nil
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
		return out
	case string:
		// HOLDFAST_EXCLUDE_PATHS=/mnt/media/4k is one pattern, exactly as
		// HOLDFAST_LIBRARY_ROOTS=/mnt/media has always been one root.
		return []string{v}
	default:
		return []string{fmt.Sprint(v)}
	}
}
